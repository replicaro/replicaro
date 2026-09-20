package vaultstatistics

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/local/replicaro/command"
	"github.com/local/replicaro/database"
	"github.com/local/replicaro/engines"
	"github.com/local/replicaro/models"
	"github.com/local/replicaro/rclone"
	"github.com/local/replicaro/repositoryadmission"
	"github.com/local/replicaro/runner"
	"github.com/local/replicaro/storageavailability"
	"github.com/local/replicaro/vaultlock"
	"github.com/local/replicaro/vaultprofile"
)

const (
	automaticCooldown  = 24 * time.Hour
	maximumOutputBytes = 1 << 20
)

var (
	materializeRclone = rclone.Materialize
	runRclone         = command.RunWithInputSecretsPrivateOutput
	measureVaultSize  = Measure
	admitRepository   = repositoryadmission.AdmitUnderLock
	now               = time.Now
)

func SetRepositoryAdmissionForTests(next func(context.Context, *sql.DB, models.Repository) (models.Repository, error)) func() {
	previous := admitRepository
	admitRepository = next
	return func() { admitRepository = previous }
}

type coordinator struct {
	mu       sync.Mutex
	ctx      context.Context
	cancel   context.CancelFunc
	accept   bool
	wg       sync.WaitGroup
	running  map[string]bool
	pending  map[string]bool
	paused   map[string]bool
	failures map[string]string
}

var coordinators struct {
	sync.Mutex
	values map[*sql.DB]*coordinator
}

func init() { coordinators.values = map[*sql.DB]*coordinator{} }

func StartContext(parent context.Context, db *sql.DB) func(context.Context) error {
	coordinators.Lock()
	if old := coordinators.values[db]; old != nil {
		coordinators.Unlock()
		return old.stop
	}
	ctx, cancel := context.WithCancel(parent)
	c := &coordinator{ctx: ctx, cancel: cancel, accept: true, running: map[string]bool{}, pending: map[string]bool{}, paused: map[string]bool{}, failures: map[string]string{}}
	coordinators.values[db] = c
	coordinators.Unlock()
	return c.stop
}

