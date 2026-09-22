package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/local/replicaro/database"
	"github.com/local/replicaro/engines"
	"github.com/local/replicaro/integrations"
	"github.com/local/replicaro/models"
	"github.com/local/replicaro/vaultidentity"
	"github.com/local/replicaro/vaultlock"
	"github.com/local/replicaro/vaultprofile"
)

const rcloneAuthSessionLifetime = 15 * time.Minute

type rcloneAuthEntry struct {
	flow       *engines.RcloneAuthorizationFlow
	provider   string
	expiresAt  time.Time
	sessionCtx context.Context
	cancel     context.CancelFunc
	cleanupErr error
	finalUse   bool
	worker     *rcloneAuthWorker
	relay      io.Closer
	generation uint64
}

type rcloneAuthRelayHandle struct {
	closer io.Closer
	once   sync.Once
	err    error
}

func (handle *rcloneAuthRelayHandle) Close() error {
	if handle == nil {
		return nil
	}
	handle.once.Do(func() {
		if handle.closer == nil {
			handle.err = fmt.Errorf("rclone authorization callback relay is absent")
			return
		}
		handle.err = handle.closer.Close()
	})
	return handle.err
}

type rcloneAuthWorker struct {
	mu           sync.Mutex
	cancel       context.CancelFunc
	done         chan struct{}
	flow         *engines.RcloneAuthorizationFlow
	err          error
	generation   uint64
	urlHandedOff bool
}

func (worker *rcloneAuthWorker) result() (*engines.RcloneAuthorizationFlow, error, bool) {
	if worker == nil {
		return nil, nil, true
	}
	select {
	case <-worker.done:
		worker.mu.Lock()
		defer worker.mu.Unlock()
		return worker.flow, worker.err, true
	default:
		return nil, nil, false
	}
}

func rcloneAuthWorkerFailure(err error) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("native rclone authorization worker returned no flow")
}

type rcloneAuthStore struct {
	mu                  sync.Mutex
	sessions            map[string]rcloneAuthEntry
	start               func(context.Context, string, string) (*engines.RcloneAuthorizationFlow, error)
	startManual         func(context.Context, string, string, func(string)) (*engines.RcloneAuthorizationFlow, error)
	manualBrowser       bool
	now                 func() time.Time
	lifetime            time.Duration
	retryInterval       time.Duration
	timer               *time.Timer
	shutdown            bool
	starting            sync.WaitGroup
	finalUses           sync.WaitGroup
	cleanups            sync.WaitGroup
	shutdownCleanupErrs []error
	beforeManualHandoff func(string)
	relayFactory        rcloneAuthRelayFactory
}

type rcloneAuthStatus struct {
	SessionID        string                      `json:"sessionId"`
	Provider         string                      `json:"provider"`
	Status           string                      `json:"status"`
	Question         *engines.RcloneAuthQuestion `json:"question,omitempty"`
	AuthorizationURL string                      `json:"authorizationUrl,omitempty"`
}

type rcloneApplicationOutcome struct {
	Activation   engines.RcloneConfigActivation `json:"activation"`
	Attached     bool                           `json:"attached"`
	Usable       *bool                          `json:"usable,omitempty"`
	FailureStage rcloneApplicationFailureStage  `json:"failureStage,omitempty"`
}

type rcloneApplicationFailureStage string

const (
	rcloneFailureAdmission                 rcloneApplicationFailureStage = "admission"
	rcloneFailureSavedVaultValidation      rcloneApplicationFailureStage = "saved_vault_validation"
	rcloneFailureNativeValidation          rcloneApplicationFailureStage = "native_validation"
	rcloneFailureProtectedRecordValidation rcloneApplicationFailureStage = "protected_record_validation"
	rcloneFailureConfigActivation          rcloneApplicationFailureStage = "config_activation"
	rcloneFailureArtifactWork              rcloneApplicationFailureStage = "artifact_work"
	rcloneFailureDatabaseCredentialUpdate  rcloneApplicationFailureStage = "database_credential_update"
)

func failRcloneApplication(outcome rcloneApplicationOutcome, stage rcloneApplicationFailureStage, err error) (rcloneApplicationOutcome, error) {
	outcome.FailureStage = stage
	return outcome, err
}

func assessedUsability(value bool) *bool { return &value }

func attachedUsableRcloneOutcome(outcome rcloneApplicationOutcome) rcloneApplicationOutcome {
	outcome.Attached = true
	outcome.Usable = assessedUsability(true)
	return outcome
}

func addRcloneOutcome(response map[string]any, outcome rcloneApplicationOutcome) {
	response["activation"] = outcome.Activation
	response["attached"] = outcome.Attached
	if outcome.Usable != nil {
		response["usable"] = *outcome.Usable
	}
	if outcome.FailureStage != "" {
		response["failureStage"] = outcome.FailureStage
	}
}

func writeRcloneAuthorizationError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func writeRcloneApplicationError(w http.ResponseWriter, status int, outcome rcloneApplicationOutcome, err error) {
	message := "native rclone configuration follow-up failed"
	switch outcome.Activation.Disposition {
	case engines.RcloneConfigRetained:
		message = "native rclone configuration was retained; activation or its prerequisite failed"
	case engines.RcloneConfigActivated:
		message = "native rclone configuration was activated, but a later attachment or follow-up step failed"
	case engines.RcloneConfigIndeterminate:
		message = "native rclone configuration activation is indeterminate; attachment and usability require review"
	}
	if err == nil {
		message = "native rclone configuration lifecycle failed without a classified cause"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		rcloneApplicationOutcome
		Error string `json:"error"`
	}{rcloneApplicationOutcome: outcome, Error: message})
}

var rcloneAuthConnectorSupported = engines.SupportsConnector

func newRcloneAuthStore(manualBrowser ...bool) *rcloneAuthStore {
	store := &rcloneAuthStore{
		sessions:      map[string]rcloneAuthEntry{},
		start:         engines.StartRcloneAuthorization,
		startManual:   engines.StartRcloneAuthorizationManual,
		now:           time.Now,
		lifetime:      rcloneAuthSessionLifetime,
		retryInterval: time.Minute,
	}
	store.manualBrowser = len(manualBrowser) == 1 && manualBrowser[0]
	return store
}

