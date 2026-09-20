package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/local/replicaro/api"
	"github.com/local/replicaro/appdata"
	"github.com/local/replicaro/appupdate"
	"github.com/local/replicaro/artifactrecovery"
	"github.com/local/replicaro/command"
	"github.com/local/replicaro/database"
	"github.com/local/replicaro/desktop"
	"github.com/local/replicaro/engines"
	"github.com/local/replicaro/instanceguard"
	"github.com/local/replicaro/kopiapolicy"
	"github.com/local/replicaro/metadata"
	"github.com/local/replicaro/models"
	"github.com/local/replicaro/notifications"
	"github.com/local/replicaro/operationlog"
	"github.com/local/replicaro/operationruntime"
	"github.com/local/replicaro/profilesync"
	"github.com/local/replicaro/rendezvous"
	"github.com/local/replicaro/repositoryadmission"
	"github.com/local/replicaro/runner"
	"github.com/local/replicaro/runtimeendpoint"
	"github.com/local/replicaro/scheduler"
	"github.com/local/replicaro/storageavailability"
	"github.com/local/replicaro/storagehelper"
	"github.com/local/replicaro/vaultlock"
	"github.com/local/replicaro/vaultprofile"
	"github.com/local/replicaro/vaultstatistics"
)

const (
	startupPublicationActivationWindow = 15 * time.Second
	activationRequestReserve           = 3 * time.Second
	containerAPIListenPort             = 9460
	containerRcloneAuthRelayPort       = 53683
	containerPackageMarkerPath         = "/opt/replicaro/.container-package"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

type runtimeOwnership struct {
	guard          *instanceguard.Guard
	listener       net.Listener
	endpoint       runtimeendpoint.Endpoint
	record         rendezvous.Record
	rendezvousPath string
	published      bool
	closeOnce      sync.Once
	closeErr       error
}

type readinessListener struct {
	net.Listener
	ready chan struct{}
	once  sync.Once
}

type startupOptions struct {
	loopbackPort            *int
	containerPublishedPort  *int
	rcloneAuthNoOpenBrowser bool
}

func parseStartupOptions(arguments []string) (startupOptions, error) {
	var options startupOptions
	for _, argument := range arguments {
		switch {
		case strings.HasPrefix(argument, "--loopback-port="):
			if options.loopbackPort != nil {
				return startupOptions{}, fmt.Errorf("--loopback-port may be supplied only once")
			}
			value := strings.TrimPrefix(argument, "--loopback-port=")
			port, err := strconv.Atoi(value)
			if err != nil || value != strconv.Itoa(port) || port < 1 || port > 65535 {
				return startupOptions{}, fmt.Errorf("--loopback-port must be a canonical decimal integer from 1 through 65535")
			}
			options.loopbackPort = &port
		case argument == "--loopback-port":
			return startupOptions{}, fmt.Errorf("--loopback-port must use exactly --loopback-port=<port>")
		case strings.HasPrefix(argument, "--container-published-port="):
			if options.containerPublishedPort != nil {
				return startupOptions{}, fmt.Errorf("--container-published-port may be supplied only once")
			}
			value := strings.TrimPrefix(argument, "--container-published-port=")
			port, err := strconv.Atoi(value)
			if err != nil || value != strconv.Itoa(port) || port < 1 || port > 65535 {
				return startupOptions{}, fmt.Errorf("--container-published-port must be a canonical decimal integer from 1 through 65535")
			}
			if port == 80 || port == 53682 {
				return startupOptions{}, fmt.Errorf("--container-published-port cannot use HTTP's implicit port 80 or rclone's reserved callback port 53682")
			}
			options.containerPublishedPort = &port
		case argument == "--container-published-port":
			return startupOptions{}, fmt.Errorf("--container-published-port must use exactly --container-published-port=<port>")
		case argument == "--rclone-auth-no-open-browser":
			if options.rcloneAuthNoOpenBrowser {
				return startupOptions{}, fmt.Errorf("--rclone-auth-no-open-browser may be supplied only once")
			}
			options.rcloneAuthNoOpenBrowser = true
		case strings.HasPrefix(argument, "--rclone-auth-no-open-browser="):
			return startupOptions{}, fmt.Errorf("--rclone-auth-no-open-browser does not accept a value")
		default:
			// Preserve the baseline CLI behavior: arguments outside the three
			// Replicaro-owned namespaces are ignored.
			continue
		}
	}
	if options.loopbackPort != nil && options.containerPublishedPort != nil {
		return startupOptions{}, fmt.Errorf("--loopback-port and --container-published-port are mutually exclusive")
	}
	return options, nil
}

func requireContainerRuntime() error {
	return requireContainerRuntimeFor(runtime.GOOS, os.Lstat)
}

func requireContainerRuntimeFor(goos string, lstat func(string) (os.FileInfo, error)) error {
	if goos != "linux" {
		return fmt.Errorf("--container-published-port requires a Linux container runtime")
	}
	info, err := lstat(containerPackageMarkerPath)
	if err == nil && info.Mode().IsRegular() && info.Mode().Perm() == 0o444 {
		return nil
	}
	return fmt.Errorf("--container-published-port requires the official container package")
}

func (listener *readinessListener) Accept() (net.Conn, error) {
	// Entering Accept proves that net/http completed its server setup without
	// requiring a synthetic client socket. Client-side socket exhaustion must
	// not tear down an otherwise initialized and already-bound local service.
	listener.once.Do(func() { close(listener.ready) })
	connection, err := listener.Listener.Accept()
	if err != nil && isNoBufferSpaceError(err) {
		// The OS may also run out of socket buffers while allocating the accepted
		// socket. Preserve the exact native cause while opting into net/http's
		// existing bounded temporary-Accept backoff for this one transient class.
		return nil, temporaryAcceptError{err: err}
	}
	return connection, err
}

type temporaryAcceptError struct{ err error }

func (err temporaryAcceptError) Error() string { return err.err.Error() }
func (err temporaryAcceptError) Unwrap() error { return err.err }
func (temporaryAcceptError) Timeout() bool     { return false }
func (temporaryAcceptError) Temporary() bool   { return true }

// Winsock reports the Windows form of ENOBUFS as WSAENOBUFS (10055), while
// Unix-family targets report syscall.ENOBUFS. Keep the exact fixed pair here
// so the readiness listener remains one shared implementation on every target.
const windowsWSAENOBUFS syscall.Errno = 10055

func isNoBufferSpaceError(err error) bool {
	return errors.Is(err, syscall.ENOBUFS) || errors.Is(err, windowsWSAENOBUFS)
}

// acquireRuntimeOwnership is the single production and process-test seam for
// guard authority, stale rendezvous cleanup, and loopback listener selection.
// A losing instance activates the owner and never creates a listener.
func acquireRuntimeOwnership(root string, explicitPort ...int) (*runtimeOwnership, bool, error) {
	return acquireRuntimeOwnershipWithListener(root, func() (net.Listener, runtimeendpoint.Endpoint, error) {
		if len(explicitPort) > 1 {
			return nil, runtimeendpoint.Endpoint{}, fmt.Errorf("at most one explicit loopback port is allowed")
		}
		var listener net.Listener
		var err error
		if len(explicitPort) == 1 {
			listener, err = runtimeendpoint.ListenPort(explicitPort[0])
		} else {
			listener, err = runtimeendpoint.Listen()
		}
		if err != nil {
			return nil, runtimeendpoint.Endpoint{}, err
		}
		endpoint, err := runtimeendpoint.FromListener(listener)
		if err != nil {
			_ = listener.Close()
			return nil, runtimeendpoint.Endpoint{}, err
		}
		return listener, endpoint, nil
	})
}

// acquireContainerRuntimeOwnership is an explicit package-only mode. Docker
// publishes the fixed internal listener onto one host IPv4-loopback port; the
// public endpoint remains loopback so Host/Origin/CSP and rendezvous identity
// never treat a container or bridge address as an approved browser origin.
func acquireContainerRuntimeOwnership(root string, publishedPort int) (*runtimeOwnership, bool, error) {
	endpoint, err := runtimeendpoint.FromPort(publishedPort)
	if err != nil {
		return nil, false, err
	}
	return acquireRuntimeOwnershipWithListener(root, func() (net.Listener, runtimeendpoint.Endpoint, error) {
		listener, listenErr := net.Listen("tcp4", "0.0.0.0:"+strconv.Itoa(containerAPIListenPort))
		return listener, endpoint, listenErr
	})
}

func acquireRuntimeOwnershipWithListener(
	root string,
	listen func() (net.Listener, runtimeendpoint.Endpoint, error),
) (*runtimeOwnership, bool, error) {
	guard, err := instanceguard.Acquire(filepath.Join(root, "replicaro.instance"))
	if err != nil {
		if errors.Is(err, instanceguard.ErrAlreadyRunning) {
			ctx, cancel := context.WithTimeout(context.Background(), startupPublicationActivationWindow)
			defer cancel()
			publicationWait := startupPublicationActivationWindow - activationRequestReserve
			activationErr := rendezvous.Activate(ctx, filepath.Join(root, rendezvous.FileName), publicationWait)
			if activationErr == nil {
				return nil, true, nil
			}
			return nil, false, errors.Join(err, activationErr)
		}
		return nil, false, err
	}
	ownership := &runtimeOwnership{
		guard: guard, rendezvousPath: filepath.Join(root, rendezvous.FileName),
	}
	fail := func(err error) (*runtimeOwnership, bool, error) {
		return nil, false, errors.Join(err, ownership.Close())
	}
	if err := rendezvous.PrepareOwner(ownership.rendezvousPath); err != nil {
		return fail(err)
	}
	ownership.listener, ownership.endpoint, err = listen()
	if err != nil {
		return fail(fmt.Errorf("listen for API: %w", err))
	}
	ownership.record, err = rendezvous.NewRecord(ownership.endpoint)
	if err != nil {
		return fail(err)
	}
	return ownership, false, nil
}

// ServeAndPublish starts the configured handler, proves that Serve has entered
// the owned listener's accept loop, and only then publishes the activation
// record. Readiness deliberately does not require a synthetic client socket.
func (ownership *runtimeOwnership) ServeAndPublish(server *http.Server) (<-chan error, error) {
	if ownership == nil || ownership.listener == nil || server == nil {
		return nil, fmt.Errorf("runtime ownership and HTTP server are required")
	}
	readyListener := &readinessListener{Listener: ownership.listener, ready: make(chan struct{})}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(readyListener) }()
	select {
	case <-readyListener.ready:
	case serveErr := <-serveDone:
		return nil, fmt.Errorf("API server stopped before readiness: %w", serveErr)
	case <-time.After(time.Second):
		_ = server.Close()
		<-serveDone
		return nil, fmt.Errorf("API accept-loop readiness timed out")
	}
	select {
	case serveErr := <-serveDone:
		return nil, fmt.Errorf("API server stopped after readiness: %w", serveErr)
	default:
	}
	if err := rendezvous.Publish(ownership.rendezvousPath, ownership.record); err != nil {
		_ = server.Close()
		<-serveDone
		return nil, fmt.Errorf("publish runtime rendezvous: %w", err)
	}
	ownership.published = true
	return serveDone, nil
}

