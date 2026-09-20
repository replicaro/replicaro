package operationlog

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/google/uuid"
)

const fileSuffix = ".log"

var ErrInvalidChunkOffset = errors.New("operation-log chunk offset is invalid")

var (
	operationLocksMu  sync.Mutex
	operationLocks    = map[string]*operationLock{}
	stateMu           sync.Mutex
	nativeBodies      = map[string][]nativeBodyFingerprint{}
	stagedNative      = map[string][]*nativeStage{}
	afterSectionWrite func()
)

type operationLock struct {
	mu   sync.Mutex
	refs int
}

type nativeStage struct {
	operationID string
	engine      string
	kind        string
	status      string
	diagnostic  string
	stdoutPath  string
	stderrPath  string
}

type nativeBodyFingerprint struct {
	length            int
	digest            [sha256.Size]byte
	endsWithLineBreak bool
}

// Section is one presentation boundary in an operation's local diagnostic
// file. Ordinary native bodies retain engine-owned content; the support export
// sanitizes only the bounded copy it reads for a public report.
type Section struct {
	Engine string
	Domain string
	Kind   string
	Status string
	Body   string
}

// Final describes one terminal operation section owned by a database
// transition. It is used when one transaction makes several operations
// terminal together.
type Final struct {
	OperationID string
	Engine      string
	Kind        string
	Status      string
	Output      string
}

func canonicalID(value string) (string, error) {
	parsed, err := uuid.Parse(value)
	if err != nil || parsed == uuid.Nil || parsed.String() != value {
		return "", fmt.Errorf("operation id is invalid")
	}
	return value, nil
}

func path(value string) (string, error) {
	id, err := canonicalID(value)
	if err != nil {
		return "", err
	}
	directory, err := operationLogDirectory()
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, id+fileSuffix), nil
}

func componentAndKind(section Section) (string, string) {
	component := "replicaro"
	kind := section.Kind
	if section.Domain == "native" && (section.Engine == "restic" || section.Engine == "kopia") {
		component = section.Engine
		if section.Engine == "kopia" && kind == "backup" {
			kind = "backup and retention"
		}
	} else if kind == "notification" {
		kind = "desktop/webhook notification"
	}
	return component, kind
}

func encodeSection(section Section) []byte {
	component, kind := componentAndKind(section)
	value := "[" + component + " · " + kind + " · " + section.Status + "]"
	// Section bodies are persisted exactly as supplied. Known friendly wording
	// belongs solely to the display formatter.
	if section.Body != "" {
		value += "\n" + section.Body
	}
	return []byte(value + "\n\n")
}

func lockOperation(operationID string) func() {
	operationLocksMu.Lock()
	entry := operationLocks[operationID]
	if entry == nil {
		entry = &operationLock{}
		operationLocks[operationID] = entry
	}
	entry.refs++
	operationLocksMu.Unlock()
	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		operationLocksMu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(operationLocks, operationID)
		}
		operationLocksMu.Unlock()
	}
}

func lockOperations(operationIDs []string) func() {
	unique := map[string]struct{}{}
	for _, operationID := range operationIDs {
		unique[operationID] = struct{}{}
	}
	ordered := make([]string, 0, len(unique))
	for operationID := range unique {
		ordered = append(ordered, operationID)
	}
	sort.Strings(ordered)
	releases := make([]func(), 0, len(ordered))
	for _, operationID := range ordered {
		releases = append(releases, lockOperation(operationID))
	}
	return func() {
		for index := len(releases) - 1; index >= 0; index-- {
			releases[index]()
		}
	}
}

