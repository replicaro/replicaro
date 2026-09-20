// Package vaultlock serializes every engine operation that can touch one
// repository. It is process-wide so API, scheduler, metadata, and job paths
// share the same admission gate.
package vaultlock

import (
	"context"
	"errors"
	"sync"
	"time"
)

var ErrLowPriorityYield = errors.New("low-priority vault work yielded to an active job")

var lowPriorityBeforePhysicalUnlockForTests func()

type lowPriorityReader struct {
	cancel context.CancelCauseFunc
	pause  func(bool)
}

type vaultMutex struct {
	lock     sync.RWMutex
	mu       sync.Mutex
	next     uint64
	priority int
	low      map[uint64]lowPriorityReader
}

var registry struct {
	sync.Mutex
	values map[string]*vaultMutex
}

func init() { registry.values = map[string]*vaultMutex{} }

func mutex(id string) *vaultMutex {
	registry.Lock()
	defer registry.Unlock()
	if value := registry.values[id]; value != nil {
		return value
	}
	value := &vaultMutex{low: map[uint64]lowPriorityReader{}}
	registry.values[id] = value
	return value
}

// tryExclusive is only the final immediate probe after public admission has
// registered priority over low-priority readers. Operation callers must use the context-aware
// exclusive entry points below; there is no public non-yielding bypass.
func tryExclusive(id string) (func(), bool) {
	lock := mutex(id)
	if !lock.lock.TryLock() {
		return nil, false
	}
	return lock.lock.Unlock, true
}

// AcquireExclusiveContext makes low-priority readers yield, then waits for
// exclusive access, including ordinary reader/writer contention. Every public
// exclusive entry point gives priority over low-priority readers. Callers choose
// only whether to wait for ordinary contention or report busy.
// An optional onWait notification reports ordinary or draining-reader contention
// once, before waiting. It grants no access and must return promptly.
// A real waiting writer prevents writer starvation. Cancellation returns
// promptly; a cleanup goroutine releases any later mutex acquisition.
func AcquireExclusiveContext(ctx context.Context, id string, onWait ...func()) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	lock := mutex(id)
	endPriority := beginPriority(lock)
	defer endPriority()
	if unlock, ok := tryExclusive(id); ok {
		if err := ctx.Err(); err != nil {
			unlock()
			return nil, err
		}
		return unlock, nil
	}
	for _, notify := range onWait {
		if notify != nil {
			notify()
		}
	}
	acquired := make(chan struct{})
	go func() {
		lock.lock.Lock()
		close(acquired)
	}()
	select {
	case <-acquired:
		if err := ctx.Err(); err != nil {
			lock.lock.Unlock()
			return nil, err
		}
		return lock.lock.Unlock, nil
	case <-ctx.Done():
		go func() {
			<-acquired
			lock.lock.Unlock()
		}()
		return nil, ctx.Err()
	}
}

// YieldLowPriorityAndTryExclusiveContext drains only cancellable low-priority
// readers before making one nonblocking exclusive attempt. Contention with an
// ordinary reader or writer still returns busy rather than becoming a wait.
func YieldLowPriorityAndTryExclusiveContext(ctx context.Context, id string) (func(), bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	lock := mutex(id)
	endPriority := beginPriority(lock)
	defer endPriority()
	for {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		lock.mu.Lock()
		drained := len(lock.low) == 0
		lock.mu.Unlock()
		if drained {
			unlock, ok := tryExclusive(id)
			if err := ctx.Err(); err != nil {
				if ok {
					unlock()
				}
				return nil, false, err
			}
			return unlock, ok, nil
		}
		// beginPriority already canceled the registered readers and prevents
		// new low-priority admission until this attempt finishes.
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, false, ctx.Err()
		case <-timer.C:
		}
	}
}

func TryShared(id string) (func(), bool) {
	lock := mutex(id)
	if !lock.lock.TryRLock() {
		return nil, false
	}
	return lock.lock.RUnlock, true
}

func AcquireShared(id string) func() {
	lock := mutex(id)
	lock.lock.RLock()
	return lock.lock.RUnlock
}

// AcquireSharedContext waits for a shared lock without outliving ctx.
func AcquireSharedContext(ctx context.Context, id string) (func(), error) {
	lock := mutex(id)
	for {
		if lock.lock.TryRLock() {
			return lock.lock.RUnlock, nil
		}
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// AcquireLowPrioritySharedContext admits a read-only background operation and
// returns a derived context that is canceled when any public exclusive
// acquisition needs the vault. The caller must stop native work and release the
// shared lock before the priority owner can proceed.
func AcquireLowPrioritySharedContext(
	ctx context.Context,
	id string,
	reportPause func(bool),
) (context.Context, func(), error) {
	lock := mutex(id)
	reportedPause, hasReportedPause := false, false
	report := func(next bool) {
		if hasReportedPause && reportedPause == next {
			return
		}
		reportedPause, hasReportedPause = next, true
		if reportPause != nil {
			reportPause(next)
		}
	}
	for {
		lock.mu.Lock()
		priority := lock.priority > 0
		lock.mu.Unlock()
		if priority {
			report(true)
			timer := time.NewTimer(5 * time.Millisecond)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				return nil, nil, ctx.Err()
			case <-timer.C:
			}
			continue
		}
		if lock.lock.TryRLock() {
			leaseContext, cancel := context.WithCancelCause(ctx)
			lock.mu.Lock()
			if lock.priority > 0 {
				lock.mu.Unlock()
				cancel(ErrLowPriorityYield)
				lock.lock.RUnlock()
				report(true)
				continue
			}
			lock.next++
			leaseID := lock.next
			lock.low[leaseID] = lowPriorityReader{cancel: cancel, pause: reportPause}
			lock.mu.Unlock()
			report(false)
			var once sync.Once
			return leaseContext, func() {
				once.Do(func() {
					cancel(nil)
					if lowPriorityBeforePhysicalUnlockForTests != nil {
						lowPriorityBeforePhysicalUnlockForTests()
					}
					lock.lock.RUnlock()
					lock.mu.Lock()
					delete(lock.low, leaseID)
					lock.mu.Unlock()
				})
			}, nil
		}
		report(true)
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func lowPriorityReadersLocked(lock *vaultMutex) []lowPriorityReader {
	readers := make([]lowPriorityReader, 0, len(lock.low))
	for _, reader := range lock.low {
		readers = append(readers, reader)
	}
	return readers
}

func beginPriority(lock *vaultMutex) func() {
	lock.mu.Lock()
	lock.priority++
	readers := lowPriorityReadersLocked(lock)
	lock.mu.Unlock()
	pauseReaders(readers)
	var once sync.Once
	return func() {
		once.Do(func() {
			lock.mu.Lock()
			lock.priority--
			lock.mu.Unlock()
		})
	}
}

func pauseReaders(readers []lowPriorityReader) {
	for _, reader := range readers {
		if reader.pause != nil {
			reader.pause(true)
		}
		reader.cancel(ErrLowPriorityYield)
	}
}
