package api

import (
	"errors"
	"regexp"
	"runtime"
	"strings"

	"github.com/local/replicaro/engines"
)

// Native command output is advisory here: only an explicit local-path OS
// operation and denial warrant macOS guidance. A bare "permission denied" may
// instead be remote authentication or provider authorization.
var macOSLocalPathDenial = regexp.MustCompile(`(?i)\b(?:lstat|stat|open|read|write|readdir|mkdir|create|rename|remove)\s+/(?:Users|Volumes|private|var|Library|Applications)(?:/[^:\r\n]*)?:\s*(?:permission denied|operation not permitted|EACCES|EPERM)(?:"\s*\})?\s*$`)

const macOSNativePermissionGuidance = "macOS may be denying access to a local file used by this operation. Make sure the path is available and Replicaro can read or write it as needed. For protected locations, you may also need to allow Replicaro access under System Settings → Privacy & Security → Files & Folders or Full Disk Access.\n\nAfter confirming access, try again."

func errorToastMessage(err error) string {
	return errorToastMessageForPlatform(runtime.GOOS, err)
}

func errorToastMessageForPlatform(platform string, err error) string {
	message := err.Error()
	if platform != "darwin" {
		return message
	}
	var native *engineCommandError
	if !errors.As(err, &native) || native.output == "" ||
		errors.Is(native.err, engines.ErrColdStorageArchivedObject) {
		return message
	}
	for _, line := range strings.Split(native.output, "\n") {
		if macOSLocalPathDenial.MatchString(line) && !strings.Contains(strings.ToLower(line), "://") {
			return message + "\n\n" + macOSNativePermissionGuidance
		}
	}
	return message
}
