package engines

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/local/replicaro/command"
	"github.com/local/replicaro/models"
)

// DownloadResult describes one file download. Command is the requested native
// command as it is shown in the operation's log: the engine's subcommand and
// its own arguments, without the repository, configuration and credential
// arguments every command of the vault gets. Output is the engine's error
// output; the file itself never appears here.
type DownloadResult struct {
	Command string
	Output  string
}

var streamingCommandRunner = runStreamingCommand

func runStreamingCommand(ctx context.Context, path string, args, env []string, timeout time.Duration, engine string, stdout io.Writer) (string, error) {
	output, err := command.RunStreamingStdout(ctx, path, args, env, timeout, engine, stdout)
	if err != nil {
		return output, &CommandError{Engine: engine, Output: output, Err: err}
	}
	return output, nil
}

// SetStreamingCommandRunnerForTests replaces the runner of the download
// commands; nil restores the real one. The returned function restores the
// runner that was in place.
func SetStreamingCommandRunnerForTests(next func(context.Context, string, []string, []string, time.Duration, string, io.Writer) (string, error)) func() {
	previous := streamingCommandRunner
	if next == nil {
		streamingCommandRunner = runStreamingCommand
	} else {
		streamingCommandRunner = next
	}
	return func() { streamingCommandRunner = previous }
}

// DownloadFile writes one file of a backup to stdout with the engine's own
// command: `restic --quiet dump <snapshot> <path>`, or
// `kopia show <root object>/<path>` (`kopia show <root object>` for a
// single-file source). Apart from Restic's --quiet (see resticEngine.download)
// nothing else is passed to those commands, and the file is never kept
// anywhere on the way.
// root is the snapshot's exact native root and path the item's path under it,
// empty for a single-file source.
//
// The caller must already know that the item is a regular file: `restic dump`
// of a folder writes a tar archive of it, and `kopia show` prints a folder's
// listing or a symlink's target, so either would be saved as the file.
//
// The engine's standard output goes only to stdout; it is not shown as live
// output or kept in the result. Its error output is shown live and returned
// in DownloadResult.Output. Errors are typed the way the other requested
// native commands' are (RequestedOperationOutcome), so the caller can tell a
// command that never started from one that failed or was cancelled.
func DownloadFile(ctx context.Context, engine Engine, repo models.Repository, snapshotID string, root models.SnapshotSourceRoot, path string, stdout io.Writer) (DownloadResult, error) {
	if public, ok := engine.(*publicEngine); ok {
		engine = public.Engine
	}
	if repo.ColdStorage {
		return DownloadResult{}, requestedOperationPreparationFailure(repo.Engine,
			fmt.Errorf("%w: files in a Cold Storage vault can't be downloaded", ErrUnsupported))
	}
	relative, err := normalizeSnapshotRelativePath(path)
	if err != nil || relative != path {
		if err == nil {
			err = fmt.Errorf("snapshot path must use exact relative native path components")
		}
		return DownloadResult{}, requestedOperationPreparationFailure(repo.Engine, err)
	}
	switch native := engine.(type) {
	case *resticEngine:
		if repo.Engine == ResticID {
			return native.download(ctx, repo, snapshotID, root, path, stdout)
		}
	case *kopiaEngine:
		if repo.Engine == KopiaID {
			return native.download(ctx, repo, snapshotID, path, stdout)
		}
	}
	return DownloadResult{}, requestedOperationPreparationFailure(repo.Engine,
		fmt.Errorf("%w: file download needs a Restic or Kopia vault", ErrUnsupported))
}