// StageNativeOutput copies exact ordinary stdout and stderr into owner-only
// operation-log staging files without retaining either stream in memory. It is
// presentation-only: callers deliberately ignore an error so native result
// truth never depends on local diagnostic persistence.
func StageNativeOutput(operationID, engine, kind, status, diagnostic string, stdout, stderr io.Reader) error {
	if _, err := canonicalID(operationID); err != nil {
		return err
	}
	if (engine != "restic" && engine != "kopia") || strings.TrimSpace(kind) == "" {
		return fmt.Errorf("native operation-log stage is incomplete")
	}
	directory, err := operationLogDirectory()
	if err != nil {
		return err
	}
	stdoutFile, err := createStageFile(directory, operationID, kind, "stdout")
	if err != nil {
		return err
	}
	stdoutPath := stdoutFile.Name()
	cleanup := func() {
		_ = stdoutFile.Close()
		_ = removeOperationLogFile(stdoutPath)
	}
	if _, err := io.Copy(stdoutFile, stdout); err != nil {
		cleanup()
		return err
	}
	if err := stdoutFile.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := stdoutFile.Close(); err != nil {
		_ = removeOperationLogFile(stdoutPath)
		return err
	}

	stderrFile, err := createStageFile(directory, operationID, kind, "stderr")
	if err != nil {
		_ = removeOperationLogFile(stdoutPath)
		return err
	}
	stderrPath := stderrFile.Name()
	if _, err := io.Copy(stderrFile, stderr); err != nil {
		_ = stderrFile.Close()
		_ = removeOperationLogFile(stdoutPath)
		_ = removeOperationLogFile(stderrPath)
		return err
	}
	if err := stderrFile.Sync(); err != nil {
		_ = stderrFile.Close()
		_ = removeOperationLogFile(stdoutPath)
		_ = removeOperationLogFile(stderrPath)
		return err
	}
	if err := stderrFile.Close(); err != nil {
		_ = removeOperationLogFile(stdoutPath)
		_ = removeOperationLogFile(stderrPath)
		return err
	}

	stage := &nativeStage{operationID: operationID, engine: engine, kind: kind,
		status: status, diagnostic: diagnostic,
		stdoutPath: stdoutPath, stderrPath: stderrPath}
	release := lockOperation(operationID)
	stateMu.Lock()
	stagedNative[operationID] = append(stagedNative[operationID], stage)
	stateMu.Unlock()
	release()
	return nil
}

func removeStage(stage *nativeStage) {
	if stage == nil {
		return
	}
	_ = removeOperationLogFile(stage.stdoutPath)
	_ = removeOperationLogFile(stage.stderrPath)
}

func removeOperationStagesLocked(operationID string) {
	stateMu.Lock()
	stages := stagedNative[operationID]
	delete(stagedNative, operationID)
	stateMu.Unlock()
	for _, stage := range stages {
		removeStage(stage)
	}
}

// CleanupStagedOutput removes only abandoned native-stream staging files after
// the application has acquired its single-instance guard. It does not inspect
// capacity, rotate logs, impose quotas, or adapt to available disk space.
func CleanupStagedOutput() error {
	directory, err := operationLogDirectory()
	if err != nil {
		return err
	}
	return cleanupStagedOutputFiles(directory)
}

func stageFilename(name string) bool {
	if len(name) <= 38 || name[0] != '.' || !strings.HasSuffix(name, ".tmp") {
		return false
	}
	id := name[1:37]
	parsed, err := uuid.Parse(id)
	if err != nil || parsed == uuid.Nil || parsed.String() != id || name[37] != '.' {
		return false
	}
	for _, marker := range []string{".stdout-", ".stderr-"} {
		index := strings.LastIndex(name, marker)
		if index > 38 && index+len(marker) < len(name)-len(".tmp") {
			return true
		}
	}
	return false
}

// AppendSection adds one step result to the single operation-owned file. This
// is local diagnostic state, not a vault object, so process-local serialization
// is sufficient and no vault or remote coordination belongs here.
func AppendSection(operationID string, section Section) error {
	filename, err := path(operationID)
	if err != nil {
		return err
	}
	release := lockOperation(operationID)
	defer release()
	if err := appendSectionFile(filename, section); err != nil {
		return err
	}
	rememberNativeBody(operationID, section)
	return nil
}

