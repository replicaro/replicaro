package storageidentity

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

// NormalizeConfiguredPath performs lexical cleanup only. Configured paths are
// operational names, not comparison keys: folding case here can change the
// object opened on a case-sensitive filesystem (including Windows directories).
// Binding retains this spelling after the helper validates the selected route;
// physical descriptor components never become an operational pathname.
func NormalizeConfiguredPath(value string) (string, error) {
	if err := ValidateConfiguredPath(value); err != nil {
		return "", err
	}
	var absolute string
	var err error
	if runtime.GOOS == "windows" {
		absolute, err = absoluteWindowsConfiguredPath(value)
	} else {
		absolute, err = filepath.Abs(value)
	}
	if err != nil {
		return "", fmt.Errorf("make storage path absolute: %w", err)
	}
	if runtime.GOOS == "windows" {
		absolute = cleanWindowsConfiguredPath(absolute)
		if absolute == "" {
			return "", fmt.Errorf("storage path is not a canonical absolute path")
		}
	} else {
		absolute = filepath.ToSlash(filepath.Clean(absolute))
	}
	if runtime.GOOS != "windows" && !canonicalConfiguredPath(absolute) {
		return "", fmt.Errorf("storage path is not a canonical absolute path")
	}
	return absolute, nil
}

// GetFullPathName (used by filepath.Abs on Windows) can strip trailing
// whitespace from the final name. Resolve only the drive/current-directory
// prefix through the OS; join the admitted user components lexically.
func absoluteWindowsConfiguredPath(value string) (string, error) {
	if filepath.IsAbs(value) {
		return value, nil
	}
	volume := filepath.VolumeName(value)
	tail := value[len(volume):]
	base, err := filepath.Abs(volume + ".")
	if err != nil {
		return "", err
	}
	if volume == "" && strings.HasPrefix(strings.ReplaceAll(tail, `\`, "/"), "/") {
		return filepath.VolumeName(base) + tail, nil
	}
	return filepath.Join(base, tail), nil
}

// ValidateConfiguredPath runs before lexical cleanup. Collapsing link/../dir
// can choose a different directory from filesystem traversal, so require the
// direct intended route instead. POSIX backslashes are filename characters.
// Existing configured-path control exclusions remain distinct from native
// archive/restore selections, where LF, CR, and tab can be valid names.
func ValidateConfiguredPath(value string) error {
	if value == "" {
		return fmt.Errorf("storage path is required")
	}
	if strings.ContainsAny(value, "\x00\r\n") {
		return fmt.Errorf("storage path contains an invalid character")
	}
	grammar := value
	if runtime.GOOS == "windows" {
		grammar = strings.ReplaceAll(grammar, `\`, "/")
	}
	for _, component := range strings.Split(grammar, "/") {
		if component == ".." {
			return fmt.Errorf("path cannot contain '..' components; enter the direct intended route")
		}
	}
	return nil
}

// BindExistingParent resolves the deepest existing parent and appends the
// requested missing tail without creating it.
func BindExistingParent(value string) (resolved string, existing string, err error) {
	absolute, err := NormalizeConfiguredPath(value)
	if err != nil {
		return "", "", err
	}
	candidate := absolute
	var tail []string
	for {
		if _, statErr := os.Lstat(candidate); statErr == nil {
			physical, evalErr := evalStorageSymlinks(candidate)
			if evalErr != nil {
				return "", "", fmt.Errorf("resolve storage parent: %w", evalErr)
			}
			existing = filepath.Clean(physical)
			resolved = existing
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return filepath.Clean(resolved), existing, nil
		} else if !os.IsNotExist(statErr) {
			return "", "", fmt.Errorf("inspect storage parent: %w", statErr)
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return "", "", fmt.Errorf("no existing storage parent")
		}
		tail = append(tail, filepath.Base(candidate))
		candidate = parent
	}
}

// Resolve performs read-only filesystem identity observation on the current
// platform. Potentially blocking callers should invoke it through ProcessRunner.
func resolveForCreation(value string) (Descriptor, error) {
	resolved, existing, err := BindExistingParent(value)
	if err != nil {
		return Descriptor{}, err
	}
	descriptor, err := resolvePlatform(resolved, existing)
	if err != nil {
		return Descriptor{}, err
	}
	if descriptor.Kind == KindPathOnly {
		absolute, absoluteErr := NormalizeConfiguredPath(value)
		if absoluteErr != nil {
			return Descriptor{}, absoluteErr
		}
		// Identityless observations still carry authoritative local facts. Keep
		// the actual filesystem type, class and physical mount-relative hint.
		// Only the configured spelling is replaced: subtracting that symlink
		// spelling from the physical mount root would mix namespaces.
		descriptor.RelativePath = absolute
		return descriptor, descriptor.Validate()
	}
	return descriptor, nil
}

func ResolveForCreation(value string) (Descriptor, error) {
	return resolveForCreation(value)
}

// ResolveRepositoryForCreation preserves every authoritative storage fact from
// the deepest existing parent while retaining the exact missing tail. These
// facts derive destination candidates; native/protected proof supplies identity.
func ResolveRepositoryForCreation(value string) (Descriptor, error) {
	return resolveForCreation(value)
}

// Resolve retains the creation-binding compatibility entry point. Product
// availability always enters ResolveExisting through the helper protocol.
func Resolve(value string) (Descriptor, error) { return ResolveForCreation(value) }

// ResolveExisting observes availability, not creation eligibility. The exact
// requested object must exist and have the applicable expected type; it never
// turns a deepest-existing-parent binding into an availability result.
func ResolveExisting(value, objectType string) (Descriptor, error) {
	descriptor, _, _, err := resolveExistingObservation(value, objectType)
	if err != nil {
		return Descriptor{}, err
	}
	return descriptor, nil
}

func resolveExistingObservation(value, objectType string) (Descriptor, string, string, error) {
	absolute, err := NormalizeConfiguredPath(value)
	if err != nil {
		return Descriptor{}, "", "", err
	}
	info, err := os.Stat(absolute)
	if os.IsNotExist(err) {
		return Descriptor{}, "", "", &MissingStorageError{}
	}
	if err != nil {
		return Descriptor{}, "", "", fmt.Errorf("inspect exact storage path: %w", err)
	}
	if objectType == "directory" && !info.IsDir() {
		return Descriptor{}, "", "", &MissingStorageError{}
	}
	if objectType == "file" && !info.Mode().IsRegular() {
		return Descriptor{}, "", "", &MissingStorageError{}
	}
	physical, err := evalStorageSymlinks(absolute)
	if err != nil {
		return Descriptor{}, "", "", fmt.Errorf("resolve exact storage path: %w", err)
	}
	physical = filepath.Clean(physical)
	descriptor, observedFilesystem, err := resolvePlatformObservation(physical, physical)
	if err == nil && descriptor.Kind == KindPathOnly {
		// Path-only bindings preserve the user's normalized configured pathname,
		// not a symlink target that could look like persistent physical identity.
		// The normalized filesystem type is retained for local source continuity;
		// it is still not a device or mount-instance identity.
		descriptor.RelativePath = absolute
		err = descriptor.Validate()
	}
	return descriptor, observedFilesystem, physical, err
}

// Go's Windows EvalSymlinks rejects the intermediate \\?\UNC\server prefix
// before it reaches an otherwise valid share. Resolve the equivalent ordinary
// UNC route through the same symlink/junction check, while callers retain the
// exact extended spelling as their configured operational path.
func evalStorageSymlinks(value string) (string, error) {
	if runtime.GOOS == "windows" && strings.HasPrefix(strings.ToLower(value), strings.ToLower(`\\?\UNC\`)) {
		ordinary := `\\` + value[len(`\\?\UNC\`):]
		physical, err := filepath.EvalSymlinks(ordinary)
		if err != nil {
			return "", err
		}
		// Verbatim UNC admits names that ordinary UNC may normalize (for
		// example trailing spaces or dots). The replacement prefix is safe
		// only when both routes prove the same opened object.
		if err := verifyResolvedUNCObject(value, physical); err != nil {
			return "", err
		}
		return physical, nil
	}
	return filepath.EvalSymlinks(value)
}

func verifyResolvedUNCObject(original, resolved string) error {
	originalInfo, err := os.Stat(original)
	if err != nil {
		return fmt.Errorf("inspect extended UNC route: %w", err)
	}
	resolvedInfo, err := os.Stat(resolved)
	if err != nil {
		return fmt.Errorf("inspect resolved UNC object: %w", err)
	}
	if !os.SameFile(originalInfo, resolvedInfo) {
		return fmt.Errorf("extended UNC route does not match its resolved object")
	}
	return nil
}

// pathOnlyDescriptor records that the configured path was valid and visible
// while the platform observer authoritatively abstained from a stable storage
// identity. Its path key is a local binding/resolution fact only: it must never
// become repository identity or infer unsupported aliases or continuity.
func pathOnlyDescriptor(value, filesystem string, class ...StorageClass) (Descriptor, error) {
	canonical, err := canonicalObservedPath(value)
	if err != nil {
		return Descriptor{}, err
	}
	storageClass := StorageClassOther
	if len(class) != 0 {
		storageClass = class[0]
	}
	descriptor := Descriptor{
		Version: DescriptorVersion, Kind: KindPathOnly,
		Filesystem:   strings.ToLower(strings.TrimSpace(filesystem)),
		RelativePath: canonical,
		StorageClass: storageClass,
	}
	return descriptor, descriptor.Validate()
}

func pathOnlyDescriptorAtMount(value, filesystem string, class StorageClass, mountRoot string) (Descriptor, error) {
	descriptor, err := pathOnlyDescriptor(value, filesystem, class)
	if err != nil {
		return Descriptor{}, err
	}
	if class == StorageClassLocal && mountRoot != "" {
		if looksLikeCanonicalWindowsPath(value) || looksLikeCanonicalWindowsPath(mountRoot) {
			descriptor.MountRoot = cleanWindowsPath(mountRoot)
		} else {
			descriptor.MountRoot = path.Clean(mountRoot)
		}
		relative, ok := configuredPathRelative(descriptor.MountRoot, descriptor.RelativePath)
		if !ok {
			return Descriptor{}, fmt.Errorf("observed storage path is outside selected mount")
		}
		descriptor.LocalRelativePath = relative
	}
	return descriptor, descriptor.Validate()
}

// canonicalObservedPath cleans the absolute path returned by an OS observer
// according to that path's own grammar. Fact parsers are deliberately pure and
// may be tested on another host; configured user input is made native and
// absolute at NormalizeConfiguredPath before it reaches this constructor.
func canonicalObservedPath(value string) (string, error) {
	if canonical := cleanWindowsPath(value); !strings.HasPrefix(value, "/") && canonical != "" {
		return canonical, nil
	}
	if !strings.HasPrefix(value, "/") {
		return "", fmt.Errorf("observed storage path is not absolute")
	}
	canonical := path.Clean(value)
	if !canonicalConfiguredPath(canonical) {
		return "", fmt.Errorf("observed storage path is not canonical")
	}
	return canonical, nil
}

type MissingStorageError struct{}

func (*MissingStorageError) Error() string {
	return "exact storage object is missing or has the wrong type"
}

// EnumerateMountedFilesystems returns only currently mounted local volumes and
// mounted/mapped SMB or NFS filesystems using OS-authoritative tables.
func EnumerateMountedFilesystems() ([]MountedFilesystem, error) {
	return enumeratePlatform()
}
