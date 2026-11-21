package api

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/local/replicaro/command"
	"github.com/local/replicaro/database"
	"github.com/local/replicaro/engines"
	"github.com/local/replicaro/models"
	"github.com/local/replicaro/operationruntime"
	"github.com/local/replicaro/vaultlock"
)

// A single-file download takes two requests. The page first asks
// downloadTicketPath, an ordinary protected request, which checks the vault,
// the backup and the file and hands out a one-time ticket. The browser then
// opens downloadPath with that ticket and saves what comes back. That second
// request can't carry the X-Replicaro-Client-UUID header, because the browser
// makes it itself; the ticket stands in for the header there and nowhere else
// (secureDownloadRequest in security.go).
//
// Unlike a restore, the download runs inside its request: the engine's output
// is the file and goes straight to the browser, with no copy kept on this
// computer. It holds the vault lock for itself, without waiting for it, and is
// recorded as a "download" operation (timeline, Active now, live error output,
// Cancel). It ends when the browser's connection drops, on Cancel, when the
// application stops, or after downloadStallLimit without progress.
//
// The ticket also names the ID the download will be recorded under, so the
// page can follow the operation (GET /api/operations?id=) once the browser
// has started it. Until then the ID exists only in the ticket: a ticket that
// is refused or never used leaves no record under it.
const (
	downloadPath          = "/api/download"
	downloadTicketPath    = "/api/download/ticket"
	downloadOperationKind = "download"
)

var (
	// downloadTicketLifetime is how long an unused ticket stays valid.
	downloadTicketLifetime = 30 * time.Second
	// downloadStallLimit ends a download that sent nothing for this long: the
	// browser paused it or stopped reading, the network vanished, or the
	// engine wrote nothing. It keeps a stuck download from holding the vault
	// lock, and with it backups, indefinitely.
	downloadStallLimit = 10 * time.Minute
	// downloadNativeFile runs the engine's download command; tests replace it.
	downloadNativeFile = engines.DownloadFile
)

var (
	errDownloadColdStorage    = errors.New("files in a Cold Storage vault can't be downloaded")
	errDownloadNotFound       = errors.New("the file was not found in this backup")
	errDownloadNotRegularFile = errors.New("only a regular file can be downloaded")
	errDownloadNotReady       = errors.New("backup data is still being prepared")
	errDownloadVaultBusy      = errors.New("vault is busy with another operation")
	errDownloadStalled        = errors.New("download made no progress")
	errDownloadClientGone     = errors.New("the browser's download connection closed")
	errDownloadAppStopping    = errors.New("application is stopping")
)

// DownloadTicketRequest names one file the way restore requests do: the
// backup, its exact native root and the path under that root. An empty path
// names a single-file source itself.
type DownloadTicketRequest struct {
	RepositoryID string `json:"repositoryId"`
	SnapshotID   string `json:"snapshotId"`
	NativeRootID string `json:"nativeRootId"`
	Path         string `json:"path"`
}

type downloadTicket struct {
	request  DownloadTicketRequest
	fileName string
	// operationID is a random UUID v4, the ID serveDownload records the
	// download under.
	operationID string
	expires     time.Time
}

// downloadTickets lives in memory only, one store per API handler, so a
// restart forgets every ticket.
type downloadTickets struct {
	mu      sync.Mutex
	tickets map[string]downloadTicket
}

func newDownloadTickets() *downloadTickets {
	return &downloadTickets{tickets: map[string]downloadTicket{}}
}

// issue stores a new ticket and returns its token and the operation ID the
// download will be recorded under.
func (store *downloadTickets) issue(request DownloadTicketRequest, fileName string) (string, string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", "", err
	}
	token := base64.RawURLEncoding.EncodeToString(raw[:])
	operationID, err := uuid.NewRandom()
	if err != nil {
		return "", "", err
	}
	now := time.Now()
	store.mu.Lock()
	defer store.mu.Unlock()
	for key, ticket := range store.tickets {
		if !now.Before(ticket.expires) {
			delete(store.tickets, key)
		}
	}
	store.tickets[token] = downloadTicket{request: request, fileName: fileName, operationID: operationID.String(), expires: now.Add(downloadTicketLifetime)}
	return token, operationID.String(), nil
}