func appendSectionFile(filename string, section Section) error {
	file, err := openAppend(filename)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Write(encodeSection(section)); err != nil {
		return err
	}
	if afterSectionWrite != nil {
		afterSectionWrite()
	}
	return file.Sync()
}

func appendStagedSectionFile(filename string, section Section, stage *nativeStage) (nativeBodyFingerprint, error) {
	if stage == nil {
		return nativeBodyFingerprint{}, fmt.Errorf("native output stage is missing")
	}
	file, err := openAppend(filename)
	if err != nil {
		return nativeBodyFingerprint{}, err
	}
	defer file.Close()
	component, kind := componentAndKind(section)
	if _, err := io.WriteString(file, "["+component+" · "+kind+" · "+section.Status+"]\n"); err != nil {
		return nativeBodyFingerprint{}, err
	}
	hasher := sha256.New()
	total := 0
	endsWithLineBreak := false
	copyPart := func(path string) (int64, error) {
		input, err := openRead(path)
		if err != nil {
			return 0, err
		}
		defer input.Close()
		info, err := input.Stat()
		if err != nil {
			return 0, err
		}
		counter := &countingHashWriter{writer: io.MultiWriter(file, hasher)}
		if _, err := io.Copy(counter, input); err != nil {
			return 0, err
		}
		total += counter.count
		if counter.count > 0 {
			endsWithLineBreak = counter.last == '\n' || counter.last == '\r'
		}
		return info.Size(), nil
	}
	stdoutSize, err := copyPart(stage.stdoutPath)
	if err != nil {
		return nativeBodyFingerprint{}, err
	}
	stderrFile, err := openRead(stage.stderrPath)
	if err != nil {
		return nativeBodyFingerprint{}, err
	}
	stderrInfo, err := stderrFile.Stat()
	closeErr := stderrFile.Close()
	if err != nil {
		return nativeBodyFingerprint{}, err
	}
	if closeErr != nil {
		return nativeBodyFingerprint{}, closeErr
	}
	if stdoutSize > 0 && stderrInfo.Size() > 0 && !endsWithLineBreak {
		if _, err := io.WriteString(file, "\n"); err != nil {
			return nativeBodyFingerprint{}, err
		}
		_, _ = hasher.Write([]byte{'\n'})
		total++
		endsWithLineBreak = true
	}
	if _, err := copyPart(stage.stderrPath); err != nil {
		return nativeBodyFingerprint{}, err
	}
	if total == 0 && section.Body != "" {
		if _, err := io.WriteString(file, section.Body); err != nil {
			return nativeBodyFingerprint{}, err
		}
		_, _ = hasher.Write([]byte(section.Body))
		total = len(section.Body)
		endsWithLineBreak = strings.HasSuffix(section.Body, "\n") || strings.HasSuffix(section.Body, "\r")
	}
	if _, err := io.WriteString(file, "\n\n"); err != nil {
		return nativeBodyFingerprint{}, err
	}
	if afterSectionWrite != nil {
		afterSectionWrite()
	}
	if err := file.Sync(); err != nil {
		return nativeBodyFingerprint{}, err
	}
	var digest [sha256.Size]byte
	copy(digest[:], hasher.Sum(nil))
	return nativeBodyFingerprint{length: total, digest: digest, endsWithLineBreak: endsWithLineBreak}, nil
}

type countingHashWriter struct {
	writer io.Writer
	count  int
	last   byte
}

func (writer *countingHashWriter) Write(value []byte) (int, error) {
	count, err := writer.writer.Write(value)
	if count > 0 {
		writer.count += count
		writer.last = value[count-1]
	}
	return count, err
}

func finalStatus(status string) string {
	switch status {
	case "success":
		return "succeeded"
	case "completed_with_issues", "partial":
		return "warning"
	case "reconnect_required":
		return "failed"
	default:
		return status
	}
}

