package engines

import (
	"os"
	"runtime"

	"github.com/google/uuid"
	"github.com/local/replicaro/models"
)

var presentationHostname = os.Hostname

// These tags are optional presentation hints on the ordinary native backup
// command. Invalid local values omit only their own tag; they must never turn
// display metadata into backup admission, ownership, or retention authority.
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