// redeem hands out a ticket once. A used, unknown or expired ticket gives
// nothing.
func (store *downloadTickets) redeem(token string) (downloadTicket, bool) {
	if token == "" {
		return downloadTicket{}, false
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	ticket, ok := store.tickets[token]
	if !ok {
		return downloadTicket{}, false
	}
	delete(store.tickets, token)
	if !time.Now().Before(ticket.expires) {
		return downloadTicket{}, false
	}
	return ticket, true
}

func registerDownloadHandlers(mux *http.ServeMux, db *sql.DB, runtimeManager *operationruntime.Manager) {
	tickets := newDownloadTickets()
	handle(mux, downloadTicketPath, func(w http.ResponseWriter, r *http.Request) {
		issueDownloadTicket(db, tickets, w, r)
	})
	mux.HandleFunc(downloadPath, func(w http.ResponseWriter, r *http.Request) {
		ticket, ok := secureDownloadRequest(w, r, tickets.redeem)
		if !ok {
			return
		}
		serveDownload(db, runtimeManager, w, r, ticket)
	})
}

// downloadTarget is a file that the metadata cache shows as a regular file
// in a backup this vault profile can see.
type downloadTarget struct {
	snapshotID string
	root       models.SnapshotSourceRoot
	path       string
	fileName   string
}

// checkDownloadTarget makes the checks the ticket request makes and the
// download repeats under the vault lock: the vault isn't Cold Storage, the
// backup is indexed and visible to this vault profile, the native root is
// exact, and the path names a regular file. The cache is the same indexed
// listing Restore and File History browse.
func checkDownloadTarget(ctx context.Context, db *sql.DB, repo models.Repository, request DownloadTicketRequest) (downloadTarget, error) {
	if repo.ColdStorage {
		return downloadTarget{}, errDownloadColdStorage
	}
	if request.SnapshotID == "" || strings.ContainsRune(request.SnapshotID, '\x00') {
		return downloadTarget{}, errDownloadNotFound
	}
	if request.Path != "" {
		if _, err := canonicalRestoreSelectionPath(request.Path); err != nil {
			return downloadTarget{}, err
		}
	} else if strings.TrimSpace(request.NativeRootID) == "" {
		// An empty path is only ever a single-file source, which is always
		// named by its exact native root.
		return downloadTarget{}, errDownloadNotRegularFile
	}
	authority, err := database.LoadMetadataReadAuthority(ctx, db, repo.ID)
	if err != nil {
		return downloadTarget{}, err
	}
	knownJobs, err := database.JobIDsForRepository(db, repo.ID)
	if err != nil {
		return downloadTarget{}, err
	}
	parent, name := "", request.Path
	if index := strings.LastIndex(request.Path, "/"); index >= 0 {
		parent, name = request.Path[:index], request.Path[index+1:]
	}
	snapshot, entries, ready, err := database.ReadyMetadataSnapshotEntriesForAuthority(ctx, db, authority, request.SnapshotID, parent)
	if errors.Is(err, database.ErrMetadataIndexingUnavailable) {
		return downloadTarget{}, errDownloadNotReady
	}
	if err != nil {
		return downloadTarget{}, err
	}
	if snapshot.ID == "" {
		return downloadTarget{}, errDownloadNotFound
	}
	if !ready {
		return downloadTarget{}, errDownloadNotReady
	}
	if models.PresentSnapshot(snapshot, repo.ProfileUUID, knownJobs).Presentation == models.SnapshotPresentationHidden {
		return downloadTarget{}, errDownloadNotFound
	}
	root, err := exactRestoreNativeRoot(snapshot, request.NativeRootID, true)
	if err != nil {
		return downloadTarget{}, errDownloadNotFound
	}
	target := downloadTarget{snapshotID: request.SnapshotID, root: root, path: request.Path}
	if request.Path == "" {
		fileRoots, err := database.MetadataSnapshotFileRootsForAuthority(ctx, db, authority, []string{request.SnapshotID})
		if errors.Is(err, database.ErrMetadataIndexingUnavailable) {
			return downloadTarget{}, errDownloadNotReady
		}
		if err != nil {
			return downloadTarget{}, err
		}
		// Both engines index a source root as a file only when it is a
		// regular file (Restic's node type "file", Kopia's root type "f").
		if _, ok := fileRoots[request.SnapshotID][root.Identity]; !ok {
			return downloadTarget{}, errDownloadNotRegularFile
		}
		target.fileName = database.MetadataFileRootName(root.Path)
		return target, nil
	}
	for _, entry := range entries {
		if entry.Path != request.Path || entry.SourceRoot != root.Path {
			continue
		}
		if entry.IsDir || !models.RegularFileMode(entry.Mode) {
			return downloadTarget{}, errDownloadNotRegularFile
		}
		target.fileName = name
		return target, nil
	}
	return downloadTarget{}, errDownloadNotFound
}

// writeDownloadRefusal answers a refused ticket request or download before
// anything was recorded. The codes let the page show its own text.
func writeDownloadRefusal(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errDownloadColdStorage):
		writeCodedError(w, http.StatusUnprocessableEntity, "download_cold_storage", err.Error())
	case errors.Is(err, errDownloadNotFound):
		writeCodedError(w, http.StatusNotFound, "download_not_found", err.Error())
	case errors.Is(err, errDownloadNotRegularFile):
		writeCodedError(w, http.StatusUnprocessableEntity, "download_not_regular_file", err.Error())
	case errors.Is(err, errDownloadNotReady):
		writeCodedError(w, http.StatusConflict, "metadata_not_ready", err.Error())
	case errors.Is(err, errDownloadVaultBusy):
		writeCodedError(w, http.StatusConflict, "vault_busy", err.Error())
	case writeVaultWorkRefusal(w, err):
	default:
		writeError(w, http.StatusInternalServerError, err)
	}
}