func (ownership *runtimeOwnership) Close() error {
	if ownership == nil {
		return nil
	}
	ownership.closeOnce.Do(func() {
		var errs []error
		if ownership.published {
			errs = append(errs, rendezvous.RemoveIfNonce(ownership.rendezvousPath, ownership.record.Nonce))
		}
		if ownership.listener != nil {
			if err := ownership.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				errs = append(errs, err)
			}
		}
		if ownership.guard != nil {
			errs = append(errs, ownership.guard.Close())
		}
		ownership.closeErr = errors.Join(errs...)
	})
	return ownership.closeErr
}

type shutdownHooks struct {
	rcloneAuth                                                           func() error
	http, scheduler, runner, metadata, statistics, profiles, kopiaPolicy func(context.Context) error
	kopiaReconnect, resticRecovery                                       func(context.Context) error
	updater, notifications, database                                     func(context.Context) error
}

var recoverKopiaFilesystemReconnects = runner.RecoverKopiaFilesystemReconnects

// startKopiaFilesystemReconnectRecovery makes native reconnect recovery an
// application-lifecycle worker. It is started only after HTTP publication;
// cancellation waits for the one admitted pass without clearing its durable
// intents, which remain available to operation-time or later startup retry.
func startKopiaFilesystemReconnectRecovery(parent context.Context, db *sql.DB) func(context.Context) error {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan error, 1)
	go func() {
		err := recoverKopiaFilesystemReconnects(ctx, db)
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("Kopia filesystem reconnect background recovery failed: %v", err)
			_ = database.LogError(db, "Kopia filesystem reconnect background recovery failed: "+err.Error())
		}
		done <- err
	}()
	return func(stopCtx context.Context) error {
		cancel()
		select {
		case err := <-done:
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		case <-stopCtx.Done():
			return stopCtx.Err()
		}
	}
}