func newContainerRcloneAuthStore(relayPort int) *rcloneAuthStore {
	store := newRcloneAuthStore(true)
	store.relayFactory = func(authorizationURL string) (io.Closer, error) {
		return startRcloneAuthRelay(relayPort, authorizationURL)
	}
	return store
}

func (store *rcloneAuthStore) expiredIDsLocked() []string {
	now := store.now()
	var ids []string
	for id, entry := range store.sessions {
		if !entry.finalUse && !now.Before(entry.expiresAt) {
			ids = append(ids, id)
		}
	}
	return ids
}

func (store *rcloneAuthStore) purgeExpired() {
	store.mu.Lock()
	ids := store.expiredIDsLocked()
	store.mu.Unlock()
	for _, id := range ids {
		store.mu.Lock()
		entry, ok := store.sessions[id]
		if !ok || entry.finalUse || store.now().Before(entry.expiresAt) {
			store.mu.Unlock()
			continue
		}
		delete(store.sessions, id)
		store.cleanups.Add(1)
		store.scheduleLocked()
		store.mu.Unlock()
		_ = store.cleanupDetachedEntry(id, entry)
	}
}

func (store *rcloneAuthStore) scheduleLocked() {
	if store.timer != nil {
		store.timer.Stop()
		store.timer = nil
	}
	if store.shutdown || len(store.sessions) == 0 {
		return
	}
	var next time.Time
	for _, entry := range store.sessions {
		if entry.finalUse {
			continue
		}
		if next.IsZero() || entry.expiresAt.Before(next) {
			next = entry.expiresAt
		}
	}
	if next.IsZero() {
		return
	}
	delay := next.Sub(store.now())
	if delay < 0 {
		delay = 0
	}
	store.timer = time.AfterFunc(delay, func() {
		store.mu.Lock()
		store.timer = nil
		store.mu.Unlock()
		store.purgeExpired()
		store.mu.Lock()
		store.scheduleLocked()
		store.mu.Unlock()
	})
}

func (store *rcloneAuthStore) launchManualWorker(
	ctx context.Context, id, provider string,
	flow *engines.RcloneAuthorizationFlow,
	answer string, generation uint64,
) (<-chan string, *rcloneAuthWorker) {
	workerCtx, cancel := context.WithCancel(ctx)
	worker := &rcloneAuthWorker{cancel: cancel, done: make(chan struct{}), generation: generation}
	urlReady := make(chan string, 1)
	onURL := func(value string) {
		select {
		case urlReady <- value:
		default:
		}
	}
	go func() {
		var result *engines.RcloneAuthorizationFlow
		var err error
		if flow == nil {
			result, err = store.startManual(workerCtx, "", provider, onURL)
		} else {
			err = flow.ContinueManual(workerCtx, answer, onURL)
			result = flow
		}
		worker.mu.Lock()
		worker.flow, worker.err = result, err
		worker.mu.Unlock()
		close(worker.done)
	}()
	return urlReady, worker
}

func (store *rcloneAuthStore) waitManualTransition(
	requestCtx context.Context,
	id, provider string,
	urlReady <-chan string,
	worker *rcloneAuthWorker,
) (rcloneAuthStatus, error) {
	select {
	case authorizationURL := <-urlReady:
		if store.beforeManualHandoff != nil {
			store.beforeManualHandoff("url")
		}
		store.mu.Lock()
		entry, live := store.sessions[id]
		if requestCtx.Err() != nil || store.shutdown || !live || entry.worker != worker ||
			entry.generation != worker.generation || worker.urlHandedOff ||
			!store.now().Before(entry.expiresAt) {
			store.mu.Unlock()
			return rcloneAuthStatus{}, errors.Join(
				fmt.Errorf("rclone authorization session is unavailable"), store.close(id),
			)
		}
		var relay io.Closer
		if store.relayFactory != nil {
			var relayErr error
			relay, relayErr = store.relayFactory(authorizationURL)
			if relayErr != nil || relay == nil {
				if relayErr == nil {
					relayErr = fmt.Errorf("relay factory returned no listener")
				}
				store.mu.Unlock()
				return rcloneAuthStatus{}, errors.Join(
					fmt.Errorf("start rclone authorization callback relay: %w", relayErr), store.close(id),
				)
			}
			relay = &rcloneAuthRelayHandle{closer: relay}
		}
		// Relay admission is serialized with delete, expiry, and shutdown.
		// Recheck cancellation and deadline after the listener is created. If
		// either won while the factory was running, publish the idempotent handle
		// into the entry before unlocking so concurrent cleanup must drain it.
		if requestCtx.Err() != nil || !store.now().Before(entry.expiresAt) {
			entry.relay = relay
			store.sessions[id] = entry
			var relayErr error
			if relay != nil {
				// Production relay shutdown is bounded. Keep the store lock until
				// this just-created listener is closed so delete/expiry cannot
				// report cleanup while it is still accepting callbacks.
				relayErr = relay.Close()
			}
			store.mu.Unlock()
			return rcloneAuthStatus{}, errors.Join(
				fmt.Errorf("rclone authorization session is unavailable"), relayErr, store.close(id),
			)
		}
		worker.urlHandedOff = true
		entry.relay = relay
		store.sessions[id] = entry
		store.mu.Unlock()
		if relay != nil {
			go store.closeRelayAfterWorker(id, worker, relay)
		}
		return rcloneAuthStatus{
			SessionID: id, Provider: provider, Status: "waiting_browser",
			AuthorizationURL: authorizationURL,
		}, nil
	case <-worker.done:
		return store.claimManualOutcome(requestCtx, id, worker)
	case <-requestCtx.Done():
		return rcloneAuthStatus{}, errors.Join(requestCtx.Err(), store.close(id))
	}
}

