package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/local/replicaro/command"
	"github.com/local/replicaro/database"
	"github.com/local/replicaro/engines"
	"github.com/local/replicaro/metadata"
	"github.com/local/replicaro/models"
	"github.com/local/replicaro/operationruntime"
	"github.com/local/replicaro/storageavailability"
	"github.com/local/replicaro/vaultlock"
)

const metadataPageSize = 200

var deferMetadataSyncForRestore = metadata.DeferRepositorySyncForRestore

// Whole recovery needs native header identity and profile visibility, not a
// working content catalog. The caller holds the vault lock and has admitted
// the repository before this fresh native read.
func visibleNativeRestoreSnapshot(ctx context.Context, db *sql.DB, repo models.Repository, engine engines.Engine, id string) (models.Snapshot, error) {
	knownJobs, err := database.JobIDsForRepository(db, repo.ID)
	if err != nil {
		return models.Snapshot{}, err
	}
	snapshots, _, err := engines.ListSnapshotsFresh(ctx, engine, repo)
	if err != nil {
		return models.Snapshot{}, err
	}
	for _, snapshot := range snapshots {
		if snapshot.ID == id {
			snapshot = models.PresentSnapshot(snapshot, repo.ProfileUUID, knownJobs)
			if snapshot.Presentation != models.SnapshotPresentationHidden {
				return snapshot, nil
			}
			break
		}
	}
	return models.Snapshot{}, fmt.Errorf("snapshot %q is absent or not visible to this vault profile", id)
}

func visibleCachedRestoreSnapshots(ctx context.Context, db *sql.DB, repo models.Repository, authority database.MetadataReadAuthority, snapshotIDs []string) (map[string]models.Snapshot, error) {
	// Restore Browse is intentionally snapshot-scoped: a ready snapshot remains
	// usable while another snapshot is indexing or the whole-vault generation is
	// dirty. Actual restore still performs its fresh locked authoritative checks.
	knownJobs, err := database.JobIDsForRepository(db, repo.ID)
	if err != nil {
		return nil, err
	}
	cached, err := database.ReadyMetadataSnapshotsForAuthority(ctx, db, authority, snapshotIDs)
	if err != nil {
		return nil, err
	}
	visible := make(map[string]models.Snapshot, len(snapshotIDs))
	wanted := make(map[string]bool, len(snapshotIDs))
	for _, id := range snapshotIDs {
		wanted[id] = true
	}
	for _, snapshot := range cached {
		if !wanted[snapshot.ID] {
			continue
		}
		snapshot = models.PresentSnapshot(snapshot, repo.ProfileUUID, knownJobs)
		if snapshot.Presentation != models.SnapshotPresentationHidden {
			visible[snapshot.ID] = snapshot
		}
	}
	for id := range wanted {
		if _, ok := visible[id]; !ok {
			return nil, fmt.Errorf("snapshot %q is absent or not visible to this vault profile", id)
		}
	}
	return visible, nil
}

func exactRestoreNativeRoot(snapshot models.Snapshot, identity string, required bool) (models.SnapshotSourceRoot, error) {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		if required {
			if len(snapshot.SourceRoots) == 1 {
				root := snapshot.SourceRoots[0]
				if root.Identity == "" {
					root = models.WithSnapshotNativeRootIdentity(root)
				}
				return root, nil
			}
			return models.SnapshotSourceRoot{}, fmt.Errorf("selected restore requires nativeRootId when a snapshot has multiple roots")
		}
		return models.SnapshotSourceRoot{}, nil
	}
	var matched *models.SnapshotSourceRoot
	for _, root := range snapshot.SourceRoots {
		if root.Identity == "" {
			root = models.WithSnapshotNativeRootIdentity(root)
		}
		if root.Identity != identity {
			continue
		}
		if matched != nil {
			return models.SnapshotSourceRoot{}, fmt.Errorf("nativeRootId is ambiguous")
		}
		copy := root
		matched = &copy
	}
	if matched == nil {
		return models.SnapshotSourceRoot{}, fmt.Errorf("nativeRootId does not belong to the visible snapshot")
	}
	return *matched, nil
}

func metadataOffset(r *http.Request) int {
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		return 0
	}
	return offset
}

func metadataExpectedRevision(r *http.Request, offset int) (*int64, error) {
	if offset == 0 {
		return nil, nil
	}
	value := r.URL.Query().Get("revision")
	if value == "" {
		return nil, fmt.Errorf("revision is required for later metadata pages")
	}
	revision, err := strconv.ParseInt(value, 10, 64)
	if err != nil || revision < 0 {
		return nil, fmt.Errorf("revision must be a non-negative integer")
	}
	return &revision, nil
}