var admitStartupStuckRepository = func(ctx context.Context, db *sql.DB, repo models.Repository) (models.Repository, error) {
	return repositoryadmission.AdmitControlPlaneReadUnderLock(
		ctx, db, repo,
		func(ctx context.Context, candidate models.Repository) error {
			store := (vaultprofile.Store{Repository: candidate}).
				WithRepositoryAvailabilityCheck(storageavailability.RequireRepositoryAvailable)
			if err := store.AssertRootIdentity(ctx); err != nil {
				return err
			}
			return store.ForProfile(candidate.ProfileUUID).
				AssertAttachment(ctx, candidate.ClientUUID, candidate.AttachmentGeneration)
		},
	)
}

var probeStartupStuckResticIdentity = func(ctx context.Context, repo models.Repository) error {
	return engines.ProbeRepositoryPassword(ctx, repo, repo.Passphrase)
}

// startupResticRecovery owns only this start's TEMP-table handoff. It reserves
// the existing UUID locks before producers or HTTP are admitted, but does no
// native work until publication. A stalled vault therefore cannot hide the UI
// or block unrelated vaults. This is not Kopia's durable reconnect lifecycle:
// cancellation releases these run-local reservations without replay or retries.
type startupResticRecovery struct {
	published chan struct{}
	done      chan struct{}
	cancel    context.CancelFunc
	err       error // read only after done closes
}

