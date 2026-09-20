package appupdate

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/local/replicaro/database"
	"github.com/local/replicaro/desktop"
	"github.com/local/replicaro/models"
)

const (
	MetadataURL              = "https://cloud.replicaro.com/updates/check.json"
	requestTimeout           = 5 * time.Second
	maximumResponseSize      = 4096
	automaticStateRetryDelay = time.Minute
	updateNotificationTitle  = "Replicaro update available"
	updateNotificationBody   = "New Replicaro version is available! Click for details."
)

type Status struct {
	RunningVersion          string `json:"runningVersion"`
	Result                  string `json:"result"`
	AvailableVersion        string `json:"availableVersion,omitempty"`
	SkippedVersion          string `json:"skippedVersion,omitempty"`
	AvailableVersionSkipped bool   `json:"availableVersionSkipped"`
	AutomaticChecksDisabled bool   `json:"automaticChecksDisabled"`
	Checking                bool   `json:"checking"`
}

type checkOperation struct {
	done   chan struct{}
	status Status
	err    error
}

type Service struct {
	db               *sql.DB
	client           *http.Client
	endpoint         string
	now              func() time.Time
	cacheValue       func() string
	showNotification func(string, string, string, bool) error

	mu            sync.Mutex
	inFlight      *checkOperation
	automaticDone chan struct{}
	automaticWake chan struct{}
	waitAutomatic func(context.Context, <-chan struct{}, time.Duration) bool
}

var cacheFallback atomic.Uint64

func New(db *sql.DB) *Service {
	client := &http.Client{
		Timeout: requestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return fmt.Errorf("application update redirects are not allowed")
		},
	}
	return &Service{
		db: db, client: client, endpoint: MetadataURL, now: time.Now,
		cacheValue: freshCacheValue, automaticWake: make(chan struct{}, 1),
		waitAutomatic:    waitForAutomatic,
		showNotification: desktop.ShowPersistentNotification,
	}
}

func freshCacheValue() string {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err == nil {
		return hex.EncodeToString(random)
	}
	return fmt.Sprintf("%x-%x", time.Now().UnixNano(), cacheFallback.Add(1))
}

func (service *Service) NormalizeStartup() error {
	return database.NormalizeAppUpdateState(service.db, models.ReplicaroVersion)
}

func (service *Service) StartAutomatic(ctx context.Context) {
	service.mu.Lock()
	if service.automaticDone != nil {
		service.mu.Unlock()
		return
	}
	done := make(chan struct{})
	service.automaticDone = done
	service.mu.Unlock()
	go func() {
		defer close(done)
		service.runAutomatic(ctx)
	}()
}

func waitForAutomatic(ctx context.Context, wake <-chan struct{}, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}()
	select {
	case <-timer.C:
		return true
	case <-wake:
		return false
	case <-ctx.Done():
		return false
	}
}

func (service *Service) runAutomatic(ctx context.Context) {
	for ctx.Err() == nil {
		delay, enabled, err := database.AppUpdateAutomaticWait(service.db, service.now())
		if err != nil {
			if !service.waitAutomatic(ctx, service.automaticWake, automaticStateRetryDelay) && ctx.Err() != nil {
				return
			}
			continue
		}
		if !enabled {
			select {
			case <-service.automaticWake:
				continue
			case <-ctx.Done():
				return
			}
		}
		if delay > 0 {
			if !service.waitAutomatic(ctx, service.automaticWake, delay) {
				if ctx.Err() != nil {
					return
				}
				continue
			}
		}
		if _, err := service.Check(ctx, true); err != nil {
			if ctx.Err() != nil {
				return
			}
			if !service.waitAutomatic(ctx, service.automaticWake, automaticStateRetryDelay) && ctx.Err() != nil {
				return
			}
		}
	}
}

// AutomaticSettingsChanged wakes the automatic loop so a saved disable or
// re-enable preference takes effect without restarting Replicaro.
func (service *Service) AutomaticSettingsChanged() {
	select {
	case service.automaticWake <- struct{}{}:
	default:
	}
}

