package engines

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/local/replicaro/command"
	"github.com/local/replicaro/models"
)

// externalSSHBatchMode is added to the ssh options Replicaro gives the engines
// for external SSH: Restic's sftp.args and Kopia's --ssh-args (see
// kopiaStorageArgs for vaults Kopia connected before). Engines run in the
// background (a hidden console on Windows, a background process group when
// Replicaro was started from a terminal), so a prompt from ssh, such as an
// unknown host key or a key passphrase, is never seen and would leave the
// command waiting forever. Authentication here is key or agent based only
// (passphrase-protected keys are unsupported), so nothing valid needs a
// prompt. With batch mode ssh fails at once where it would have asked.
//
// Batch mode does not change host-key checking: an unknown or changed key is
// still refused. Debian and Ubuntu builds of OpenSSH also turn on a keepalive
// in batch mode (ServerAliveInterval 300, ServerAliveCountMax 3), so there a
// server that stops responding ends the connection after about 15 minutes;
// other OpenSSH builds keep their own keepalive default.
const externalSSHBatchMode = "BatchMode=yes"

// SSHHostKeyUnknownHelp and SSHHostKeyChangedHelp explain an SFTP connection
// refused over the server's host key: one not in known_hosts yet, or one that
// differs from the key there. Replicaro never accepts or replaces a host key
// itself.
const (
	SSHHostKeyUnknownHelp = "The SFTP server's host key isn't trusted yet, so Replicaro refused the connection. Replicaro never accepts a new host key on its own. To trust this server, connect once from a terminal (ssh -p <port> <user>@<host>) and accept the key, or add it to ~/.ssh/known_hosts (Windows: %USERPROFILE%\\.ssh\\known_hosts). Then try again."
	SSHHostKeyChangedHelp = "The SFTP server's host key has changed since it was trusted. This can mean the server was reinstalled, or that something is intercepting the connection. Check with the server's administrator before updating known_hosts."
)

// SSHAuthenticationHelp explains an SFTP server turning down the SSH key. Only
// key file and agent sign-in are supported, so the fix is always on the key or
// the username, never a password.
const SSHAuthenticationHelp = "The SFTP server didn't accept the SSH key. Check that the key file or the key in your SSH agent is added to this server's authorized keys and that the username is correct."

// SSHHostKeyHelp returns the help for an error caused by an SFTP server's
// host key, or "" for any other error. It reads OpenSSH's messages (Restic,
// and Kopia with an SSH agent or with host-key verification off, run ssh),
// the Go knownhosts errors Kopia's own SFTP client prints (with a key file
// and verification on), and rclone's, which arrive only as a classified
// private failure. A changed key is checked first: OpenSSH also prints "Host
// key verification failed." for it.
func SSHHostKeyHelp(err error) string {
	if err == nil {
		return ""
	}
	switch command.PrivateOutputFailureKind(err) {
	case command.PrivateFailureSSHHostKeyChanged:
		return SSHHostKeyChangedHelp
	case command.PrivateFailureSSHHostKeyUnknown:
		return SSHHostKeyUnknownHelp
	}
	lower := strings.ToLower(err.Error())
	switch {
	case strings.Contains(lower, "remote host identification has changed"),
		strings.Contains(lower, "knownhosts: key mismatch"):
		return SSHHostKeyChangedHelp
	case strings.Contains(lower, "host key verification failed"),
		strings.Contains(lower, "knownhosts: key is unknown"):
		return SSHHostKeyUnknownHelp
	}
	return ""
}

// SSHAuthenticationFailureHelp returns SSHAuthenticationHelp when the SFTP
// server refused the key, or "" for any other error. Callers check
// SSHHostKeyHelp first, because a host-key refusal is the more specific
// problem. Like SSHHostKeyHelp it reads OpenSSH's own message, the Go
// x/crypto/ssh error Kopia's SFTP client prints, and rclone's sidecar failure,
// which only arrives as a classified kind since its text is never kept.
func SSHAuthenticationFailureHelp(err error) string {
	if err == nil {
		return ""
	}
	if command.PrivateOutputFailureKind(err) == command.PrivateFailureSSHAuthentication {
		return SSHAuthenticationHelp
	}
	if command.SSHKeySignInRefused(err.Error()) {
		return SSHAuthenticationHelp
	}
	return ""
}

func externalSSHRequired(repo models.Repository) bool {
	if strings.ToLower(strings.TrimSpace(repo.Connector)) != "sftp" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(repo.Engine)) {
	case ResticID:
		return true
	case KopiaID:
		// Kopia's own SFTP client needs a key file and a known_hosts file, so it
		// is used only for a key file with host-key verification on. An agent
		// socket alone, or verification turned off, goes through OpenSSH.
		options := repo.ConnectorOptions
		return sftpIdentityFile(options) == "" || boolOption(options, "insecure_ignore_host_key")
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
