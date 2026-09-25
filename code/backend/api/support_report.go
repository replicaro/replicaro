package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/local/replicaro/database"
	"github.com/local/replicaro/models"
	"github.com/local/replicaro/operationlog"
	"github.com/local/replicaro/storageavailability"
)

const (
	supportReportPath                     = "/api/support/report"
	supportReportIssueLimit               = 200
	supportReportByteLimit                = 256 << 10
	supportDetailByteLimit                = 8 << 10
	supportRawDetailByteLimit             = supportDetailByteLimit * 2
	supportOperationStepLimit             = 128
	supportRedactionContextValueLimit     = 20_000
	supportRedactionContextByteLimit      = 8 << 20
	supportRedactionContextValueByteLimit = supportRawDetailByteLimit - supportDetailByteLimit
)

func init() {
	endpointMethods[supportReportPath] = []string{http.MethodGet}
}

type supportResponseRecorder struct {
	http.ResponseWriter
	status                   int
	err                      error
	expected                 bool
	body                     strings.Builder
	storageObservationDetail string
}

func (recorder *supportResponseRecorder) Unwrap() http.ResponseWriter { return recorder.ResponseWriter }

func (recorder *supportResponseRecorder) WriteHeader(status int) {
	if recorder.status == 0 {
		recorder.status = status
	}
	recorder.ResponseWriter.WriteHeader(status)
}

func (recorder *supportResponseRecorder) Write(data []byte) (int, error) {
	if recorder.status == 0 {
		recorder.status = http.StatusOK
	}
	written, err := recorder.ResponseWriter.Write(data)
	if recorder.status >= http.StatusBadRequest && written > 0 && recorder.body.Len() < supportDetailByteLimit {
		remaining := supportDetailByteLimit - recorder.body.Len()
		captured := written
		if captured > remaining {
			captured = remaining
		}
		recorder.body.Write(data[:captured])
	}
	return written, err
}

func markSupportResponseError(w http.ResponseWriter, err error, expected bool) {
	if recorder, ok := w.(*supportResponseRecorder); ok {
		recorder.err = err
		recorder.expected = recorder.expected || expected
	}
}

// Only a storage admission caller may add these fixed labels. Arbitrary
// observer errors can contain paths or native output and never enter activity.
func markSupportStorageObservation(w http.ResponseWriter, stage string, err error) {
	var binding *storageBindingFailure
	var macAccess *macOSAccessError
	// Preview and retry can also fail native work. A deadline from that work
	// is not evidence of a storage observation, even at a storage stage.
	if err == nil || (!errors.As(err, &binding) && !errors.As(err, &macAccess)) ||
		errors.Is(err, context.Canceled) ||
		!errors.Is(err, storageavailability.ErrObserverUnavailable) && !errors.Is(err, context.DeadlineExceeded) {
		return
	}
	switch stage {
	case "vault_create_destination", "vault_connect_destination", "job_create_source", "job_bind_source":
	default:
		return
	}
	reason := storageavailability.ResolutionReason(err)
	switch reason {
	case database.AvailabilityReasonStorageMissing, database.AvailabilityReasonObservationFailed,
		database.AvailabilityReasonObservationTimeout, database.AvailabilityReasonIdentityMismatch:
	default:
		return
	}
	if recorder, ok := w.(*supportResponseRecorder); ok {
		recorder.storageObservationDetail = "stage=" + stage + " reason=" + reason
	}
}

func recordSupportResponseErrors(db *sql.DB, next http.Handler) http.Handler {
	if db == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := &supportResponseRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		if !eligibleSupportResponseError(r, recorder) {
			return
		}
		category := "api"
		if supportVaultCreateRequest(r) {
			category = "vault_create"
		} else if supportVaultConnectRequest(r) {
			category = "vault_connect"
		}
		message := fmt.Sprintf(
			"Support diagnostic: method=%s category=%s status=%d",
			supportMethodLabel(r.Method), category, recorder.status,
		)
		if detail := supportResponseErrorDetail(recorder); detail != "" {
			message += " detail=" + quoteBoundedSupportDetail(detail)
		}
		if recorder.storageObservationDetail != "" {
			// Fixed storage labels belong in the export, while the failed
			// request itself remains the user's visible result.
			_ = database.LogSupport(db, message)
		} else {
			_ = database.LogError(db, message)
		}
	})
}

func supportResponseErrorDetail(recorder *supportResponseRecorder) string {
	if recorder.storageObservationDetail != "" {
		return recorder.storageObservationDetail
	}
	if recorder.err != nil {
		value, truncated := limitSupportRawDetail(recorder.err.Error())
		return strings.TrimSpace(supportBoundedRawDetail(value, truncated))
	}
	var response struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(recorder.body.String()), &response); err != nil {
		return ""
	}
	return strings.TrimSpace(strings.ToValidUTF8(response.Error, "�"))
}