func rememberNativeBody(operationID string, section Section) {
	if section.Domain == "native" && section.Body != "" {
		// Aggregate operation results sometimes repeat an already persisted
		// native payload. Keep only fixed-size fingerprints until finalization so
		// deduplication does not retain another copy of a potentially large log.
		stateMu.Lock()
		nativeBodies[operationID] = append(nativeBodies[operationID], nativeBodyFingerprint{
			length:            len(section.Body),
			digest:            sha256.Sum256([]byte(section.Body)),
			endsWithLineBreak: strings.HasSuffix(section.Body, "\n"),
		})
		stateMu.Unlock()
	}
}

func rememberNativeFingerprint(operationID string, fingerprint nativeBodyFingerprint) {
	if fingerprint.length > 0 {
		stateMu.Lock()
		nativeBodies[operationID] = append(nativeBodies[operationID], fingerprint)
		stateMu.Unlock()
	}
}

func rememberNativeDiagnostic(operationID, diagnostic string) {
	if diagnostic == "" {
		return
	}
	stateMu.Lock()
	nativeBodies[operationID] = append(nativeBodies[operationID], nativeBodyFingerprint{
		length:            len(diagnostic),
		digest:            sha256.Sum256([]byte(diagnostic)),
		endsWithLineBreak: strings.HasSuffix(diagnostic, "\n") || strings.HasSuffix(diagnostic, "\r"),
	})
	stateMu.Unlock()
}

func appendNativeStage(operationID, filename string, stage *nativeStage, kind, fallbackStatus string) {
	status := stage.status
	if status == "" {
		status = fallbackStatus
	}
	fingerprint, err := appendStagedSectionFile(filename, Section{
		Engine: stage.engine, Domain: "native", Kind: kind, Status: status,
	}, stage)
	removeStage(stage)
	if err == nil {
		rememberNativeFingerprint(operationID, fingerprint)
		rememberNativeDiagnostic(operationID, stage.diagnostic)
	}
}

func appendPendingNativeStages(operationID, filename string) {
	stateMu.Lock()
	stages := stagedNative[operationID]
	delete(stagedNative, operationID)
	stateMu.Unlock()
	for _, stage := range stages {
		appendNativeStage(operationID, filename, stage, stage.kind, stage.status)
	}
}

func stripRepresentedNativePrefix(operationID, output string) string {
	stateMu.Lock()
	bodies := append([]nativeBodyFingerprint(nil), nativeBodies[operationID]...)
	stateMu.Unlock()
	remaining := output
	for {
		bestLength := 0
		for _, body := range bodies {
			if len(remaining) < body.length || sha256.Sum256([]byte(remaining[:body.length])) != body.digest {
				continue
			}
			if len(remaining) == body.length {
				return ""
			}
			if body.length <= bestLength {
				continue
			}
			suffix := remaining[body.length:]
			if body.endsWithLineBreak || strings.HasPrefix(suffix, "\n") || strings.HasPrefix(suffix, "\r\n") {
				bestLength = body.length
			}
		}
		if bestLength == 0 {
			return remaining
		}
		remaining = remaining[bestLength:]
		remaining = strings.TrimPrefix(remaining, "\r\n")
		remaining = strings.TrimPrefix(remaining, "\n")
	}
}

func clearNativeBodies(operationID string) {
	stateMu.Lock()
	delete(nativeBodies, operationID)
	stateMu.Unlock()
}

func fileHasContent(filename string) (bool, error) {
	file, err := openRead(filename)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	info, statErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil {
		return false, statErr
	}
	if closeErr != nil {
		return false, closeErr
	}
	return info.Size() > 0, nil
}

func appendFinalFile(operationID, filename, engine, kind, status, output string) error {
	body := stripRepresentedNativePrefix(operationID, output)
	if body == "" {
		hasContent, err := fileHasContent(filename)
		if err != nil || hasContent {
			return err
		}
	}
	return appendSectionFile(filename, Section{
		Engine: engine, Domain: "application", Kind: kind, Status: finalStatus(status), Body: body,
	})
}