func (e *resticEngine) download(ctx context.Context, repo models.Repository, snapshotID string, root models.SnapshotSourceRoot, path string, stdout io.Writer) (DownloadResult, error) {
	if err := ValidateSnapshotIDArgument(snapshotID); err != nil {
		return DownloadResult{}, resticPreparationFailure(err)
	}
	if root.Path == "" {
		return DownloadResult{}, resticPreparationFailure(fmt.Errorf("restic download requires an exact native source root"))
	}
	// The same archive-tree address restore and browse use: a Windows drive
	// is /C/..., a UNC volume one /\\server\share component.
	target, err := resticSnapshotTreePath(root.Path)
	if err != nil {
		return DownloadResult{}, resticPreparationFailure(err)
	}
	if path != "" {
		target = strings.TrimSuffix(target, "/") + "/" + path
	}
	if target == "/" {
		return DownloadResult{}, resticPreparationFailure(fmt.Errorf("restic download requires a path that names a file"))
	}
	// --quiet is not cosmetic and must stay. When the repository is locked,
	// Restic writes "repo already locked, waiting up to 0s for the lock" to its
	// standard output, which here is the file itself, and then tries once more.
	// If the other lock is released in that moment, dump carries on and exits
	// 0, so without --quiet the browser would save that line in front of the
	// file and the download would still count as a success. --quiet drops only
	// that kind of message; the file's bytes, the exit code and the errors on
	// standard error (a missing path, a wrong password, a lock that stays) are
	// the same.
	global := []string{"--quiet"}
	args := []string{"dump", snapshotID, target}
	result := DownloadResult{Command: displayNativeCommand(ResticID, append(append([]string{}, global...), args...))}
	output, err := e.runRepoCommand(ctx, repo, global, args, command.NoTotalDeadline,
		func(commandCtx context.Context, env, commandArgs []string, timeout time.Duration) (string, error, bool) {
			if e.path == "" {
				return "", fmt.Errorf("bundled restic engine is unavailable"), false
			}
			// Only error output is shown live; the runner never passes the file on.
			commandCtx = command.ContextWithLiveOutputEnabled(commandCtx)
			out, runErr := streamingCommandRunner(commandCtx, e.path, commandArgs, env, timeout, ResticID, stdout)
			return out, runErr, false
		})
	result.Output = output
	return result, resticRequestedOperationError(err)
}

func (e *kopiaEngine) download(ctx context.Context, repo models.Repository, snapshotID, path string, stdout io.Writer) (DownloadResult, error) {
	if e.path == "" {
		return DownloadResult{}, kopiaPreparationFailure(fmt.Errorf("bundled kopia engine is unavailable"))
	}
	// The listing and the download share one repository session, so the
	// binding check runs once.
	ctx = WithOperationSessionScope(ctx)
	rows, _, err := e.snapshotRows(ctx, repo)
	if err != nil {
		return DownloadResult{}, kopiaPreparationFailure(err)
	}
	var object, rootType string
	for _, row := range rows {
		if row.ID == snapshotID {
			object, rootType = row.RootEntry.Obj, row.RootEntry.Type
			break
		}
	}
	if object == "" {
		return DownloadResult{}, kopiaPreparationFailure(fmt.Errorf("kopia snapshot %q was not found", snapshotID))
	}
	switch {
	case path == "" && rootType != "f":
		return DownloadResult{}, kopiaPreparationFailure(fmt.Errorf("Kopia snapshot root is not a regular file"))
	case path != "" && rootType != "d":
		return DownloadResult{}, kopiaPreparationFailure(fmt.Errorf("Kopia snapshot root is not a directory"))
	}
	if strings.HasPrefix(object, "-") || strings.ContainsAny(object, "/\x00") {
		return DownloadResult{}, kopiaPreparationFailure(fmt.Errorf("Kopia snapshot root object is invalid"))
	}
	// Kopia follows the slash-separated names below the root object itself,
	// the same way `kopia show` resolves any object path.
	if path != "" {
		object += "/" + path
	}
	session, _, err := e.openRepositorySession(ctx, repo, false)
	if err != nil {
		return DownloadResult{}, kopiaPreparationFailure(err)
	}
	args := []string{"show", object}
	result := DownloadResult{Command: displayNativeCommand(KopiaID, args)}
	commandContext, processStarted := command.ContextWithProcessStartTracking(ctx)
	commandContext = command.ContextWithLiveOutputEnabled(commandContext)
	output, runErr := streamingCommandRunner(commandContext, e.path, kopiaCommandArgs(session.base, args), session.env, command.NoTotalDeadline, KopiaID, stdout)
	result.Output = output
	return result, requestedOperationCommandFailure(KopiaID, runErr, nil, processStarted() || runErr == nil)
}

// displayNativeCommand writes a command for the operation's log, quoting an
// argument Go-style when it holds a space, a quote, a backslash or a character
// that isn't printable, so a name with a newline stays on one line.
func displayNativeCommand(engine string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, engine)
	for _, arg := range args {
		plain := arg != ""
		for _, r := range arg {
			if r == ' ' || r == '"' || r == '\'' || r == '\\' || !unicode.IsPrint(r) {
				plain = false
				break
			}
		}
		if plain {
			parts = append(parts, arg)
		} else {
			parts = append(parts, strconv.Quote(arg))
		}
	}
	return strings.Join(parts, " ")
}
