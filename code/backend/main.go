package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
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
	"github.com/local/replicaro/locale"
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
	// secondLaunchPublicationWait is how long a launch that lost the instance
	// guard keeps looking for the owner's rendezvous record. The owner takes
	// the guard before it opens the database and runs startup recovery, and
	// only publishes once the API is accepting connections.
	secondLaunchPublicationWait  = 12 * time.Second
	containerAPIListenPort       = 9460
	containerRcloneAuthRelayPort = 53683
	containerPackageMarkerPath   = "/opt/replicaro/.container-package"
)

// openRunningWebUI opens the running instance's URL for a second launch. Tests
// replace it so they don't start a real browser.
var openRunningWebUI = desktop.OpenWebUI

func main() {
	err := run()
	os.Exit(finishRun(err, serviceStarted.Load(), headlessStart, stderrCaptured(), startupLogPath, showStartupFailure))
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
	// noRoot asks the container package to switch from root to noRootUser,
	// or to defaultNoRootUser when no user was given.
	noRoot     bool
	noRootUser *containerUser
	// lanOrigins lives only in process memory. It is not persisted, has no
	// environment-variable or settings alias, and is never handed to desktop,
	// notification, rendezvous, or rclone code, which all stay on loopback.
	lanOrigins runtimeendpoint.LANOrigins
	// unknownOptions holds the names of unrecognized "--" arguments, without
	// any "=value" part, so run() can warn about them. A mistyped option can
	// still carry a password or token in its value, so the value is dropped
	// here and never reaches the log.
	unknownOptions []string
	// repairInstallation is set by --repair-replicaro. It replaces a damaged
	// installation ID without asking; an intact one is left alone.
	repairInstallation bool
	// noStartupBrowser is set by --no-startup-browser. It applies to this
	// process only and is never saved, so a later start without it opens the
	// web interface as usual.
	noStartupBrowser bool
	// help is set by --help or -h. When it is set, every other field is left
	// empty and run() only prints startupUsage.
	help bool
}

// startupUsage is printed for --help and -h. Keep it in step with the cases in
// parseStartupOptions, and keep lines within 80 columns so it reads well in a
// plain terminal.
const startupUsage = `Usage: replicaro [options]

Options:
  --loopback-port=<port>
      Use exactly this loopback port, and fail to start if it is busy.
      Default: 9460, or any free port.
  --container-published-port=<port>
      Docker package only: the host port the UI is published on.
  --lan-origin=<origin>
      Accept browsers coming through your HTTPS reverse proxy at this
      address. Repeatable. Needs one of the port options above. On Linux
      outside the Docker package, this also keeps Start at login on.
  --rclone-auth-no-open-browser
      Don't open a browser on this machine for cloud sign-in, for headless
      installs. The sign-in link shows in Replicaro either way. On Linux
      outside the Docker package, this also keeps Start at login on.
  --headless
      Another name for --rclone-auth-no-open-browser.
  --no-root[=<uid>:<gid>]
      Docker package only: run as this user and group instead of root.
      Default: 1000:1000.
  --repair-replicaro
      If Replicaro reports that it has been damaged, repair it and start.
      You'll need to reconnect your vaults afterwards. Does nothing when
      Replicaro isn't damaged.
  --no-startup-browser
      Don't open Replicaro in your browser on start. If it's already
      running, just exit. The tray or menu bar icon still opens it. Start
      at login uses this.
  --help, -h
      Show this help and exit.
`