func writeDownloadHTTPVersionRefusal(w http.ResponseWriter) {
	writeCodedError(w, http.StatusHTTPVersionNotSupported, "download_http_version", "downloads need HTTP/1.1 or later")
}

func issueDownloadTicket(db *sql.DB, tickets *downloadTickets, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		badRequest(w, "invalid method")
		return
	}
	// The download address refuses HTTP/1.0 (see serveDownload), and the page
	// can't show that answer: the browser makes that request itself. A reverse
	// proxy that speaks HTTP/1.0 to Replicaro does so for this request too, so
	// refusing here lets the page show why instead of a failed download.
	if !r.ProtoAtLeast(1, 1) {
		writeDownloadHTTPVersionRefusal(w)
		return
	}
	var request DownloadTicketRequest
	if err := decodeRequest(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if request.RepositoryID == "" || request.SnapshotID == "" {
		badRequest(w, "repositoryId and snapshotId are required")
		return
	}
	if request.Path != "" {
		if _, err := canonicalRestoreSelectionPath(request.Path); err != nil {
			badRequest(w, err.Error())
			return
		}
	}
	repo, err := repositoryByID(db, request.RepositoryID)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	target, err := checkDownloadTarget(r.Context(), db, repo, request)
	if err != nil {
		writeDownloadRefusal(w, err)
		return
	}
	if refuseWhilePasswordChangeUnfinished(w, db, repo.ID) {
		return
	}
	if err := database.RequireVaultNotBeingRemoved(db, repo.ID); err != nil {
		writeDownloadRefusal(w, err)
		return
	}
	// Only a look: no lock is taken and nothing is recorded, so a ticket that
	// is never used leaves no trace. The download takes the lock itself and
	// gives the same answer when the vault became busy in between.
	if vaultlock.Busy(repo.ID) {
		writeDownloadRefusal(w, errDownloadVaultBusy)
		return
	}
	token, operationID, err := tickets.issue(request, target.fileName)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, map[string]any{
		"url":              downloadPath + "?ticket=" + token,
		"fileName":         target.fileName,
		"expiresInSeconds": int(downloadTicketLifetime / time.Second),
		"operationId":      operationID,
	})
}

