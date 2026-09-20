package engines

import (
	"fmt"
	"runtime"

	"github.com/local/replicaro/models"
)

type kopiaConcurrencySettings struct {
	maintenanceList int
	backup          int
	restore         int
	verify          int
	verifyFiles     int
}

func normalizedConcurrencyMode(repo models.Repository) (string, error) {
	return models.NormalizeConcurrencyMode(repo.ConcurrencyMode)
}

func resticConcurrency(repo models.Repository) (backend string, connections, readConcurrency int, err error) {
	mode, err := normalizedConcurrencyMode(repo)
	if err != nil {
		return "", 0, 0, err
	}
	switch {
	case repo.Connector == "" || repo.Connector == "fs":
		backend = "local"
	case repo.Connector == "s3":
		backend = "s3"
	case repo.Connector == "sftp":
		backend = "sftp"
	case repo.Connector == "azblob":
		backend = "azure"
	case repo.Connector == "gcs":
		backend = "gs"
	case IsResticRcloneConnector(repo.Connector):
		backend = "rclone"
	default:
		return "", 0, 0, fmt.Errorf("Restic concurrency is unsupported for connector %q", repo.Connector)
	}
	localConnections, remoteConnections := 0, 0
	switch mode {
	case models.ConcurrencyReduced:
		localConnections, remoteConnections, readConcurrency = 1, 3, 1
	case models.ConcurrencyNative:
		localConnections, remoteConnections, readConcurrency = 2, 5, 2
	case models.ConcurrencyIncreased:
		localConnections, remoteConnections, readConcurrency = 3, 7, 3
	case models.ConcurrencyMaximum:
		localConnections, remoteConnections, readConcurrency = 4, 10, 4
	}
	connections = remoteConnections
	if backend == "local" {
		connections = localConnections
	}
	return backend, connections, readConcurrency, nil
}

func kopiaConcurrency(repo models.Repository) (kopiaConcurrencySettings, error) {
	return kopiaConcurrencyForCPU(repo, runtime.NumCPU())
}

func kopiaConcurrencyForCPU(repo models.Repository, logicalCPUs int) (kopiaConcurrencySettings, error) {
	mode, err := normalizedConcurrencyMode(repo)
	if err != nil {
		return kopiaConcurrencySettings{}, err
	}
	if logicalCPUs < 1 {
		logicalCPUs = 1
	}
	scale := func(numerator int) int {
		value := logicalCPUs * numerator / 2
		if value < 1 {
			return 1
		}
		return value
	}
	switch mode {
	case models.ConcurrencyReduced:
		return kopiaConcurrencySettings{maintenanceList: 8, backup: scale(1), restore: 4, verify: 4, verifyFiles: scale(1)}, nil
	case models.ConcurrencyNative:
		return kopiaConcurrencySettings{maintenanceList: 16, backup: logicalCPUs, restore: 8, verify: 8, verifyFiles: logicalCPUs}, nil
	case models.ConcurrencyIncreased:
		return kopiaConcurrencySettings{maintenanceList: 24, backup: scale(3), restore: 12, verify: 12, verifyFiles: scale(3)}, nil
	case models.ConcurrencyMaximum:
		return kopiaConcurrencySettings{maintenanceList: 32, backup: logicalCPUs * 2, restore: 16, verify: 16, verifyFiles: logicalCPUs * 2}, nil
	default:
		return kopiaConcurrencySettings{}, fmt.Errorf("unsupported concurrency mode %q", mode)
	}
}