// closeRelayAfterWorker makes callback admission follow native worker
// lifetime even when the browser never polls the completed result. The entry
// retains the idempotent handle so the claimant or session cleanup still owns
// and observes any close error.
func (store *rcloneAuthStore) closeRelayAfterWorker(id string, worker *rcloneAuthWorker, relay io.Closer) {
	<-worker.done
	store.mu.Lock()
	entry, live := store.sessions[id]
	owned := live && entry.worker == worker && entry.generation == worker.generation && entry.relay == relay
	store.mu.Unlock()
	if owned {
		_ = relay.Close()
	}
}

func (store *rcloneAuthStore) claimManualOutcome(
	requestCtx context.Context,
	id string,
	worker *rcloneAuthWorker,
) (rcloneAuthStatus, error) {
	flow, workerErr, complete := worker.result()
	if !complete {
		return rcloneAuthStatus{}, fmt.Errorf("rclone authorization transition is still running")
	}
	var detached *rcloneAuthEntry
	store.mu.Lock()
	entry, ok := store.sessions[id]
	if store.shutdown || !ok || entry.worker != worker || entry.generation != worker.generation ||
		!store.now().Before(entry.expiresAt) {
		store.mu.Unlock()
		return rcloneAuthStatus{}, fmt.Errorf("rclone authorization session is unavailable")
	}
	if requestCtx != nil && requestCtx.Err() != nil {
		requestErr := requestCtx.Err()
		delete(store.sessions, id)
		store.cleanups.Add(1)
		store.scheduleLocked()
		store.mu.Unlock()
		cleanupErr := store.cleanupDetachedEntry(id, entry)
		return rcloneAuthStatus{}, errors.Join(requestErr, cleanupErr)
	}
	if workerErr != nil || flow == nil {
		delete(store.sessions, id)
		store.cleanups.Add(1)
		copy := entry
		detached = &copy
		store.scheduleLocked()
		store.mu.Unlock()
		cleanupErr := store.cleanupDetachedEntry(id, *detached)
		return rcloneAuthStatus{}, errors.Join(rcloneAuthWorkerFailure(workerErr), cleanupErr)
	}
	relay := entry.relay
	store.mu.Unlock()
	if relay != nil {
		if relayErr := relay.Close(); relayErr != nil {
			return rcloneAuthStatus{}, errors.Join(
				fmt.Errorf("close rclone authorization callback relay: %w", relayErr), store.close(id),
			)
		}
	}
	ready, question := flow.Status()
	if store.beforeManualHandoff != nil {
		store.beforeManualHandoff("outcome")
	}
	store.mu.Lock()
	current, live := store.sessions[id]
	valid := live && !store.shutdown && current.worker == worker && current.relay == relay &&
		current.generation == worker.generation && store.now().Before(current.expiresAt)
	if !valid {
		store.mu.Unlock()
		return rcloneAuthStatus{}, fmt.Errorf("rclone authorization session is unavailable")
	}
	if requestCtx != nil && requestCtx.Err() != nil {
		requestErr := requestCtx.Err()
		delete(store.sessions, id)
		store.cleanups.Add(1)
		store.scheduleLocked()
		store.mu.Unlock()
		cleanupErr := store.cleanupDetachedEntry(id, current)
		return rcloneAuthStatus{}, errors.Join(requestErr, cleanupErr)
	}
	if !ready && question == nil {
		store.mu.Unlock()
		return rcloneAuthStatus{}, errors.Join(
			fmt.Errorf("native rclone authorization returned no next action"), store.close(id),
		)
	}
	current.flow = flow
	current.worker = nil
	current.relay = nil
	store.sessions[id] = current
	store.mu.Unlock()
	return authStatus(id, current.provider, ready, question), nil
}

func (store *rcloneAuthStore) startManualFlow(
	ctx context.Context,
	provider string,
) (rcloneAuthStatus, error) {
	id := uuid.NewString()
	store.mu.Lock()
	if store.shutdown {
		store.mu.Unlock()
		return rcloneAuthStatus{}, fmt.Errorf("rclone authorization is shutting down")
	}
	store.starting.Add(1)
	defer store.starting.Done()
	sessionCtx, cancel := context.WithTimeout(context.Background(), store.lifetime)
	urlReady, worker := store.launchManualWorker(sessionCtx, id, provider, nil, "", 1)
	store.sessions[id] = rcloneAuthEntry{
		provider: provider, expiresAt: store.now().Add(store.lifetime), sessionCtx: sessionCtx,
		cancel: cancel, worker: worker, generation: 1,
	}
	store.scheduleLocked()
	store.mu.Unlock()
	return store.waitManualTransition(ctx, id, provider, urlReady, worker)
}

func (store *rcloneAuthStore) startFlow(
	ctx context.Context,
	provider string,
) (rcloneAuthStatus, error) {
	if store.manualBrowser {
		return store.startManualFlow(ctx, provider)
	}
	store.mu.Lock()
	if store.shutdown {
		store.mu.Unlock()
		return rcloneAuthStatus{}, fmt.Errorf("rclone authorization is shutting down")
	}
	store.starting.Add(1)
	store.mu.Unlock()
	defer store.starting.Done()

	flow, err := store.start(ctx, "", provider)
	if err != nil {
		if flow != nil {
			cleanupErr := flow.Close()
			if cleanupErr != nil {
				store.mu.Lock()
				if store.shutdown {
					store.mu.Unlock()
					for attempt := 1; attempt < 3 && cleanupErr != nil; attempt++ {
						cleanupErr = flow.Close()
					}
					if cleanupErr != nil {
						store.mu.Lock()
						store.shutdownCleanupErrs = append(store.shutdownCleanupErrs, cleanupErr)
						store.mu.Unlock()
					}
				} else {
					store.sessions[uuid.NewString()] = rcloneAuthEntry{flow: flow, expiresAt: store.now().Add(store.retryInterval), cleanupErr: cleanupErr}
					store.scheduleLocked()
					store.mu.Unlock()
				}
				err = errors.Join(err, cleanupErr)
			}
		}
		return rcloneAuthStatus{}, err
	}
	ready, question := flow.Status()
	if !ready && question == nil {
		cleanupErr := flow.Close()
		if cleanupErr != nil {
			store.mu.Lock()
			if store.shutdown {
				store.mu.Unlock()
				for attempt := 1; attempt < 3 && cleanupErr != nil; attempt++ {
					cleanupErr = flow.Close()
				}
				if cleanupErr != nil {
					store.mu.Lock()
					store.shutdownCleanupErrs = append(store.shutdownCleanupErrs, cleanupErr)
					store.mu.Unlock()
				}
			} else {
				store.sessions[uuid.NewString()] = rcloneAuthEntry{flow: flow, expiresAt: store.now().Add(store.retryInterval), cleanupErr: cleanupErr}
				store.scheduleLocked()
				store.mu.Unlock()
			}
		}
		return rcloneAuthStatus{}, errors.Join(
			fmt.Errorf("native rclone authorization returned no next action"),
			cleanupErr,
		)
	}
	id := uuid.NewString()
	store.purgeExpired()
	store.mu.Lock()
	if store.shutdown {
		store.mu.Unlock()
		cleanupErr := flow.Close()
		for attempt := 1; attempt < 3 && cleanupErr != nil; attempt++ {
			cleanupErr = flow.Close()
		}
		if cleanupErr != nil {
			store.mu.Lock()
			store.shutdownCleanupErrs = append(store.shutdownCleanupErrs, cleanupErr)
			store.mu.Unlock()
		}
		return rcloneAuthStatus{}, errors.Join(
			fmt.Errorf("rclone authorization is shutting down"),
			cleanupErr,
		)
	}
	store.sessions[id] = rcloneAuthEntry{
		flow: flow, expiresAt: store.now().Add(store.lifetime),
	}
	store.scheduleLocked()
	store.mu.Unlock()
	return authStatus(id, flow.Provider(), ready, question), nil
}