func (c *coordinator) stop(ctx context.Context) error {
	c.mu.Lock()
	if c.accept {
		c.accept = false
		c.cancel()
	}
	c.mu.Unlock()
	done := make(chan struct{})
	go func() { c.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func coordinatorFor(db *sql.DB) *coordinator {
	coordinators.Lock()
	defer coordinators.Unlock()
	if value := coordinators.values[db]; value != nil {
		return value
	}
	ctx, cancel := context.WithCancel(context.Background())
	value := &coordinator{ctx: ctx, cancel: cancel, accept: true, running: map[string]bool{}, pending: map[string]bool{}, paused: map[string]bool{}, failures: map[string]string{}}
	coordinators.values[db] = value
	return value
}

func Status(db *sql.DB, repositoryID string) (models.VaultSizeStatus, error) {
	return StatusWithReader(db, db, repositoryID)
}

// StatusWithReader keeps runtime state attached to the writer-owned coordinator
// while loading persisted fields through the query pool. Coordinators are keyed
// by *sql.DB, so using readDB for both jobs would create a second, empty
// coordinator and hide running, pending, paused, and failure state from the UI.
func StatusWithReader(coordinatorDB, readDB *sql.DB, repositoryID string) (models.VaultSizeStatus, error) {
	repo, err := database.GetRepository(readDB, repositoryID)
	if err != nil {
		return models.VaultSizeStatus{}, err
	}
	c := coordinatorFor(coordinatorDB)
	c.mu.Lock()
	running, pending, paused, failure := c.running[repositoryID], c.pending[repositoryID], c.paused[repositoryID], c.failures[repositoryID]
	c.mu.Unlock()
	return models.VaultSizeStatus{VaultSizeBytes: repo.VaultSizeBytes, VaultSizeMeasuredAt: repo.VaultSizeMeasuredAt,
		VaultSizeDirty: repo.VaultSizeDirty, Fresh: database.RepositoryVaultSizeFresh(repo, now()),
		Running: running, Pending: pending, Paused: paused && (running || pending), Failure: failure}, nil
}

func Prepare(db *sql.DB, repositoryID string, force bool) (models.VaultSizeStatus, error) {
	repo, err := database.GetRepository(db, repositoryID)
	if err != nil {
		return models.VaultSizeStatus{}, err
	}
	c := coordinatorFor(db)
	c.mu.Lock()
	if !c.accept {
		c.mu.Unlock()
		return models.VaultSizeStatus{}, runner.ErrAdmissionClosed
	}
	current := now()
	if c.running[repositoryID] || c.pending[repositoryID] {
		if force {
			if err := database.RecordVaultSizeAttempt(db, repositoryID, current); err != nil {
				c.mu.Unlock()
				return models.VaultSizeStatus{}, err
			}
		}
		c.mu.Unlock()
		return Status(db, repositoryID)
	}
	needed := !database.RepositoryVaultSizeFresh(repo, current)
	if !force && !needed {
		c.mu.Unlock()
		return Status(db, repositoryID)
	}
	if !force && repo.VaultSizeLastAttemptAt != "" {
		attempt, parseErr := time.Parse(time.RFC3339Nano, repo.VaultSizeLastAttemptAt)
		if parseErr == nil && !attempt.After(current) && current.Sub(attempt) < automaticCooldown {
			c.mu.Unlock()
			return Status(db, repositoryID)
		}
	}
	if err := database.RecordVaultSizeAttempt(db, repositoryID, current); err != nil {
		c.mu.Unlock()
		return models.VaultSizeStatus{}, err
	}
	c.pending[repositoryID] = true
	delete(c.failures, repositoryID)
	c.wg.Add(1)
	c.mu.Unlock()
	go c.run(db, repo)
	return Status(db, repositoryID)
}

func (c *coordinator) run(db *sql.DB, repo models.Repository) {
	defer c.wg.Done()
	var err error
	for c.ctx.Err() == nil {
		workContext, unlock, lockErr := vaultlock.AcquireLowPrioritySharedContext(
			c.ctx,
			repo.ID,
			func(paused bool) {
				c.mu.Lock()
				if paused {
					c.paused[repo.ID] = true
				} else {
					delete(c.paused, repo.ID)
				}
				c.mu.Unlock()
			},
		)
		if lockErr != nil {
			err = lockErr
			break
		}

		currentRepo, reloadErr := database.GetRepository(db, repo.ID)
		if errors.Is(reloadErr, sql.ErrNoRows) {
			unlock()
			err = nil
			break
		}
		if reloadErr != nil || currentRepo.Engine != repo.Engine || currentRepo.Connector != repo.Connector {
			err = errors.Join(reloadErr, fmt.Errorf("queued Vault Size authority changed before measurement"))
			unlock()
			break
		}
		currentRepo, reloadErr = admitRepository(workContext, db, currentRepo)
		if reloadErr != nil {
			err = reloadErr
		} else {
			releaseAdmission, admissionErr := runner.AcquireBackgroundNativeAdmission(workContext, db)
			if admissionErr == nil {
				c.mu.Lock()
				delete(c.pending, repo.ID)
				c.running[repo.ID] = true
				c.mu.Unlock()
				bytes, measureErr := measureVaultSize(workContext, currentRepo)
				if measureErr == nil && context.Cause(workContext) == nil && c.ctx.Err() == nil {
					measureErr = database.PublishVaultSize(db, currentRepo.ID, bytes, now())
				}
				releaseAdmission()
				err = measureErr
			} else {
				err = admissionErr
			}
		}
		unlock()

		// Validation also runs under the low-priority context. An exclusive
		// writer can interrupt it before measurement starts; retain that same
		// pending refresh instead of dropping it into the automatic cooldown.
		if errors.Is(context.Cause(workContext), vaultlock.ErrLowPriorityYield) && c.ctx.Err() == nil {
			c.mu.Lock()
			delete(c.running, repo.ID)
			c.pending[repo.ID] = true
			c.paused[repo.ID] = true
			c.mu.Unlock()
			continue
		}
		break
	}
	c.mu.Lock()
	delete(c.running, repo.ID)
	delete(c.pending, repo.ID)
	delete(c.paused, repo.ID)
	if err != nil && c.ctx.Err() == nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		c.failures[repo.ID] = "Vault Size refresh failed."
		_ = database.LogWarning(db, "Vault Size refresh failed for vault "+repo.ID)
	}
	c.mu.Unlock()
}

func Measure(ctx context.Context, repo models.Repository) (result int64, err error) {
	base, err := vaultprofile.BaseRemoteForVault(repo)
	if err != nil {
		return 0, fmt.Errorf("prepare base vault root")
	}
	binding, err := engines.OpenExistingRcloneStatisticsConfigBinding(ctx, repo)
	if err != nil {
		return 0, fmt.Errorf("open canonical rclone configuration")
	}
	defer func() { err = errors.Join(err, binding.Close()) }()
	binary, err := materializeRclone()
	if err != nil {
		return 0, fmt.Errorf("prepare pinned rclone")
	}
	if err := binding.Revalidate(ctx); err != nil {
		return 0, fmt.Errorf("validate canonical rclone configuration")
	}
	environment := append([]string(nil), base.Env...)
	for index, item := range environment {
		key, raw, ok := strings.Cut(item, "=")
		if !ok || key != "RCLONE_CONFIG_BASE_PASS" || raw == "" {
			continue
		}
		obscuredOutput, obscureErr := runRclone(ctx, binary,
			[]string{"obscure", "-", "--config", binding.NativePath()}, nil, raw+"\n", time.Minute, "rclone")
		if obscureErr != nil {
			return 0, fmt.Errorf("prepare SFTP credential")
		}
		obscured := strings.TrimSpace(obscuredOutput)
		if obscured == "" || strings.ContainsAny(obscured, "\r\n") {
			return 0, fmt.Errorf("prepare SFTP credential")
		}
		environment[index] = key + "=" + obscured
	}
	launchContext := command.ContextWithBeforeProcess(ctx, func(check context.Context) error {
		if err := binding.Revalidate(check); err != nil {
			return err
		}
		return storageavailability.RequireRepositoryAvailable(check, repo)
	})
	output, nativeErr := runRclone(launchContext, binary,
		[]string{"size", base.Root, "--json", "--fast-list", "--config", binding.NativePath(), "--log-level", "ERROR"},
		environment, "", 10*time.Minute, "rclone")
	postErr := binding.Revalidate(context.WithoutCancel(ctx))
	if nativeErr != nil || postErr != nil {
		return 0, fmt.Errorf("native Vault Size measurement failed")
	}
	return ParseSizeOutput(output)
}

func ParseSizeOutput(output string) (int64, error) {
	if len(output) > maximumOutputBytes {
		return 0, fmt.Errorf("rclone size output is excessive")
	}
	decoder := json.NewDecoder(strings.NewReader(output))
	decoder.UseNumber()
	value, err := readUnique(decoder)
	if err != nil {
		return 0, fmt.Errorf("invalid rclone size output")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return 0, fmt.Errorf("invalid trailing rclone size output")
	}
	object, ok := value.(map[string]any)
	if !ok {
		return 0, fmt.Errorf("rclone size output is not an object")
	}
	bytes, ok := exactNonnegativeInt(object["bytes"])
	if !ok {
		return 0, fmt.Errorf("rclone size bytes are invalid")
	}
	if _, ok := exactNonnegativeInt(object["sizeless"]); !ok {
		return 0, fmt.Errorf("rclone size sizeless is invalid")
	}
	return bytes, nil
}

func exactNonnegativeInt(value any) (int64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	parsed, err := strconv.ParseInt(string(number), 10, 64)
	return parsed, err == nil && parsed >= 0
}

func readUnique(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delim, isDelim := token.(json.Delim)
	if !isDelim {
		return token, nil
	}
	switch delim {
	case '{':
		object := map[string]any{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, fmt.Errorf("invalid object key")
			}
			if _, exists := object[key]; exists {
				return nil, fmt.Errorf("duplicate field")
			}
			child, err := readUnique(decoder)
			if err != nil {
				return nil, err
			}
			object[key] = child
		}
		_, err = decoder.Token()
		return object, err
	case '[':
		array := []any{}
		for decoder.More() {
			child, err := readUnique(decoder)
			if err != nil {
				return nil, err
			}
			array = append(array, child)
		}
		_, err = decoder.Token()
		return array, err
	default:
		return nil, fmt.Errorf("invalid delimiter")
	}
}