func supportMethodLabel(method string) string {
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return method
	default:
		return "OTHER"
	}
}

func eligibleSupportResponseError(r *http.Request, response *supportResponseRecorder) bool {
	if r.URL.Path == supportReportPath || !strings.HasPrefix(r.URL.Path, "/api/") {
		return false
	}
	if response.expected && response.storageObservationDetail == "" ||
		(response.storageObservationDetail == "" &&
			(errors.Is(response.err, context.Canceled) || errors.Is(response.err, context.DeadlineExceeded))) ||
		errors.Is(r.Context().Err(), context.Canceled) || errors.Is(r.Context().Err(), context.DeadlineExceeded) {
		return false
	}
	switch response.status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusProxyAuthRequired:
		return false
	}
	if response.status >= http.StatusInternalServerError {
		return true
	}
	if response.storageObservationDetail != "" &&
		(response.status == http.StatusBadRequest || response.status == http.StatusConflict) {
		return true
	}
	if response.status != http.StatusConflict ||
		(!supportVaultCreateRequest(r) && !supportVaultConnectRequest(r)) {
		return false
	}
	return true
}

func supportVaultCreateRequest(r *http.Request) bool {
	return r.URL.Path == "/api/repositories" && r.Method == http.MethodPost
}

func supportVaultConnectRequest(r *http.Request) bool {
	return r.URL.Path == "/api/vaults/connect" ||
		r.URL.Path == "/api/vaults/connect/preview" ||
		r.URL.Path == "/api/vaults/connect/retry"
}

type supportIssue struct {
	timestamp string
	time      time.Time
	source    string
	detail    string
	sequence  int
}

func truncateSupportText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	suffix := "…[truncated]"
	cut := limit - len(suffix)
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut] + suffix
}

var (
	supportPrivateKeyStartPattern      = regexp.MustCompile(`(?i)-----BEGIN ([A-Z0-9 ]*PRIVATE KEY)-----`)
	supportAuthorizationPattern        = regexp.MustCompile(`(?i)\b(authorization\s*[:=]\s*)[^\r\n]+`)
	supportCredentialSchemePattern     = regexp.MustCompile(`(?i)\b(basic|bearer|digest)\s+\S+`)
	supportCredentialAssignmentPattern = regexp.MustCompile(`(?i)\b((?:["'])?(?:password|passphrase|token|secret|api[_ -]?key|access[_ -]?key|secret[_ -]?key|account[_ -]?key|storage[_ -]?key|application[_ -]?key|private[_ -]?key|credential|cookie|[A-Z][A-Z0-9_]*(?:PASSWORD|PASSPHRASE|TOKEN|SECRET|ACCESS_KEY|ACCOUNT_KEY|STORAGE_KEY|APPLICATION_KEY|PRIVATE_KEY|CREDENTIAL|COOKIE)[A-Z0-9_]*|[A-Z][A-Z0-9_]*_(?:PASS|KEY))(?:["'])?\s*[:=]\s*)(?:"[^"]*"|'[^']*'|[^\s,;]+)`)
	supportEmailPattern                = regexp.MustCompile(`\b[A-Za-z0-9.!#$%&'*+/=?^_{}|~-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+\b`)
	supportStructuredHost              = regexp.MustCompile(`(?i)((?:["'])?(?:host|hostname|computer|computer[_ -]?name)(?:["'])?\s*[:=]\s*)(?:"[^"\r\n]*"|'[^'\r\n]*'|[A-Za-z0-9][A-Za-z0-9._-]*)`)
	supportURLPattern                  = regexp.MustCompile(`(?i)\b(?:https?|sftp|s3|file)://[^\r\n]*`)
	supportNativeRemote                = regexp.MustCompile(`(?i)\b(?:[A-Za-z][A-Za-z0-9_-]*:[^\r\n]*/[^\r\n]*|[^\s@"']+@[^\s:"']+:/[^\r\n]*)`)
	supportWindowsPath                 = regexp.MustCompile(`(?i)(?:\\\\\?\\UNC\\|\\\\\?\\|\\\\\.\\|\\\\|[a-z]:\\)[^\r\n]*`)
	supportPOSIXPath                   = regexp.MustCompile(`(^|[\s=:,(\[])\/[^\r\n]*`)
	supportRelativePath                = regexp.MustCompile(`(^|[\s=:,(\[])\.\.?[/\\][^\r\n]*`)
	supportBareRelativePath            = regexp.MustCompile(`\b(?:[A-Za-z0-9._-]+[/\\]){1,}[A-Za-z0-9._-]+\b`)
	supportStructuredPath              = regexp.MustCompile(`(?i)((?:["'])?(?:path|source|target|destination|directory|folder)(?:["'])?\s*[:=]\s*)(?:"[^"\r\n]+"|'[^'\r\n]+'|[^\r\n]+)`)
	supportStructuredFilename          = regexp.MustCompile(`(?i)\b(file(?:name)?)\s*[:=]\s*(?:"[^"]+"|'[^']+'|[^\s,;]+)`)
	supportQuotedFilename              = regexp.MustCompile(`["'][^"'\r\n/\\]+\.[A-Za-z][A-Za-z0-9]{0,15}["']`)
	supportFilename                    = regexp.MustCompile(`\b[A-Za-z][A-Za-z0-9_-]{0,99}\.[A-Za-z][A-Za-z0-9]{0,15}\b`)
	supportPresentationTag             = regexp.MustCompile(`replicaro-(?:client|machine):[A-Za-z0-9._%@-]+`)
)