func authStatus(
	id, provider string,
	ready bool,
	question *engines.RcloneAuthQuestion,
) rcloneAuthStatus {
	status := "question"
	if ready {
		status = "ready"
	}
	return rcloneAuthStatus{
		SessionID: id, Provider: provider, Status: status, Question: question,
	}
}

func (store *rcloneAuthStore) continueFlow(
	ctx context.Context,
	id, answer string,
) (rcloneAuthStatus, error) {
	if store.manualBrowser {
		return store.continueManualFlow(ctx, id, answer)
	}
	store.purgeExpired()
	store.mu.Lock()
	entry, ok := store.sessions[id]
	if ok && entry.cleanupErr == nil && !entry.finalUse {
		entry.expiresAt = store.now().Add(store.lifetime)
		store.sessions[id] = entry
	}
	store.scheduleLocked()
	store.mu.Unlock()
	if !ok {
		return rcloneAuthStatus{}, fmt.Errorf("rclone authorization session is absent or expired")
	}
	if entry.cleanupErr != nil {
		return rcloneAuthStatus{}, fmt.Errorf("rclone authorization session cleanup is pending: %w", entry.cleanupErr)
	}
	if entry.finalUse {
		return rcloneAuthStatus{}, fmt.Errorf("rclone authorization session is completing a vault transaction")
	}
	if err := entry.flow.Continue(ctx, answer); err != nil {
		return rcloneAuthStatus{}, errors.Join(err, store.close(id))
	}
	ready, question := entry.flow.Status()
	if !ready && question == nil {
		return rcloneAuthStatus{}, errors.Join(
			fmt.Errorf("native rclone authorization returned no next action"),
			store.close(id),
		)
	}
	return authStatus(id, entry.flow.Provider(), ready, question), nil
}

func (store *rcloneAuthStore) continueManualFlow(
	ctx context.Context,
	id, answer string,
) (rcloneAuthStatus, error) {
	store.purgeExpired()
	store.mu.Lock()
	entry, ok := store.sessions[id]
	if store.shutdown || !ok || entry.cleanupErr != nil || entry.finalUse {
		store.mu.Unlock()
		return rcloneAuthStatus{}, fmt.Errorf("rclone authorization session is unavailable")
	}
	if entry.worker != nil {
		store.mu.Unlock()
		return rcloneAuthStatus{}, fmt.Errorf("rclone authorization transition is already running")
	}
	flow := entry.flow
	if flow == nil {
		store.mu.Unlock()
		return rcloneAuthStatus{}, fmt.Errorf("rclone authorization session is unavailable")
	}
	entry.generation++
	urlReady, worker := store.launchManualWorker(entry.sessionCtx, id, entry.provider, flow, answer, entry.generation)
	entry.worker = worker
	store.sessions[id] = entry
	store.scheduleLocked()
	store.mu.Unlock()
	return store.waitManualTransition(ctx, id, entry.provider, urlReady, worker)
}

func (store *rcloneAuthStore) status(id string) (rcloneAuthStatus, error) {
	if !store.manualBrowser {
		return rcloneAuthStatus{}, fmt.Errorf("manual rclone authorization status is unavailable")
	}
	store.purgeExpired()
	store.mu.Lock()
	entry, ok := store.sessions[id]
	store.mu.Unlock()
	if !ok || entry.cleanupErr != nil || entry.finalUse {
		return rcloneAuthStatus{}, fmt.Errorf("rclone authorization session is absent or expired")
	}
	if entry.worker != nil {
		_, _, complete := entry.worker.result()
		if complete {
			return store.claimManualOutcome(nil, id, entry.worker)
		}
		store.mu.Lock()
		current, live := store.sessions[id]
		valid := live && current.worker == entry.worker && current.generation == entry.generation &&
			!store.shutdown && store.now().Before(current.expiresAt)
		store.mu.Unlock()
		if !valid {
			return rcloneAuthStatus{}, fmt.Errorf("rclone authorization session is absent or expired")
		}
		return rcloneAuthStatus{
			SessionID: id, Provider: entry.provider, Status: "waiting_browser",
		}, nil
	}
	flow := entry.flow
	if flow == nil {
		return rcloneAuthStatus{}, fmt.Errorf("rclone authorization session is absent or expired")
	}
	ready, question := flow.Status()
	if !ready && question == nil {
		return rcloneAuthStatus{}, errors.Join(
			fmt.Errorf("native rclone authorization returned no next action"), store.close(id),
		)
	}
	store.mu.Lock()
	current, live := store.sessions[id]
	valid := live && !store.shutdown && current.flow == flow && current.worker == nil &&
		current.generation == entry.generation && store.now().Before(current.expiresAt)
	store.mu.Unlock()
	if !valid {
		return rcloneAuthStatus{}, fmt.Errorf("rclone authorization session is absent or expired")
	}
	return authStatus(id, entry.provider, ready, question), nil
}

