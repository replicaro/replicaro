// Package operationruntime owns bounded, process-local previews and cancellation
// handles for operations whose durable state remains in the database, and the
// application-owned lifetime of operations that a request starts and then hands
// off to a background worker.
package operationruntime

import (
	"container/list"
	"context"
	"errors"
	"sync"
)

type managerContextKey struct{}

func ContextWithManager(ctx context.Context, manager *Manager) context.Context {
	return context.WithValue(ctx, managerContextKey{}, manager)
}

func FromContext(ctx context.Context) *Manager {
	manager, _ := ctx.Value(managerContextKey{}).(*Manager)
	return manager
}

const (
	DefaultMaxEntries        = 1000
	DefaultMaxOperationBytes = 128 << 10
	DefaultMaxProcessBytes   = 8 << 20
)

var (
	ErrNotFound      = errors.New("operation runtime is unavailable")
	ErrNotCancelable = errors.New("operation is not cancelable")
	// ErrStopping is returned once shutdown has begun. The app calls BeginStop
	// before it drains HTTP, so a request still in flight at that point gets
	// this error (the API answers 503) instead of creating an operation record
	// that nothing would run.
	ErrStopping = errors.New("background operations are stopping")
)

type Entry struct {
	Stream string `json:"stream"`
	Text   string `json:"text"`
}

type Snapshot struct {
	Available       bool    `json:"available"`
	Entries         []Entry `json:"entries"`
	Truncated       bool    `json:"truncated"`
	Cancelable      bool    `json:"cancelable"`
	CancelRequested bool    `json:"cancelRequested"`
}

type CancelResult struct {
	Accepted  bool
	Retryable bool
}

type cancelState uint8

const (
	cancelIdle cancelState = iota
	cancelInFlight
	cancelAccepted
	cancelClosed
)

type cancelAttempt struct {
	done chan struct{}
	err  error
}

type runtimeEntry struct {
	entries   list.List
	bytes     int
	truncated bool
	cancel    func() error
	state     cancelState
	attempt   *cancelAttempt
}

type storedEntry struct {
	Entry
	operationID string
	size        int
	operation   *list.Element
	global      *list.Element
}

type Manager struct {
	mu                sync.Mutex
	entries           map[string]*runtimeEntry
	order             list.List
	totalBytes        int
	maxEntries        int
	maxOperationBytes int
	maxProcessBytes   int

	// Background work started by a request (restore, manual check and
	// maintenance, snapshot and job deletion, vault password change, settings
	// save and removal) must not live on the request's
	// context: closing the browser or a proxy timeout would cancel it. It runs
	// on this lifetime instead. BeginStop cancels it and Wait joins every
	// worker before main closes the database, the same contract the backup
	// runner and scheduler follow.
	lifetime       context.Context
	cancelLifetime context.CancelFunc
	stopping       bool
	background     sync.WaitGroup
}

func New() *Manager {
	return NewWithLimits(DefaultMaxEntries, DefaultMaxOperationBytes, DefaultMaxProcessBytes)
}

func NewWithLimits(maxEntries, maxOperationBytes, maxProcessBytes int) *Manager {
	if maxEntries <= 0 || maxOperationBytes <= 0 || maxProcessBytes <= 0 {
		panic("operation runtime limits must be positive")
	}
	lifetime, cancel := context.WithCancel(context.Background())
	return &Manager{entries: map[string]*runtimeEntry{}, maxEntries: maxEntries,
		maxOperationBytes: maxOperationBytes, maxProcessBytes: maxProcessBytes,
		lifetime: lifetime, cancelLifetime: cancel}
}

// BeginBackground admits one background worker. The returned context ends when
// BeginStop (or Stop) is called; done must be called exactly once when the worker has saved
// its final state and released everything it holds. Admission and Stop share
// the manager mutex, so a worker is either counted before Stop starts waiting
// or refused with ErrStopping.
func (m *Manager) BeginBackground() (context.Context, func(), error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopping {
		return nil, nil, ErrStopping
	}
	m.background.Add(1)
	var once sync.Once
	return m.lifetime, func() { once.Do(m.background.Done) }, nil
}

// BeginStop refuses new background workers and cancels the running ones
// without waiting for them. Shutdown is split in two on purpose: main calls
// BeginStop before it drains HTTP and joins the workers later with Wait.
// A request can be blocked on a vault lock that a background worker holds; if
// the cancel only came after the HTTP drain, that drain would time out, the
// rest of shutdown would be skipped and the worker would never be cancelled
// or record its interrupted state. Calling it again is harmless.
func (m *Manager) BeginStop() {
	m.mu.Lock()
	m.stopping = true
	m.mu.Unlock()
	m.cancelLifetime()
}

// Wait waits until every admitted background worker has called its done, or
// until ctx ends. A worker has to write its interrupted state after the
// cancel, so callers keep the database open until Wait returns nil.
func (m *Manager) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		m.background.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stop is BeginStop followed by Wait.
func (m *Manager) Stop(ctx context.Context) error {
	m.BeginStop()
	return m.Wait(ctx)
}