type supportPrivatePattern struct {
	pattern     *regexp.Regexp
	replacement string
}

func sanitizeSupportDetail(value string, privatePatterns []supportPrivatePattern) string {
	value = strings.ToValidUTF8(value, "�")
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	value = redactSupportPrivateKeys(value)
	// Native logs remain byte-preserved. The public report copy removes both
	// internal presentation namespaces explicitly, including percent-encoded
	// machine labels that ordinary hostname matching cannot recognize.
	value = supportPresentationTag.ReplaceAllString(value, "[REDACTED PRESENTATION TAG]")
	for _, pattern := range privatePatterns {
		if pattern.replacement == "[REDACTED CREDENTIAL]" {
			value = pattern.pattern.ReplaceAllString(value, pattern.replacement)
		}
	}
	value = supportAuthorizationPattern.ReplaceAllString(value, "$1[REDACTED]")
	value = supportCredentialSchemePattern.ReplaceAllString(value, "$1 [REDACTED]")
	value = supportCredentialAssignmentPattern.ReplaceAllString(value, "$1[REDACTED]")
	value = supportEmailPattern.ReplaceAllString(value, "[REDACTED EMAIL]")
	for _, pattern := range privatePatterns {
		if pattern.replacement != "[REDACTED CREDENTIAL]" {
			value = pattern.pattern.ReplaceAllString(value, pattern.replacement)
		}
	}
	value = supportStructuredHost.ReplaceAllString(value, "$1[REDACTED COMPUTER]")
	value = supportURLPattern.ReplaceAllString(value, "[REDACTED URL]")
	value = supportNativeRemote.ReplaceAllString(value, "[REDACTED REMOTE]")
	value = supportStructuredPath.ReplaceAllString(value, "$1[REDACTED PATH]")
	value = supportWindowsPath.ReplaceAllString(value, "[REDACTED PATH]")
	value = supportPOSIXPath.ReplaceAllString(value, "$1[REDACTED PATH]")
	value = supportRelativePath.ReplaceAllString(value, "$1[REDACTED PATH]")
	value = supportBareRelativePath.ReplaceAllString(value, "[REDACTED PATH]")
	value = supportStructuredFilename.ReplaceAllString(value, "$1=[REDACTED FILENAME]")
	value = supportQuotedFilename.ReplaceAllString(value, "[REDACTED FILENAME]")
	value = supportFilename.ReplaceAllString(value, "[REDACTED FILENAME]")
	value = strings.TrimSpace(value)
	if value == "" || supportPrivateKeyStartPattern.MatchString(value) ||
		supportAuthorizationPattern.MatchString(value) || supportCredentialAssignmentPattern.MatchString(value) {
		return ""
	}
	meaningful := value
	for _, placeholder := range []string{"[REDACTED]", "[REDACTED CREDENTIAL]", "[REDACTED COMPUTER]", "[REDACTED URL]", "[REDACTED REMOTE]", "[REDACTED PATH]", "[REDACTED FILENAME]", "[REDACTED PRESENTATION TAG]", "[REDACTED EMAIL]", "Basic ", "Bearer ", "Digest "} {
		meaningful = strings.ReplaceAll(meaningful, placeholder, "")
	}
	meaningfulLines := strings.Split(meaningful, "\n")
	for index, line := range meaningfulLines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") && strings.Count(trimmed, " · ") == 2 {
			// Generated file section headings describe metadata already present in
			// the report. They must not make an otherwise fully redacted body look
			// safe or meaningful.
			meaningfulLines[index] = ""
		}
	}
	meaningful = strings.Join(meaningfulLines, "\n")
	if strings.Trim(meaningful, " \t\r\n,.;:()[]{}<>'\"") == "" {
		return ""
	}
	return truncateSupportText(value, supportDetailByteLimit)
}