func (store *rcloneAuthStore) options(
	id, provider string,
) (map[string]string, error) {
	store.purgeExpired()
	store.mu.Lock()
	entry, ok := store.sessions[id]
	flow, complete := entry.flow, entry.worker == nil
	if ok && complete && flow != nil && entry.cleanupErr == nil && !entry.finalUse &&
		flow.Provider() == provider {
		if !store.manualBrowser {
			entry.expiresAt = store.now().Add(store.lifetime)
		}
		entry.flow = flow
		store.sessions[id] = entry
	}
	store.scheduleLocked()
	store.mu.Unlock()
	if !ok || !complete || flow == nil || flow.Provider() != provider {
		return nil, fmt.Errorf("rclone authorization session does not match this provider")
	}
	if entry.cleanupErr != nil {
		return nil, fmt.Errorf("rclone authorization session cleanup is pending: %w", entry.cleanupErr)
	}
	if entry.finalUse {
		return nil, fmt.Errorf("rclone authorization session is completing a vault transaction")
	}
	return flow.IdentityOptions()
}

func (store *rcloneAuthStore) configPath(id, provider string) (string, error) {
	store.purgeExpired()
	store.mu.Lock()
	entry, ok := store.sessions[id]
	store.scheduleLocked()
	store.mu.Unlock()
	flow, complete := entry.flow, entry.worker == nil
	if !ok || !complete || flow == nil || flow.Provider() != provider {
		return "", fmt.Errorf("rclone authorization session does not match this provider")
	}
	if entry.cleanupErr != nil {
		return "", fmt.Errorf("rclone authorization session cleanup is pending: %w", entry.cleanupErr)
	}
	return flow.StagedConfigPath()
}

func (store *rcloneAuthStore) publishConfig(
	ctx context.Context,
	id, provider, repositoryID string,
) (engines.RcloneConfigActivation, error) {
	store.mu.Lock()
	entry, ok := store.sessions[id]
	store.mu.Unlock()
	flow, complete := entry.flow, entry.worker == nil
	if !ok || !complete || flow == nil || flow.Provider() != provider || !entry.finalUse {
		return engines.RcloneConfigActivation{Disposition: engines.RcloneConfigRetained}, fmt.Errorf("rclone authorization final-use ownership is unavailable")
	}
	return flow.PublishVaultConfig(ctx, repositoryID)
}

func (store *rcloneAuthStore) close(id string) error {
	store.mu.Lock()
	entry, ok := store.sessions[id]
	if ok {
		if entry.finalUse {
			store.mu.Unlock()
			return fmt.Errorf("rclone authorization session is completing a vault transaction")
		}
		delete(store.sessions, id)
		store.cleanups.Add(1)
	}
	store.scheduleLocked()
	store.mu.Unlock()
	if !ok {
		return nil
	}
	return store.cleanupDetachedEntry(id, entry)
}

func (store *rcloneAuthStore) cleanupDetachedEntry(id string, entry rcloneAuthEntry) error {
	defer store.cleanups.Done()
	var relayErr error
	if entry.relay != nil {
		relayErr = entry.relay.Close()
		entry.relay = nil
	}
	if entry.cancel != nil {
		entry.cancel()
	}
	if entry.worker != nil {
		entry.worker.cancel()
		<-entry.worker.done
		flow, _, _ := entry.worker.result()
		if flow != nil {
			entry.flow = flow
		}
	}
	if entry.flow != nil {
		if err := entry.flow.Close(); err != nil {
			store.mu.Lock()
			shuttingDown := store.shutdown
			store.mu.Unlock()
			if shuttingDown {
				for attempt := 1; attempt < 3 && err != nil; attempt++ {
					err = entry.flow.Close()
				}
				if err != nil {
					wrapped := fmt.Errorf("clean up rclone authorization session %s: %w", id, err)
					store.mu.Lock()
					store.shutdownCleanupErrs = append(store.shutdownCleanupErrs, wrapped)
					store.mu.Unlock()
					return errors.Join(relayErr, wrapped)
				}
				return relayErr
			}
			entry.cleanupErr = err
			entry.expiresAt = store.now().Add(store.retryInterval)
			entry.worker = nil
			store.mu.Lock()
			if _, exists := store.sessions[id]; !store.shutdown && !exists {
				store.sessions[id] = entry
				store.scheduleLocked()
			}
			store.mu.Unlock()
			return errors.Join(relayErr, fmt.Errorf("clean up rclone authorization session: %w", err))
		}
	}
	return relayErr
}

func (store *rcloneAuthStore) closeAll() error {
	store.mu.Lock()
	store.shutdown = true
	if store.timer != nil {
		store.timer.Stop()
		store.timer = nil
	}
	entries := make(map[string]rcloneAuthEntry, len(store.sessions))
	for id, entry := range store.sessions {
		if entry.finalUse {
			continue
		}
		entries[id] = entry
		delete(store.sessions, id)
	}
	store.mu.Unlock()
	var errs []error
	// Relay closure and cancellation are deliberately a separate first pass:
	// one slow reap must never leave a later callback or worker admitted until
	// its 15-minute deadline.
	for id, entry := range entries {
		if entry.relay != nil {
			if relayErr := entry.relay.Close(); relayErr != nil {
				errs = append(errs, relayErr)
			}
			entry.relay = nil
			entries[id] = entry
		}
		if entry.cancel != nil {
			entry.cancel()
		}
		if entry.worker != nil {
			entry.worker.cancel()
		}
	}
	store.starting.Wait()
	store.finalUses.Wait()
	store.mu.Lock()
	for id, entry := range store.sessions {
		entries[id] = entry
		delete(store.sessions, id)
	}
	store.mu.Unlock()
	for _, entry := range entries {
		if entry.relay != nil {
			if relayErr := entry.relay.Close(); relayErr != nil {
				errs = append(errs, relayErr)
			}
		}
		if entry.cancel != nil {
			entry.cancel()
		}
		if entry.worker != nil {
			entry.worker.cancel()
		}
	}
	for id, entry := range entries {
		if entry.worker != nil {
			<-entry.worker.done
			if flow, _, _ := entry.worker.result(); flow != nil {
				entry.flow = flow
			}
		}
		if entry.flow == nil {
			continue
		}
		var cleanupErr error
		for attempt := 0; attempt < 3; attempt++ {
			cleanupErr = entry.flow.Close()
			if cleanupErr == nil {
				break
			}
		}
		if cleanupErr != nil {
			errs = append(errs, fmt.Errorf("clean up rclone authorization session %s: %w", id, cleanupErr))
		}
	}
	store.cleanups.Wait()
	store.mu.Lock()
	errs = append(errs, store.shutdownCleanupErrs...)
	store.mu.Unlock()
	return errors.Join(errs...)
}