func parseStartupOptions(arguments []string) (startupOptions, error) {
	// Help wins over everything else on the command line, including options
	// that would otherwise be rejected, so asking for help always gets an
	// answer. Repeating it is harmless.
	for _, argument := range arguments {
		if argument == "--help" || argument == "-h" {
			return startupOptions{help: true}, nil
		}
	}
	var options startupOptions
	// --headless is an alias of --rclone-auth-no-open-browser, and both set the
	// same field. Each spelling is tracked on its own so passing both is
	// accepted while repeating either one is still rejected.
	var sawNoOpenBrowser, sawHeadless, sawNoRoot bool
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
			if sawNoOpenBrowser {
				return startupOptions{}, fmt.Errorf("--rclone-auth-no-open-browser may be supplied only once")
			}
			sawNoOpenBrowser = true
			options.rcloneAuthNoOpenBrowser = true
		case strings.HasPrefix(argument, "--rclone-auth-no-open-browser="):
			return startupOptions{}, fmt.Errorf("--rclone-auth-no-open-browser does not accept a value")
		case argument == "--headless":
			// Only an easier name to put in a service drop-in. Keep it a pure
			// alias: anything headless-specific belongs to
			// rcloneAuthNoOpenBrowser, so both spellings always behave the same.
			if sawHeadless {
				return startupOptions{}, fmt.Errorf("--headless may be supplied only once")
			}
			sawHeadless = true
			options.rcloneAuthNoOpenBrowser = true
		case strings.HasPrefix(argument, "--headless="):
			return startupOptions{}, fmt.Errorf("--headless does not accept a value")
		case strings.HasPrefix(argument, "--lan-origin="):
			// Unlike the other options this one repeats, so one install can be
			// reached by, say, a LAN hostname and a VPN hostname. There are no
			// numbered or comma-separated forms: a "--lan-origin2" falls through
			// to the unknown-option warning below and otherwise does nothing.
			origin, err := runtimeendpoint.ParseLANOrigin(strings.TrimPrefix(argument, "--lan-origin="))
			if err != nil {
				return startupOptions{}, fmt.Errorf("--lan-origin: %w", err)
			}
			options.lanOrigins = append(options.lanOrigins, origin)
		case argument == "--lan-origin":
			return startupOptions{}, fmt.Errorf("--lan-origin must use exactly --lan-origin=<origin>")
		case argument == "--no-root", strings.HasPrefix(argument, "--no-root="):
			if sawNoRoot {
				return startupOptions{}, fmt.Errorf("--no-root may be supplied only once")
			}
			sawNoRoot = true
			options.noRoot = true
			if value, ok := strings.CutPrefix(argument, "--no-root="); ok {
				user, err := parseContainerUser(value)
				if err != nil {
					return startupOptions{}, fmt.Errorf("--no-root: %w", err)
				}
				options.noRootUser = &user
			}
		case argument == "--repair-replicaro":
			if options.repairInstallation {
				return startupOptions{}, fmt.Errorf("--repair-replicaro may be supplied only once")
			}
			options.repairInstallation = true
		case strings.HasPrefix(argument, "--repair-replicaro="):
			return startupOptions{}, fmt.Errorf("--repair-replicaro does not accept a value")
		case argument == "--no-startup-browser":
			if options.noStartupBrowser {
				return startupOptions{}, fmt.Errorf("--no-startup-browser may be supplied only once")
			}
			options.noStartupBrowser = true
		case strings.HasPrefix(argument, "--no-startup-browser="):
			return startupOptions{}, fmt.Errorf("--no-startup-browser does not accept a value")
		case strings.HasPrefix(argument, "--help="), strings.HasPrefix(argument, "-h="):
			// Handled like the other options that take no value, which reject
			// one (see --headless=), rather than showing help or ignoring it.
			// That includes -h=, even though other single-dash arguments are
			// ignored: -h is a known option, so a value on it is a mistake worth
			// reporting.
			name, _, _ := strings.Cut(argument, "=")
			return startupOptions{}, fmt.Errorf("%s does not accept a value", name)
		default:
			// Unrecognized arguments are ignored, but "--" ones are recorded so
			// run() can warn about a typo such as "--headles" that would
			// otherwise leave the intended option quietly unset. Do not make
			// this fail startup: an older build started from a drop-in that
			// passes a newer option must keep working, and launchers may add
			// arguments of their own. Arguments without "--" stay silent.
			if strings.HasPrefix(argument, "--") {
				name, _, _ := strings.Cut(argument, "=")
				options.unknownOptions = append(options.unknownOptions, name)
			}
		}
	}
	// The checks below fail on a combination of options, and a mistyped name
	// (say "--loopback-prt") is the most likely reason the combination looks
	// wrong. Keep the unknown names on these errors so run() can still warn
	// about the typo next to the error it caused.
	if options.loopbackPort != nil && options.containerPublishedPort != nil {
		return startupOptions{unknownOptions: options.unknownOptions}, fmt.Errorf("--loopback-port and --container-published-port are mutually exclusive")
	}
	// The reverse proxy is configured with a fixed upstream port. Without an
	// explicit port a host install falls back to a random loopback port when
	// 9460 is busy, and the proxy would quietly point at nothing (or at
	// something else), so require the port to be pinned.
	if len(options.lanOrigins) > 0 && options.loopbackPort == nil && options.containerPublishedPort == nil {
		return startupOptions{unknownOptions: options.unknownOptions}, fmt.Errorf("--lan-origin requires --loopback-port=<port> or --container-published-port=<port>")
	}
	// Only the container package starts as root and can switch users. A
	// desktop or service install already runs as the user who started it.
	if options.noRoot && options.containerPublishedPort == nil {
		return startupOptions{unknownOptions: options.unknownOptions}, fmt.Errorf("--no-root requires --container-published-port=<port>")
	}
	return options, nil
}

