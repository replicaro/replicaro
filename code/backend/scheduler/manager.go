package scheduler

import (
	"context"
	"database/sql"
	"log"
	"sync"
	"time"

	"github.com/local/replicaro/models"
	"github.com/local/replicaro/operationruntime"
)

const repositoryTaskConcurrency = 2

var schedulerInterval = time.Minute
var runRepositoryTask = func(ctx context.Context, db *sql.DB, repo models.Repository, operation string) (string, error) {
	return RunRepositoryTaskContextWithRuntime(ctx, db, repo, operation, operationruntime.FromContext(ctx))
}

type repositoryTaskCoordinator struct {
	db        *sql.DB
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	accepting bool
	active    map[string]bool
	sem       chan struct{}
	wg        sync.WaitGroup
	runtime   *operationruntime.Manager
}

func newRepositoryTaskCoordinator(db *sql.DB, parent context.Context, runtimeManagers ...*operationruntime.Manager) *repositoryTaskCoordinator {
	ctx, cancel := context.WithCancel(parent)
	runtimeManager := operationruntime.New()
	if len(runtimeManagers) > 0 && runtimeManagers[0] != nil {
		runtimeManager = runtimeManagers[0]
	}
	return &repositoryTaskCoordinator{db: db, ctx: ctx, cancel: cancel, accepting: true, active: map[string]bool{}, sem: make(chan struct{}, repositoryTaskConcurrency), runtime: runtimeManager}
}

func (c *repositoryTaskCoordinator) admit(repo models.Repository, operation string) bool {
	identity := repo.ID
	c.mu.Lock()
	if !c.accepting || c.ctx.Err() != nil || c.active[identity] {
		c.mu.Unlock()
		return false
	}
	c.active[identity] = true
	c.wg.Add(1)
	c.mu.Unlock()
	go func() {
		defer c.wg.Done()
		defer func() { c.mu.Lock(); delete(c.active, identity); c.mu.Unlock() }()
		select {
		case c.sem <- struct{}{}:
			defer func() { <-c.sem }()
		case <-c.ctx.Done():
			return
		}
		runCtx := operationruntime.ContextWithManager(c.ctx, c.runtime)
		if _, err := runRepositoryTask(runCtx, c.db, repo, operation); err != nil && c.ctx.Err() == nil {
			log.Printf("scheduler repository %s: %v", operation, err)
		}
	}()
	return true
}

func (c *repositoryTaskCoordinator) stop(ctx context.Context) error {
	c.mu.Lock()
	c.accepting = false
	c.mu.Unlock()
	c.cancel()
	done := make(chan struct{})
	go func() { c.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func Start(db *sql.DB) func() {
	stop := StartContext(db)
	return func() { _ = stop(context.Background()) }
}

// StartContext returns a stop function bounded by the caller's context.
func StartContext(db *sql.DB) func(context.Context) error {
	return StartContextWithRuntime(db, operationruntime.New())
}

func StartContextWithRuntime(db *sql.DB, runtimeManager *operationruntime.Manager) func(context.Context) error {
	ctx, cancel := context.WithCancel(context.Background())
	coordinator := newRepositoryTaskCoordinator(db, ctx, runtimeManager)
	var once sync.Once
	done := make(chan struct{})
	go func() {
		defer close(done)
		CheckDueJobsContext(ctx, db, coordinator) // immediate overdue admission
		ticker := time.NewTicker(schedulerInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				CheckDueJobsContext(ctx, db, coordinator)
			}
		}
	}()
	stopDone := make(chan struct{})
	return func(stopCtx context.Context) error {
		once.Do(func() {
			cancel() // stop tick admission first
			go func() {
				<-done
				_ = coordinator.stop(context.Background())
				close(stopDone)
			}()
		})
		select {
		case <-stopDone:
			return nil
		case <-stopCtx.Done():
			return stopCtx.Err()
		}
	}
}