func (store *rcloneAuthStore) mergeOptions(
	sessionID, provider string,
	submitted map[string]string,
) (map[string]string, error) {
	if !engines.IsRcloneNativeLoginProvider(provider) {
		if strings.TrimSpace(sessionID) != "" {
			return nil, fmt.Errorf("rclone authorization session is invalid for this storage type")
		}
		result := make(map[string]string, len(submitted))
		for key, value := range submitted {
			result[key] = value
		}
		return result, nil
	}
	if strings.TrimSpace(sessionID) == "" {
		return nil, fmt.Errorf("connect the %s account with rclone before continuing", provider)
	}
	native, err := store.options(sessionID, provider)
	if err != nil {
		return nil, err
	}
	return mergeRcloneAuthorizedOptions(submitted, native)
}

func mergeRcloneAuthorizedOptions(
	submitted, native map[string]string,
) (map[string]string, error) {
	result := make(map[string]string, len(native)+2)
	for key, value := range submitted {
		if strings.HasPrefix(key, "rclone_") {
			result[key] = value
			continue
		}
		if strings.TrimSpace(value) != "" {
			return nil, fmt.Errorf("native rclone login fields cannot be supplied through the API")
		}
	}
	for key, value := range native {
		result[key] = value
	}
	return result, nil
}

// mergeOptionsForFinalTransaction exclusively leases a completed native
// authorization while a create/connect transaction is in flight. Only a
// retained publication releases the lease for retry. Activated and
// indeterminate publication consume the session before cleanup, and a later
// error never restores usability.
func (store *rcloneAuthStore) mergeOptionsForFinalTransaction(
	sessionID, provider string,
	submitted map[string]string,
) (map[string]string, func(engines.RcloneConfigDisposition) error, error) {
	if !engines.IsRcloneNativeLoginProvider(provider) {
		options, err := store.mergeOptions(sessionID, provider, submitted)
		return options, func(engines.RcloneConfigDisposition) error { return nil }, err
	}
	if strings.TrimSpace(sessionID) == "" {
		return nil, nil, fmt.Errorf(
			"connect the %s account with rclone before continuing", provider,
		)
	}

	store.purgeExpired()
	store.mu.Lock()
	entry, ok := store.sessions[sessionID]
	flow, complete := entry.flow, entry.worker == nil
	switch {
	case store.shutdown:
		store.scheduleLocked()
		store.mu.Unlock()
		return nil, nil, fmt.Errorf("rclone authorization is shutting down")
	case !ok || !complete || flow == nil || flow.Provider() != provider:
		store.scheduleLocked()
		store.mu.Unlock()
		return nil, nil, fmt.Errorf("rclone authorization session does not match this provider")
	case entry.cleanupErr != nil:
		store.scheduleLocked()
		store.mu.Unlock()
		return nil, nil, fmt.Errorf(
			"rclone authorization session cleanup is pending: %w", entry.cleanupErr,
		)
	case entry.finalUse:
		store.scheduleLocked()
		store.mu.Unlock()
		return nil, nil, fmt.Errorf("rclone authorization session is already completing a vault transaction")
	}
	entry.flow = flow
	entry.finalUse = true
	if !store.manualBrowser {
		entry.expiresAt = store.now().Add(store.lifetime)
	}
	store.sessions[sessionID] = entry
	store.finalUses.Add(1)
	store.scheduleLocked()
	store.mu.Unlock()

	var once sync.Once
	var finishErr error
	finish := func(disposition engines.RcloneConfigDisposition) error {
		once.Do(func() {
			store.mu.Lock()
			current, exists := store.sessions[sessionID]
			if !exists || !current.finalUse {
				finishErr = fmt.Errorf("rclone authorization final-use ownership was lost")
			} else if disposition == engines.RcloneConfigRetained {
				current.finalUse = false
				if !store.manualBrowser {
					current.expiresAt = store.now().Add(store.lifetime)
				}
				store.sessions[sessionID] = current
			} else {
				delete(store.sessions, sessionID)
			}
			store.scheduleLocked()
			store.mu.Unlock()
			if exists && current.finalUse && disposition != engines.RcloneConfigRetained {
				if current.cancel != nil {
					current.cancel()
				}
				if err := current.flow.Close(); err != nil {
					current.finalUse = false
					current.cleanupErr = err
					current.expiresAt = store.now().Add(store.retryInterval)
					store.mu.Lock()
					shuttingDown := store.shutdown
					if !shuttingDown {
						store.sessions[sessionID] = current
						store.scheduleLocked()
					}
					store.mu.Unlock()
					if shuttingDown {
						for attempt := 1; attempt < 3 && err != nil; attempt++ {
							err = current.flow.Close()
						}
						if err != nil {
							store.mu.Lock()
							store.shutdownCleanupErrs = append(store.shutdownCleanupErrs, err)
							store.mu.Unlock()
						}
					}
					if err != nil {
						finishErr = fmt.Errorf("clean up consumed rclone authorization session: %w", err)
					}
				}
			}
			store.finalUses.Done()
		})
		return finishErr
	}

	native, err := entry.flow.IdentityOptions()
	if err != nil {
		_ = finish(engines.RcloneConfigRetained)
		return nil, nil, err
	}
	result, err := mergeRcloneAuthorizedOptions(submitted, native)
	if err != nil {
		_ = finish(engines.RcloneConfigRetained)
		return nil, nil, err
	}
	return result, finish, nil
}