// headlessServiceMode is Linux with at least one --lan-origin or with
// --rclone-auth-no-open-browser (also spelled --headless), outside the
// container package. Replicaro cannot tell from the environment that it runs
// as a headless user service: desktop Start at login uses the same systemd
// user unit, and desktop services often have no display variable. These
// options are the only reliable signal, and in that layout Start at login is
// what keeps replicaro.service enabled (see keepStartAtLoginOn). The container
// package is excluded because it never manages a login service. On other
// platforms Start at login is a normal desktop preference with no headless
// service behind it, and these options don't change how it works.
func headlessServiceMode(goos string, options startupOptions) bool {
	return goos == "linux" && (len(options.lanOrigins) > 0 || options.rcloneAuthNoOpenBrowser) &&
		options.containerPublishedPort == nil
}

// keepStartAtLoginOn saves Start at login as on when it is off, before the
// desktop settings are applied. On Linux, applying Start at login off runs
// `systemctl --user disable replicaro.service` on every start, which would
// leave a headless install unable to come back after a reboot. The settings API
// enforces the same rule for later saves. Removing the options that select
// headlessServiceMode leaves the setting on, and the user can then turn it off
// as usual.
//
// This runs before HTTP is published and before any background worker starts,
// so nothing else can be writing settings at the same time.
func keepStartAtLoginOn(db *sql.DB, settings *models.Settings) error {
	if settings.StartAtLogin || settings.StartWithWindows {
		return nil
	}
	settings.StartAtLogin = true
	settings.StartWithWindows = true
	return database.SaveSettings(db, *settings)
}

const damagedInstallationText = "Replicaro has been damaged, so Replicaro can't start. " +
	"Your backed up data is safe and isn't affected. " +
	"Replicaro can repair the damage on its own with your permission. " +
	"After the repair, you'll need to reconnect your vaults."

var (
	// errRepairNeeded stops a start that can't ask about the repair: Linux,
	// Docker, a terminal or redirected start, or a dialog that couldn't be
	// shown. finishRun shows its text as it is.
	errRepairNeeded = errors.New(damagedInstallationText + " To repair it, start Replicaro once with --repair-replicaro.")
	// errRepairDeclined stops a start where the user answered No. The
	// question comes back on the next start, so finishRun shows nothing more.
	errRepairDeclined = errors.New("Replicaro has been damaged and the repair was declined, so Replicaro didn't start")
	// confirmStartupRepair asks the repair question in a dialog. Tests
	// replace it.
	confirmStartupRepair = desktop.ConfirmStartupRepair
)

// startupRepairQuestion returns the dialog that asks whether to repair a
// damaged installation, or nil where the start can't ask. Only Windows and
// macOS show a dialog, and only where they would show the startup failure
// notice (see stderrCaptured): a terminal or scripted start would wait on a
// dialog nobody sees. Linux shows notifications, which can't take an answer.
func startupRepairQuestion(goos string, stderrCaptured bool) func(string) (answered, repair bool) {
	if (goos != "windows" && goos != "darwin") || stderrCaptured {
		return nil
	}
	return confirmStartupRepair
}

