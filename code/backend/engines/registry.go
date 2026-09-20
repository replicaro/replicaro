package engines

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/local/replicaro/models"
)

var (
	registryMu        sync.RWMutex
	descriptorProbeMu sync.Mutex
	paths             = map[string]string{}
	descriptorCache   []Descriptor
)

// ConfigurePaths is reserved for backend test fixtures. The application does
// not load user-supplied engine paths; production resolution uses bundled
// assets extracted into local app data.
func ConfigurePaths(value map[string]string) {
	registryMu.Lock()
	defer registryMu.Unlock()
	paths = map[string]string{}
	descriptorCache = nil
	for id, path := range value {
		paths[id] = path
	}
}

func ConfiguredPaths() map[string]string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := map[string]string{}
	for id, path := range paths {
		out[id] = path
	}
	return out
}

func Resolve(repo models.Repository) (Engine, error) {
	return ResolveWithRepositoryAvailabilityCheck(repo, nil)
}

func ResolveWithRepositoryAvailabilityCheck(
	repo models.Repository,
	check RepositoryAvailabilityCheck,
) (Engine, error) {
	registryMu.RLock()
	custom := paths[repo.Engine]
	registryMu.RUnlock()
	var resolved Engine
	switch repo.Engine {
	case ResticID:
		resolved = newRestic(custom)
	case KopiaID:
		resolved = newKopia(custom)
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnsupported, repo.Engine)
	}
	if err := models.ValidateVaultPassword(repo.Passphrase); err != nil {
		return nil, err
	}
	return &publicEngine{Engine: resolved, repo: repo, availabilityCheck: check}, nil
}

func AllDescriptors(ctx context.Context) []Descriptor {
	registryMu.RLock()
	cached := append([]Descriptor(nil), descriptorCache...)
	registryMu.RUnlock()
	if cached != nil {
		return cached
	}
	descriptorProbeMu.Lock()
	defer descriptorProbeMu.Unlock()
	registryMu.RLock()
	cached = append([]Descriptor(nil), descriptorCache...)
	registryMu.RUnlock()
	if cached != nil {
		return cached
	}
	result := make([]Descriptor, 0, 2)
	for _, engine := range []Engine{newRestic(pathFor(ResticID)), newKopia(pathFor(KopiaID))} {
		descriptor := DescriptorContext(ctx, engine)
		if descriptor.Installed && descriptor.Version == "" {
			if version, err := engineVersion(ctx, engine); err == nil {
				descriptor.Version = version
			}
		}
		result = append(result, descriptor)
	}
	if ctx.Err() == nil {
		registryMu.Lock()
		descriptorCache = append([]Descriptor(nil), result...)
		registryMu.Unlock()
	}
	return append([]Descriptor(nil), result...)
}

// DescriptorContext preserves the ordinary Descriptor API while allowing
// cancellation-aware callers such as the standalone E2E runner to bound native
// version probes.
func DescriptorContext(ctx context.Context, engine Engine) Descriptor {
	if public, ok := engine.(*publicEngine); ok {
		engine = public.Engine
	}
	switch value := engine.(type) {
	case *resticEngine:
		return value.descriptor(ctx)
	case *kopiaEngine:
		return value.descriptor(ctx)
	default:
		return engine.Descriptor()
	}
}

func pathFor(id string) string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return paths[id]
}

type versioner interface {
	version(context.Context) (string, error)
}

func engineVersion(ctx context.Context, engine Engine) (string, error) {
	if value, ok := engine.(versioner); ok {
		return value.version(ctx)
	}
	return "", os.ErrNotExist
}
