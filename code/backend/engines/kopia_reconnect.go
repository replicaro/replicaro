package engines

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/local/replicaro/appdata"
	"github.com/local/replicaro/command"
	"github.com/local/replicaro/models"
)

const maximumKopiaReconnectConfigBytes = 4 << 20

// KopiaFilesystemReconnectPaths are local, per-vault artifacts. The durable
// database intent stores their token and fingerprints, never vault secrets.
type KopiaFilesystemReconnectPaths struct {
	Active   string
	Staged   string
	Previous string
}

func kopiaReconnectPaths(target *kopiaEngine, repo models.Repository, token string) (KopiaFilesystemReconnectPaths, error) {
	if strings.TrimSpace(token) == "" || strings.ContainsAny(token, `/\\.`) {
		return KopiaFilesystemReconnectPaths{}, fmt.Errorf("Kopia reconnect token is invalid")
	}
	active, _, err := target.config(repo)
	if err != nil {
		return KopiaFilesystemReconnectPaths{}, err
	}
	return KopiaFilesystemReconnectPaths{
		Active: active, Staged: active + ".reconnect-" + token + ".staged",
		Previous: active + ".reconnect-" + token + ".previous",
	}, nil
}

func fingerprintRegularFile(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > maximumKopiaReconnectConfigBytes {
		return "", fmt.Errorf("Kopia configuration is not a bounded regular file")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(file, maximumKopiaReconnectConfigBytes+1)); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// KopiaActiveConfigFingerprint returns the exact isolated active-config
// fingerprint that is persisted before any relocation staging begins.
func KopiaActiveConfigFingerprint(engine Engine, repo models.Repository) (KopiaFilesystemReconnectPaths, string, error) {
	target, _, err := concreteKopiaEngine(engine)
	if err != nil {
		return KopiaFilesystemReconnectPaths{}, "", err
	}
	paths, err := kopiaReconnectPaths(target, repo, "fingerprint")
	if err != nil {
		return paths, "", err
	}
	fingerprint, err := fingerprintRegularFile(paths.Active)
	return paths, fingerprint, err
}

func KopiaFilesystemReconnectArtifacts(engine Engine, repo models.Repository, token string) (KopiaFilesystemReconnectPaths, error) {
	target, _, err := concreteKopiaEngine(engine)
	if err != nil {
		return KopiaFilesystemReconnectPaths{}, err
	}
	return kopiaReconnectPaths(target, repo, token)
}

func copyKopiaReconnectConfig(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > maximumKopiaReconnectConfigBytes {
		return fmt.Errorf("Kopia configuration is not a bounded regular file")
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	remove := true
	defer func() {
		_ = output.Close()
		if remove {
			_ = os.Remove(destination)
		}
	}()
	written, err := io.Copy(output, io.LimitReader(input, maximumKopiaReconnectConfigBytes+1))
	if err != nil || written != info.Size() {
		if err == nil {
			err = fmt.Errorf("Kopia configuration changed while it was copied")
		}
		return err
	}
	if err := output.Sync(); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	remove = false
	return appdata.SecurePath(destination, false)
}

func kopiaReconnectBase(target *kopiaEngine, repo models.Repository, config string) ([]string, []string, error) {
	base, env, err := target.baseArgs(repo)
	if err != nil {
		return nil, nil, err
	}
	if len(base) < 2 || base[0] != "--config-file" {
		return nil, nil, fmt.Errorf("Kopia isolated configuration arguments are invalid")
	}
	base = append([]string(nil), base...)
	base[1] = config
	return base, env, nil
}

func validateKopiaReconnectStatus(repo models.Repository, output string) error {
	if err := validateKopiaBinding(repo, output); err != nil {
		return err
	}
	var repositoryStatus kopiaRepositoryStatus
	if err := strictJSON([]byte(output), &repositoryStatus); err != nil {
		return fmt.Errorf("decode Kopia repository status: %w", err)
	}
	user, host, err := kopiaClientIdentity(output)
	if err != nil || user != repo.ClientUUID || host != "replicaro" {
		return fmt.Errorf("Kopia client identity readback did not match %s@replicaro", repo.ClientUUID)
	}
	// Both staged and active readback must prove the final cache policy.
	// Authentication alone would not detect ordinary caching left disabled,
	// or enrolled clients allowed to reuse stale provider-retention settings.
	expectedCacheDuration := int64(15 * time.Minute)
	if repo.ObjectLock.Enrolled {
		expectedCacheDuration = -1
	}
	if repositoryStatus.ClientOptions.FormatBlobCacheDuration != expectedCacheDuration {
		return fmt.Errorf("Kopia repository format cache policy did not match the reconnect requirement")
	}
	fingerprint, err := RepositoryFingerprint(repo, output)
	if err != nil {
		return err
	}
	if fingerprint != repo.NativeRepositoryID {
		return fmt.Errorf("Kopia native repository identity changed during reconnect")
	}
	return nil
}

// PrepareKopiaFilesystemReconnect creates and verifies an isolated staged
// connection. It never invokes repository create/init and leaves the active
// configuration untouched.
func PrepareKopiaFilesystemReconnect(ctx context.Context, engine Engine, repo models.Repository, token, expectedPriorSHA string) (KopiaFilesystemReconnectPaths, string, error) {
	target, check, err := concreteKopiaEngine(engine)
	if err != nil {
		return KopiaFilesystemReconnectPaths{}, "", err
	}
	if repo.Engine != KopiaID || repo.Connector == "" || repo.NativeRepositoryID == "" || repo.ClientUUID == "" {
		return KopiaFilesystemReconnectPaths{}, "", fmt.Errorf("Kopia reconnect identity is incomplete")
	}
	release, err := acquireKopiaInitialization(ctx, repo.ID)
	if err != nil {
		return KopiaFilesystemReconnectPaths{}, "", err
	}
	defer release()
	paths, err := kopiaReconnectPaths(target, repo, token)
	if err != nil {
		return paths, "", err
	}
	priorSHA, err := fingerprintRegularFile(paths.Active)
	if err != nil || priorSHA != expectedPriorSHA {
		return paths, "", fmt.Errorf("active Kopia configuration changed before reconnect staging")
	}
	_ = os.Remove(paths.Staged)
	if _, err := os.Stat(paths.Previous); os.IsNotExist(err) {
		if err := copyKopiaReconnectConfig(paths.Active, paths.Previous); err != nil {
			return paths, "", err
		}
	} else if err != nil {
		return paths, "", err
	} else if backupSHA, err := fingerprintRegularFile(paths.Previous); err != nil || backupSHA != expectedPriorSHA {
		return paths, "", fmt.Errorf("saved prior Kopia configuration does not match reconnect intent")
	}
	base, env, err := kopiaReconnectBase(target, repo, paths.Staged)
	if err != nil {
		return paths, "", err
	}
	args, input, err := kopiaStorageInvocation(repo, "connect")
	if err != nil {
		return paths, "", err
	}
	// Staging changes the config path, not this vault's native cache directory.
	// A peer can still cache format data encrypted with the previous password,
	// so its first connect after rotation must bypass that stale entry. Kopia
	// owns the refresh; parsing or deleting its cache here is not necessary.
	args = append(args, "--disable-repository-format-cache")
	ctx = repositoryProcessContext(ctx, repo, check)
	var output string
	if input != "" {
		output, err = target.runWithInput(ctx, env, append(base, args...), input, command.NoTotalDeadline)
	} else {
		output, err = target.run(ctx, env, append(base, args...), command.NoTotalDeadline)
	}
	if err != nil {
		return paths, output, err
	}
	// Restore the pinned native 15-minute default before fingerprinting and
	// activating an ordinary staged config. Enrolled Object Lock clients keep
	// caching disabled even while locking is paused: the format also carries
	// repository-wide provider-retention settings that require fresh readback.
	setClient := []string{"repository", "set-client", "--username", repo.ClientUUID, "--hostname", "replicaro"}
	if repo.ObjectLock.Enrolled {
		setClient = append(setClient, "--disable-repository-format-cache")
	} else {
		setClient = append(setClient, "--repository-format-cache-duration=15m")
	}
	setOutput, err := target.run(ctx, env, append(base, setClient...), command.NoTotalDeadline)
	output = strings.TrimSpace(output + "\n" + setOutput)
	if err != nil {
		return paths, output, err
	}
	status, err := target.run(ctx, env, append(base, "repository", "status", "--json"), command.NoTotalDeadline)
	output = strings.TrimSpace(output + "\n" + status)
	if err != nil {
		return paths, output, err
	}
	if err := validateKopiaReconnectStatus(repo, status); err != nil {
		return paths, output, err
	}
	return paths, output, nil
}

func KopiaReconnectConfigFingerprint(path string) (string, error) {
	return fingerprintRegularFile(path)
}

// ActivateKopiaFilesystemReconnect atomically activates only the exact staged
// bytes whose fingerprints were durably recorded.
func ActivateKopiaFilesystemReconnect(paths KopiaFilesystemReconnectPaths, priorSHA, stagedSHA string) error {
	activeSHA, err := fingerprintRegularFile(paths.Active)
	if err != nil || activeSHA != priorSHA {
		return fmt.Errorf("active Kopia configuration changed before reconnect activation")
	}
	actualStaged, err := fingerprintRegularFile(paths.Staged)
	if err != nil || actualStaged != stagedSHA {
		return fmt.Errorf("staged Kopia configuration changed before reconnect activation")
	}
	return appdata.AtomicReplace(paths.Staged, paths.Active)
}

// VerifyActiveKopiaFilesystemReconnect verifies the activated exact config,
// native repository ID, storage path, and client identity.
func VerifyActiveKopiaFilesystemReconnect(ctx context.Context, engine Engine, repo models.Repository, token, stagedSHA string) (KopiaFilesystemReconnectPaths, string, error) {
	target, check, err := concreteKopiaEngine(engine)
	if err != nil {
		return KopiaFilesystemReconnectPaths{}, "", err
	}
	paths, err := kopiaReconnectPaths(target, repo, token)
	if err != nil {
		return paths, "", err
	}
	activeSHA, err := fingerprintRegularFile(paths.Active)
	if err != nil || activeSHA != stagedSHA {
		return paths, "", fmt.Errorf("active Kopia configuration does not match reconnect intent")
	}
	base, env, err := kopiaReconnectBase(target, repo, paths.Active)
	if err != nil {
		return paths, "", err
	}
	ctx = repositoryProcessContext(ctx, repo, check)
	status, err := target.run(ctx, env, append(base, "repository", "status", "--json"), command.NoTotalDeadline)
	if err != nil {
		return paths, status, err
	}
	if err := validateKopiaReconnectStatus(repo, status); err != nil {
		return paths, status, err
	}
	return paths, status, nil
}

func CleanupKopiaFilesystemReconnect(paths KopiaFilesystemReconnectPaths) error {
	var result error
	for _, path := range []string{paths.Staged, paths.Previous} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			result = errors.Join(result, err)
		}
	}
	return result
}

func RestoreKopiaFilesystemReconnect(paths KopiaFilesystemReconnectPaths, priorSHA string) error {
	backupSHA, err := fingerprintRegularFile(paths.Previous)
	if err != nil || backupSHA != priorSHA {
		return fmt.Errorf("saved prior Kopia configuration does not match reconnect intent")
	}
	temporary := paths.Active + ".restore"
	_ = os.Remove(temporary)
	if err := copyKopiaReconnectConfig(paths.Previous, temporary); err != nil {
		return err
	}
	return appdata.AtomicReplace(temporary, paths.Active)
}