// AppendFinal suppresses only an exact complete native payload already written
// in this process, optionally followed by a wrapper-only suffix. Repeated text
// in distinct sections remains attributable and is never line-deduplicated.
func AppendFinal(operationID, engine, kind, status, output string) error {
	filename, err := path(operationID)
	if err != nil {
		// This call represents a terminal outcome even when the diagnostic path is
		// unavailable, so no deduplication state should survive it.
		release := lockOperation(operationID)
		removeOperationStagesLocked(operationID)
		clearNativeBodies(operationID)
		release()
		return err
	}
	release := lockOperation(operationID)
	defer release()
	appendPendingNativeStages(operationID, filename)
	err = appendFinalFile(operationID, filename, engine, kind, status, output)
	clearNativeBodies(operationID)
	return err
}

// FinalizeSection holds the narrow local file barrier across the authoritative
// SQLite step transition and its owned append attempt. A diagnostic I/O error
// remains result-neutral once persistence succeeds.
func FinalizeSection(operationID string, section Section, persist func() error) error {
	filename, err := path(operationID)
	if err != nil {
		return persist()
	}
	release := lockOperation(operationID)
	defer release()
	if err := persist(); err != nil {
		return err
	}
	if section.Domain == "native" {
		stateMu.Lock()
		stages := stagedNative[operationID]
		lastMatch := -1
		for index, stage := range stages {
			if stage.kind == section.Kind && stage.engine == section.Engine {
				lastMatch = index
			}
		}
		var precedingStages []*nativeStage
		if lastMatch >= 0 {
			precedingStages = append(precedingStages, stages[:lastMatch+1]...)
			stages = stages[lastMatch+1:]
		} else if len(stages) > 0 {
			// Every staged child completed before this durable native outcome.
			// If none represents the outcome itself (for example, a successful
			// inventory followed by a preflight parse failure), persist that
			// chronological prefix before appending the unmatched outcome.
			precedingStages = append(precedingStages, stages...)
			stages = nil
		}
		if len(stages) == 0 {
			delete(stagedNative, operationID)
		} else {
			stagedNative[operationID] = stages
		}
		stateMu.Unlock()
		if len(precedingStages) > 0 {
			for _, stage := range precedingStages {
				kind := stage.kind
				if stage.engine == section.Engine && stage.kind == section.Kind {
					kind = section.Kind
				}
				appendNativeStage(operationID, filename, stage, kind, section.Status)
			}
			if lastMatch >= 0 {
				rememberNativeBody(operationID, section)
				return nil
			}
		}
	}
	if appendSectionFile(filename, section) == nil {
		rememberNativeBody(operationID, section)
	}
	return nil
}

// FinalizeOperation applies the same local barrier to terminal operation
// persistence. Database status is authoritative even if the file append fails.
func FinalizeOperation(operationID, engine, kind, status, output string, persist func() error) error {
	filename, err := path(operationID)
	if err != nil {
		if err := persist(); err != nil {
			return err
		}
		release := lockOperation(operationID)
		removeOperationStagesLocked(operationID)
		clearNativeBodies(operationID)
		release()
		return nil
	}
	release := lockOperation(operationID)
	defer release()
	if err := persist(); err != nil {
		return err
	}
	appendPendingNativeStages(operationID, filename)
	_ = appendFinalFile(operationID, filename, engine, kind, status, output)
	clearNativeBodies(operationID)
	return nil
}

