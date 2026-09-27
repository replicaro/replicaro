package engines

import (
	"os"
	"runtime"

	"github.com/google/uuid"
	"github.com/local/replicaro/models"
)

var presentationHostname = os.Hostname

// These tags are optional display hints added to the normal native backup
// command. An invalid local value drops only its own tag. Never use these tags
// to decide whether a backup may run, who owns a snapshot, or what retention
// keeps.
func backupPresentationTags(repo models.Repository) []string {
	tags := []string{}
	if parsed, err := uuid.Parse(repo.ClientUUID); err == nil && parsed != uuid.Nil && parsed.String() == repo.ClientUUID {
		tags = append(tags, models.SnapshotClientMarkerPrefix+repo.ClientUUID)
	}
	hostname, err := presentationHostname()
	if err != nil {
		return tags
	}
	if value, ok := models.EncodeSnapshotMachineTag(hostname, runtime.GOOS); ok {
		tags = append(tags, value)
	}
	return tags
}