func registerRcloneAuthHandlers(
	mux *http.ServeMux,
	db *sql.DB,
	store *rcloneAuthStore,
) {
	handle(mux, "/api/rclone/auth/start", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Provider string `json:"provider"`
		}
		if err := decodeRequest(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if !rcloneAuthConnectorSupported(engines.ResticID, req.Provider) {
			badRequest(w, "this Restic rclone connector is unavailable for the current target")
			return
		}
		status, err := store.startFlow(r.Context(), req.Provider)
		if err != nil {
			writeRcloneAuthorizationError(w, http.StatusBadRequest,
				"native rclone authorization could not be started")
			return
		}
		if status.AuthorizationURL != "" {
			w.Header().Set("Cache-Control", "no-store")
		}
		writeJSON(w, status)
	})
	handle(mux, "/api/rclone/auth/continue", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			SessionID string `json:"sessionId"`
			Answer    string `json:"answer"`
		}
		if err := decodeRequest(r, &req); err != nil ||
			strings.TrimSpace(req.SessionID) == "" {
			badRequest(w, "sessionId and answer are required")
			return
		}
		status, err := store.continueFlow(r.Context(), req.SessionID, req.Answer)
		if err != nil {
			writeRcloneAuthorizationError(w, http.StatusBadRequest,
				"native rclone authorization could not be continued")
			return
		}
		if status.AuthorizationURL != "" {
			w.Header().Set("Cache-Control", "no-store")
		}
		writeJSON(w, status)
	})
	handle(mux, "/api/rclone/auth/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			SessionID string `json:"sessionId"`
		}
		if err := decodeRequest(r, &req); err != nil || strings.TrimSpace(req.SessionID) == "" {
			badRequest(w, "sessionId is required")
			return
		}
		status, err := store.status(req.SessionID)
		if err != nil {
			writeRcloneAuthorizationError(w, http.StatusNotFound,
				"native rclone authorization session is unavailable")
			return
		}
		status.AuthorizationURL = ""
		writeJSON(w, status)
	})
	handle(mux, "/api/rclone/auth/session", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if err := store.close(strings.TrimSpace(r.URL.Query().Get("id"))); err != nil {
			writeRcloneAuthorizationError(w, http.StatusInternalServerError,
				"temporary native authorization cleanup failed; retry cleanup")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	handle(mux, "/api/rclone/auth/apply", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			SessionID    string `json:"sessionId"`
			RepositoryID string `json:"repositoryId"`
		}
		if err := decodeRequest(r, &req); err != nil ||
			strings.TrimSpace(req.SessionID) == "" ||
			strings.TrimSpace(req.RepositoryID) == "" {
			badRequest(w, "sessionId and repositoryId are required")
			return
		}
		repo, err := database.GetRepository(db, req.RepositoryID)
		if err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		if repo.Engine != engines.ResticID ||
			!engines.IsRcloneNativeLoginProvider(repo.Connector) {
			badRequest(w, "this vault does not use native rclone login")
			return
		}
		options, finish, err := store.mergeOptionsForFinalTransaction(
			req.SessionID, repo.Connector, nil,
		)
		if err != nil {
			writeRcloneAuthorizationError(w, http.StatusBadRequest,
				"native rclone authorization session is unavailable or not ready")
			return
		}
		stagedConfig, err := store.configPath(req.SessionID, repo.Connector)
		if err != nil {
			_ = finish(engines.RcloneConfigRetained)
			writeRcloneAuthorizationError(w, http.StatusBadRequest,
				"native rclone authorization session configuration is unavailable")
			return
		}
		outcome, applyErr := applyRcloneAuthorizedCredentials(
			r.Context(), db, repo, options, stagedConfig,
			func() (engines.RcloneConfigActivation, error) {
				return store.publishConfig(
					r.Context(), req.SessionID, repo.Connector, repo.ID,
				)
			},
		)
		finishErr := finish(outcome.Activation.Disposition)
		if applyErr != nil {
			status := http.StatusBadRequest
			if errors.Is(applyErr, database.ErrRepositoryConnectionReserved) ||
				errors.Is(applyErr, database.ErrVaultPasswordChangeRecoveryRequired) {
				status = http.StatusConflict
			}
			writeRcloneApplicationError(w, status, outcome, errors.Join(applyErr, finishErr))
			return
		}
		if finishErr != nil {
			outcome.FailureStage = rcloneFailureArtifactWork
			writeRcloneApplicationError(w, http.StatusInternalServerError, outcome, fmt.Errorf(
				"rclone login was saved, but its temporary authorization session needs cleanup: %w",
				finishErr,
			))
			return
		}
		writeJSON(w, outcome)
	})
}