func (service *Service) Wait(ctx context.Context) error {
	service.mu.Lock()
	automaticDone := service.automaticDone
	service.mu.Unlock()
	if automaticDone != nil {
		select {
		case <-automaticDone:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	service.mu.Lock()
	operation := service.inFlight
	service.mu.Unlock()
	if operation == nil {
		return nil
	}
	select {
	case <-operation.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (service *Service) Status() (Status, error) {
	return service.StatusWithReader(service.db)
}

// StatusWithReader separates the status query from the writer-owned update
// workflow while retaining the service's in-memory single-flight state.
func (service *Service) StatusWithReader(readDB *sql.DB) (Status, error) {
	state, err := database.GetAppUpdateState(readDB)
	if err != nil {
		return Status{}, err
	}
	service.mu.Lock()
	checking := service.inFlight != nil
	service.mu.Unlock()
	return statusFromState(state, checking), nil
}

func statusFromState(state database.AppUpdateState, checking bool) Status {
	return Status{
		RunningVersion:          models.ReplicaroVersion,
		Result:                  state.LastResult,
		AvailableVersion:        state.LastAvailableVersion,
		SkippedVersion:          state.SkippedVersion,
		AvailableVersionSkipped: state.LastAvailableVersion != "" && state.LastAvailableVersion == state.SkippedVersion,
		AutomaticChecksDisabled: state.DisableAutomaticChecks,
		Checking:                checking,
	}
}

func (service *Service) Check(ctx context.Context, automatic bool) (status Status, err error) {
	service.mu.Lock()
	if existing := service.inFlight; existing != nil {
		service.mu.Unlock()
		select {
		case <-existing.done:
			return existing.status, existing.err
		case <-ctx.Done():
			return Status{}, ctx.Err()
		}
	}
	if automatic {
		due, err := database.AppUpdateAutomaticDue(service.db, service.now())
		if err != nil {
			service.mu.Unlock()
			return Status{}, err
		}
		if !due {
			service.mu.Unlock()
			return service.Status()
		}
	}
	operation := &checkOperation{done: make(chan struct{})}
	service.inFlight = operation
	service.mu.Unlock()
	defer func() {
		service.mu.Lock()
		operation.status = status
		operation.err = err
		if service.inFlight == operation {
			service.inFlight = nil
		}
		close(operation.done)
		service.mu.Unlock()
	}()

	if automatic {
		if err := database.RecordAppUpdateAutomaticAttempt(service.db, service.now()); err != nil {
			return Status{}, err
		}
	}
	result, version := service.fetch(ctx)
	if err := database.CompleteAppUpdateCheck(service.db, result, version, models.ReplicaroVersion); err != nil {
		return Status{}, err
	}
	state, err := database.GetAppUpdateState(service.db)
	if err != nil {
		return Status{}, err
	}
	status = statusFromState(state, false)
	if automatic && !status.AutomaticChecksDisabled && status.Result == "update_available" && !status.AvailableVersionSkipped {
		if notifyErr := service.showNotification(updateNotificationTitle, updateNotificationBody, "", true); notifyErr != nil {
			log.Printf("application update notification: %v", notifyErr)
		}
	}
	return status, nil
}

func (service *Service) Skip(displayedVersion string) (Status, bool, error) {
	state, stored, err := database.SkipCurrentAppUpdateVersion(service.db, displayedVersion, models.ReplicaroVersion)
	if err != nil {
		return Status{}, false, err
	}
	service.mu.Lock()
	checking := service.inFlight != nil
	service.mu.Unlock()
	return statusFromState(state, checking), stored, nil
}

func (service *Service) fetch(ctx context.Context) (string, string) {
	target, err := url.Parse(service.endpoint)
	if err != nil {
		return "unavailable", ""
	}
	query := target.Query()
	query.Set("check", service.cacheValue())
	target.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return "unavailable", ""
	}
	response, err := service.client.Do(request)
	if err != nil {
		return "unavailable", ""
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maximumResponseSize+1))
		return "unavailable", ""
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maximumResponseSize+1))
	if err != nil || len(data) > maximumResponseSize {
		return "unavailable", ""
	}
	version, err := parseMetadata(data)
	if err != nil {
		return "unavailable", ""
	}
	running, err := models.ParseSemanticVersion(models.ReplicaroVersion)
	if err != nil {
		return "unavailable", ""
	}
	if models.CompareSemanticVersions(version.parsed, running) > 0 {
		return "update_available", version.raw
	}
	return "up_to_date", ""
}

type parsedVersion struct {
	raw    string
	parsed models.SemanticVersion
}

func parseMetadata(data []byte) (parsedVersion, error) {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return parsedVersion{}, fmt.Errorf("application update metadata must be an object")
	}
	seenVersion := false
	versionValue := ""
	for decoder.More() {
		fieldToken, err := decoder.Token()
		if err != nil {
			return parsedVersion{}, err
		}
		field, ok := fieldToken.(string)
		if !ok || field != "version" || seenVersion {
			return parsedVersion{}, fmt.Errorf("application update metadata has an unknown or duplicate field")
		}
		seenVersion = true
		if err := decoder.Decode(&versionValue); err != nil {
			return parsedVersion{}, err
		}
	}
	closing, err := decoder.Token()
	if err != nil || closing != json.Delim('}') || !seenVersion {
		return parsedVersion{}, fmt.Errorf("application update metadata is incomplete")
	}
	if token, err := decoder.Token(); err != io.EOF || token != nil {
		return parsedVersion{}, fmt.Errorf("application update metadata has trailing data")
	}
	parsed, err := models.ParseSemanticVersion(versionValue)
	if err != nil {
		return parsedVersion{}, err
	}
	return parsedVersion{raw: versionValue, parsed: parsed}, nil
}
