package engines

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/local/replicaro/appdata"
	"github.com/local/replicaro/models"
)

type integrityKopiaConfigKey struct{}

const integrityKopiaConfigDirectoryName = "integrity-check"

// PrepareIntegrityCheck creates the run-local resources and applies the native
// cache policy required before an engine's full check. Its output and error
// belong to orchestration preparation; the later Check call reports only the
// requested native verification command.
func PrepareIntegrityCheck(
	ctx context.Context,
	engine Engine,
	repo models.Repository,
) (context.Context, string, func() error, error) {
	checkContext, cleanup, err := prepareIntegrityCheckResources(ctx, repo)
	if err != nil {
		return nil, "", nil, err
	}
	if repo.Engine == ResticID {
		return checkContext, "", cleanup, nil
	}
	target := engine
	var availabilityCheck RepositoryAvailabilityCheck
	if public, ok := engine.(*publicEngine); ok {
		target = public.Engine
		availabilityCheck = public.availabilityCheck
	}
	kopia, ok := target.(*kopiaEngine)
	if !ok {
		return checkContext, "", cleanup, fmt.Errorf("Kopia integrity preparation requires the Kopia engine")
	}
	checkContext = repositoryProcessContext(checkContext, repo, availabilityCheck)
	output, err := kopia.prepareIntegrityCheckCache(checkContext, repo)
	return checkContext, output, cleanup, err
}

// prepareIntegrityCheckResources creates only the operation-owned filesystem
// resources. Restic disables its cache with --no-cache and needs no artifact.
func prepareIntegrityCheckResources(ctx context.Context, repo models.Repository) (context.Context, func() error, error) {
	if repo.Engine == ResticID {
		return ctx, func() error { return nil }, nil
	}
	if repo.Engine != KopiaID {
		return nil, nil, fmt.Errorf("unsupported integrity-check engine %q", repo.Engine)
	}
	if strings.TrimSpace(repo.ID) == "" {
		return nil, nil, fmt.Errorf("Kopia vault ID is required for isolated integrity configuration")
	}

	// Kopia's repository configuration can contain native credential material.
	// Keep the copy under the vault's already-private configuration directory so
	// it receives the same filesystem protection as the canonical file.
	root, err := appdata.Directory(filepath.Join("engines", KopiaID, repo.ID, "config"))
	if err != nil {
		return nil, nil, fmt.Errorf("prepare Kopia integrity configuration root: %w", err)
	}
	root, err = secureIntegrityArtifactPath(root, "")
	if err != nil {
		return nil, nil, err
	}
	// A deterministic child bounds crash residue to one private directory. The
	// next check removes a structurally valid leftover before creating its own
	// copy; an unexpected child type or link fails closed instead of being
	// followed.
	child := filepath.Join(root, integrityKopiaConfigDirectoryName)
	if _, statErr := os.Lstat(child); statErr == nil {
		if _, err := secureIntegrityArtifactPath(root, child); err != nil {
			return nil, nil, fmt.Errorf("validate stale Kopia integrity configuration: %w", err)
		}
		if err := os.RemoveAll(child); err != nil {
			return nil, nil, fmt.Errorf("remove stale Kopia integrity configuration: %w", err)
		}
	} else if !os.IsNotExist(statErr) {
		return nil, nil, fmt.Errorf("inspect stale Kopia integrity configuration: %w", statErr)
	}
	if err := os.Mkdir(child, 0o700); err != nil {
		return nil, nil, fmt.Errorf("create Kopia integrity configuration directory: %w", err)
	}
	if err := appdata.SecurePath(child, true); err != nil {
		_ = os.RemoveAll(child)
		return nil, nil, fmt.Errorf("secure Kopia integrity configuration directory: %w", err)
	}
	child, err = secureIntegrityArtifactPath(root, child)
	if err != nil {
		_ = os.RemoveAll(child)
		return nil, nil, err
	}
	canonical := filepath.Join(root, "repository.config")
	operationConfig := filepath.Join(child, "repository.config")
	canonicalInfo, err := os.Lstat(canonical)
	if err != nil || canonicalInfo.Mode()&os.ModeSymlink != 0 || !canonicalInfo.Mode().IsRegular() {
		_ = os.RemoveAll(child)
		if err == nil {
			err = fmt.Errorf("canonical Kopia configuration is not a regular file")
		}
		return nil, nil, fmt.Errorf("inspect canonical Kopia integrity configuration: %w", err)
	}
	// Reuse the bounded, synced Kopia config copier so the duplicate never
	// bypasses the platform-specific file protections already used by reconnect.
	if err := copyKopiaReconnectConfig(canonical, operationConfig); err != nil {
		_ = os.RemoveAll(child)
		return nil, nil, fmt.Errorf("copy Kopia integrity configuration: %w", err)
	}
	cleanup := func() error {
		if _, err := secureIntegrityArtifactPath(root, child); err != nil {
			return fmt.Errorf("validate Kopia integrity configuration cleanup: %w", err)
		}
		if err := os.RemoveAll(child); err != nil {
			return fmt.Errorf("remove Kopia integrity configuration: %w", err)
		}
		return nil
	}
	return context.WithValue(ctx, integrityKopiaConfigKey{}, operationConfig), cleanup, nil
}

func secureIntegrityKopiaConfig(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect operation-owned Kopia integrity configuration: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
		info.Size() < 1 || info.Size() > maximumKopiaReconnectConfigBytes {
		return fmt.Errorf("operation-owned Kopia integrity configuration is not a bounded regular file")
	}
	if err := appdata.SecurePath(path, false); err != nil {
		return fmt.Errorf("secure operation-owned Kopia integrity configuration: %w", err)
	}
	return nil
}

func secureIntegrityArtifactPath(root, child string) (string, error) {
	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve integrity artifact root: %w", err)
	}
	rootInfo, err := os.Lstat(absoluteRoot)
	if err != nil {
		return "", fmt.Errorf("inspect integrity artifact root: %w", err)
	}
	if !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("integrity artifact root is not a real directory")
	}
	if err := os.Chmod(absoluteRoot, 0o700); err != nil {
		return "", fmt.Errorf("secure integrity artifact root: %w", err)
	}
	if child == "" {
		return absoluteRoot, nil
	}
	absoluteChild, err := filepath.Abs(child)
	if err != nil {
		return "", fmt.Errorf("resolve integrity artifact: %w", err)
	}
	relative, err := filepath.Rel(absoluteRoot, absoluteChild)
	if err != nil || relative == "." || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) ||
		filepath.Dir(relative) != "." {
		return "", fmt.Errorf("integrity artifact is outside its operation root")
	}
	info, err := os.Lstat(absoluteChild)
	if err != nil {
		return "", fmt.Errorf("inspect integrity artifact: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("integrity artifact is not a real directory")
	}
	return absoluteChild, nil
}

func integrityCheckKopiaConfig(ctx context.Context) string {
	value, _ := ctx.Value(integrityKopiaConfigKey{}).(string)
	return value
}
