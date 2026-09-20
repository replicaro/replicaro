package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/local/replicaro/appdata"
	_ "modernc.org/sqlite"
)

const maxReadConnections = 4

var openedDatabasePaths sync.Map

func Open(path string) (*sql.DB, error) {
	if err := preflightExistingDatabase(path); err != nil {
		return nil, err
	}
	return openSQLiteWriter(path)
}

func openSQLiteWriter(path string) (*sql.DB, error) {
	if runtime.GOOS != "windows" {
		if directory := filepath.Dir(path); directory != "." {
			if err := os.MkdirAll(directory, 0o700); err != nil {
				return nil, err
			}
			// Validate all ancestors without following a stale symlink. The
			// database path is a managed file and must not be redirected outside
			// its intended directory tree.
			current, _ := filepath.Abs(directory)
			for {
				info, statErr := os.Lstat(current)
				if statErr != nil {
					return nil, statErr
				}
				if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
					return nil, fmt.Errorf("database parent is not a real directory: %s", current)
				}
				if current == filepath.Dir(current) {
					break
				}
				current = filepath.Dir(current)
			}
			if err := os.Chmod(directory, 0o700); err != nil {
				return nil, err
			}
		}
		if info, statErr := os.Lstat(path); statErr == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				return nil, fmt.Errorf("database path must be a regular file")
			}
		} else if !os.IsNotExist(statErr) {
			return nil, statErr
		}
		file, err := os.OpenFile(path, os.O_CREATE, 0o600)
		if err != nil {
			return nil, err
		}
		if err := file.Close(); err != nil {
			return nil, err
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, err
		}
	}
	if runtime.GOOS == "windows" {
		directory := filepath.Dir(path)
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, err
		}
		if err := appdata.SecurePath(directory, true); err != nil {
			return nil, err
		}
		file, err := os.OpenFile(path, os.O_CREATE, 0o600)
		if err != nil {
			return nil, err
		}
		if err := file.Close(); err != nil {
			return nil, err
		}
		if err := appdata.SecurePath(path, false); err != nil {
			return nil, err
		}
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(path); err == nil && info.Mode().Perm() != 0o600 {
			if err := os.Chmod(path, 0o600); err != nil {
				return nil, err
			}
		}
		for _, sidecar := range []string{path + "-wal", path + "-shm"} {
			if info, statErr := os.Lstat(sidecar); statErr == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
				return nil, fmt.Errorf("database sidecar path is unsafe")
			}
		}
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dsn, err := sqliteWriterDSN(absolutePath, runtime.GOOS)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite serializes writes even in WAL mode. Keep one physical writer so
	// Replicaro's short control-plane transactions have deterministic ordering
	// instead of competing through SQLITE_BUSY timeouts. API reads use the
	// separate read-only pool opened by OpenReadPool. This is an architecture
	// bound, not a general tuning default; changing it requires rerunning the
	// write-ordering, busy-contention, and publication-atomicity tests.
	db.SetMaxOpenConns(1)
	// WAL permits dashboard reads while background jobs record status. A busy
	// timeout turns short write contention into bounded waiting instead of
	// intermittent "database is locked" failures.
	for _, pragma := range []string{
		`PRAGMA journal_mode = WAL`,
	} {
		if _, err := db.Exec(pragma); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	if runtime.GOOS != "windows" {
		for _, sidecar := range []string{path, path + "-wal", path + "-shm"} {
			if err := os.Chmod(sidecar, 0o600); err != nil && !os.IsNotExist(err) {
				_ = db.Close()
				return nil, err
			}
		}
	}
	if runtime.GOOS == "windows" {
		for _, sidecar := range []string{path, path + "-wal", path + "-shm"} {
			if err := secureOptionalDatabaseFile(sidecar); err != nil {
				_ = db.Close()
				return nil, err
			}
		}
	}
	openedDatabasePaths.Store(db, path)
	return db, nil
}