func prepareStartupResticRecovery(parent context.Context, db *sql.DB) (*startupResticRecovery, error) {
	repositories, err := database.TakeStartupStuckResticRepositories(db)
	if err != nil {
		return nil, fmt.Errorf("load stuck Restic vault recovery: %w", err)
	}
	ctx, cancel := context.WithCancel(parent)
	recovery := &startupResticRecovery{published: make(chan struct{}), done: make(chan struct{}), cancel: cancel}
	unlocks := make([]func(), 0, len(repositories))
	release := func() {
		for _, unlock := range unlocks {
			if unlock != nil {
				unlock()
			}
		}
	}
	for _, repo := range repositories {
		// No producer has started yet. Unexpected contention is a startup error,
		// not another unbounded pre-publication wait or a new lock/fence registry.
		unlock, acquired, lockErr := vaultlock.YieldLowPriorityAndTryExclusiveContext(ctx, repo.ID)
		if lockErr != nil || !acquired {
			cancel()
			release()
			return nil, errors.Join(fmt.Errorf("reserve startup Restic recovery for vault %s", repo.ID), lockErr)
		}
		unlocks = append(unlocks, unlock)
	}
	go func() {
		defer close(recovery.done)
		defer release()
		select {
		case <-ctx.Done():
			recovery.err = ctx.Err()
			return
		case <-recovery.published:
		}
		for index, repo := range repositories {
			if err := ctx.Err(); err != nil {
				recovery.err = err
				return
			}
			err := unlockStartupStuckResticRepository(ctx, db, repo)
			unlocks[index]()
			unlocks[index] = nil
			if err != nil {
				recovery.err = err
				if !errors.Is(err, context.Canceled) {
					log.Printf("Restic startup recovery failed: %v", err)
					_ = database.LogError(db, "Restic startup recovery failed: "+err.Error())
				}
				return
			}
		}
	}()
	return recovery, nil
}