// downloadWriter passes the engine's output to the response. Each write is
// flushed to the connection at once, so a write only counts as progress once
// the bytes have left this process, and restarts the no-progress timer.
// A failed write means the browser's connection is gone, which ends the
// engine at once, unless this side cut the connection itself (stalled or
// cut): then the reason is already known.
//
// The connection has no write deadline of its own (the server sets no
// WriteTimeout), so a write the browser isn't taking waits until one of two
// things in runDownloadOperation sets the deadline to now: the no-progress
// timer, or the cut on Cancel, the application stopping or the browser
// leaving. Nothing here moves the deadline. A write that pushed it forward
// would undo the one the timer or the cut set, and a write the engine makes on
// its way out would then wait on a paused browser for another whole limit.
type downloadWriter struct {
	w          http.ResponseWriter
	controller *http.ResponseController
	fileName   string
	headerSent bool
	sent       int64
	err        error
	stall      *time.Timer
	stalled    atomic.Bool
	// cut is set when the download was ended (Cancel, the application
	// stopping, the browser leaving) while a write might be waiting on a
	// browser that has paused the download; see runDownloadOperation.
	cut        atomic.Bool
	clientGone context.CancelCauseFunc
}

func (writer *downloadWriter) sendHeader() {
	if writer.headerSent {
		return
	}
	writer.headerSent = true
	header := writer.w.Header()
	header.Set("Content-Type", "application/octet-stream")
	header.Set("Content-Disposition", downloadContentDisposition(writer.fileName))
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Cache-Control", "no-store")
	// No Content-Length: Restic's cached size is rounded text, and the
	// chunked encoding without it already lets an aborted response show as
	// a failed transfer.
	writer.w.WriteHeader(http.StatusOK)
}

func (writer *downloadWriter) Write(value []byte) (int, error) {
	if writer.err != nil {
		return 0, writer.err
	}
	writer.sendHeader()
	written, err := writer.w.Write(value)
	writer.sent += int64(written)
	if err == nil {
		err = writer.controller.Flush()
	}
	if err != nil {
		writer.err = err
		if !writer.stalled.Load() && !writer.cut.Load() {
			writer.clientGone(errDownloadClientGone)
		}
		return written, err
	}
	if !writer.stalled.Load() {
		writer.stall.Reset(downloadStallLimit)
	}
	return written, nil
}

