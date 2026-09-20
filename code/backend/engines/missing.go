package engines

import (
	"os"
	"regexp"
	"strings"

	"github.com/local/replicaro/models"
)

// RepositoryMissing is the only creation classifier used by the API. It is
// deliberately conservative: authentication, authorization, credential,
// network, timeout, and ambiguous failures are never treated as absence.
func RepositoryMissing(repo models.Repository, output string) bool {
	switch repo.Engine {
	case ResticID:
		return resticRepositoryMissing(repo, output)
	case KopiaID:
		lower := strings.ToLower(output)
		if containsUnsafeFailure(lower) {
			return false
		}
		if repo.Connector != "" && repo.Connector != "fs" {
			return strings.Contains(lower, "repository not initialized in the provided storage")
		}
		if KopiaRepositoryExists(repo.Location) {
			return false
		}
		return localRepositoryMissing(repo.Location) && containsAny(lower, "config file", "config not found", "repository")
	default:
		return false
	}
}

// RepositoryAccessRejected recognizes only native output that affirmatively
// attributes repository validation failure to authentication or authorization.
// Ambiguous transport and service failures must remain unclassified.
func RepositoryAccessRejected(output string) bool {
	for _, line := range strings.Split(output, "\n") {
		line = strings.ToLower(strings.TrimSpace(line))
		for _, prefix := range []string{"fatal: ", "error: ", "error "} {
			line = strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
		switch line {
		case "wrong password or no key found",
			"wrong password", "incorrect password", "invalid password", "wrong passphrase", "no key found",
			"authentication failed", "access denied", "unauthorized", "forbidden", "accessdenied",
			"request failed with status code: 403", "request failed with statuscode=403":
			return true
		}
		if sshPermissionDeniedPattern.MatchString(line) {
			return true
		}
	}
	return false
}

var sshPermissionDeniedPattern = regexp.MustCompile(`^(?:[^[:space:]:]+@[^[:space:]:]+: )?permission denied \((?:publickey|keyboard-interactive|password)(?:,(?:publickey|keyboard-interactive|password))*\)\.?$`)

func resticRepositoryMissing(repo models.Repository, output string) bool {
	lower := strings.ToLower(output)
	if containsUnsafeFailure(lower) {
		return false
	}
	if repo.Connector != "" && repo.Connector != "fs" {
		return strings.Contains(lower, "repository does not exist") &&
			strings.Contains(lower, "unable to open config file")
	}
	if !localRepositoryMissing(repo.Location) {
		return false
	}
	return containsAny(lower, "config file", "repository does not exist")
}

func localRepositoryMissing(location string) bool {
	if strings.TrimSpace(location) == "" {
		return false
	}
	info, err := os.Stat(location)
	if os.IsNotExist(err) {
		return true
	}
	if err != nil || !info.IsDir() {
		return false
	}
	entries, err := os.ReadDir(location)
	if err != nil {
		return false
	}
	return len(entries) == 0
}

func containsUnsafeFailure(lower string) bool {
	return containsAny(lower,
		"wrong password", "incorrect password", "wrong passphrase", "invalid password", "access denied", "permission denied",
		"unauthorized", "forbidden", "403", "authentication", "authorization", "credential", "credentials", "password file",
		"secret", "token", "api key", "network is unreachable", "network error", "network failure", "timeout", "timed out", "connection refused", "connection reset",
		"tls", "certificate", "temporary failure", "dns")
}

func containsAny(value string, markers ...string) bool {
	for _, marker := range markers {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}