func (recovery *startupResticRecovery) stop(ctx context.Context) error {
	recovery.cancel()
	select {
	case <-recovery.done:
		if errors.Is(recovery.err, context.Canceled) {
			return nil
		}
		return recovery.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func unlockStartupStuckResticRepository(ctx context.Context, db *sql.DB, repo models.Repository) error {
	if passwordErr := database.RequireNoPrecommitVaultPasswordChange(db, repo.ID); passwordErr != nil {
		if errors.Is(passwordErr, database.ErrVaultPasswordChangeRecoveryRequired) {
			log.Printf("native Restic startup auto unlock skipped for vault %s while vault-password recovery is unresolved", repo.ID)
			return nil
		}
		return fmt.Errorf("check vault-password recovery before startup auto unlock: %w", passwordErr)
	}
	// Startup unlock is a recovery operation for a repository that may be
	// impossible to open for ordinary lock-taking work. Pinned Restic's
	// read-only `cat config` probe does not require auto-unlock, so every
	// connector must authenticate and prove the actual native ID independently
	// after protected attachment selection and before native lock mutation.
	admitted, resolveErr := admitStartupStuckRepository(ctx, db, repo)
	if resolveErr == nil {
		repo = admitted
		resolveErr = probeStartupStuckResticIdentity(ctx, repo)
	}
	if resolveErr == nil {
		engine, engineErr := engines.ResolveWithRepositoryAvailabilityCheck(
			repo, storageavailability.RequireRepositoryAvailable,
		)
		resolveErr = engineErr
		if resolveErr == nil {
			_, resolveErr = engines.UnlockResticRepository(ctx, engine, repo)
		}
	}
	if resolveErr != nil {
		log.Printf("native Restic startup auto unlock failed for vault %s: %v", repo.ID, resolveErr)
		_ = database.LogError(db, "Native Restic startup auto unlock failed for vault "+repo.Name+": "+resolveErr.Error())
		return ctx.Err()
	}
	_ = database.LogActivity(db, "Native Restic startup auto unlock completed for vault "+repo.Name)
	return nil
}

func shutdownInOrder(ctx context.Context, hooks shutdownHooks) error {
	var errs []error
	if hooks.http != nil {
		httpErr := hooks.http(ctx)
		errs = append(errs, httpErr)
		if ctxErr := ctx.Err(); ctxErr != nil {
			if !errors.Is(httpErr, ctxErr) {
				errs = append(errs, ctxErr)
			}
			return errors.Join(errs...)
		}
	}
	if hooks.rcloneAuth != nil {
		errs = append(errs, hooks.rcloneAuth())
	}
	if hooks.updater != nil {
		errs = append(errs, hooks.updater(ctx))
		if ctxErr := ctx.Err(); ctxErr != nil {
			return errors.Join(errs...)
		}
	}

	// Admission is closed and active requests are drained before worker
	// lifecycles stop, so no handler can submit work to a coordinator being
	// torn down. The remaining producers quiesce together while shared
	// resources stay open.
	producers := []func(context.Context) error{
		hooks.scheduler, hooks.statistics, hooks.runner, hooks.metadata, hooks.profiles, hooks.kopiaPolicy,
		hooks.kopiaReconnect, hooks.resticRecovery,
	}
	done := make(chan error, len(producers))
	pending := 0
	for _, stop := range producers {
		if stop == nil {
			continue
		}
		pending++
		go func(stop func(context.Context) error) { done <- stop(ctx) }(stop)
	}
	for pending > 0 {
		select {
		case err := <-done:
			errs = append(errs, err)
			pending--
		case <-ctx.Done():
			errs = append(errs, ctx.Err())
			return errors.Join(errs...)
		}
	}

	for _, stop := range []func(context.Context) error{
		hooks.notifications, hooks.database,
	} {
		// A stop hook can return the expired context before its worker has
		// drained. When both channels are ready, select does not prioritize
		// ctx.Done over that result. Recheck before each resource stage so a
		// timed-out producer join (or notification drain) leaves shared resources
		// open instead of allowing a still-running worker to use a closed DB.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return errors.Join(append(errs, ctxErr)...)
		}
		if stop == nil {
			continue
		}
		finished := make(chan error, 1)
		go func(stop func(context.Context) error) { finished <- stop(ctx) }(stop)
		select {
		case err := <-finished:
			errs = append(errs, err)
		case <-ctx.Done():
			errs = append(errs, ctx.Err())
			return errors.Join(errs...)
		}
	}
	return errors.Join(errs...)
}

func shutdownHTTPServer(ctx context.Context, server *http.Server) error {
	if err := server.Shutdown(ctx); err != nil {
		return errors.Join(err, server.Close())
	}
	return nil
}

func normalizeHTTPServeError(err error) error {
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func startupRollbackHooks(
	db, readDB *sql.DB,
	stopMetadata func(context.Context) error,
	stopKopiaPolicy func(context.Context) error,
) shutdownHooks {
	closeDatabases := func(ctx context.Context) error {
		var readErr error
		if readDB != nil {
			readErr = readDB.Close()
		}
		return errors.Join(database.CloseMetadataCaches(ctx), readErr, db.Close())
	}
	return shutdownHooks{
		runner:      func(ctx context.Context) error { return runner.ShutdownContext(ctx, db) },
		metadata:    stopMetadata,
		kopiaPolicy: stopKopiaPolicy,
		database:    closeDatabases,
	}
}

func run() (result error) {
	options, err := parseStartupOptions(os.Args[1:])
	if err != nil {
		return err
	}
	if options.containerPublishedPort != nil {
		if err := requireContainerRuntime(); err != nil {
			return err
		}
	}
	root, err := appdata.Directory("")
	if err != nil {
		return err
	}
	var runtimeOwner *runtimeOwnership
	var activatedExisting bool
	if options.containerPublishedPort != nil {
		runtimeOwner, activatedExisting, err = acquireContainerRuntimeOwnership(root, *options.containerPublishedPort)
	} else {
		var explicitPort []int
		if options.loopbackPort != nil {
			explicitPort = append(explicitPort, *options.loopbackPort)
		}
		runtimeOwner, activatedExisting, err = acquireRuntimeOwnership(root, explicitPort...)
	}
	if err != nil {
		return err
	}
	if activatedExisting {
		return nil
	}
	defer func() {
		result = errors.Join(result, runtimeOwner.Close())
	}()

	endpoint := runtimeOwner.endpoint
	if err := desktop.ConfigureEndpoint(endpoint); err != nil {
		return err
	}
	record := runtimeOwner.record
	storageRunner, err := storagehelper.NewRunner(storageavailability.DefaultObservationTimeout, 4)
	if err != nil {
		return err
	}
	storageavailability.Configure(storageRunner, storageavailability.DefaultObservationTimeout)

	log.Printf("Replicaro starting on %s", endpoint.String())
	if err := command.CleanupCapturedOutput(); err != nil {
		log.Printf("Replicaro could not remove every abandoned command-output file: %v", err)
	}
	if err := operationlog.CleanupStagedOutput(); err != nil {
		log.Printf("Replicaro could not remove every abandoned operation-log staging file: %v", err)
	}
	if err := vaultprofile.CleanupStaging(); err != nil {
		return fmt.Errorf("clean stale recovery-profile staging files: %w", err)
	}
	databasePath, err := appdata.File("replicaro.db")
	if err != nil {
		return err
	}

	db, err := database.Open(databasePath)

	if err != nil {
		return err
	}
	if err := database.Migrate(db); err != nil {
		_ = db.Close()
		return err
	}
	if err := artifactrecovery.Reconcile(db); err != nil {
		_ = db.Close()
		return fmt.Errorf("recover local engine artifacts: %w", err)
	}
	if err := engines.CleanupRcloneSessions(); err != nil {
		_ = db.Close()
		return fmt.Errorf("clean stale rclone sessions: %w", err)
	}
	rcloneConfigOwners, err := database.RcloneVaultConfigOwnerIDs(db)
	if err != nil {
		_ = db.Close()
		return fmt.Errorf("classify native rclone vault configs: %w", err)
	}
	if err := engines.ReconcileRcloneVaultConfigs(rcloneConfigOwners); err != nil {
		_ = db.Close()
		return fmt.Errorf("reconcile native rclone vault configs: %w", err)
	}
	passwordRecoveryCtx, cancelPasswordRecovery := context.WithTimeout(context.Background(), 30*time.Minute)
	if err := runner.RecoverVaultPasswordChangesOnce(passwordRecoveryCtx, db); err != nil {
		cancelPasswordRecovery()
		_ = db.Close()
		return fmt.Errorf("recover interrupted vault-password changes: %w", err)
	}
	cancelPasswordRecovery()
	settings, err := database.GetSettings(db)
	if err != nil {
		_ = db.Close()
		return err
	}
	if err := desktop.ApplySettings(db); err != nil {
		log.Printf("desktop settings: %v", err)
	}
	updater := appupdate.New(db)
	if err := updater.NormalizeStartup(); err != nil {
		_ = db.Close()
		return fmt.Errorf("normalize application update state: %w", err)
	}
	readDB, err := database.OpenReadPool(databasePath)
	if err != nil {
		_ = db.Close()
		return fmt.Errorf("open database read pool: %w", err)
	}

	// One explicitly owned ephemeral runtime is shared by every operation
	// producer and the loopback API; restart intentionally discards its state.
	applicationCtx, cancelApplication := context.WithCancel(context.Background())
	defer cancelApplication()
	resticRecovery, err := prepareStartupResticRecovery(applicationCtx, db)
	if err != nil {
		return errors.Join(err, readDB.Close(), db.Close())
	}
	operationRuntime := operationruntime.New()
	runner.ConfigureWithRuntime(db, settings.MaxConcurrentJobRuns, operationRuntime)
	updateCtx, cancelUpdate := context.WithCancel(context.Background())
	defer cancelUpdate()
	stopKopiaPolicy := kopiapolicy.StartContext(applicationCtx, db)
	if err := kopiapolicy.QueueAll(db); err != nil {
		rollbackCtx, cancelRollback := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelRollback()
		rollbackHooks := startupRollbackHooks(db, readDB, nil, stopKopiaPolicy)
		rollbackHooks.resticRecovery = resticRecovery.stop
		rollbackErr := shutdownInOrder(rollbackCtx, rollbackHooks)
		return errors.Join(fmt.Errorf("prepare startup Kopia policy verification: %w", err), rollbackErr)
	}
	stopMetadata := metadata.StartContext(db)
	stopStatistics := vaultstatistics.StartContext(applicationCtx, db)

	// Establish every request-admitted worker lifecycle before HTTP publication.
	var server *http.Server
	var closeRcloneAuth func() error
	if options.containerPublishedPort != nil {
		server, closeRcloneAuth = api.NewContainerServerAtWithReaderAndRuntime(
			db, readDB, endpoint, record, desktop.RestoreWindow,
			containerRcloneAuthRelayPort, operationRuntime, updater,
		)
	} else {
		server, closeRcloneAuth = api.NewServerAtWithReaderAndRuntime(
			db, readDB, endpoint, record, desktop.RestoreWindow,
			options.rcloneAuthNoOpenBrowser, operationRuntime, updater,
		)
	}
	defer func() {
		result = errors.Join(result, closeRcloneAuth())
	}()
	serveDone, err := runtimeOwner.ServeAndPublish(server)
	if err != nil {
		rollbackCtx, cancelRollback := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelRollback()
		rollbackHooks := startupRollbackHooks(db, readDB, stopMetadata, stopKopiaPolicy)
		rollbackHooks.resticRecovery = resticRecovery.stop
		rollbackHooks.statistics = stopStatistics
		rollbackHooks.rcloneAuth = closeRcloneAuth
		rollbackErr := shutdownInOrder(
			rollbackCtx,
			rollbackHooks,
		)
		return errors.Join(err, rollbackErr)
	}
	// Native recovery may wait on unavailable storage. Start the reserved
	// Restic pass and existing Kopia reconnect recovery after API/UI publication.
	close(resticRecovery.published)
	stopKopiaReconnect := startKopiaFilesystemReconnectRecovery(applicationCtx, db)
	stopScheduler := scheduler.StartContextWithRuntime(db, operationRuntime)
	stopProfiles := profilesync.StartContext(db)
	updater.StartAutomatic(updateCtx)
	serveErr := desktop.Run(func() error { return <-serveDone })

	// Close HTTP admission and drain/cancel request contexts before any handler
	// can recreate or submit work to a worker coordinator being torn down.
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	// A request may be waiting behind startup recovery's reserved vault lock.
	// Begin canceling its native child before HTTP drain, then join the worker
	// with the other producers before closing SQLite. Cancellation has no total
	// native deadline during normal operation and does not replay interrupted work.
	resticRecovery.cancel()
	cancelUpdate()
	shutdownErr := shutdownInOrder(shutdownCtx, shutdownHooks{
		http:           func(ctx context.Context) error { return shutdownHTTPServer(ctx, server) },
		rcloneAuth:     closeRcloneAuth,
		updater:        updater.Wait,
		scheduler:      stopScheduler,
		runner:         func(ctx context.Context) error { return runner.ShutdownContext(ctx, db) },
		metadata:       stopMetadata,
		statistics:     stopStatistics,
		profiles:       stopProfiles,
		kopiaPolicy:    stopKopiaPolicy,
		kopiaReconnect: stopKopiaReconnect,
		resticRecovery: resticRecovery.stop,
		notifications:  func(ctx context.Context) error { return notifications.Wait(ctx) },
		database: func(ctx context.Context) error {
			return errors.Join(database.CloseMetadataCaches(ctx), readDB.Close(), db.Close())
		},
	})
	serveErr = normalizeHTTPServeError(serveErr)
	result = errors.Join(serveErr, shutdownErr)
	return result
}
