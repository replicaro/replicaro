package engines

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/local/replicaro/models"
)

func externalSSHRequired(repo models.Repository) bool {
	if strings.ToLower(strings.TrimSpace(repo.Connector)) != "sftp" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(repo.Engine)) {
	case ResticID:
		return true
	case KopiaID:
		options := repo.ConnectorOptions
		if strings.TrimSpace(options["password"]) != "" {
			return false
		}
		hasIdentity := strings.TrimSpace(options["identity"]) != "" ||
			strings.TrimSpace(options["ssh_private_key"]) != ""
		return !hasIdentity || boolOption(options, "insecure_ignore_host_key")
	default:
		return false
	}
}

// ValidateExternalSSHAvailable fails before a native repository mutation when
// the selected native SFTP mode delegates transport to the host OpenSSH client.
func ValidateExternalSSHAvailable(repo models.Repository) error {
	if !externalSSHRequired(repo) {
		return nil
	}
	if _, err := exec.LookPath("ssh"); err != nil {
		return fmt.Errorf("This SFTP configuration requires the OpenSSH client, which was not found on your machine. Install OpenSSH (free), make sure the `ssh` command is available, and retry this action.")
	}
	return nil
}