// settleInstallationID makes sure the stored installation ID can be used
// before startup goes on. A damaged ID is replaced only with the user's
// go-ahead, given by --repair-replicaro (repair) or by answering Yes in the
// dialog ask shows, because a new ID means every vault has to be reconnected.
// A read error is returned as a failed start and never repaired, so the next
// start tries again.
func settleInstallationID(db *sql.DB, repair bool, ask func(string) (answered, repair bool)) error {
	_, err := database.InstallationID(db)
	if err == nil {
		return nil
	}
	if !errors.Is(err, database.ErrInstallationIDDamaged) {
		return fmt.Errorf("read the installation ID: %w", err)
	}
	if !repair {
		if ask == nil {
			return errRepairNeeded
		}
		answered, yes := ask(damagedInstallationText + " Repair Replicaro now?")
		if !answered {
			return errRepairNeeded
		}
		if !yes {
			return errRepairDeclined
		}
	}
	if _, err := database.RepairInstallationID(db); err != nil {
		return fmt.Errorf("repair the installation ID: %w", err)
	}
	log.Print("Replicaro replaced its damaged installation ID; vaults need to be reconnected")
	return nil
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
// the instance guard, stale rendezvous cleanup, and loopback listener
// selection. A losing instance opens the running owner's web interface
// (unless it was started with --no-startup-browser) and never creates a
// listener; the returned bool reports that hand-off.
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
			if openErr := openRunningInstance(root); openErr != nil {
				return nil, false, errors.Join(err, openErr)
			}
			return nil, true, nil
		}
		return nil, false, err
	}
	ownership := &runtimeOwnership{
		guard: guard, rendezvousPath: filepath.Join(root, rendezvous.FileName),
	}
	fail := func(err error) (*runtimeOwnership, bool, error) {
		return nil, false, errors.Join(err, ownership.Close())
	}
	discarded, err := rendezvous.PrepareOwner(ownership.rendezvousPath)
	if err != nil {
		return fail(err)
	}
	if discarded != nil {
		log.Printf("Replicaro removed a leftover rendezvous record it could not read (%s): %v", ownership.rendezvousPath, discarded)
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

// openRunningInstance is the whole second-launch path: read the running
// owner's URL from its rendezvous record, open it in the browser from this
// process, and let the caller exit. With --no-startup-browser it reads and
// opens nothing and the caller just exits. There's deliberately no request to
// the running instance. Only local programs can read the owner-only record,
// and Replicaro doesn't defend against those; web pages can't read it, and
// the API keeps them out with its Host, Origin, Fetch-Metadata, JSON and
// installation-UUID checks. Opening the URL from here also lets the browser
// come forward with the focus the user just gave this launch.
//
// Don't add an activation endpoint or token back here without a product
// decision: it would only guard against local programs, and tools that read
// the record still find the same fields.
func openRunningInstance(root string) error {
	// With --no-startup-browser this launch was asked not to open the web
	// interface, and the running instance already serves, so there's nothing
	// left to do. It exits at once instead of waiting up to
	// secondLaunchPublicationWait for the record it wouldn't use. This keeps a
	// scheduled task, login item or script that starts Replicaro again while
	// it runs from opening a browser.
	if desktop.NoStartupBrowser() {
		log.Print("Replicaro is already running; not opening the web interface because of --no-startup-browser")
		return nil
	}
	target, err := rendezvous.WaitForRunningURL(filepath.Join(root, rendezvous.FileName), secondLaunchPublicationWait)
	if err != nil {
		return err
	}
	webURL := target + "/"
	log.Printf("Replicaro is already running at %s", webURL)
	if err := openRunningWebUI(webURL); err != nil {
		return fmt.Errorf("open the running Replicaro web interface: %w", err)
	}
	return nil
}

// ServeAndPublish starts the configured handler, proves that Serve has entered
// the owned listener's accept loop, and only then publishes the rendezvous
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

// Unpublish removes the rendezvous record while this process still holds the
// instance guard. run calls it as the very first shutdown step, before the
// HTTP drain and the worker stops, which together can take several seconds.
// A launch arriving in that window finds no record, waits for one, and fails
// with the usual "already running" error (or opens the next owner if one
// starts in time), instead of reading a record whose PID is still alive and
// opening a listener that has already closed. Close calls it again; once the
// record is gone that's a no-op.
//
// A launch that read the record a moment before this runs can still open the
// URL just after the listener closes. That gap is milliseconds wide and only
// costs a browser tab that doesn't load, so there's no connection probe.
//
// Don't move this back into Close alone: Close runs after the whole shutdown.
func (ownership *runtimeOwnership) Unpublish() error {
	if ownership == nil || !ownership.published {
		return nil
	}
	if err := rendezvous.Remove(ownership.rendezvousPath); err != nil {
		return err
	}
	ownership.published = false
	return nil
}

func (ownership *runtimeOwnership) Close() error {
	if ownership == nil {
		return nil
	}
	ownership.closeOnce.Do(func() {
		var errs []error
		errs = append(errs, ownership.Unpublish())
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
	kopiaReconnect, resticRecovery, passwordRecovery                     func(context.Context) error
	// operations joins the request-started background operations (restore,
	// manual check and maintenance, snapshot and job deletion): it waits for
	// each to save its final state. Their cancel happens earlier, before the
	// HTTP drain; see shutdownWithTrackedOperations.
	operations                       func(context.Context) error
	updater, notifications, database func(context.Context) error
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
			store := (vaultprofile.Store{Repository: candidate})
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
	// Startup password recovery already holds a vault whose change is still
	// before commit, and the unlock below skips such a vault anyway. Leave it
	// out instead of failing startup on that lock. Skipping it is safe: until
	// the change commits, every operation kind that selects a vault for this
	// unlock is refused on it, the backups and checks that run once it ends
	// still run their own Restic unlock first, and Migrate has already reset
	// the statuses that selected the vault.
	repositories = slices.DeleteFunc(repositories, func(repo models.Repository) bool {
		if !runner.StartupPasswordRecoveryRunning(repo.ID) {
			return false
		}
		log.Printf("native Restic startup auto unlock skipped for vault %s while vault-password recovery is unresolved", repo.ID)
		return true
	})
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
		// No producer has started yet, so contention here is unexpected: fail
		// startup instead of waiting with no bound before publication. Don't add
		// a separate lock/fence registry to work around it.
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
		engine, engineErr := engines.Resolve(repo)
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
		hooks.kopiaReconnect, hooks.resticRecovery, hooks.passwordRecovery, hooks.operations,
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

// shutdownWithTrackedOperations cancels the request-started background
// operations before anything else and then runs shutdownInOrder with their
// join as one of the producers. The cancel has to come before the HTTP drain:
// a request can be waiting for a vault lock that one of these workers holds
// (for example a vault ownership read behind a running restore). If the
// workers were only cancelled in the producer step, that request would keep
// the drain busy until the shutdown deadline, shutdownInOrder would return
// early, and the worker would never be cancelled or record its interrupted
// state. Startup Restic recovery is cancelled early in run for the same reason.
func shutdownWithTrackedOperations(ctx context.Context, operations *operationruntime.Manager, hooks shutdownHooks) error {
	operations.BeginStop()
	hooks.operations = operations.Stop
	return shutdownInOrder(ctx, hooks)
}

func shutdownHTTPServer(ctx context.Context, server *http.Server) error {
	if err := server.Shutdown(ctx); err != nil {
		return errors.Join(err, server.Close())
	}
	return nil
}

// httpShutdownHook gives every request on server a base context that is
// cancelled when shutdown begins, and returns the HTTP shutdown step that does
// that cancel right before shutdownHTTPServer. It has to be called before
// Serve, because Serve reads BaseContext once.
//
// http.Server.Shutdown closes the listener and waits for active requests, but
// it never cancels their contexts. Several foreground actions (connecting a
// vault, ownership takeover and the ownership status read, credential
// rotation, cloud sign-in apply, the existing-vault Check) wait on
// r.Context() for a vault lock. When a scheduled backup or another runner
// task holds that lock, such a request would keep the drain busy until the
// shutdown deadline; shutdownInOrder then stops at the HTTP step, so the
// scheduler, runner and workers are never stopped and the database is never
// closed. Cancelling the base context ends those waits at once. A request that
// is still waiting has not changed anything yet. A request already past its
// wait gets the same cancellation a closed browser tab gives it, which
// foreground work has to handle anyway. Background operations a request
// started run on the operation runtime's lifetime, not on the request
// context, so they are unaffected here and are still stopped and joined by
// shutdownWithTrackedOperations.
func httpShutdownHook(server *http.Server) func(context.Context) error {
	requests, cancelRequests := context.WithCancel(context.Background())
	server.BaseContext = func(net.Listener) context.Context { return requests }
	return func(ctx context.Context) error {
		cancelRequests()
		return shutdownHTTPServer(ctx, server)
	}
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

func warnAboutUnknownOptions(names []string) {
	for _, name := range names {
		log.Printf("ignoring unknown option %q", name)
	}
}

// timeZoneWarning returns the startup line for a set TZ that Go couldn't
// load (local is then UTC), or "". Go falls back to UTC without saying so,
// which in Docker means cron schedules quietly run at UTC. Go strips one
// leading ":" and treats an empty value and "UTC" as UTC on purpose, so those
// get no warning. Windows ignores TZ (Go asks Windows for the zone), so it
// never warns there.
func timeZoneWarning(tz string, local *time.Location) string {
	if runtime.GOOS == "windows" {
		return ""
	}
	name := strings.TrimPrefix(tz, ":")
	if name == "" || name == "UTC" || local.String() != "UTC" {
		return ""
	}
	return fmt.Sprintf("Replicaro could not load the time zone TZ=%q and uses UTC instead; schedules run in UTC. Use a zone name such as Europe/Berlin.", tz)
}

func run() (result error) {
	options, err := parseStartupOptions(os.Args[1:])
	// Answer help before any startup work, so asking for it never starts
	// Replicaro, never hands off to (and so never opens) a running instance,
	// and never touches app data, the instance lock, or settings.
	//
	// The Windows build is a GUI program with no console, so there the text is
	// only visible when output is redirected, as in
	// "replicaro.exe --help > help.txt". Attaching to the parent console would
	// not help much: the shell doesn't wait for a GUI program, so the text
	// would land after its next prompt. A write error is ignored for the same
	// reason: with no console there is nowhere to report it.
	if options.help {
		_, _ = io.WriteString(os.Stdout, startupUsage)
		return nil
	}
	// Warn before checking the error: the parser keeps the unknown names when
	// a combination of options is rejected, and the typo is usually the cause.
	// A command line that is accepted gets its warnings once the startup log
	// is open, below, so they're in the file too.
	if err != nil || options.containerPublishedPort != nil {
		warnAboutUnknownOptions(options.unknownOptions)
	}
	if err != nil {
		return err
	}
	headlessStart = headlessServiceMode(runtime.GOOS, options)
	// Set before the instance guard: a second launch decides from it whether
	// to open the running instance's web interface.
	desktop.SetNoStartupBrowser(options.noStartupBrowser)
	if options.containerPublishedPort != nil {
		if err := requireContainerRuntime(); err != nil {
			return err
		}
		// The image has zone files, so in Docker Restic, Kopia and rclone
		// follow TZ like the rest of the container. Desktop builds never get
		// here and keep their engines on the system's time zone.
		command.InheritTimeZone()
		if err := settleContainerUser(options.noRoot, options.noRootUser); err != nil {
			return err
		}
	}
	// The startup log is opened here, after the options are known to be
	// valid: a rejected command line must not create app data. Its error
	// still reaches stderr and the failure notice, just not the file.
	if options.containerPublishedPort == nil {
		startupLogPath = startStartupLog()
		warnAboutUnknownOptions(options.unknownOptions)
	}
	root, err := appdata.Directory("")
	if err != nil {
		return err
	}
	var runtimeOwner *runtimeOwnership
	var openedExisting bool
	if options.containerPublishedPort != nil {
		runtimeOwner, openedExisting, err = acquireContainerRuntimeOwnership(root, *options.containerPublishedPort)
	} else {
		var explicitPort []int
		if options.loopbackPort != nil {
			explicitPort = append(explicitPort, *options.loopbackPort)
		}
		runtimeOwner, openedExisting, err = acquireRuntimeOwnership(root, explicitPort...)
	}
	if err != nil {
		return err
	}
	if openedExisting {
		return nil
	}
	defer func() {
		result = errors.Join(result, runtimeOwner.Close())
	}()

	endpoint := runtimeOwner.endpoint
	if err := desktop.ConfigureEndpoint(endpoint); err != nil {
		return err
	}
	storageRunner, err := storagehelper.NewRunner(storageavailability.DefaultObservationTimeout, 4)
	if err != nil {
		return err
	}
	storageavailability.Configure(storageRunner, storageavailability.DefaultObservationTimeout)

	log.Printf("Replicaro starting on %s", endpoint.String())
	if tz, set := os.LookupEnv("TZ"); set {
		if warning := timeZoneWarning(tz, time.Local); warning != "" {
			log.Print(warning)
		}
	}
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
	// Every vault and the browser's client UUID check use the installation ID,
	// so a damaged one is settled before anything else uses the database.
	if err := settleInstallationID(db, options.repairInstallation, startupRepairQuestion(runtime.GOOS, stderrCaptured())); err != nil {
		_ = db.Close()
		return err
	}
	// Storage support diagnostics (fixed step labels and OS codes only) go to
	// the support-only activity level so they appear in support exports.
	storageavailability.ConfigureSupportLog(func(message string) { _ = database.LogSupport(db, message) })
	if err := database.EnsureLanguageSetting(db); err != nil {
		_ = db.Close()
		return fmt.Errorf("initialize language setting: %w", err)
	}
	if err := artifactrecovery.Reconcile(db); err != nil {
		_ = db.Close()
		return fmt.Errorf("recover local engine artifacts: %w", err)
	}
	if err := cleanUpStartupLeftovers(db); err != nil {
		_ = db.Close()
		return err
	}
	// Vaults with an unfinished password change are reserved here, before any
	// worker or request exists; their recovery runs once the UI is served (see
	// runner.StartupPasswordRecovery). A start that fails before then only
	// releases the reservations.
	passwordRecovery, err := runner.PrepareStartupPasswordRecovery(db, 30*time.Minute)
	if err != nil {
		_ = db.Close()
		return fmt.Errorf("recover interrupted vault-password changes: %w", err)
	}
	defer passwordRecovery.Cancel()
	settings, err := database.GetSettings(db)
	if err != nil {
		_ = db.Close()
		return err
	}
	if headlessServiceMode(runtime.GOOS, options) {
		if err := keepStartAtLoginOn(db, &settings); err != nil {
			_ = db.Close()
			return fmt.Errorf("keep Start at login on for headless service mode: %w", err)
		}
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
		rollbackHooks.passwordRecovery = passwordRecovery.Stop
		rollbackErr := shutdownInOrder(rollbackCtx, rollbackHooks)
		return errors.Join(fmt.Errorf("prepare startup Kopia policy verification: %w", err), rollbackErr)
	}
	stopMetadata := metadata.StartContext(db)
	// The one remote refresh startup begins on its own: vaults whose File
	// History cache is from an older version are indexed again, once.
	metadata.ScheduleOutdatedCacheRebuilds(db)
	stopStatistics := vaultstatistics.StartContext(applicationCtx, db)

	// Establish every request-admitted worker lifecycle before HTTP publication.
	var server *http.Server
	var closeRcloneAuth func() error
	if options.containerPublishedPort != nil {
		server, closeRcloneAuth = api.NewContainerServerAtWithReaderAndRuntime(
			db, readDB, endpoint, options.lanOrigins,
			containerRcloneAuthRelayPort, operationRuntime, updater,
		)
	} else {
		server, closeRcloneAuth = api.NewServerAtWithReaderAndRuntime(
			db, readDB, endpoint, options.lanOrigins,
			options.rcloneAuthNoOpenBrowser, headlessServiceMode(runtime.GOOS, options), operationRuntime, updater,
		)
	}
	defer func() {
		result = errors.Join(result, closeRcloneAuth())
	}()
	shutdownHTTP := httpShutdownHook(server)
	serveDone, err := runtimeOwner.ServeAndPublish(server)
	if err != nil {
		rollbackCtx, cancelRollback := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelRollback()
		rollbackHooks := startupRollbackHooks(db, readDB, stopMetadata, stopKopiaPolicy)
		rollbackHooks.resticRecovery = resticRecovery.stop
		rollbackHooks.passwordRecovery = passwordRecovery.Stop
		rollbackHooks.statistics = stopStatistics
		rollbackHooks.rcloneAuth = closeRcloneAuth
		rollbackErr := shutdownInOrder(
			rollbackCtx,
			rollbackHooks,
		)
		return errors.Join(err, rollbackErr)
	}
	// Native recovery may wait on unavailable storage. Start the reserved
	// Restic pass, the reserved password-change recovery and the existing Kopia
	// reconnect recovery after API/UI publication.
	close(resticRecovery.published)
	passwordRecovery.Start()
	stopKopiaReconnect := startKopiaFilesystemReconnectRecovery(applicationCtx, db)
	stopScheduler := scheduler.StartContextWithRuntime(db, operationRuntime)
	stopProfiles := profilesync.StartContext(db)
	updater.StartAutomatic(updateCtx)
	// The staged shutdown runs once. It normally starts when desktop.Run
	// returns. On Windows at the end of a session, and on macOS for Quit and
	// the end of a session, the OS instead expects the app to finish while
	// its own event loop waits, so desktop.Run starts it from there; on macOS
	// AppKit then exits the process without desktop.Run returning. Otherwise
	// the call after desktop.Run waits for that run and returns its result.
	shutdown := sync.OnceValue(func() error {
		// Withdraw the rendezvous record first, while the guard is still held,
		// so a launch during the rest of shutdown can't open a URL that's about
		// to stop answering. See runtimeOwnership.Unpublish.
		unpublishErr := runtimeOwner.Unpublish()

		// Close HTTP admission and drain/cancel request contexts before any
		// handler can recreate or submit work to a worker coordinator being
		// torn down.
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelShutdown()
		// A request may be waiting behind startup recovery's reserved vault
		// lock. Begin canceling its native child before HTTP drain, then join
		// the worker with the other producers before closing SQLite.
		// Cancellation has no total native deadline during normal operation
		// and does not replay interrupted work.
		resticRecovery.cancel()
		passwordRecovery.Cancel()
		cancelUpdate()
		shutdownErr := shutdownWithTrackedOperations(shutdownCtx, operationRuntime, shutdownHooks{
			http:             shutdownHTTP,
			rcloneAuth:       closeRcloneAuth,
			updater:          updater.Wait,
			scheduler:        stopScheduler,
			runner:           func(ctx context.Context) error { return runner.ShutdownContext(ctx, db) },
			metadata:         stopMetadata,
			statistics:       stopStatistics,
			profiles:         stopProfiles,
			kopiaPolicy:      stopKopiaPolicy,
			kopiaReconnect:   stopKopiaReconnect,
			resticRecovery:   resticRecovery.stop,
			passwordRecovery: passwordRecovery.Stop,
			notifications:    func(ctx context.Context) error { return notifications.Wait(ctx) },
			database: func(ctx context.Context) error {
				return errors.Join(database.CloseMetadataCaches(ctx), readDB.Close(), db.Close())
			},
		})
		// shutdownInOrder returns at the HTTP step when the drain runs out of
		// time, before its rclone sign-in step, and the deferred close in run
		// never runs when the OS ends the process after this shutdown (macOS
		// after a terminate request, Windows after WM_ENDSESSION). So the
		// cloud sign-in sessions are closed here as well, which cancels their
		// rclone work and removes their session folders. When the step above
		// already closed them, this call finds nothing left to close.
		rcloneAuthErr := closeRcloneAuth()
		return errors.Join(unpublishErr, shutdownErr, rcloneAuthErr)
	})
	// The tray icon follows the dashboard's "Active now" list and Issues
	// count. It reads them through the same read pool as the dashboard, once
	// when Run sets up the tray and again after each database write that can
	// change them; without a tray the notice does nothing.
	desktop.WatchDashboard(readDB)
	database.NotifyDashboardChanges(desktop.DashboardChanged)
	// The tray labels, the tray tooltips and the Quit prompt use the saved UI
	// language. The labels and tooltips are read once, when Run sets up the
	// tray; the prompt reads the setting each time it opens.
	desktop.SetText(func() func(key string) string { return savedLanguageLookup(db) })
	serveErr := desktop.Run(func() error {
		serviceStarted.Store(true)
		// Startup password recovery holds its notifications until the desktop
		// integration exists; desktop.Run has set it up by the time it calls
		// this, so a native notification is no longer dropped.
		passwordRecovery.DeliverNotifications()
		return <-serveDone
	}, shutdown)
	serveErr = normalizeHTTPServeError(serveErr)
	result = errors.Join(serveErr, shutdown())
	return result
}

// savedLanguageLookup resolves the saved UI language once and returns a
// catalog lookup for it. When the setting can't be read, as during shutdown
// once the database is closed, it falls back to English.
func savedLanguageLookup(db *sql.DB) func(key string) string {
	language := models.LanguageEnglish
	if settings, err := database.GetSettings(db); err == nil {
		language = settings.Language
	}
	effective := locale.Effective(language)
	return func(key string) string { return locale.Text(effective, key, nil) }
}

// cleanUpStartupLeftovers reads once which vault IDs a saved vault, pending
// creation or pending connection owns, and removes what an earlier process
// left behind for any other ID. Without that list nothing is removed and
// startup stops: removing a live vault's rclone config would sign it out.
func cleanUpStartupLeftovers(db *sql.DB) error {
	owners, err := database.RcloneVaultConfigOwnerIDs(db)
	if err != nil {
		return fmt.Errorf("classify native rclone vault configs: %w", err)
	}
	cleanUpRcloneStartupState(owners)
	cleanUpEngineStartupLeftovers(owners)
	return nil
}

// cleanUpEngineStartupLeftovers removes engine files an earlier process left
// behind: the engine cache folders of vault IDs not in owners, and the
// temporary copies an interrupted install of a bundled program left next to
// it. It runs before any worker or request can start an engine or install
// one.
//
// Both only take up space, so a failure is logged and startup goes on; the
// next start tries again.
func cleanUpEngineStartupLeftovers(owners map[string]bool) {
	removed, err := engines.ReconcileRepositoryCaches(owners)
	if removed > 0 {
		log.Printf("Replicaro removed %d orphaned engine cache folders", removed)
	}
	if err != nil {
		log.Printf("Replicaro could not remove every orphaned engine cache: %v", err)
	}
	if err := appdata.RemoveStaleInstallTemporaries(
		filepath.Join("engines", engines.ResticID), filepath.Join("engines", engines.KopiaID),
		filepath.Join("components", "rclone"), filepath.Join("components", "storage-helper"),
	); err != nil {
		log.Printf("Replicaro could not remove every interrupted engine install: %v", err)
	}
}

// cleanUpRcloneStartupState removes what an earlier process left behind in
// Replicaro's private rclone folders: stale sign-in and operation sessions,
// and the config folders of vaults not in owners. It runs after the database
// is open and before any rclone operation can start.
//
// Failing to remove something is only logged. Leftover sessions never affect
// new ones, and a leftover vault config is retried on the next start, so
// neither is a reason to keep Replicaro from starting.
func cleanUpRcloneStartupState(owners map[string]bool) {
	if err := engines.CleanupRcloneSessions(); err != nil {
		log.Printf("Replicaro could not remove every stale rclone session: %v", err)
	}
	if err := engines.ReconcileRcloneVaultConfigs(owners); err != nil {
		log.Printf("Replicaro could not remove every orphaned rclone vault config: %v", err)
	}
}