// OpenReadPool opens the bounded, query-only side of Replicaro's SQLite
// topology. Call it only after Open, migration, and startup recovery have
// established a valid database.
func OpenReadPool(path string) (*sql.DB, error) {
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dsn, err := sqliteReadPoolDSN(absolutePath, runtime.GOOS)
	if err != nil {
		return nil, err
	}
	readOnly, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// Four readers cover the UI's small parallel request fan-out without
	// allowing expensive metadata queries to multiply CPU, memory, and WAL
	// snapshot pressure without bound. database/sql opens additional readers on
	// demand after the startup ping validates the pool. Four is an architecture
	// bound rather than a throughput knob; changes need the UI fan-out,
	// cancellation, WAL-pressure, and large File History measurements rerun.
	readOnly.SetMaxOpenConns(maxReadConnections)
	readOnly.SetMaxIdleConns(maxReadConnections)
	openedDatabasePaths.Store(readOnly, absolutePath)
	if err := readOnly.PingContext(context.Background()); err != nil {
		openedDatabasePaths.Delete(readOnly)
		_ = readOnly.Close()
		return nil, err
	}
	return readOnly, nil
}

func preflightExistingDatabase(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("database path must be a regular file")
	}
	if runtime.GOOS != "windows" {
		if directory := filepath.Dir(path); directory != "." {
			current, _ := filepath.Abs(directory)
			for {
				ancestor, statErr := os.Lstat(current)
				if statErr != nil {
					return statErr
				}
				if ancestor.Mode()&os.ModeSymlink != 0 || !ancestor.IsDir() {
					return fmt.Errorf("database parent is not a real directory: %s", current)
				}
				if current == filepath.Dir(current) {
					break
				}
				current = filepath.Dir(current)
			}
		}
	}
	hasPendingWAL := false
	for _, sidecar := range []string{path + "-wal", path + "-shm"} {
		sidecarInfo, statErr := os.Lstat(sidecar)
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return statErr
		}
		if sidecarInfo.Mode()&os.ModeSymlink != 0 || !sidecarInfo.Mode().IsRegular() {
			return fmt.Errorf("database sidecar path is unsafe")
		}
		if sidecar == path+"-wal" && sidecarInfo.Size() > 0 {
			hasPendingWAL = true
		}
	}
	if info.Size() == 0 {
		if hasPendingWAL {
			return ErrDatabaseResetRequired
		}
		return nil
	}

	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	dsn := sqliteReadOnlyDSN(absolutePath, runtime.GOOS)
	readOnly, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	readOnly.SetMaxOpenConns(1)
	defer readOnly.Close()

	var version int
	if err := readOnly.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version == 0 {
		var existing int
		if err := readOnly.QueryRow(`SELECT COUNT(*) FROM sqlite_schema
			WHERE name NOT GLOB 'sqlite_*'`).Scan(&existing); err != nil {
			return err
		}
		if existing > 0 {
			return ErrDatabaseResetRequired
		}
		return nil
	}
	if version != CurrentSchemaVersion {
		return fmt.Errorf("%w (found version %d, expected %d)",
			ErrDatabaseResetRequired, version, CurrentSchemaVersion)
	}
	return nil
}

func sqliteReadOnlyDSN(absolutePath, goos string) string {
	sqlitePath := filepath.ToSlash(absolutePath)
	if goos == "windows" && len(sqlitePath) >= 2 && sqlitePath[1] == ':' {
		// A Windows drive path must be represented as file:///C:/... .
		// file:C:/... is parsed by SQLite as URI authority "C".
		sqlitePath = "/" + sqlitePath
	}
	return (&url.URL{
		Scheme:   "file",
		Path:     sqlitePath,
		RawQuery: "mode=ro",
	}).String()
}

// Connection-local settings must also apply after an interrupted connection is replaced.
func sqliteWriterDSN(absolutePath, goos string) (string, error) {
	parsed, err := url.Parse(sqliteReadOnlyDSN(absolutePath, goos))
	if err != nil {
		return "", err
	}
	query := parsed.Query()
	query.Set("mode", "rw") // The protected literal file was already created above.
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "foreign_keys(1)")
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func sqliteReadPoolDSN(absolutePath, goos string) (string, error) {
	parsed, err := url.Parse(sqliteReadOnlyDSN(absolutePath, goos))
	if err != nil {
		return "", err
	}
	query := parsed.Query()
	// Driver DSN pragmas run for every lazily opened physical connection.
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "query_only(1)")
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func secureOptionalDatabaseFile(path string) error {
	err := appdata.SecurePath(path, false)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
