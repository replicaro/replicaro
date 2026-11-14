package engines

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/local/replicaro/models"
)

// ErrRestoreDestinationNotAbsolute refuses a destination that is not an
// absolute path for this computer. A relative destination would resolve
// against the process working directory, which is not something the user
// chose (in the Docker image it is the container's unmounted "/"), and Kopia
// would read a destination such as "--shallow=1" as a flag and "~/x" as a
// literal "~" folder.
var ErrRestoreDestinationNotAbsolute = errors.New("The restore destination must be a full path, not a relative one.")

// ErrRestoreSelectionNameInvalid refuses a selected item whose listed path
// cannot be handed back to the engine as the name it archived.
var ErrRestoreSelectionNameInvalid = errors.New("Replicaro cannot restore just this one file because the file name contains invalid characters. To restore this file, select to restore the folder that contains the file.")

// ValidateRestoreRequest runs every check that can refuse a restore before
// any work is queued: the engine's declared restore capability, the
// destination and selection grammar the engine adapters enforce again, and
// the selected-name check below.
func ValidateRestoreRequest(descriptor Descriptor, options RestoreOptions) error {
	if err := ValidateRestoreCapability(descriptor, options); err != nil {
		return err
	}
	if _, err := validateRestoreOptions(options); err != nil {
		return err
	}
	return validateRestoreSelectionName(descriptor.ID, options.Selection, runtime.GOOS)
}

// Engine listings come back as JSON, and Go's JSON encoder replaces bytes
// that are not valid UTF-8 with U+FFFD, so a name that is not valid text on
// the source file system is listed with U+FFFD in place of its real bytes.
// Restic archives the real bytes, so an include pattern built from the listed
// name matches nothing and restic still exits 0 having restored nothing.
// Restoring the containing folder restores the file with its original bytes.
// Such names are refused rather than renamed or guessed at: there is no
// spelling Replicaro could pass that names the archived file.
//
// Kopia is the exception. Pinned Kopia stores U+FFFD in its own directory
// manifest at backup time, so the listed name is the archived name and a
// selected restore of it restores that entry; restoring the folder would give
// the same name. A future engine is refused until it is known to behave like
// Kopia. A name that genuinely contains U+FFFD is refused too, because it
// cannot be told apart from a replaced byte.
//
// On Windows a Restic selection containing a backslash is refused as well.
// Selections are slash-separated, so a backslash there is part of a name
// archived on Linux or macOS, and restic on Windows cannot select that name
// with an include pattern (see resticLiteralIncludePath). Without this check
// the restore would restore nothing, or a sibling such as "back/slash.txt".
// goos is a parameter so tests can cover the Windows rule on any OS.
func validateRestoreSelectionName(engineID, selection, goos string) error {
	if engineID == KopiaID {
		return nil
	}
	if strings.ContainsRune(selection, utf8.RuneError) ||
		engineID == ResticID && goos == "windows" && strings.Contains(selection, `\`) {
		return ErrRestoreSelectionNameInvalid
	}
	return nil
}

// ValidateFileRootVersionRestore checks, before any work is queued, that the
// engine can name the file a single-file root recorded. For Restic that is the
// last element of its archive tree path, with the same name rule as a selected
// item. Kopia restores under the name its fresh root entry reports, and checks
// that entry when it restores.
func ValidateFileRootVersionRestore(engineID string, root models.SnapshotSourceRoot) error {
	switch engineID {
	case ResticID:
		_, _, err := resticFileRootParentAndName(root.Path)
		return err
	case KopiaID:
		return nil
	default:
		return fmt.Errorf("%w: restoring a single-file source version is unavailable", ErrUnsupported)
	}
}

func validateRestoreOptions(options RestoreOptions) (string, error) {
	if options.Destination == "" && !options.OriginalLocation {
		return "", fmt.Errorf("restore destination is required")
	}
	if strings.ContainsRune(options.Destination, '\x00') {
		return "", fmt.Errorf("restore destination contains an invalid character")
	}
	// Restore destinations may contain valid POSIX control characters. Reject
	// NUL at the process boundary and traversal before any local path cleanup.
	if !options.OriginalLocation {
		// filepath.IsAbs follows this computer's rules: on Windows it accepts
		// C:\... and \\server\share\... and refuses the drive-relative C:foo
		// and the current-drive \foo.
		if !filepath.IsAbs(options.Destination) {
			return "", ErrRestoreDestinationNotAbsolute
		}
		grammar := options.Destination
		if runtime.GOOS == "windows" {
			grammar = strings.ReplaceAll(grammar, `\`, "/")
		}
		for _, component := range strings.Split(grammar, "/") {
			if component == ".." {
				return "", fmt.Errorf("path cannot contain '..' components; enter the direct intended route")
			}
		}
	}

	return normalizeSnapshotRelativePath(options.Selection)
}

func isDriveAbsolutePath(value string) bool {
	return len(value) >= 3 && isASCIILetter(value[0]) && value[1] == ':' && (value[2] == '/' || value[2] == '\\')
}

func isASCIILetter(value byte) bool {
	return (value >= 'A' && value <= 'Z') || (value >= 'a' && value <= 'z')
}

// Archive descendants always use slash hierarchy. A Windows viewer must not
// reinterpret literal backslashes, drive-shaped names, or case from Unix data.
func normalizeSnapshotRelativePath(value string) (string, error) {
	if value == "" || value == "." {
		return "", nil
	}
	if strings.ContainsRune(value, '\x00') {
		return "", fmt.Errorf("snapshot path contains an invalid character")
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || component == "." || component == ".." {
			return "", fmt.Errorf("snapshot path must use exact relative native path components")
		}
	}
	return value, nil
}