// serveDownload runs one download for a redeemed ticket. Every refusal
// before the operation is recorded is a JSON error with nothing recorded and
// the vault lock, if it was taken, released. Once the operation is recorded,
// a failure ends the response by cutting the connection
// (http.ErrAbortHandler), after the lock is released and the final status
// saved: with no complete response the browser reports a failed download
// instead of saving a short file.
func serveDownload(db *sql.DB, runtimeManager *operationruntime.Manager, w http.ResponseWriter, r *http.Request, ticket downloadTicket) {
	repo, err := repositoryByID(db, ticket.request.RepositoryID)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	if repo.ColdStorage {
		writeDownloadRefusal(w, errDownloadColdStorage)
		return
	}
	// An HTTP/1.0 response without a length ends by closing the connection,
	// so a download cut short would look complete. The ticket request already
	// refuses HTTP/1.0; this is checked again for the request that streams.
	if !r.ProtoAtLeast(1, 1) {
		writeDownloadHTTPVersionRefusal(w)
		return
	}
	lifetime, done, err := runtimeManager.BeginBackground()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	defer done()
	unlock, ok, err := vaultlock.YieldLowPriorityAndTryExclusiveContext(r.Context(), repo.ID)
	if err != nil {
		// The browser's request already ended.
		return
	}
	if !ok {
		writeDownloadRefusal(w, errDownloadVaultBusy)
		return
	}
	// The lock is released early, as soon as the engine has exited, and in any
	// case when this handler ends, panics included: net/http recovers a
	// handler's panic and the application keeps running, so a lock left held
	// here would block the vault until a restart. A record a panic leaves
	// running is marked interrupted by the startup clean-up, as one whose final
	// write failed is.
	release := sync.OnceFunc(unlock)
	defer release()
	// A removal queued since the ticket was issued gets the vault next; the
	// download doesn't start and hold it back for a whole transfer.
	if err := database.RequireVaultNotBeingRemoved(db, repo.ID); err != nil {
		release()
		writeDownloadRefusal(w, err)
		return
	}
	if err := database.StartOperationWithID(db, ticket.operationID, downloadOperationKind, downloadTitle(ticket.fileName), "", repo.ID, time.Now()); err != nil {
		release()
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	succeeded := runDownloadOperation(db, runtimeManager, lifetime, w, r, repo, ticket, ticket.operationID, release)
	release()
	if !succeeded {
		panic(http.ErrAbortHandler)
	}
}

// runDownloadOperation does the recorded part of a download and reports
// whether it succeeded: the engine exited successfully and the connection
// accepted every byte it wrote. That is as far as this side can see; bytes
// still on their way when the browser goes away at the very end are not
// confirmed by anything. It always saves the final status
// before it returns. release frees the vault lock; it is called as soon as
// the engine has exited, so whoever sees the final status can take the
// vault at once.
func runDownloadOperation(db *sql.DB, runtimeManager *operationruntime.Manager, lifetime context.Context, w http.ResponseWriter, r *http.Request, repo models.Repository, ticket downloadTicket, operationID string, release func()) bool {
	// The download ends with the browser's request, on Cancel, or when the
	// application stops; the native command also ends on the no-progress
	// limit, which is a failure rather than an interruption.
	requestCtx, endRequest := context.WithCancelCause(r.Context())
	defer endRequest(nil)
	stopWithApp := context.AfterFunc(lifetime, func() { endRequest(errDownloadAppStopping) })
	defer stopWithApp()
	operationCtx, cancelOperation := context.WithCancel(requestCtx)
	defer cancelOperation()
	persisted := false
	finish := func(status string, lines ...string) bool {
		release()
		runtimeManager.CloseCancel(operationID)
		output := strings.Join(lines, "\n")
		if err := finishOperationDurably(db, operationID, status, output, time.Now()); err != nil {
			_ = database.LogError(db, "Operation "+operationID+" "+err.Error())
			return false
		}
		persisted = true
		return status == "success"
	}
	// cancelled tells Cancel apart from the browser leaving: both can end with
	// a failed write once the connection is cut.
	var cancelled atomic.Bool
	if err := runtimeManager.Register(operationID, func() error { cancelled.Store(true); cancelOperation(); return nil }); err != nil {
		return finish("failed", err.Error())
	}
	defer func() {
		// A runtime stays attached when the final write failed, so it keeps
		// matching the running record that startup clean-up owns.
		if persisted {
			runtimeManager.Remove(operationID)
		}
	}()
	operationCtx = command.ContextWithLiveOutput(operationCtx, func(stream, text string) {
		runtimeManager.Append(operationID, stream, text)
	})
	repo, err := admitPersistedRepository(operationCtx, db, repo)
	if err != nil {
		return finish(downloadEndStatus(operationCtx, err), err.Error())
	}
	engine, err := resolveEngine(repo)
	if err != nil {
		return finish("failed", err.Error())
	}
	target, err := checkDownloadTarget(operationCtx, db, repo, ticket.request)
	if err != nil {
		return finish(downloadEndStatus(operationCtx, err), err.Error())
	}
	if err := startOperationStep(db, operationID, "native", downloadOperationKind, time.Now()); err != nil {
		return finish("failed", "persist native download start: "+err.Error())
	}

	nativeCtx, stopNative := context.WithCancelCause(operationCtx)
	defer stopNative(nil)
	controller := http.NewResponseController(w)
	writer := &downloadWriter{w: w, controller: controller, fileName: target.fileName, clientGone: endRequest}
	// stallMu keeps a late timer or cut from acting once the result is being
	// decided: after ended is set, neither marks the download nor touches the
	// connection, which the server may reuse. Under the same lock, a timer
	// that fires just after a Cancel or application stop has cut the
	// connection doesn't record the download as "made no progress". A cut
	// after the timer (the timer's own stopNative runs it) marks the download
	// cut too, which changes nothing: stalled is checked first when the
	// result is decided.
	var stallMu sync.Mutex
	ended := false
	writer.stall = time.AfterFunc(downloadStallLimit, func() {
		stallMu.Lock()
		defer stallMu.Unlock()
		if ended || writer.cut.Load() {
			return
		}
		writer.stalled.Store(true)
		stopNative(errDownloadStalled)
		// Unblock a write the browser isn't taking, and fail at once any write
		// the engine makes on its way out. This and the cut below are the only
		// places that set a write deadline on the connection; see
		// downloadWriter.
		_ = controller.SetWriteDeadline(time.Now())
	})
	// Cancel, the application stopping or the browser leaving end the engine
	// through nativeCtx, but a write blocked on a browser that has paused the
	// download would hold the vault until the no-progress limit. Cut the
	// connection at once instead; that also rules out a success, so the
	// response is aborted and the connection never reused.
	stopCut := context.AfterFunc(nativeCtx, func() {
		stallMu.Lock()
		defer stallMu.Unlock()
		if ended {
			return
		}
		writer.cut.Store(true)
		_ = controller.SetWriteDeadline(time.Now())
	})
	// A Restic vault whose automatic unlock is on gets Restic's own `unlock`
	// first, exactly as a restore does (engines.AutoUnlockRestic is the code
	// Restore runs). It runs before the header, so a failed unlock ends the
	// download before anything reaches the browser, and it runs under nativeCtx,
	// so Cancel, the application stopping, the browser leaving and the
	// no-progress limit end it the way they end the engine's download. Its
	// output is never written to the browser. A failed unlock is a preparation
	// failure: the download's own command never starts.
	var result engines.DownloadResult
	nativeErr := engines.AutoUnlockRestic(nativeCtx, engine, repo)
	nativeRan := false
	if nativeErr == nil {
		// The browser's download starts now, before the engine's first byte: the
		// checks are done, and what remains can only end it as a failed transfer.
		// The no-progress timer is already running, so a browser that never takes
		// the header is ended by it too.
		writer.sendHeader()
		if err := controller.Flush(); err != nil {
			writer.err = err
			endRequest(errDownloadClientGone)
		}
		nativeRan = writer.err == nil
		if nativeRan {
			result, nativeErr = downloadNativeFile(nativeCtx, engine, repo, target.snapshotID, target.root, target.path, writer)
		}
	}
	release()
	deliveryErr := writer.err
	stallMu.Lock()
	ended = true
	writer.stall.Stop()
	stopCut()
	stallMu.Unlock()
	stalled := writer.stalled.Load()
	cut := writer.cut.Load()

	_, processStarted, _, _, _, known := engines.RequestedOperationOutcome(nativeErr)
	processStarted = nativeRan && (processStarted || !known)
	status, reason := "success", fmt.Sprintf("Downloaded %d bytes.", writer.sent)
	switch {
	case stalled:
		status, reason = "failed", fmt.Sprintf("The download made no progress for %s and was stopped.", downloadStallLimit)
	case nativeErr == nil && deliveryErr == nil && !cut:
	case operationCtx.Err() != nil || deliveryErr != nil || cut:
		status = "interrupted"
		switch {
		case errors.Is(context.Cause(requestCtx), errDownloadAppStopping):
			reason = "The application stopped before the download completed."
		case cancelled.Load():
			reason = "The download was cancelled."
		default:
			reason = "The browser's download connection closed before the download completed."
		}
	default:
		// The engine's error output is already the native step's output, so
		// the summary doesn't repeat it.
		status, reason = "failed", nativeErr.Error()
		if processStarted && strings.TrimSpace(result.Output) != "" {
			reason = "The native download failed; see the engine's error output."
		}
	}
	if status != "success" {
		// The reason can be the engine step's error text, which often has no
		// full stop of its own; end it with one so the two sentences read as
		// two.
		reason = strings.TrimRight(reason, " \t\r\n")
		if !strings.HasSuffix(reason, ".") {
			reason += "."
		}
		reason += fmt.Sprintf(" %d bytes were sent before it ended.", writer.sent)
	}
	stepStatus, stepOutput := "succeeded", result.Output
	switch {
	case !processStarted:
		stepStatus, stepOutput = "skipped", "native download did not start"
	case status == "interrupted":
		stepStatus = "interrupted"
	case status == "failed" && stalled:
		stepStatus = "failed"
	case status == "failed" && strings.TrimSpace(result.Output) == "":
		stepStatus, stepOutput = "failed", nativeErr.Error()
	case status == "failed":
		stepStatus = "failed"
	}
	if err := finishNativeOperationStep(db, operationID, downloadOperationKind, stepStatus, stepOutput, time.Now()); err != nil {
		reason += "\npersist native download result: " + err.Error()
	}
	lines := []string{}
	if result.Command != "" {
		lines = append(lines, "Native command: "+result.Command)
	}
	return finish(status, append(lines, reason)...)
}

// downloadEndStatus is the final status of a download that ended before its
// native command ran.
func downloadEndStatus(operationCtx context.Context, err error) string {
	if operationruntime.TerminalStatus(operationCtx, err) == "interrupted" || operationCtx.Err() != nil {
		return "interrupted"
	}
	return "failed"
}

// downloadTitle names the operation after the file. Control characters
// (a newline is valid in a file name) become spaces so the title stays one
// line in the timeline and the tray.
func downloadTitle(fileName string) string {
	return "Download: " + strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, fileName)
}

// downloadContentDisposition always marks the response as an attachment, so
// a backed-up HTML or SVG file is saved and never shown as a Replicaro page.
// filename* carries the exact name, percent-encoded as UTF-8 (RFC 6266 and
// RFC 8187); filename is a plain ASCII fallback for clients without it.
func downloadContentDisposition(name string) string {
	var fallback strings.Builder
	for _, r := range name {
		if r < 0x20 || r >= 0x7f || r == '"' || r == '\\' || r == '%' {
			fallback.WriteByte('_')
			continue
		}
		fallback.WriteRune(r)
	}
	value := "attachment; filename=\"" + fallback.String() + "\""
	if !utf8.ValidString(name) || name == "" {
		return value
	}
	var encoded strings.Builder
	for index := 0; index < len(name); index++ {
		character := name[index]
		if character < 0x80 && (character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || strings.IndexByte("!#$&+-.^_`|~", character) >= 0) {
			encoded.WriteByte(character)
			continue
		}
		encoded.WriteString("%" + strings.ToUpper(strconv.FormatUint(uint64(character)|0x100, 16)[1:]))
	}
	return value + "; filename*=UTF-8''" + encoded.String()
}