// Register is called only after the durable operation row exists. Re-registering
// an active ID is rejected so one workflow cannot replace another's cancel owner.
func (m *Manager) Register(operationID string, cancel func() error) error {
	if operationID == "" {
		return ErrNotFound
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.entries[operationID]; exists {
		return errors.New("operation runtime is already registered")
	}
	state := cancelClosed
	if cancel != nil {
		state = cancelIdle
	}
	m.entries[operationID] = &runtimeEntry{cancel: cancel, state: state}
	return nil
}

// Append accepts bounded complete native records. The command boundary owns
// record assembly; this manager owns only retention and ordering.
func (m *Manager) Append(operationID, stream, value string) {
	if value == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.entries[operationID]
	if entry == nil {
		return
	}
	size := len(value)
	stored := &storedEntry{Entry: Entry{Stream: stream, Text: value}, operationID: operationID, size: size}
	stored.operation = entry.entries.PushBack(stored)
	stored.global = m.order.PushBack(stored)
	entry.bytes += size
	m.totalBytes += size
	for entry.entries.Len() > m.maxEntries || entry.bytes > m.maxOperationBytes {
		m.evict(entry.entries.Front().Value.(*storedEntry))
	}
	for m.totalBytes > m.maxProcessBytes && m.order.Len() > 0 {
		m.evict(m.order.Front().Value.(*storedEntry))
	}
}

func (m *Manager) evict(stored *storedEntry) {
	if stored == nil || stored.operation == nil || stored.global == nil {
		return
	}
	entry := m.entries[stored.operationID]
	if entry == nil {
		return
	}
	// Direct list nodes keep both per-operation and process-wide oldest-entry
	// eviction O(1) while the manager mutex protects deterministic ordering.
	entry.entries.Remove(stored.operation)
	m.order.Remove(stored.global)
	stored.operation = nil
	stored.global = nil
	entry.bytes -= stored.size
	m.totalBytes -= stored.size
	entry.truncated = true
}

func (m *Manager) Snapshot(operationID string) Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.entries[operationID]
	if entry == nil {
		return Snapshot{}
	}
	entries := make([]Entry, 0, entry.entries.Len())
	for element := entry.entries.Front(); element != nil; element = element.Next() {
		entries = append(entries, element.Value.(*storedEntry).Entry)
	}
	return Snapshot{Available: true, Entries: entries,
		Truncated: entry.truncated, Cancelable: entry.state == cancelIdle,
		CancelRequested: entry.state == cancelAccepted || entry.state == cancelInFlight}
}

// Cancel coalesces concurrent attempts without holding the manager lock while
// workflow-owned queue or database arbitration runs. A failure reopens only
// this explicit gate; the workflow decides whether its work stays quarantined.
func (m *Manager) Cancel(operationID string) (CancelResult, error) {
	m.mu.Lock()
	entry := m.entries[operationID]
	if entry == nil {
		m.mu.Unlock()
		return CancelResult{}, ErrNotFound
	}
	switch entry.state {
	case cancelAccepted:
		m.mu.Unlock()
		return CancelResult{Accepted: true}, nil
	case cancelClosed:
		m.mu.Unlock()
		return CancelResult{}, ErrNotCancelable
	case cancelInFlight:
		attempt := entry.attempt
		m.mu.Unlock()
		<-attempt.done
		if attempt.err != nil {
			return CancelResult{Retryable: true}, attempt.err
		}
		return CancelResult{Accepted: true}, nil
	case cancelIdle:
		attempt := &cancelAttempt{done: make(chan struct{})}
		entry.state = cancelInFlight
		entry.attempt = attempt
		callback := entry.cancel
		m.mu.Unlock()

		err := callback()
		m.mu.Lock()
		attempt.err = err
		if current := m.entries[operationID]; current == entry && current.attempt == attempt {
			if err == nil {
				current.state = cancelAccepted
			} else {
				current.state = cancelIdle
			}
			current.attempt = nil
		}
		close(attempt.done)
		m.mu.Unlock()
		if err != nil {
			return CancelResult{Retryable: true}, err
		}
		return CancelResult{Accepted: true}, nil
	default:
		m.mu.Unlock()
		return CancelResult{}, ErrNotCancelable
	}
}

// CloseCancel atomically closes launch admission against a late cancel request.
// It returns false only when an accepted/in-flight request already won.
func (m *Manager) CloseCancel(operationID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.entries[operationID]
	if entry == nil {
		return true
	}
	if entry.state == cancelIdle {
		entry.state = cancelClosed
		entry.cancel = nil
		return true
	}
	return entry.state == cancelClosed
}

// Remove follows durable terminal persistence and owner cleanup. Cancellation
// ownership is deliberately independent from any earlier log eviction.
func (m *Manager) Remove(operationID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := m.entries[operationID]
	if entry == nil {
		return
	}
	for entry.entries.Len() > 0 {
		m.evict(entry.entries.Front().Value.(*storedEntry))
	}
	delete(m.entries, operationID)
}