func fileSearch(writerDB, readDB *sql.DB, w http.ResponseWriter, r *http.Request) {
	authority, err := database.LoadMetadataReadAuthority(r.Context(), readDB, r.URL.Query().Get("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	repoModel := models.Repository{ID: authority.RepositoryID, CanonicalIdentity: authority.CanonicalIdentity}

	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(query) < 2 {
		badRequest(w, "search must contain at least two characters")
		return
	}
	offset := metadataOffset(r)
	expectedRevision, err := metadataExpectedRevision(r, offset)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	// This authoritative read and the following coherent cache transaction are
	// the request's complete database handshake. A later main mutation linearizes
	// after this read and is gated on the next request; no second main read is
	// needed to make this response truthful.
	results, revision, state, err := database.SearchReadyMetadataFilesPageForAuthority(
		r.Context(), readDB, authority, query, metadataPageSize+1, offset, expectedRevision,
	)
	if errors.Is(err, database.ErrMetadataRevisionChanged) {
		writeCodedError(w, http.StatusConflict, "metadata_revision_changed", "backup history changed; restart paging")
		return
	}
	if errors.Is(err, database.ErrMetadataIndexingUnavailable) {
		writeJSON(w, map[string]any{"items": []models.FileSearchResult{}, "indexing": true, "index": state,
			"revision": revision, "offset": offset, "nextOffset": offset, "hasMore": false})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	hasMore := len(results) > metadataPageSize
	if hasMore {
		results = results[:metadataPageSize]
	}
	coordinator := metadata.RepositoryCoordinatorState(writerDB, repoModel)
	indexing := coordinator.Running || coordinator.Pending
	writeJSON(w, map[string]any{"items": results, "indexing": indexing, "index": state, "revision": revision, "offset": offset, "nextOffset": offset + len(results), "hasMore": hasMore})
}

func fileBrowse(writerDB, readDB *sql.DB, w http.ResponseWriter, r *http.Request) {
	authority, err := database.LoadMetadataReadAuthority(r.Context(), readDB, r.URL.Query().Get("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	repoModel := models.Repository{ID: authority.RepositoryID, CanonicalIdentity: authority.CanonicalIdentity}

	source := r.URL.Query().Get("source")
	rawParent := r.URL.Query().Get("parent")
	if strings.ContainsRune(source, '\x00') || strings.ContainsRune(rawParent, '\x00') {
		badRequest(w, "source and parent must not contain null characters")
		return
	}

	offset := metadataOffset(r)
	expectedRevision, err := metadataExpectedRevision(r, offset)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	parent := rawParent
	if source == "" && parent != "" {
		badRequest(w, "source is required when browsing a folder")
		return
	}
	items, revision, state, err := database.BrowseReadyMetadataEntriesPageForAuthority(
		r.Context(), readDB, authority, source, parent, metadataPageSize+1, offset, expectedRevision,
	)
	if errors.Is(err, database.ErrMetadataRevisionChanged) {
		writeCodedError(w, http.StatusConflict, "metadata_revision_changed", "backup history changed; restart paging")
		return
	}
	if errors.Is(err, database.ErrMetadataIndexingUnavailable) {
		writeJSON(w, map[string]any{"items": []models.FileBrowseEntry{}, "indexing": true, "index": state,
			"revision": revision, "offset": offset, "nextOffset": offset, "hasMore": false})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	hasMore := len(items) > metadataPageSize
	if hasMore {
		items = items[:metadataPageSize]
	}
	coordinator := metadata.RepositoryCoordinatorState(writerDB, repoModel)
	indexing := coordinator.Running || coordinator.Pending
	writeJSON(w, map[string]any{"items": items, "indexing": indexing, "index": state, "revision": revision, "offset": offset, "nextOffset": offset + len(items), "hasMore": hasMore})
}

func fileHistory(writerDB, readDB *sql.DB, w http.ResponseWriter, r *http.Request) {
	authority, err := database.LoadMetadataReadAuthority(r.Context(), readDB, r.URL.Query().Get("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	repoModel := models.Repository{ID: authority.RepositoryID, CanonicalIdentity: authority.CanonicalIdentity}

	rawPath := r.URL.Query().Get("path")
	source := r.URL.Query().Get("source")
	if source == "" {
		badRequest(w, "source is required")
		return
	}
	if strings.ContainsRune(source, '\x00') || strings.ContainsRune(rawPath, '\x00') {
		badRequest(w, "source and path must not contain null characters")
		return
	}

	offset := metadataOffset(r)
	expectedRevision, err := metadataExpectedRevision(r, offset)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	path := rawPath
	if path == "" {
		badRequest(w, "missing file path")
		return
	}
	history, revision, state, err := database.ReadyMetadataFileHistoryPageForAuthority(
		r.Context(), readDB, authority, path, source, metadataPageSize+1, offset, expectedRevision,
	)
	if errors.Is(err, database.ErrMetadataRevisionChanged) {
		writeCodedError(w, http.StatusConflict, "metadata_revision_changed", "backup history changed; restart paging")
		return
	}
	if errors.Is(err, database.ErrMetadataIndexingUnavailable) {
		writeJSON(w, map[string]any{"items": []models.FileVersion{}, "indexing": true, "index": state,
			"revision": revision, "offset": offset, "nextOffset": offset, "hasMore": false})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	hasMore := len(history) > metadataPageSize
	if hasMore {
		history = history[:metadataPageSize]
	}
	coordinator := metadata.RepositoryCoordinatorState(writerDB, repoModel)
	indexing := coordinator.Running || coordinator.Pending
	writeJSON(w, map[string]any{"items": history, "indexing": indexing, "index": state, "revision": revision, "offset": offset, "nextOffset": offset + len(history), "hasMore": hasMore})
}

func retryFileIndex(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	repoModel, err := repoFromRequest(db, r)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSONStatus(w, http.StatusAccepted, map[string]bool{"admitted": metadata.RetryRepositoryEntries(db, repoModel)})
}

type metadataPreparationRequest struct {
	Action string `json:"action"`
}

func metadataStatusResponse(ctx context.Context, coordinatorDB, readDB *sql.DB, repo models.Repository) (map[string]any, error) {
	state, readySnapshotIDs, coordinator, err := metadata.RepositoryStatusSnapshotWithReader(ctx, coordinatorDB, readDB, repo)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"index": state, "running": coordinator.Running, "pending": coordinator.Pending,
		"paused": coordinator.Paused,
		"stage":  coordinator.Stage, "completeHeaderListing": coordinator.CompleteHeaderListing,
		"readySnapshotIds": readySnapshotIDs,
	}, nil
}

func metadataStatus(writerDB, readDB *sql.DB, w http.ResponseWriter, r *http.Request) {
	repo, err := repoFromRequest(readDB, r)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	response, err := metadataStatusResponse(r.Context(), writerDB, readDB, repo)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, response)
}

func prepareMetadata(db, readDB *sql.DB, w http.ResponseWriter, r *http.Request) {
	repo, err := repoFromRequest(readDB, r)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	var request metadataPreparationRequest
	if err := decodeRequest(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var admitted bool
	switch request.Action {
	case "access":
		admitted, err = metadata.ScheduleRepositoryAccessWithReader(db, readDB, repo)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	case "retry":
		admitted = metadata.RetryRepositoryEntries(db, repo)
	case "force":
		admitted = metadata.ForceRepositoryRefresh(db, repo)
	default:
		badRequest(w, "metadata action must be access, retry, or force")
		return
	}
	response, err := metadataStatusResponse(r.Context(), db, readDB, repo)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	response["admitted"] = admitted
	writeJSONStatus(w, http.StatusAccepted, response)
}

func snapshotIDs(snapshots []models.Snapshot) []string {
	ids := make([]string, 0, len(snapshots))
	for _, snapshot := range snapshots {
		ids = append(ids, snapshot.ID)
	}
	return ids
}

func logMetadataError(operation, repository string, err error) {
	log.Printf("metadata cache %s for %s: %v", operation, repository, err)
}

// These strings name the native tree, never the viewing machine's filesystem.
func normalizeSnapshotPath(value string) string { return strings.Trim(value, "/") }

// Slash components define archive containment. LF/CR/tab and backslashes can
// name native entries; configured-source eligibility and host path grammar do
// not apply here. Reject traversal instead of cleaning it to another selection.
func canonicalRestoreSelectionPath(value string) (string, error) {
	if value == "" || strings.ContainsRune(value, '\x00') {
		return "", fmt.Errorf("restore selection contains an invalid character")
	}
	if strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("restore selection must be relative to the snapshot source")
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || component == "." || component == ".." {
			return "", fmt.Errorf("restore selection must use exact native path components")
		}
	}
	return value, nil
}

func pathBase(value string) string {
	value = normalizeSnapshotPath(value)
	if index := strings.LastIndex(value, "/"); index >= 0 {
		return value[index+1:]
	}
	return value
}

func restoreSelection(db *sql.DB, runtimeManager *operationruntime.Manager, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		badRequest(w, "invalid method")
		return
	}

	var req RestoreSelectionRequest
	if err := decodeRequest(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.RepositoryID == "" || req.TargetPath == "" || len(req.Items) == 0 {
		badRequest(w, "repositoryId, targetPath and at least one item are required")
		return
	}
	if len(req.Items) > 100 {
		badRequest(w, "no more than 100 items can be restored at once")
		return
	}
	repoModel, err := repositoryByID(db, req.RepositoryID)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	normalized := make([]RestoreSelectionItem, len(req.Items))
	for index, item := range req.Items {
		snapshotID := item.SnapshotID
		if item.Content != nil {
			if item.Content.Kind != "snapshot" || item.Content.SnapshotID == "" ||
				snapshotID != "" && snapshotID != item.Content.SnapshotID {
				writeCodedError(w, http.StatusUnprocessableEntity, "unsupported_content_reference",
					"snapshot restore requires content {kind: \"snapshot\", snapshotId: ...}")
				return
			}
			snapshotID = item.Content.SnapshotID
		}
		path, pathErr := canonicalRestoreSelectionPath(item.Path)
		if snapshotID == "" || pathErr != nil {
			badRequest(w, "every restore item needs a snapshotId and path")
			return
		}
		normalized[index] = RestoreSelectionItem{SnapshotID: snapshotID, Path: path, NativeRootID: item.NativeRootID}
	}
	for left := 0; left < len(normalized); left++ {
		leftPath := normalized[left].Path
		for right := left + 1; right < len(normalized); right++ {
			rightPath := normalized[right].Path
			if leftPath == rightPath || strings.HasPrefix(leftPath, rightPath+"/") || strings.HasPrefix(rightPath, leftPath+"/") {
				badRequest(w, "restore selection contains duplicate or overlapping paths")
				return
			}
		}
	}
	requestedID, operationIDErr := requestedOperationID(req.OperationID)
	if operationIDErr != nil {
		writeError(w, http.StatusBadRequest, operationIDErr)
		return
	}
	if requestedID != nil {
		if err := database.ValidateOperationID(*requestedID); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		exists, err := database.OperationIDExists(db, *requestedID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if exists {
			writeError(w, http.StatusConflict, database.ErrOperationIDExists)
			return
		}
	}
	manager, engineErr := resolveEngine(repoModel)
	if engineErr != nil {
		writeError(w, http.StatusBadRequest, engineErr)
		return
	}
	descriptor := manager.Descriptor()
	if descriptor.ID == "" {
		descriptor.ID = manager.ID()
	}
	for _, item := range normalized {
		options := engines.RestoreOptions{Destination: req.TargetPath, Selection: item.Path, ConflictMode: req.ConflictMode}
		if capabilityErr := engines.ValidateRestoreCapability(descriptor, options); capabilityErr != nil {
			writeError(w, http.StatusUnprocessableEntity, capabilityErr)
			return
		}
	}
	releaseMetadataDeferral, yieldErr := deferMetadataSyncForRestore(r.Context(), db, repoModel)
	if yieldErr != nil {
		return
	}
	unlock, ok, lockErr := vaultlock.YieldLowPriorityAndTryExclusiveContext(r.Context(), repoModel.ID)
	if lockErr != nil {
		releaseMetadataDeferral()
		return
	}
	if !ok {
		releaseMetadataDeferral()
		writeError(w, http.StatusConflict, fmt.Errorf("vault is busy with another operation"))
		return
	}
	var releaseRestoreRuntime func(bool)
	terminalPersisted := false
	defer func() {
		unlock()
		releaseMetadataDeferral()
		if releaseRestoreRuntime != nil {
			releaseRestoreRuntime(terminalPersisted)
		}
	}()
	repoModel, err = admitPersistedRepository(r.Context(), db, repoModel)
	if err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	manager, err = resolveEngine(repoModel)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	snapshotIDs := make([]string, 0, len(normalized))
	for _, item := range normalized {
		snapshotIDs = append(snapshotIDs, item.SnapshotID)
	}
	authority, authorityErr := database.LoadMetadataReadAuthority(r.Context(), db, repoModel.ID)
	if authorityErr != nil {
		writeError(w, http.StatusConflict, authorityErr)
		return
	}
	visibleSnapshots, visibilityErr := visibleCachedRestoreSnapshots(r.Context(), db, repoModel, authority, snapshotIDs)
	if visibilityErr != nil {
		writeError(w, http.StatusForbidden, visibilityErr)
		return
	}
	nativeRoots := make(map[string]models.SnapshotSourceRoot, len(normalized))
	for _, item := range normalized {
		root, rootErr := exactRestoreNativeRoot(visibleSnapshots[item.SnapshotID], item.NativeRootID, true)
		if rootErr != nil {
			writeError(w, http.StatusUnprocessableEntity, rootErr)
			return
		}
		nativeRoots[item.SnapshotID+"\x00"+item.Path] = root
	}
	started := time.Now()
	operationID, operationErr := database.StartOperationWithID(
		db,
		"restore",
		fmt.Sprintf("Restore %d selected items", len(req.Items)),
		"",
		repoModel.ID,
		requestedID,
		started,
	)
	if operationErr != nil {
		if errors.Is(operationErr, database.ErrInvalidOperationID) {
			writeError(w, http.StatusBadRequest, operationErr)
			return
		}
		if errors.Is(operationErr, database.ErrOperationIDExists) {
			writeError(w, http.StatusConflict, operationErr)
			return
		}
		writeError(w, http.StatusInternalServerError, operationErr)
		return
	}
	operationCtx, closeCancelGate, releaseRuntime, runtimeErr := beginOperationRuntime(r.Context(), runtimeManager, operationID)
	if runtimeErr != nil {
		_ = finishOperationDurably(db, operationID, "failed", runtimeErr.Error(), time.Now())
		writeError(w, http.StatusInternalServerError, runtimeErr)
		return
	}
	releaseRestoreRuntime = releaseRuntime

	var kopiaNames *engines.KopiaRestoreFileNames
	if manager.ID() == engines.KopiaID {
		selections := make([]string, 0, len(normalized))
		for _, item := range normalized {
			selections = append(selections, item.Path)
		}
		kopiaNames = engines.NewKopiaRestoreFileNames(selections)
	}
	attempted, restored, failed, notAttempted, orchestrationFailed := 0, 0, 0, 0, 0
	results := make([]RestoreSelectionItemResult, 0, len(normalized))
	stopReason := ""
	cancellationInterrupted := false
	for itemIndex, item := range normalized {
		if stopReason != "" || operationCtx.Err() != nil {
			closeCancelGate()
			notAttempted++
			reason := stopReason
			if reason == "" {
				reason = "not attempted because the restore request was cancelled"
				cancellationInterrupted = true
			}
			results = append(results, RestoreSelectionItemResult{
				SnapshotID: item.SnapshotID, Path: item.Path, Domain: "orchestration", Status: "not_attempted",
				Error: reason,
			})
			continue
		}
		destinationNotice := ""
		options := engines.RestoreOptions{Destination: req.TargetPath, Selection: item.Path, KopiaFileNames: kopiaNames, ReportDestination: func(notice string) { destinationNotice = notice },
			NativeRoot: nativeRoots[item.SnapshotID+"\x00"+item.Path], ConflictMode: req.ConflictMode}
		nativeContext := command.ContextWithFinalCancellationAdmission(operationCtx)
		output, restoreErr := manager.Restore(nativeContext, repoModel, item.SnapshotID, options)
		operationStatus, processStarted, _, nativeStageErr, followupErr, stageKnown :=
			engines.RequestedOperationOutcome(restoreErr)
		if itemIndex == len(normalized)-1 || operationCtx.Err() != nil ||
			stageKnown && operationStatus == engines.RequestedOperationInterrupted {
			// The last requested restore child has returned, or cancellation has
			// already made every later selection ineligible. Close before per-item
			// bookkeeping so a late request cannot rewrite established native truth.
			closeCancelGate()
		}
		result := RestoreSelectionItemResult{
			SnapshotID: item.SnapshotID, Path: item.Path, Domain: "native", Status: "restored", Output: output,
			NativeStatus: "restored", NativeOutput: output,
		}
		if destinationNotice != "" {
			result.Output = destinationNotice + "\n" + output
		}
		if restoreErr != nil {
			orchestrationErrors := make([]string, 0, 1)
			if followupErr != nil {
				orchestrationErrors = append(orchestrationErrors, followupErr.Error())
			}
			orchestrationError := strings.Join(orchestrationErrors, "\n")
			if stageKnown && operationStatus == engines.RequestedOperationSucceeded {
				attempted++
				restored++
				failed++
				orchestrationFailed++
				result.Domain = "orchestration"
				result.Status = "failed"
				result.NativeStatus = "restored"
				result.NativeOutput = output
				result.OrchestrationStatus = "failed"
				result.OrchestrationError = orchestrationError
				if result.OrchestrationError == "" {
					result.OrchestrationError = "native restore succeeded but its orchestration follow-up failed"
				}
				results = append(results, result)
				continue
			}
			if stageKnown && (operationStatus == engines.RequestedOperationNotStarted ||
				operationStatus == engines.RequestedOperationInterrupted && !processStarted) {
				notAttempted++
				orchestrationFailed++
				result.Domain = "orchestration"
				result.Status = "not_attempted"
				result.NativeStatus = "not_attempted"
				result.NativeOutput = ""
				result.NativeError = ""
				result.OrchestrationStatus = "failed"
				result.OrchestrationError = "native engine preparation failed; requested native restore was not started"
				if operationStatus == engines.RequestedOperationInterrupted {
					cancellationInterrupted = true
					stopReason = "not attempted because the restore request was cancelled"
				} else {
					stopReason = "not attempted because native engine preparation failed"
				}
			} else if errors.Is(restoreErr, storageavailability.ErrRepositoryStorageUnavailable) {
				failed++
				orchestrationFailed++
				result.Domain = "orchestration"
				result.Status = "failed"
				result.Error = restoreErr.Error()
				result.NativeStatus = "not_attempted"
				result.NativeOutput = ""
				result.NativeError = ""
				result.OrchestrationStatus = "failed"
				result.OrchestrationError = result.Error
				stopReason = "not attempted because repository storage became unavailable"
			} else {
				attempted++
				result.Status = "failed"
				result.Error = restoreErr.Error()
				result.NativeStatus = "failed"
				result.NativeError = result.Error
				if stageKnown && nativeStageErr != nil {
					result.Error = nativeStageErr.Error()
					result.NativeError = result.Error
				}
				if stageKnown && operationStatus == engines.RequestedOperationInterrupted {
					result.Status = "interrupted"
					result.NativeStatus = "interrupted"
					cancellationInterrupted = true
					stopReason = "not attempted because the restore request was cancelled"
				} else {
					failed++
				}
			}
			// Native failure or interruption and wrapper follow-up failure are
			// independent facts. Preserve both in the per-item result instead of
			// allowing native status classification to discard the follow-up.
			if stageKnown && processStarted && orchestrationError != "" {
				if result.OrchestrationStatus != "failed" {
					orchestrationFailed++
				}
				result.OrchestrationStatus = "failed"
				if result.OrchestrationError == "" {
					result.OrchestrationError = orchestrationError
				} else {
					result.OrchestrationError += "\n" + orchestrationError
				}
			}
		} else {
			attempted++
			restored++
		}
		results = append(results, result)
	}
	status := "success"
	if failed > 0 || orchestrationFailed > 0 {
		status = "failed"
	}
	encoded, _ := json.Marshal(map[string]any{"attempted": attempted, "restored": restored, "failed": failed, "orchestrationFailed": orchestrationFailed, "notAttempted": notAttempted, "items": results})
	operationOutput := string(encoded)
	persistedStatus := status
	if cancellationInterrupted {
		persistedStatus = "interrupted"
	}
	closeCancelGate()
	var dispatchNotification func()
	if persistedStatus != "interrupted" {
		dispatchNotification = prepareOperationNotification(db, operationID, "restore", fmt.Sprintf("Restore %d selected items", len(req.Items)), status, len(req.Items))
	}
	if finishErr := finishOperationDurably(db, operationID, persistedStatus, operationOutput, time.Now()); finishErr != nil {
		writeError(w, http.StatusInternalServerError, finishErr)
		return
	}
	terminalPersisted = true
	if dispatchNotification != nil {
		dispatchNotification()
	}
	response := map[string]any{"status": persistedStatus, "attempted": attempted, "restored": restored, "failed": failed, "orchestrationFailed": orchestrationFailed, "notAttempted": notAttempted, "items": results}
	if persistedStatus != "success" {
		writeJSONStatus(w, http.StatusMultiStatus, response)
		return
	}
	writeJSON(w, response)
}