func applyRcloneAuthorizedCredentials(
	ctx context.Context,
	db *sql.DB,
	repo models.Repository,
	providerOptions map[string]string,
	stagedConfig string,
	publishConfig func() (engines.RcloneConfigActivation, error),
) (rcloneApplicationOutcome, error) {
	outcome := rcloneApplicationOutcome{
		Activation: engines.RcloneConfigActivation{Disposition: engines.RcloneConfigRetained},
		Attached:   true,
	}
	// A native-login apply is an attended foreground transaction. In particular,
	// OneDrive and Google Drive can still be completing an ordinary protected
	// profile publication when the browser flow returns. Wait for that exact
	// in-process vault owner, then revalidate every admission and saved-vault
	// precondition below while holding the lock. A one-shot try here made slower
	// providers report a spurious busy failure; cancellation still leaves the
	// retained authorization retryable. This wait neither unlocks native work
	// nor masks remote multi-computer races.
	unlock, lockErr := vaultlock.AcquireExclusiveContext(ctx, repo.ID)
	if lockErr != nil {
		return failRcloneApplication(outcome, rcloneFailureAdmission, lockErr)
	}
	defer unlock()
	// Authorization may outlive the saved-row review. A pending reconnect or
	// password change must be rejected under the vault lock before native
	// validation, artifact staging, or canonical rclone config publication.
	// Publication used to precede the database reservation check at commit.
	if err := database.ValidateRepositoryMutationAdmission(db, repo.ID); err != nil {
		// The handler loaded repo before waiting for the vault owner. Removal may
		// have committed under that owner in the meantime, so only conclusive
		// under-lock absence replaces the pre-wait attachment truth.
		if errors.Is(err, sql.ErrNoRows) {
			outcome.Attached = false
		}
		return failRcloneApplication(outcome, rcloneFailureAdmission, err)
	}
	current, err := database.GetRepository(db, repo.ID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			outcome.Attached = false
		}
		return failRcloneApplication(outcome, rcloneFailureSavedVaultValidation, err)
	}
	if current.Engine != repo.Engine || current.Connector != repo.Connector {
		return failRcloneApplication(outcome, rcloneFailureSavedVaultValidation, fmt.Errorf("vault changed during rclone authorization"))
	}
	repo = current
	integration, ok := integrations.Find(repo.Connector)
	if !ok {
		return failRcloneApplication(outcome, rcloneFailureSavedVaultValidation, fmt.Errorf("rclone provider definition is unavailable"))
	}
	merged := make(map[string]string, len(providerOptions)+2)
	for key, value := range providerOptions {
		merged[key] = value
	}
	normalized, err := engines.NormalizeConnectorOptions(repo.Engine, integration, merged)
	if err != nil {
		return failRcloneApplication(outcome, rcloneFailureSavedVaultValidation, err)
	}
	oldIdentity, oldErr := vaultidentity.PhysicalIdentityWithOptions(
		repo.Connector, repo.Location, repo.ConnectorOptions,
	)
	newIdentity, newErr := vaultidentity.PhysicalIdentityWithOptions(
		repo.Connector, repo.Location, normalized,
	)
	if oldErr != nil || newErr != nil || oldIdentity != newIdentity {
		return failRcloneApplication(outcome, rcloneFailureSavedVaultValidation, fmt.Errorf("rclone login addresses a different physical vault"))
	}
	candidate := repo
	candidate.ConnectorOptions = normalized
	candidate.RcloneConfigPath = stagedConfig
	// This uses the staged rclone credentials for the existing native access
	// probe. The protected root/profile checks below match the saved vault
	// records; ordinary backup admission compares the actual native ID before
	// backup. Do not add another Restic config read for the narrow false-success
	// case described at validateRotatedCredentials: it would add a remote round
	// trip without a demonstrated backup-integrity benefit.
	if err := validateRotatedCredentials(ctx, candidate); err != nil {
		return failRcloneApplication(outcome, rcloneFailureNativeValidation, fmt.Errorf("rclone login could not access the native vault"))
	}
	rootData, err := readRootWithRotatedCredentials(ctx, candidate)
	if err != nil {
		return failRcloneApplication(outcome, rcloneFailureProtectedRecordValidation, fmt.Errorf("rclone login could not access the vault root"))
	}
	root, err := vaultprofile.ParseRoot(rootData, repo.Connector)
	if err != nil {
		return failRcloneApplication(outcome, rcloneFailureProtectedRecordValidation, fmt.Errorf("rclone login returned an invalid vault root"))
	}
	if root.VaultUUID != repo.ID || root.Repository.Engine != repo.Engine ||
		root.Repository.NativeRepositoryID != repo.NativeRepositoryID {
		return failRcloneApplication(outcome, rcloneFailureProtectedRecordValidation, fmt.Errorf("rclone login addresses a different vault"))
	}
	profileData, err := readProfileWithRotatedCredentials(ctx, candidate)
	if err != nil {
		return failRcloneApplication(outcome, rcloneFailureProtectedRecordValidation, fmt.Errorf("rclone login could not access the recovery profile"))
	}
	profile, err := vaultprofile.Parse(profileData)
	if err != nil {
		return failRcloneApplication(outcome, rcloneFailureProtectedRecordValidation, fmt.Errorf("rclone login returned an invalid recovery profile"))
	}
	if profile.VaultUUID != repo.ID || profile.ProfileUUID != repo.ProfileUUID ||
		profile.Attachment.ClientUUID != repo.ClientUUID || profile.Attachment.Generation != repo.AttachmentGeneration {
		return failRcloneApplication(outcome, rcloneFailureProtectedRecordValidation, fmt.Errorf("rclone login addresses a different vault"))
	}
	if publishConfig == nil {
		return failRcloneApplication(outcome, rcloneFailureConfigActivation, fmt.Errorf("rclone vault config publication is unavailable"))
	}
	activation, publishErr := publishConfig()
	outcome.Activation = activation
	if publishErr != nil {
		return failRcloneApplication(outcome, rcloneFailureConfigActivation, fmt.Errorf("rclone login could not activate the native vault config"))
	}
	stage, err := stageRepositoryArtifacts(
		repo, engines.RepositoryArtifactCredentialRotation,
	)
	if err != nil {
		return failRcloneApplication(outcome, rcloneFailureArtifactWork, fmt.Errorf("stale local engine credentials could not be staged: %w", err))
	}
	if err := updateRepositoryCredentials(db, repo.ID, normalized); err != nil {
		if restoreErr := restoreRepositoryArtifacts(stage); restoreErr != nil {
			return failRcloneApplication(outcome, rcloneFailureDatabaseCredentialUpdate, fmt.Errorf("credential update failed and old local engine artifacts could not be restored: %v; restore error: %w", err, restoreErr))
		}
		return failRcloneApplication(outcome, rcloneFailureDatabaseCredentialUpdate, err)
	}
	outcome.Attached = true
	outcome.Usable = assessedUsability(true)
	if err := finalizeRepositoryArtifacts(stage); err != nil {
		return failRcloneApplication(outcome, rcloneFailureArtifactWork, fmt.Errorf("credentials were updated but quarantined old engine artifacts require manual cleanup: %w", err))
	}
	_ = database.LogActivity(db, "Repository rclone account reconnected: "+repo.Name)
	return outcome, nil
}