func redactSupportPrivateKeys(value string) string {
	var result strings.Builder
	for {
		match := supportPrivateKeyStartPattern.FindStringSubmatchIndex(value)
		if match == nil {
			result.WriteString(value)
			return result.String()
		}
		result.WriteString(value[:match[0]])
		result.WriteString("[REDACTED CREDENTIAL]")
		label := value[match[2]:match[3]]
		endMarker := "-----END " + label + "-----"
		remainder := value[match[1]:]
		end := strings.Index(strings.ToUpper(remainder), strings.ToUpper(endMarker))
		if end < 0 {
			return result.String()
		}
		value = remainder[end+len(endMarker):]
	}
}

func supportDetailField(value string, privatePatterns []supportPrivatePattern) string {
	detail := sanitizeSupportDetail(value, privatePatterns)
	if detail == "" {
		return "detail=omitted"
	}
	return "detail=" + quoteBoundedSupportDetail(detail)
}

func quoteBoundedSupportDetail(value string) string {
	return quoteSupportDetailWithin(value, supportDetailByteLimit)
}

func quoteSupportDetailWithin(value string, limit int) string {
	quoted := strconv.Quote(value)
	if len(quoted) <= limit {
		return quoted
	}
	suffix := "…[truncated]"
	if len(strconv.Quote(suffix)) > limit {
		return ""
	}
	value = strings.TrimSuffix(value, suffix)
	runes := []rune(value)
	low, high := 0, len(runes)
	for low < high {
		middle := (low + high + 1) / 2
		if len(strconv.Quote(string(runes[:middle])+suffix)) <= limit {
			low = middle
		} else {
			high = middle - 1
		}
	}
	return strconv.Quote(string(runes[:low]) + suffix)
}

type supportQueryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

type supportVaultCounts struct {
	restic int
	kopia  int
}

func buildSupportReport(ctx context.Context, db *sql.DB, generatedAt time.Time) (string, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return "", fmt.Errorf("begin support report snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	report, err := buildSupportReportSnapshot(ctx, tx, generatedAt)
	if err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("finish support report snapshot: %w", err)
	}
	return report, nil
}

func buildSupportReportSnapshot(ctx context.Context, query supportQueryer, generatedAt time.Time) (string, error) {
	settings, err := loadSupportSettings(query)
	if err != nil {
		return "", err
	}
	vaultCounts, err := loadSupportVaultCounts(query)
	if err != nil {
		return "", err
	}
	generatedAt = generatedAt.UTC()
	cutoff := generatedAt.AddDate(0, 0, -settings.LogRetentionDays)
	privatePatterns, err := loadSupportPrivatePatterns(query)
	if err != nil {
		return "", err
	}
	issues, err := loadSupportIssues(query, cutoff, generatedAt, privatePatterns)
	if err != nil {
		return "", err
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}

	var report strings.Builder
	appendSupportLine(&report, "Replicaro diagnostic error log", supportReportByteLimit)
	appendSupportLine(&report, "", supportReportByteLimit)
	appendSupportLine(&report, "Generated (UTC): "+generatedAt.Format(time.RFC3339), supportReportByteLimit)
	appendSupportLine(&report, "Replicaro version: "+models.ReplicaroVersion, supportReportByteLimit)
	appendSupportLine(&report, "Operating system: "+runtime.GOOS, supportReportByteLimit)
	appendSupportLine(&report, "Architecture: "+runtime.GOARCH, supportReportByteLimit)
	appendSupportLine(&report, "", supportReportByteLimit)
	appendSupportLine(&report, "Managed vault counts:", supportReportByteLimit)
	appendSupportLine(&report, fmt.Sprintf("- Restic vaults: %d", vaultCounts.restic), supportReportByteLimit)
	appendSupportLine(&report, fmt.Sprintf("- Kopia vaults: %d", vaultCounts.kopia), supportReportByteLimit)
	appendSupportLine(&report, "", supportReportByteLimit)
	appendSupportLine(&report, fmt.Sprintf("Recent issues (newest first; retention=%d days; maximum=%d):", settings.LogRetentionDays, supportReportIssueLimit), supportReportByteLimit)
	if len(issues) == 0 {
		appendSupportLine(&report, "- No retained issues were found.", supportReportByteLimit)
	}
	const truncationLine = "- [report truncated at 256 KiB]"
	issueLimit := supportReportByteLimit - len(truncationLine) - 1
	for _, issue := range issues {
		line := fmt.Sprintf("- [%s] %s %s", issue.timestamp, issue.source, issue.detail)
		if !appendSupportLine(&report, line, issueLimit) {
			appendSupportLine(&report, truncationLine, supportReportByteLimit)
			break
		}
	}
	return report.String(), nil
}