// FinalizeOperations keeps a batch transaction and all of its local terminal
// append attempts behind the same read barrier. Database state remains the
// authority if any diagnostic append fails.
func FinalizeOperations(finals []Final, persist func() error) error {
	filenames := make([]string, len(finals))
	for index, final := range finals {
		filename, err := path(final.OperationID)
		if err != nil {
			if err := persist(); err != nil {
				return err
			}
			operationIDs := make([]string, 0, len(finals))
			for _, final := range finals {
				operationIDs = append(operationIDs, final.OperationID)
			}
			release := lockOperations(operationIDs)
			for _, final := range finals {
				removeOperationStagesLocked(final.OperationID)
				clearNativeBodies(final.OperationID)
			}
			release()
			return nil
		}
		filenames[index] = filename
	}
	operationIDs := make([]string, 0, len(finals))
	for _, final := range finals {
		operationIDs = append(operationIDs, final.OperationID)
	}
	release := lockOperations(operationIDs)
	defer release()
	if err := persist(); err != nil {
		return err
	}
	for index, final := range finals {
		appendPendingNativeStages(final.OperationID, filenames[index])
		_ = appendFinalFile(final.OperationID, filenames[index], final.Engine,
			final.Kind, final.Status, final.Output)
		clearNativeBodies(final.OperationID)
	}
	return nil
}

func Read(operationID string) (string, error) {
	value, _, err := ReadBounded(operationID, -1)
	return value, err
}

// Chunk is one bounded completed-log page. Offsets and sizes are file-byte
// positions; no page read scales with the total operation-log size.
type Chunk struct {
	Available      bool
	Data           []byte
	DisplayData    []byte
	Offset         int64
	PreviousOffset int64
	NextOffset     int64
	Size           int64
	EOF            bool
}

func ReadChunk(operationID string, offset int64, limit int) (Chunk, error) {
	filename, err := path(operationID)
	if err != nil {
		return Chunk{}, err
	}
	if offset < 0 || limit <= 0 {
		return Chunk{}, fmt.Errorf("operation-log chunk bounds are invalid")
	}
	release := lockOperation(operationID)
	defer release()
	file, err := openRead(filename)
	if errors.Is(err, os.ErrNotExist) {
		return Chunk{EOF: true}, nil
	}
	if err != nil {
		return Chunk{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Chunk{}, err
	}
	size := info.Size()
	if offset > size {
		return Chunk{}, ErrInvalidChunkOffset
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return Chunk{}, err
	}
	data := make([]byte, limit)
	count, readErr := io.ReadFull(file, data)
	if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
		return Chunk{}, readErr
	}
	data = data[:count]
	next := offset + int64(count)
	displayData, err := chunkDisplayData(file, data, offset, next, size)
	if err != nil {
		return Chunk{}, err
	}
	previous := offset - int64(limit)
	if previous < 0 {
		previous = 0
	}
	return Chunk{Available: size > 0, Data: data, Offset: offset,
		DisplayData: displayData, PreviousOffset: previous, NextOffset: next, Size: size, EOF: next >= size}, nil
}

func chunkDisplayData(file *os.File, data []byte, offset, next, size int64) ([]byte, error) {
	var prefix, suffix []byte
	leading := 0
	for leading < len(data) && leading < 3 && data[leading]&0xc0 == 0x80 {
		leading++
	}
	if leading > 0 && offset > 0 {
		available := int64(3)
		if offset < available {
			available = offset
		}
		before := make([]byte, available)
		if _, err := file.ReadAt(before, offset-available); err != nil && err != io.EOF {
			return nil, err
		}
		for count := 1; count <= len(before) && count+leading <= utf8.UTFMax; count++ {
			candidate := append(append([]byte(nil), before[len(before)-count:]...), data[:leading]...)
			if utf8.Valid(candidate) && utf8.RuneCount(candidate) == 1 {
				prefix = append([]byte(nil), before[len(before)-count:]...)
				break
			}
		}
	}
	if next < size {
		safe := trimIncompleteUTF8Tail(data)
		if len(safe) < len(data) {
			incomplete := data[len(safe):]
			missing := utf8RuneWidth(incomplete[0]) - len(incomplete)
			if missing > 0 && missing <= 3 && int64(missing) <= size-next {
				after := make([]byte, missing)
				if _, err := file.ReadAt(after, next); err != nil && err != io.EOF {
					return nil, err
				}
				candidate := append(append([]byte(nil), incomplete...), after...)
				if utf8.Valid(candidate) && utf8.RuneCount(candidate) == 1 {
					suffix = after
				}
			}
		}
	}
	display := make([]byte, 0, len(prefix)+len(data)+len(suffix))
	display = append(display, prefix...)
	display = append(display, data...)
	display = append(display, suffix...)
	return display, nil
}

