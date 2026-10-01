package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
	"github.com/local/replicaro/runner"
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
	// Preview and retry can also fail inside native work. A deadline from that
	// work does not mean the storage check timed out, even at a storage stage.
	if err == nil || (!errors.As(err, &binding) && !errors.As(err, &macAccess)) ||
		errors.Is(err, context.Canceled) ||
		!errors.Is(err, storageavailability.ErrObserverUnavailable) && !errors.Is(err, context.DeadlineExceeded) {
		return
	}
	switch stage {
	case storageavailability.StageVaultCreate, storageavailability.StageVaultConnect,
		storageavailability.StageJobCreateSource, storageavailability.StageJobBindSource,
		storageavailability.StageJobSourceUpdate:
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
		// Fixed labels only: the step name and numeric OS code, never a path,
		// share name, credential, or OS message text.
		step, code := storageavailability.ResolutionStep(err)
		recorder.storageObservationDetail = "stage=" + stage + " reason=" + reason +
			" step=" + step + " code=" + strconv.FormatInt(code, 10)
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
	supportPrivateKeyEndPattern        = regexp.MustCompile(`(?i)-----END ([A-Z0-9 ]*PRIVATE KEY)-----`)
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

// Restic's --json error records (restore and backup) name the affected file
// only in "item":"<path>", and the message beside it may not repeat the
// path. The general path patterns need a separator before the leading "/"
// and only match ASCII segments, so without this a quoted item value such
// as "/home/ünïcode/file" would leave its non-ASCII parts behind. Only the
// quoted JSON key form is matched: the word "item" is common in ordinary
// messages ("failed item 1234") and must not swallow the rest of a line.
// JSON escapes are followed so an escaped quote cannot end the value early.
var supportStructuredItem = regexp.MustCompile(`(?i)(["']item["']\s*:\s*)"(?:[^"\\\r\n]|\\.)*"`)

type supportPrivatePattern struct {
	pattern       *regexp.Regexp
	replacement   string
	literalSecret string
}

func sanitizeSupportDetail(value string, privatePatterns []supportPrivatePattern) string {
	// Configured values can overlap assignment labels. Keep the original
	// credential omission decision before any replacement hides those labels.
	if supportAuthorizationPattern.MatchString(value) || supportCredentialAssignmentPattern.MatchString(value) {
		return ""
	}
	for _, pattern := range []*regexp.Regexp{supportStructuredHost, supportStructuredPath, supportStructuredFilename} {
		for _, match := range pattern.FindAllStringSubmatchIndex(value, -1) {
			start, end := match[3], match[1]
			if pattern == supportStructuredFilename {
				// This pattern captures the label without its assignment separator.
				for start < end && strings.ContainsRune(" \t\r\n\f:=", rune(value[start])) {
					start++
				}
			}
			if start >= end || value[start] != '"' && value[start] != '\'' {
				continue
			}
			// These patterns do not follow escaped quotes. Omit a detail if
			// the matched range cannot establish the complete quoted value.
			if end <= start+1 || value[end-1] != value[start] {
				return ""
			}
			backslashes := 0
			for index := end - 2; index > start && value[index] == '\\'; index-- {
				backslashes++
			}
			if backslashes%2 != 0 {
				return ""
			}
		}
	}
	value = redactSupportSecrets(value, privatePatterns)
	value = strings.ToValidUTF8(value, "�")
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	// Native logs remain byte-preserved. The public report copy removes both
	// internal presentation namespaces explicitly, including percent-encoded
	// machine labels that ordinary hostname matching cannot recognize.
	value = supportPresentationTag.ReplaceAllString(value, "[REDACTED PRESENTATION TAG]")
	value = supportAuthorizationPattern.ReplaceAllString(value, "$1[REDACTED]")
	value = supportCredentialSchemePattern.ReplaceAllString(value, "$1 [REDACTED]")
	value = supportCredentialAssignmentPattern.ReplaceAllString(value, "$1[REDACTED]")
	value = supportEmailPattern.ReplaceAllString(value, "[REDACTED EMAIL]")
	for _, pattern := range privatePatterns {
		if pattern.literalSecret == "" {
			value = pattern.pattern.ReplaceAllString(value, pattern.replacement)
		}
	}
	value = supportStructuredHost.ReplaceAllString(value, "$1[REDACTED COMPUTER]")
	value = supportURLPattern.ReplaceAllString(value, "[REDACTED URL]")
	value = supportNativeRemote.ReplaceAllString(value, "[REDACTED REMOTE]")
	value = supportStructuredItem.ReplaceAllString(value, "$1[REDACTED PATH]")
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

// Discover sensitive ranges before editing the detail. Their overlapping
// ranges are combined so no replacement can hide another privacy match and
// leave its original bytes behind.
func redactSupportSecrets(value string, patterns []supportPrivatePattern) string {
	// One marker represents each combined range. Credentials take precedence;
	// email and presentation markers remain recognizable when paths or filenames
	// overlap them.
	const (
		genericMarker = iota
		filenameMarker
		filenameAssignmentMarker
		pathMarker
		remoteMarker
		urlMarker
		computerMarker
		emailMarker
		presentationMarker
		credentialMarker
	)
	replacements := [...]string{
		"[REDACTED]", "[REDACTED FILENAME]", "=[REDACTED FILENAME]", "[REDACTED PATH]",
		"[REDACTED REMOTE]", "[REDACTED URL]", "[REDACTED COMPUTER]", "[REDACTED EMAIL]",
		"[REDACTED PRESENTATION TAG]", "[REDACTED CREDENTIAL]",
	}
	var coverage []int
	var markerStarts []uint8
	cover := func(start, end int, marker uint8) {
		if coverage == nil {
			coverage = make([]int, len(value)+1)
			markerStarts = make([]uint8, len(value))
		}
		coverage[start]++
		coverage[end]--
		if marker > markerStarts[start] {
			markerStarts[start] = marker
		}
	}
	for _, pattern := range patterns {
		fragment := pattern.literalSecret
		if fragment == "" {
			marker := uint8(pathMarker)
			if pattern.replacement == "[REDACTED COMPUTER]" {
				marker = computerMarker
			}
			for _, match := range pattern.pattern.FindAllStringIndex(value, -1) {
				cover(match[0], match[1], marker)
			}
			continue
		}
		for offset := 0; offset < len(value); {
			index := strings.Index(value[offset:], fragment)
			if index < 0 {
				break
			}
			start := offset + index
			cover(start, start+len(fragment), credentialMarker)
			offset = start + 1
		}
	}
	for offset := 0; offset < len(value); {
		match := supportPrivateKeyStartPattern.FindStringSubmatchIndex(value[offset:])
		if match == nil {
			break
		}
		start := offset + match[0]
		label := value[offset+match[2] : offset+match[3]]
		end := len(value)
		for search := offset + match[1]; search < len(value); {
			closing := supportPrivateKeyEndPattern.FindStringSubmatchIndex(value[search:])
			if closing == nil {
				break
			}
			if strings.EqualFold(label, value[search+closing[2]:search+closing[3]]) {
				end = search + closing[1]
				break
			}
			search += closing[1]
		}
		cover(start, end, credentialMarker)
		offset = end
	}
	for _, pattern := range []*regexp.Regexp{supportAuthorizationPattern, supportCredentialSchemePattern, supportCredentialAssignmentPattern} {
		for _, match := range pattern.FindAllStringSubmatchIndex(value, -1) {
			start := match[3]
			// The scheme captures only its name; keep the separator outside
			// the sensitive range, just like an assignment's captured label.
			if pattern == supportCredentialSchemePattern {
				for start < match[1] && strings.ContainsRune(" \t\r\n\f", rune(value[start])) {
					start++
				}
			}
			cover(start, match[1], genericMarker)
		}
	}
	for _, item := range []struct {
		pattern    *regexp.Regexp
		marker     uint8
		keepPrefix bool
	}{
		{supportPresentationTag, presentationMarker, false},
		{supportEmailPattern, emailMarker, false},
		{supportStructuredHost, computerMarker, true},
		{supportURLPattern, urlMarker, false},
		{supportNativeRemote, remoteMarker, false},
		{supportStructuredItem, pathMarker, true},
		{supportStructuredPath, pathMarker, true},
		{supportWindowsPath, pathMarker, false},
		{supportPOSIXPath, pathMarker, true},
		{supportRelativePath, pathMarker, true},
		{supportBareRelativePath, pathMarker, false},
		{supportStructuredFilename, filenameAssignmentMarker, true},
		{supportQuotedFilename, filenameMarker, false},
		{supportFilename, filenameMarker, false},
	} {
		for _, match := range item.pattern.FindAllStringSubmatchIndex(value, -1) {
			start := match[0]
			if item.keepPrefix {
				start = match[3]
			}
			cover(start, match[1], item.marker)
		}
	}
	if coverage == nil {
		return value
	}
	var result strings.Builder
	depth := 0
	marker := uint8(genericMarker)
	for index := 0; index <= len(value); index++ {
		previous := depth
		depth += coverage[index]
		if previous > 0 && depth == 0 {
			result.WriteString(replacements[marker])
			marker = genericMarker
		}
		if index < len(value) {
			if markerStarts[index] > marker {
				marker = markerStarts[index]
			}
			if depth == 0 {
				result.WriteByte(value[index])
			}
		}
	}
	return result.String()
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
		fragment           bool
	}
	values := make([]privateValue, 0, len(configured.Paths)+len(configured.Hosts)+len(configured.Secrets)*4+len(configured.SecretFragments)+1)
	valueBytes := 0
	appendValue := func(value, replacement string, caseInsensitive, wordBoundary, fragment bool) error {
		value = strings.ToValidUTF8(value, "�")
		if !fragment {
			value = strings.TrimSpace(value)
		}
		if value == "" {
			return nil
		}
		if len(value) > supportRedactionContextValueByteLimit || len(values) >= supportRedactionContextValueLimit ||
			valueBytes+len(value) > supportRedactionContextByteLimit {
			return fmt.Errorf("support privacy context exceeds its safe bound")
		}
		values = append(values, privateValue{value: value, replacement: replacement, caseInsensitive: caseInsensitive, wordBoundary: wordBoundary, fragment: fragment})
		valueBytes += len(value)
		return nil
	}
	if err := appendValue(hostname, "[REDACTED COMPUTER]", true, true, false); err != nil {
		return nil, err
	}
	for _, value := range configured.Paths {
		if err := appendValue(value, "[REDACTED PATH]", runtime.GOOS == "windows", false, false); err != nil {
			return nil, err
		}
	}
	for _, value := range configured.Hosts {
		if err := appendValue(value, "[REDACTED COMPUTER]", true, true, false); err != nil {
			return nil, err
		}
	}
	for _, secret := range configured.Secrets {
		for _, representation := range database.SupportSecretRepresentations(secret) {
			if err := appendValue(representation, "[REDACTED CREDENTIAL]", false, false, false); err != nil {
				return nil, err
			}
		}
	}
	for _, fragment := range configured.SecretFragments {
		if err := appendValue(fragment, "[REDACTED CREDENTIAL]", false, false, true); err != nil {
			return nil, err
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
		key := item.value + "\x00" + item.replacement + strconv.FormatBool(item.fragment)
		if item.caseInsensitive {
			key = strings.ToLower(key)
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		if item.fragment || item.replacement == "[REDACTED CREDENTIAL]" {
			patterns = append(patterns, supportPrivatePattern{literalSecret: item.value})
			continue
		}
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
	for _, candidate := range []string{"fs", "s3", "sftp", "azblob", "gcs", "webdav", "google_drive", "dropbox", "onedrive", "rclone_remote"} {
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
	return value
}

func supportHasIncompleteSecretTail(value string, patterns []supportPrivatePattern) bool {
	var prefix []int
	for _, pattern := range patterns {
		literal := pattern.literalSecret
		if len(literal) < 2 || value == "" {
			continue
		}
		// Match only the raw EOF against proper prefixes. An unfinished append
		// can contain blank lines that otherwise look like complete sections.
		// Prefix fallback keeps repeated bytes linear in the literal's size.
		if cap(prefix) < len(literal) {
			prefix = make([]int, len(literal))
		} else {
			prefix = prefix[:len(literal)]
		}
		for index, matched := 1, 0; index < len(literal); index++ {
			for matched > 0 && literal[index] != literal[matched] {
				matched = prefix[matched-1]
			}
			if literal[index] == literal[matched] {
				matched++
			}
			prefix[index] = matched
		}
		start := len(value) - len(literal) + 1
		if start < 0 {
			start = 0
		}
		matched := 0
		for index := start; index < len(value); index++ {
			for matched > 0 && value[index] != literal[matched] {
				matched = prefix[matched-1]
			}
			if value[index] == literal[matched] {
				matched++
			}
		}
		if matched > 0 {
			return true
		}
	}
	return false
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
					// Quoting can cut a raw error below the SQL read bound. A valid
					// quoted prefix still cannot establish complete privacy matches.
					recorded = supportBoundedRawDetail(recorded, strings.HasSuffix(recorded, "…[truncated]"))
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
			output, truncated, _ := operationlog.ReadBoundedBestEffort(operation.id, supportRawDetailByteLimit)
			if !truncated && !supportHasIncompleteSecretTail(output, privatePatterns) {
				if strings.HasSuffix(output, "\n\n") {
					operation.output = output
				} else if boundary := strings.LastIndex(output, "\n\n["); boundary >= 0 {
					operation.output = output[:boundary+2]
				}
			}
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
			"metadata_cache", "metadata_cache_invalidation",
			runner.VaultPasswordChangeBlockingStep, vaultRemovalProfileStep),
		supportAllowedLabel(step.status, "running", "succeeded", "failed", "skipped", "warning", "interrupted"))
}

func buildSupportOperationDetail(kind, status, engine, storage, output string, steps []supportOperationStep,
	redactionContextComplete bool, privatePatterns []supportPrivatePattern,
) (string, error) {
	base := fmt.Sprintf("kind=%s status=%s engine=%s storage=%s",
		supportAllowedLabel(kind, "backup", "restore", "retention", "check", "maintenance", "delete", database.JobDeletionKind,
			database.VaultPasswordChangeKind, database.VaultSettingsKind, database.VaultRemovalKind),
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