func loadSupportPrivatePatterns(query supportQueryer) ([]supportPrivatePattern, error) {
	configured, err := database.LoadSupportPrivacyContext(query, supportRedactionContextValueLimit,
		supportRedactionContextByteLimit, supportRedactionContextValueByteLimit)
	if err != nil {
		return nil, err
	}
	hostname, err := os.Hostname()
	if err != nil || strings.TrimSpace(hostname) == "" {
		return nil, fmt.Errorf("read local computer name for support privacy context")
	}
	type privateValue struct {
		value, replacement string
		caseInsensitive    bool
		wordBoundary       bool
	}
	values := make([]privateValue, 0, len(configured.Paths)+len(configured.Hosts)+len(configured.Secrets)*4+1)
	valueBytes := 0
	appendValue := func(value, replacement string, caseInsensitive, wordBoundary bool) error {
		value = strings.TrimSpace(strings.ToValidUTF8(value, "�"))
		if value == "" {
			return nil
		}
		if len(value) > supportRedactionContextValueByteLimit || len(values) >= supportRedactionContextValueLimit ||
			valueBytes+len(value) > supportRedactionContextByteLimit {
			return fmt.Errorf("support privacy context exceeds its safe bound")
		}
		values = append(values, privateValue{value, replacement, caseInsensitive, wordBoundary})
		valueBytes += len(value)
		return nil
	}
	if err := appendValue(hostname, "[REDACTED COMPUTER]", true, true); err != nil {
		return nil, err
	}
	for _, value := range configured.Paths {
		if err := appendValue(value, "[REDACTED PATH]", runtime.GOOS == "windows", false); err != nil {
			return nil, err
		}
	}
	for _, value := range configured.Hosts {
		if err := appendValue(value, "[REDACTED COMPUTER]", true, true); err != nil {
			return nil, err
		}
	}
	for _, secret := range configured.Secrets {
		for _, representation := range supportSecretRepresentations(secret) {
			if err := appendValue(representation, "[REDACTED CREDENTIAL]", false, false); err != nil {
				return nil, err
			}
		}
	}
	sort.Slice(values, func(i, j int) bool {
		if len(values[i].value) != len(values[j].value) {
			return len(values[i].value) > len(values[j].value)
		}
		return values[i].value < values[j].value
	})
	patterns := make([]supportPrivatePattern, 0, len(values))
	seen := map[string]bool{}
	for _, item := range values {
		key := item.value + "\x00" + item.replacement
		if item.caseInsensitive {
			key = strings.ToLower(key)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		expression := regexp.QuoteMeta(item.value)
		if item.wordBoundary {
			expression = `\b` + expression + `\b`
		}
		if item.caseInsensitive {
			expression = `(?i:` + expression + `)`
		}
		pattern, err := regexp.Compile(expression)
		if err != nil {
			return nil, fmt.Errorf("prepare support privacy context: %w", err)
		}
		patterns = append(patterns, supportPrivatePattern{pattern: pattern, replacement: item.replacement})
	}
	return patterns, nil
}

func supportSecretRepresentations(secret string) []string {
	if secret == "" {
		return nil
	}
	encoded, _ := json.Marshal(secret)
	return []string{secret, url.QueryEscape(secret), url.PathEscape(secret), strconv.Quote(secret),
		string(encoded), strings.ReplaceAll(secret, `\`, `\\`)}
}

func loadSupportSettings(query supportQueryer) (models.Settings, error) {
	settings := models.Settings{LogRetentionDays: 30}
	rows, err := query.Query(`SELECT key,value FROM settings WHERE key='logRetentionDays'`)
	if err != nil {
		return settings, fmt.Errorf("read support report settings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return settings, err
		}
		switch key {
		case "logRetentionDays":
			if days, parseErr := strconv.Atoi(value); parseErr == nil && days > 0 {
				settings.LogRetentionDays = days
			}
		}
	}
	return settings, rows.Err()
}

func loadSupportVaultCounts(query supportQueryer) (supportVaultCounts, error) {
	var counts supportVaultCounts
	err := query.QueryRow(`SELECT
		COALESCE(SUM(CASE WHEN engine='restic' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN engine='kopia' THEN 1 ELSE 0 END),0)
		FROM repositories`).Scan(&counts.restic, &counts.kopia)
	if err != nil {
		return supportVaultCounts{}, fmt.Errorf("read support vault counts: %w", err)
	}
	return counts, nil
}

func appendSupportLine(report *strings.Builder, line string, limit int) bool {
	if report.Len()+len(line)+1 > limit {
		return false
	}
	report.WriteString(line)
	report.WriteByte('\n')
	return true
}

var supportRecordedActivityPattern = regexp.MustCompile(`^Support diagnostic: method=(GET|POST|PUT|PATCH|DELETE|OTHER) category=(api|vault_create|vault_connect) status=([0-9]{3})(?: detail=(.*))?$`)

func supportAllowedLabel(value string, allowed ...string) string {
	for _, candidate := range allowed {
		if value == candidate {
			return value
		}
	}
	return "other"
}

func supportEngineLabel(value string) string {
	if value == "restic" || value == "kopia" {
		return value
	}
	return "unknown"
}

func supportStorageLabel(value string) string {
	for _, candidate := range []string{"fs", "s3", "sftp", "azblob", "gcs", "google_drive", "dropbox", "onedrive"} {
		if value == candidate {
			return value
		}
	}
	return "unknown"
}

func limitSupportRawDetail(value string) (string, bool) {
	if len(value) <= supportRawDetailByteLimit {
		return value, false
	}
	return value[:supportRawDetailByteLimit], true
}

func supportBoundedRawDetail(value string, truncated bool) string {
	if truncated {
		return ""
	}
	value = strings.ToValidUTF8(value, "�")
	return value
}

func loadSupportIssues(query supportQueryer, cutoff, generatedAt time.Time, privatePatterns []supportPrivatePattern) ([]supportIssue, error) {
	issues := make([]supportIssue, 0, supportReportIssueLimit*3)
	sequence := 0
	cutoffText := cutoff.Format(time.RFC3339Nano)
	generatedText := generatedAt.Format(time.RFC3339Nano)
	activityRows, err := query.Query(`SELECT timestamp,level,CAST(substr(CAST(message AS BLOB),1,?) AS TEXT),
		length(CAST(message AS BLOB))>?
		FROM activity_log
		WHERE level IN ('ERROR','WARN','SUPPORT') AND julianday(timestamp) >= julianday(?) AND julianday(timestamp) <= julianday(?)
		ORDER BY julianday(timestamp) DESC,id DESC LIMIT ?`, supportRawDetailByteLimit, supportRawDetailByteLimit,
		cutoffText, generatedText, supportReportIssueLimit)
	if err != nil {
		return nil, fmt.Errorf("read support activity: %w", err)
	}
	for activityRows.Next() {
		var timestamp, level, message string
		var truncated bool
		if err := activityRows.Scan(&timestamp, &level, &message, &truncated); err != nil {
			_ = activityRows.Close()
			return nil, err
		}
		message = supportBoundedRawDetail(message, truncated)
		detail := "level=" + supportAllowedLabel(level, "ERROR", "WARN", "SUPPORT") + " " + supportDetailField(message, privatePatterns)
		if matches := supportRecordedActivityPattern.FindStringSubmatch(message); matches != nil {
			detail = fmt.Sprintf("level=%s method=%s category=%s status=%s",
				supportAllowedLabel(level, "ERROR", "WARN", "SUPPORT"), matches[1], matches[2], matches[3])
			if matches[4] != "" {
				if recorded, unquoteErr := strconv.Unquote(matches[4]); unquoteErr == nil {
					detail += " " + supportDetailField(recorded, privatePatterns)
				}
			}
		}
		issues = append(issues, newSupportIssue(timestamp, "activity", detail, sequence))
		sequence++
	}
	if err := activityRows.Err(); err != nil {
		_ = activityRows.Close()
		return nil, err
	}
	if err := activityRows.Close(); err != nil {
		return nil, err
	}

	operationRows, err := query.Query(`SELECT o.id,o.kind,o.status,o.engine,COALESCE(r.connector,''),o.started_at,o.finished_at,
		o.repository_id,o.job_id,
		CASE WHEN r.id IS NOT NULL AND (o.job_id='' OR EXISTS(SELECT 1 FROM backup_jobs j WHERE j.id=o.job_id)) THEN 1 ELSE 0 END
		FROM operations o LEFT JOIN repositories r ON r.id=o.repository_id
		WHERE o.status IN ('failed','interrupted','partial','completed_with_issues','reconnect_required')
		AND julianday(CASE WHEN o.finished_at='' THEN o.started_at ELSE o.finished_at END) >= julianday(?)
		AND julianday(CASE WHEN o.finished_at='' THEN o.started_at ELSE o.finished_at END) <= julianday(?)
		ORDER BY julianday(CASE WHEN o.finished_at='' THEN o.started_at ELSE o.finished_at END) DESC,o.id DESC LIMIT ?`,
		cutoffText, generatedText, supportReportIssueLimit)
	if err != nil {
		return nil, fmt.Errorf("read support operations: %w", err)
	}
	type supportOperation struct {
		id, kind, status, engine, storage, startedAt, finishedAt string
		repositoryID, jobID, output                              string
		redactionContextComplete                                 bool
	}
	operations := make([]supportOperation, 0, supportReportIssueLimit)
	for operationRows.Next() {
		var operation supportOperation
		if err := operationRows.Scan(&operation.id, &operation.kind, &operation.status, &operation.engine,
			&operation.storage, &operation.startedAt, &operation.finishedAt, &operation.repositoryID,
			&operation.jobID, &operation.redactionContextComplete); err != nil {
			_ = operationRows.Close()
			return nil, err
		}
		operations = append(operations, operation)
	}
	if err := operationRows.Err(); err != nil {
		_ = operationRows.Close()
		return nil, err
	}
	if err := operationRows.Close(); err != nil {
		return nil, err
	}
	for _, operation := range operations {
		if operation.redactionContextComplete {
			// The report snapshot owns SQLite's connection, so it must not wait on the
			// terminal append barrier while a finalizer waits for that same connection.
			// A best-effort diagnostic sample may be incomplete without changing the
			// authoritative snapshot. Read only the bounded window and omit an
			// over-limit or unsafe detail.
			operation.output, _ = operationlog.ReadBoundedBestEffortCompleteSections(operation.id, supportRawDetailByteLimit)
		}
		steps, err := loadSupportOperationSteps(query, operation.id, generatedAt)
		if err != nil {
			return nil, fmt.Errorf("read support operation steps: %w", err)
		}
		detail, err := buildSupportOperationDetail(operation.kind, operation.status, operation.engine, operation.storage,
			operation.output, steps, operation.redactionContextComplete, privatePatterns)
		if err != nil {
			return nil, err
		}
		timestamp := operation.finishedAt
		if timestamp == "" {
			timestamp = operation.startedAt
		}
		issues = append(issues, newSupportIssue(timestamp, "operation", detail, sequence))
		sequence++
	}
	for _, source := range []struct {
		name  string
		query string
	}{
		{"vault_create", `SELECT updated_at,phase,engine,connector,
			CAST(substr(CAST(last_error AS BLOB),1,?) AS TEXT),length(CAST(last_error AS BLOB))>? FROM repository_creation_intents
			WHERE last_error<>'' AND julianday(updated_at)>=julianday(?) AND julianday(updated_at)<=julianday(?) ORDER BY julianday(updated_at) DESC,id DESC LIMIT ?`},
		{"vault_connect", `SELECT updated_at,state,'' AS engine,connector,
			CAST(substr(CAST(error AS BLOB),1,?) AS TEXT),length(CAST(error AS BLOB))>? FROM repository_connection_intents
			WHERE error<>'' AND julianday(updated_at)>=julianday(?) AND julianday(updated_at)<=julianday(?) ORDER BY julianday(updated_at) DESC,id DESC LIMIT ?`},
	} {
		rows, err := query.Query(source.query, supportRawDetailByteLimit, supportRawDetailByteLimit,
			cutoffText, generatedText, supportReportIssueLimit)
		if err != nil {
			return nil, fmt.Errorf("read %s support states: %w", source.name, err)
		}
		for rows.Next() {
			var timestamp, state, engine, storage, issueDetail string
			var truncated bool
			if err := rows.Scan(&timestamp, &state, &engine, &storage, &issueDetail, &truncated); err != nil {
				_ = rows.Close()
				return nil, err
			}
			issueDetail = supportBoundedRawDetail(issueDetail, truncated)
			issues = append(issues, newSupportIssue(timestamp, source.name,
				fmt.Sprintf("state=%s engine=%s storage=%s %s", supportAllowedLabel(state,
					"prepared", "native_started", "native_ready", "publication_started", "attachment_pending"),
					supportEngineLabel(engine), supportStorageLabel(storage), supportDetailField(issueDetail, privatePatterns)), sequence))
			sequence++
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}

	sort.SliceStable(issues, func(i, j int) bool {
		if !issues[i].time.Equal(issues[j].time) {
			return issues[i].time.After(issues[j].time)
		}
		if issues[i].source != issues[j].source {
			return issues[i].source < issues[j].source
		}
		return issues[i].sequence < issues[j].sequence
	})
	if len(issues) > supportReportIssueLimit {
		issues = issues[:supportReportIssueLimit]
	}
	return issues, nil
}

type supportOperationStep struct {
	domain, kind, status string
}

func supportOperationStepLabel(step supportOperationStep) string {
	return fmt.Sprintf("%s/%s/%s",
		supportAllowedLabel(step.domain, "native", "orchestration", "application"),
		supportAllowedLabel(step.kind,
			"backup", "restore", "retention", "check", "maintenance", "delete", "snapshot_deletion", "notification",
			"before_script", "after_script", "backup_admission",
			"repository_writer_storage_validation", "repository_storage_admission", "native_process_admission",
			"integrity_cache_prepare",
			"repository_integrity_check", "integrity_cache_cleanup", "kopia_policy_reconciliation",
			"metadata_cache", "metadata_cache_invalidation"),
		supportAllowedLabel(step.status, "running", "succeeded", "failed", "skipped", "warning", "interrupted"))
}

func buildSupportOperationDetail(kind, status, engine, storage, output string, steps []supportOperationStep,
	redactionContextComplete bool, privatePatterns []supportPrivatePattern,
) (string, error) {
	base := fmt.Sprintf("kind=%s status=%s engine=%s storage=%s",
		supportAllowedLabel(kind, "backup", "restore", "retention", "check", "maintenance", "delete"),
		supportAllowedLabel(status, "failed", "interrupted", "partial", "completed_with_issues", "reconnect_required"),
		supportEngineLabel(engine), supportStorageLabel(storage))
	stepDetails := make([]string, len(steps))
	for index, step := range steps {
		stepDetails[index] = supportOperationStepLabel(step)
		// Step payloads live only in the one operation file; SQLite supplies
		// status/order metadata and cannot duplicate those bodies here.
	}
	opOutput := ""
	if redactionContextComplete {
		opOutput = sanitizeSupportDetail(output, privatePatterns)
	}
	detail := base + " detail=omitted"
	if len(stepDetails) > 0 {
		detail += " steps=[" + strings.Join(stepDetails, "; ") + "]"
	}
	if len(detail) > supportDetailByteLimit {
		return "", fmt.Errorf("support operation categories exceed their safe bound")
	}
	extra := supportDetailByteLimit - len(detail)
	opField := "omitted"
	if opOutput != "" {
		if quoted := quoteSupportDetailWithin(opOutput, len(opField)+extra); quoted != "" {
			extra -= len(quoted) - len(opField)
			opField = quoted
		}
	}
	detail = base + " detail=" + opField
	if len(stepDetails) > 0 {
		detail += " steps=[" + strings.Join(stepDetails, "; ") + "]"
	}
	if len(detail) > supportDetailByteLimit {
		return "", fmt.Errorf("support operation detail exceeds its safe bound")
	}
	return detail, nil
}

func loadSupportOperationSteps(query supportQueryer, operationID string, generatedAt time.Time) ([]supportOperationStep, error) {
	generatedText := generatedAt.UTC().Format(time.RFC3339Nano)
	rows, err := query.Query(`SELECT domain,kind,
		CASE WHEN finished_at='' OR julianday(finished_at) IS NULL OR julianday(finished_at)>julianday(?)
			THEN 'running' ELSE status END
		FROM operation_steps
		WHERE operation_id=? AND julianday(started_at)<=julianday(?) ORDER BY started_at,id LIMIT ?`,
		generatedText,
		operationID, generatedText, supportOperationStepLimit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	steps := []supportOperationStep{}
	for rows.Next() {
		var step supportOperationStep
		if err := rows.Scan(&step.domain, &step.kind, &step.status); err != nil {
			return nil, err
		}
		steps = append(steps, step)
		if len(steps) > supportOperationStepLimit {
			return nil, fmt.Errorf("support operation steps exceed their safe bound")
		}
	}
	return steps, rows.Err()
}

func newSupportIssue(timestamp, source, detail string, sequence int) supportIssue {
	parsed := parseSupportTimestamp(timestamp)
	displayed := "unknown"
	if !parsed.IsZero() {
		displayed = parsed.UTC().Format(time.RFC3339)
	}
	return supportIssue{timestamp: displayed, time: parsed, source: source, detail: detail, sequence: sequence}
}

func parseSupportTimestamp(value string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

func handleSupportReport(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet {
			badRequest(w, "invalid method")
			return
		}
		report, err := buildSupportReport(r.Context(), db, time.Now())
		if err != nil {
			writeError(w, http.StatusInternalServerError, errors.New("The diagnostic error log could not be generated."))
			return
		}
		writeJSON(w, map[string]string{"report": report})
	}
}