func utf8RuneWidth(first byte) int {
	switch {
	case first >= 0xc2 && first <= 0xdf:
		return 2
	case first >= 0xe0 && first <= 0xef:
		return 3
	case first >= 0xf0 && first <= 0xf4:
		return 4
	default:
		return 0
	}
}

func trimIncompleteUTF8Tail(data []byte) []byte {
	if len(data) == 0 {
		return data
	}
	start := len(data) - 1
	for start > 0 && len(data)-start < 4 && data[start]&0xc0 == 0x80 {
		start--
	}
	width := 0
	switch first := data[start]; {
	case first >= 0xc2 && first <= 0xdf:
		width = 2
	case first >= 0xe0 && first <= 0xef:
		width = 3
	case first >= 0xf0 && first <= 0xf4:
		width = 4
	}
	if width == 0 || len(data)-start >= width {
		return data
	}
	for _, value := range data[start+1:] {
		if value&0xc0 != 0x80 {
			return data
		}
	}
	return data[:start]
}

// ReadBounded reads at most limit bytes plus one truncation probe. Support
// reports use this path so generating a report never loads a whole large log.
func ReadBounded(operationID string, limit int) (string, bool, error) {
	filename, err := path(operationID)
	if err != nil {
		return "", false, err
	}
	release := lockOperation(operationID)
	defer release()
	return readBoundedFile(filename, limit)
}

// ReadBoundedBestEffort samples a diagnostic file without taking the terminal
// append barrier. It is intended for bounded reports that already hold a
// database snapshot: such callers accept an incomplete diagnostic sample and
// must not invert the finalizers' log-then-database lock order.
func ReadBoundedBestEffort(operationID string, limit int) (string, bool, error) {
	filename, err := path(operationID)
	if err != nil {
		return "", false, err
	}
	return readBoundedFile(filename, limit)
}

// ReadBoundedBestEffortCompleteSections takes the same unlocked bounded sample
// but returns only section frames proven complete in that sample. An append in
// progress may leave a partial final header or body; that tail is omitted while
// earlier complete sections remain available to the support export.
func ReadBoundedBestEffortCompleteSections(operationID string, limit int) (string, error) {
	value, _, err := ReadBoundedBestEffort(operationID, limit)
	if err != nil || value == "" {
		return "", err
	}
	if strings.HasSuffix(value, "\n\n") {
		return value, nil
	}
	boundary := strings.LastIndex(value, "\n\n[")
	if boundary < 0 {
		return "", nil
	}
	return value[:boundary+2], nil
}

func readBoundedFile(filename string, limit int) (string, bool, error) {
	file, err := openRead(filename)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer file.Close()
	if limit < 0 {
		data, err := io.ReadAll(file)
		return string(data), false, err
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return "", false, err
	}
	if len(data) > limit {
		return string(data[:limit]), true, nil
	}
	return string(data), false, nil
}

func Remove(operationID string) error {
	filename, err := path(operationID)
	if err != nil {
		return err
	}
	release := lockOperation(operationID)
	defer release()
	removeOperationStagesLocked(operationID)
	file, err := openRead(filename)
	if errors.Is(err, os.ErrNotExist) {
		clearNativeBodies(operationID)
		return nil
	}
	if err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := removeOperationLogFile(filename); err != nil {
		return err
	}
	clearNativeBodies(operationID)
	return nil
}
