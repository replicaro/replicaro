package database

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// Only this cache schema version is read. It stores shared blocks and ordered
// block lists, each file's and folder's native modified time, and for File
// History versions each file's Kopia object ID plus each Restic snapshot's diff
// marks. A Restic entry's size and every backup's size are stored as the text
// engines.FormatBytes writes, in decimal units ("2.2 MB"); version 6 caches
// hold the binary units of earlier builds ("2.1 MiB"). The two must never be
// mixed in one cache: a Restic entry's size text is part of the key of the
// cached file-version rows, and a backup's size text is compared on its
// snapshot header when the cache is reconciled, so old text next to new text
// would make every header look changed. Older formats are never migrated:
// they lack facts that only the vault can give or hold text in another
// format, so a cache of an older version is thrown away and rebuilt from the
// vault like a missing one, whatever vault it was bound to, since nothing in
// it is used (see openMetadataCache and DiscardOutdatedMetadataCache). A newer
// version is refused and left untouched.
const CurrentMetadataCacheSchemaVersion = 7

var (
	ErrMetadataCacheIncompatible = errors.New("vault metadata cache schema is incompatible")
	// errMetadataCacheOutdated marks a cache written in an older schema version.
	// It wraps ErrMetadataCacheIncompatible so every caller that only checks for
	// an unusable cache keeps working.
	errMetadataCacheOutdated = fmt.Errorf("%w: written by an older version", ErrMetadataCacheIncompatible)
	ErrMetadataCacheBinding  = errors.New("vault metadata cache binding does not match the authoritative database")
)

type MetadataReadAuthority struct {
	RepositoryID       string
	CanonicalIdentity  string
	Binding            string
	RequiredGeneration int64
}

type MetadataCacheCleanupError struct{ Err error }

func (e *MetadataCacheCleanupError) Error() string {
	return "vault metadata cache requires cleanup: " + e.Err.Error()
}
func (e *MetadataCacheCleanupError) Unwrap() error { return e.Err }

type metadataCacheHandle struct {
	path    string
	binding string
	writer  *sql.DB
	readers *sql.DB
	// Protected by metadataCaches: admitted cache queries must drain before
	// replacement closes pools, including callers waiting to begin a transaction.
	readersInUse int
	readsDrained chan struct{}
}

type metadataCacheOpening struct {
	done chan struct{}
}

var metadataCaches = struct {
	sync.Mutex
	handles    map[string]*metadataCacheHandle
	opening    map[string]*metadataCacheOpening
	closing    map[string]bool
	rebuilding map[string]bool
	retired    map[metadataCacheRetirement]struct{}
	readers    chan struct{}
	closingAll bool
	closeDone  chan struct{}
}{
	handles:    map[string]*metadataCacheHandle{},
	opening:    map[string]*metadataCacheOpening{},
	closing:    map[string]bool{},
	rebuilding: map[string]bool{},
	retired:    map[metadataCacheRetirement]struct{}{},
	readers:    make(chan struct{}, maxReadConnections),
}

type metadataCacheRetirement struct {
	path    string
	binding string
}

var removeMetadataCacheFile = os.Remove
var metadataValidationBeforeFinalSourceCheck = func(string) {}

func LoadMetadataReadAuthority(ctx context.Context, db *sql.DB, repositoryID string) (MetadataReadAuthority, error) {
	var authority MetadataReadAuthority
	err := db.QueryRowContext(ctx, `SELECT id,canonical_identity,metadata_cache_binding,required_generation
		FROM repositories WHERE id=?`, repositoryID).Scan(
		&authority.RepositoryID, &authority.CanonicalIdentity, &authority.Binding, &authority.RequiredGeneration,
	)
	if err != nil {
		return authority, err
	}
	if err := validateMetadataAuthority(authority); err != nil {
		return MetadataReadAuthority{}, err
	}
	return authority, nil
}

func validateMetadataAuthority(authority MetadataReadAuthority) error {
	if !strictCanonicalUUID(authority.RepositoryID) || !strictCanonicalUUID(authority.Binding) || authority.RequiredGeneration < 0 {
		return fmt.Errorf("authoritative vault metadata binding is invalid")
	}
	return nil
}

func strictCanonicalUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed != uuid.Nil && parsed.String() == value
}

func metadataRootForDatabase(db *sql.DB) (string, error) {
	value, ok := openedDatabasePaths.Load(db)
	if !ok {
		return "", fmt.Errorf("database path is unavailable for metadata cache routing")
	}
	path, ok := value.(string)
	if !ok || path == "" {
		return "", fmt.Errorf("database path is unavailable for metadata cache routing")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(absolute), "metadata"), nil
}

func MetadataCachePath(db *sql.DB, repositoryID string) (string, error) {
	if !strictCanonicalUUID(repositoryID) {
		return "", fmt.Errorf("vault UUID must be canonical")
	}
	root, err := metadataRootForDatabase(db)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, repositoryID+".db"), nil
}

func metadataCacheForAuthority(db *sql.DB, authority MetadataReadAuthority) (*metadataCacheHandle, error) {
	if err := validateMetadataAuthority(authority); err != nil {
		return nil, err
	}
	path, err := MetadataCachePath(db, authority.RepositoryID)
	if err != nil {
		return nil, err
	}
	for {
		metadataCaches.Lock()
		if _, retired := metadataCaches.retired[metadataCacheRetirement{path: path, binding: authority.Binding}]; retired {
			metadataCaches.Unlock()
			return nil, ErrMetadataCacheBinding
		}
		if metadataCaches.closingAll || metadataCaches.closing[path] || metadataCaches.rebuilding[path] {
			metadataCaches.Unlock()
			return nil, ErrMetadataIndexingUnavailable
		}
		if handle := metadataCaches.handles[path]; handle != nil {
			metadataCaches.Unlock()
			if handle.binding != authority.Binding {
				return nil, ErrMetadataCacheBinding
			}
			return handle, nil
		}
		if opening := metadataCaches.opening[path]; opening != nil {
			done := opening.done
			metadataCaches.Unlock()
			<-done
			continue
		}
		// Cold WAL admission can copy a large cache. Reserve only this vault's
		// opening while disk I/O runs outside the registry lock, so unrelated
		// vaults remain usable. Close/removal must drain this reservation before
		// retiring its handle; unlocking without it would race publication.
		opening := &metadataCacheOpening{done: make(chan struct{})}
		metadataCaches.opening[path] = opening
		metadataCaches.Unlock()

		handle, err := openMetadataCache(path, authority.RepositoryID, authority.Binding)
		if err == nil {
			if cleanupErr := removeAbandonedMetadataReplacements(path); cleanupErr != nil {
				err = errors.Join(&MetadataCacheCleanupError{Err: cleanupErr}, closeMetadataCacheHandle(handle))
				handle = nil
			}
		}
		metadataCaches.Lock()
		delete(metadataCaches.opening, path)
		if err == nil {
			metadataCaches.handles[path] = handle
		}
		close(opening.done)
		metadataCaches.Unlock()
		return handle, err
	}
}

func metadataCacheWriter(ctx context.Context, mainDB *sql.DB, repositoryID string) (*metadataCacheHandle, MetadataReadAuthority, error) {
	authority, err := LoadMetadataReadAuthority(ctx, mainDB, repositoryID)
	if err != nil {
		return nil, authority, err
	}
	handle, err := metadataCacheForAuthority(mainDB, authority)
	return handle, authority, err
}

func metadataCacheReader(ctx context.Context, mainDB *sql.DB, repositoryID string) (*metadataCacheHandle, MetadataReadAuthority, func(), error) {
	authority, err := LoadMetadataReadAuthority(ctx, mainDB, repositoryID)
	if err != nil {
		return nil, authority, nil, err
	}
	return metadataCacheReaderForAuthority(ctx, mainDB, authority)
}

func metadataCacheReaderForAuthority(ctx context.Context, mainDB *sql.DB, authority MetadataReadAuthority) (*metadataCacheHandle, MetadataReadAuthority, func(), error) {
	handle, err := metadataCacheForAuthority(mainDB, authority)
	if err != nil {
		return nil, authority, nil, err
	}
	select {
	case metadataCaches.readers <- struct{}{}:
	case <-ctx.Done():
		return nil, authority, nil, ctx.Err()
	}
	metadataCaches.Lock()
	if metadataCaches.closing[handle.path] || metadataCaches.rebuilding[handle.path] || metadataCaches.handles[handle.path] != handle {
		metadataCaches.Unlock()
		<-metadataCaches.readers
		return nil, authority, nil, ErrMetadataIndexingUnavailable
	}
	handle.readersInUse++
	metadataCaches.Unlock()
	return handle, authority, func() {
		metadataCaches.Lock()
		handle.readersInUse--
		if handle.readersInUse == 0 && handle.readsDrained != nil {
			close(handle.readsDrained)
			handle.readsDrained = nil
		}
		metadataCaches.Unlock()
		<-metadataCaches.readers
	}, nil
}

func openMetadataCache(path, repositoryID, binding string) (*metadataCacheHandle, error) {
	initialize, err := preflightMetadataCache(path, repositoryID, binding)
	if errors.Is(err, errMetadataCacheOutdated) {
		// A cache from an older version lacks facts only the vault can give
		// (see CurrentMetadataCacheSchemaVersion), so it is treated as missing:
		// its files are removed and an empty current cache takes their place.
		// The next refresh then indexes the vault again. The version is checked
		// before the binding, so an older cache goes whatever vault it was
		// bound to: nothing in it is used. Only an older version gets this; a
		// newer or corrupt cache, or a current-version cache bound to another
		// vault, is still refused and left alone, because this version can't
		// tell what it holds or whose it is.
		if err := discardOutdatedMetadataCache(path); err != nil {
			return nil, err
		}
		initialize, err = preflightMetadataCache(path, repositoryID, binding)
	}
	if err != nil {
		return nil, err
	}
	writer, err := openSQLiteWriter(path)
	if err != nil {
		return nil, err
	}
	closeOnError := func(err error) (*metadataCacheHandle, error) {
		_ = closeMetadataCacheDatabase(writer)
		return nil, err
	}
	if initialize {
		if err := initializeMetadataCache(writer, repositoryID, binding); err != nil {
			return closeOnError(err)
		}
	}
	if err := validateOpenedMetadataCache(writer, repositoryID, binding); err != nil {
		return closeOnError(err)
	}
	readers, err := OpenReadPool(path)
	if err != nil {
		return closeOnError(err)
	}
	return &metadataCacheHandle{path: path, binding: binding, writer: writer, readers: readers}, nil
}

// discardOutdatedMetadataCache removes an older-version cache and its SQLite
// sidecars. It runs while this vault's cache opening is reserved, so nothing
// in this process has the file open. The -wal and -shm files go first: a
// leftover WAL next to the new empty cache would be replayed into it by
// SQLite. If removal stops part way (a crash, or Windows refusing a file that
// another program holds), what is left is still the old cache or nothing,
// and the next open finds it outdated or missing and simply tries again.
func discardOutdatedMetadataCache(path string) error {
	base, err := sqliteSidecarBase(path)
	if err != nil {
		return err
	}
	for _, candidate := range []string{base + "-wal", base + "-shm", path} {
		if err := removeMetadataCacheFile(candidate); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove metadata cache of an older version: %w", err)
		}
	}
	log.Printf("Removed the File History cache %s written by an older Replicaro version; the vault will be indexed again", filepath.Base(path))
	return nil
}

// DiscardOutdatedMetadataCache removes the vault's cache when its main file
// says it was written in an older cache schema (1 to CurrentMetadataCacheSchemaVersion-1),
// and reports whether it did. Startup calls it once per vault, before anything
// opens a cache, and queues a refresh for each vault whose cache it removed.
//
// It reads only the main file's SQLite header (the format magic and
// user_version), without opening the database or copying anything, so a
// leftover WAL costs nothing here, whatever its size. Nothing in an older cache
// is used, so it is removed whichever vault it was bound to; the binding is not
// checked. Anything else is left alone and not queued: a missing or empty file,
// version 0, a current or newer version, or a file that isn't SQLite. The
// normal open path (openMetadataCache) decides about those, so a newer,
// corrupt, or wrongly bound current-version cache is refused there and left
// untouched. Because the old cache is gone after the
// first start whether or not its vault could be reached, the rebuild is queued
// once after an upgrade and never again.
func DiscardOutdatedMetadataCache(db *sql.DB, repositoryID string) (bool, error) {
	path, err := MetadataCachePath(db, repositoryID)
	if err != nil {
		return false, err
	}
	version, ok, err := metadataCacheFileVersion(path)
	if err != nil || !ok || version < 1 || version >= CurrentMetadataCacheSchemaVersion {
		return false, err
	}
	return true, discardOutdatedMetadataCache(path)
}

// metadataCacheFileVersion reads user_version from a SQLite main file's
// header: 4 bytes, big-endian, at offset 60, after the 16-byte format magic.
// ok is false for a missing, short, non-regular or non-SQLite file.
func metadataCacheFileVersion(path string) (version uint32, ok bool, err error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return 0, false, err
	}
	if !info.Mode().IsRegular() {
		return 0, false, nil
	}
	var header [64]byte
	if _, err := io.ReadFull(file, header[:]); errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return 0, false, nil
	} else if err != nil {
		return 0, false, err
	}
	if string(header[:16]) != "SQLite format 3\x00" {
		return 0, false, nil
	}
	return binary.BigEndian.Uint32(header[60:64]), true, nil
}

func closeMetadataCacheDatabase(db *sql.DB) error {
	if db == nil {
		return nil
	}
	openedDatabasePaths.Delete(db)
	return db.Close()
}

func closeMetadataCacheHandle(handle *metadataCacheHandle) error {
	if handle == nil {
		return nil
	}
	return errors.Join(closeMetadataCacheDatabase(handle.readers), closeMetadataCacheDatabase(handle.writer))
}

func preflightMetadataCache(path, repositoryID, binding string) (bool, error) {
	// Like the main database, the cache lives wherever the app-data root
	// resolves to and links are followed. There's deliberately no ancestor
	// walk or link refusal on the cache or its -wal/-shm files: those would
	// only resist tampering by anyone with access to the computer (other
	// local users or software running on it), which Replicaro doesn't try to
	// defend against, and they break symlinked home or state folders. The
	// regular-file checks just turn a stray folder into a clear error.
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("metadata cache path must be a regular file")
	}
	// If the cache file is a link, SQLite keeps -wal and -shm next to its
	// target; see sqliteSidecarBase.
	base, err := sqliteSidecarBase(path)
	if err != nil {
		return false, err
	}
	var walInfo, shmInfo os.FileInfo
	for _, sidecar := range []string{base + "-wal", base + "-shm"} {
		sidecarInfo, sidecarErr := os.Stat(sidecar)
		if errors.Is(sidecarErr, os.ErrNotExist) {
			continue
		}
		if sidecarErr != nil {
			return false, sidecarErr
		}
		if !sidecarInfo.Mode().IsRegular() {
			return false, fmt.Errorf("metadata cache sidecar must be a regular file")
		}
		if sidecar == base+"-wal" {
			walInfo = sidecarInfo
		} else {
			shmInfo = sidecarInfo
		}
	}
	if info.Size() == 0 {
		if walInfo != nil && walInfo.Size() > 0 {
			return false, fmt.Errorf("%w: empty database has a nonempty WAL", ErrMetadataCacheIncompatible)
		}
		return true, nil
	}
	// Even a compatible main file can have an incompatible schema or binding
	// committed only in WAL. Inspect every nonempty WAL, not just caches whose
	// immutable main-file version check would fail.
	if walInfo != nil && walInfo.Size() > 0 {
		if err := validateMetadataCacheWALCopy(path, base, info, walInfo, shmInfo, repositoryID, binding); err != nil {
			return false, err
		}
		return false, nil
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return false, err
	}
	readOnlyDSN := sqliteReadOnlyDSN(absolute, runtime.GOOS)
	separator := "?"
	if strings.Contains(readOnlyDSN, "?") {
		separator = "&"
	}
	readOnly, err := sql.Open("sqlite", readOnlyDSN+separator+"immutable=1")
	if err != nil {
		return false, err
	}
	readOnly.SetMaxOpenConns(1)
	defer readOnly.Close()
	if err := validatePreflightMetadataCache(readOnly, repositoryID, binding); err != nil {
		return false, err
	}
	return false, nil
}

func validatePreflightMetadataCache(db *sql.DB, repositoryID, binding string) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version > 0 && version < CurrentMetadataCacheSchemaVersion {
		return fmt.Errorf("%w (found version %d, expected %d)", errMetadataCacheOutdated, version, CurrentMetadataCacheSchemaVersion)
	}
	if version != CurrentMetadataCacheSchemaVersion {
		return fmt.Errorf("%w (found version %d, expected %d)", ErrMetadataCacheIncompatible, version, CurrentMetadataCacheSchemaVersion)
	}
	if err := validateMetadataCacheSchema(db); err != nil {
		return fmt.Errorf("%w: %v", ErrMetadataCacheIncompatible, err)
	}
	var storedRepositoryID, storedBinding string
	if err := db.QueryRow(`SELECT id,metadata_cache_binding FROM repositories WHERE database_id=1`).Scan(&storedRepositoryID, &storedBinding); err != nil {
		return fmt.Errorf("%w: %v", ErrMetadataCacheIncompatible, err)
	}
	if storedRepositoryID != repositoryID || storedBinding != binding {
		return ErrMetadataCacheBinding
	}
	return nil
}

// An immutable SQLite read ignores WAL, while an ordinary read-only open can
// create or update SHM beside the original. Neither can admit a crashed cache
// while preserving the no-touch guarantee for rejected schema/binding state.
// Let SQLite interpret a private DB/WAL copy instead; omit SHM so SQLite rebuilds
// that disposable index. This copy is only for validation, never publication or
// recovery. Reusing exact-vault replacement names lets existing cleanup reclaim
// crash residue without adding a second recovery lifecycle. sidecarBase is
// what the -wal and -shm names hang off (see sqliteSidecarBase); the private
// copy itself always goes next to path. Both copied files are created 0600 and,
// on Windows, inherit the application folder's protected DACL, so they get no
// ACL write of their own.
func validateMetadataCacheWALCopy(path, sidecarBase string, databaseInfo, walInfo, shmInfo os.FileInfo, repositoryID, binding string) (resultErr error) {
	temporaryFile, err := os.CreateTemp(filepath.Dir(path), metadataReplacementPrefix(path)+"*.db")
	if err != nil {
		return err
	}
	temporary := temporaryFile.Name()
	defer func() {
		var cleanupErr error
		for _, candidate := range []string{temporary + "-shm", temporary + "-wal", temporary} {
			if err := removeMetadataCacheFile(candidate); err != nil && !errors.Is(err, os.ErrNotExist) {
				cleanupErr = errors.Join(cleanupErr, err)
			}
		}
		if cleanupErr != nil {
			resultErr = errors.Join(resultErr, &MetadataCacheCleanupError{Err: cleanupErr})
		}
	}()
	databaseSource, openedDatabaseInfo, err := openMetadataValidationSource(path, databaseInfo)
	if err != nil {
		_ = temporaryFile.Close()
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, databaseSource.Close()) }()
	walSource, openedWALInfo, err := openMetadataValidationSource(sidecarBase+"-wal", walInfo)
	if err != nil {
		_ = temporaryFile.Close()
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, walSource.Close()) }()
	if err := copyMetadataValidationFile(databaseSource, openedDatabaseInfo, temporaryFile); err != nil {
		_ = temporaryFile.Close()
		return err
	}
	if err := temporaryFile.Close(); err != nil {
		return err
	}

	temporaryWAL := temporary + "-wal"
	walFile, err := os.OpenFile(temporaryWAL, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := copyMetadataValidationFile(walSource, openedWALInfo, walFile); err != nil {
		_ = walFile.Close()
		return err
	}
	if err := walFile.Close(); err != nil {
		return err
	}
	if err := metadataValidationSourceUnchanged(databaseSource, openedDatabaseInfo); err != nil {
		return err
	}
	if err := metadataValidationSourceUnchanged(walSource, openedWALInfo); err != nil {
		return err
	}

	absolute, err := filepath.Abs(temporary)
	if err != nil {
		return err
	}
	readOnly, err := sql.Open("sqlite", sqliteReadOnlyDSN(absolute, runtime.GOOS))
	if err != nil {
		return err
	}
	readOnly.SetMaxOpenConns(1)
	if err := validatePreflightMetadataCache(readOnly, repositoryID, binding); err != nil {
		return errors.Join(err, readOnly.Close())
	}
	if err := readOnly.Close(); err != nil {
		return err
	}
	metadataValidationBeforeFinalSourceCheck(path)
	if err := metadataValidationPathUnchanged(path, openedDatabaseInfo); err != nil {
		return err
	}
	if err := metadataValidationPathUnchanged(sidecarBase+"-wal", openedWALInfo); err != nil {
		return err
	}
	return metadataValidationOptionalPathUnchanged(sidecarBase+"-shm", shmInfo)
}

func openMetadataValidationSource(path string, expected os.FileInfo) (*os.File, os.FileInfo, error) {
	source, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	opened, err := source.Stat()
	if err != nil {
		_ = source.Close()
		return nil, nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(expected, opened) || opened.Size() != expected.Size() {
		_ = source.Close()
		return nil, nil, fmt.Errorf("metadata cache changed before validation copy")
	}
	return source, opened, nil
}

func copyMetadataValidationFile(source *os.File, opened os.FileInfo, destination *os.File) error {
	written, err := io.Copy(destination, io.LimitReader(source, opened.Size()+1))
	if err != nil {
		return err
	}
	if written != opened.Size() {
		return fmt.Errorf("metadata cache changed during validation copy")
	}
	// This private copy is read immediately and discarded, never published or
	// reused after a crash. Ordinary writes make its bytes available to SQLite;
	// forcing them to stable storage adds I/O without protecting recovery state.
	// The caller still checks Close errors. Original DB/WAL durability is separate.
	return nil
}

func metadataValidationSourceUnchanged(source *os.File, opened os.FileInfo) error {
	after, err := source.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(opened, after) || after.Size() != opened.Size() || !after.ModTime().Equal(opened.ModTime()) {
		return fmt.Errorf("metadata cache changed during validation copy")
	}
	return nil
}

// The two checks below re-stat the cache files after the validation copy,
// following links like the rest of preflight. The database and WAL are
// compared against the stat of the file we opened and copied; -shm isn't
// copied, so it's compared against what preflight saw. A mismatch means the
// files changed while we copied them and the copy can't be trusted. These are
// consistency checks, not a defense against deliberate file swaps.
func metadataValidationPathUnchanged(path string, opened os.FileInfo) error {
	current, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("metadata cache changed after validation copy: %w", err)
	}
	if !current.Mode().IsRegular() || !os.SameFile(opened, current) ||
		current.Size() != opened.Size() || !current.ModTime().Equal(opened.ModTime()) {
		return fmt.Errorf("metadata cache changed after validation copy")
	}
	return nil
}

func metadataValidationOptionalPathUnchanged(path string, opened os.FileInfo) error {
	current, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		if opened == nil {
			return nil
		}
		return fmt.Errorf("metadata cache changed after validation copy")
	}
	if err != nil {
		return err
	}
	if opened == nil || !current.Mode().IsRegular() || !os.SameFile(opened, current) ||
		current.Size() != opened.Size() || !current.ModTime().Equal(opened.ModTime()) {
		return fmt.Errorf("metadata cache changed after validation copy")
	}
	return nil
}

func validateOpenedMetadataCache(db *sql.DB, repositoryID, binding string) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version != CurrentMetadataCacheSchemaVersion {
		return ErrMetadataCacheIncompatible
	}
	if err := validateMetadataCacheSchema(db); err != nil {
		return fmt.Errorf("%w: %v", ErrMetadataCacheIncompatible, err)
	}
	var storedRepositoryID, storedBinding string
	if err := db.QueryRow(`SELECT id,metadata_cache_binding FROM repositories WHERE database_id=1`).Scan(&storedRepositoryID, &storedBinding); err != nil {
		return err
	}
	if storedRepositoryID != repositoryID || storedBinding != binding {
		return ErrMetadataCacheBinding
	}
	return nil
}

func validateMetadataCacheSchema(db *sql.DB) error {
	expected := make(map[string]string)
	for _, statement := range strings.Split(metadataCacheSchema, ";") {
		normalized := strings.Join(strings.Fields(statement), " ")
		if normalized == "" {
			continue
		}
		fields := strings.Fields(normalized)
		if len(fields) < 3 || fields[0] != "CREATE" {
			return fmt.Errorf("current cache schema definition contains an invalid statement")
		}
		expected[fields[2]] = normalized
	}

	rows, err := db.Query(`SELECT name,sql FROM sqlite_schema
		WHERE name NOT GLOB 'sqlite_*' AND sql IS NOT NULL ORDER BY name`)
	if err != nil {
		return err
	}
	defer rows.Close()
	actual := make(map[string]string)
	for rows.Next() {
		var name, definition string
		if err := rows.Scan(&name, &definition); err != nil {
			return err
		}
		actual[name] = strings.Join(strings.Fields(definition), " ")
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(actual) != len(expected) {
		return fmt.Errorf("current cache schema catalog contains %d objects, expected %d", len(actual), len(expected))
	}
	for name, definition := range expected {
		if actual[name] != definition {
			return fmt.Errorf("current cache schema object %q does not match", name)
		}
	}
	return nil
}

func initializeMetadataCache(db *sql.DB, repositoryID, binding string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(metadataCacheSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO repositories(database_id,id,metadata_cache_binding) VALUES (1,?,?)`, repositoryID, binding); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO metadata_cache_state(repository_database_id) VALUES (1)`); err != nil {
		return err
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, CurrentMetadataCacheSchemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

func CloseMetadataCaches(ctx context.Context) error {
	var shutdownDone chan struct{}
	for {
		metadataCaches.Lock()
		if !metadataCaches.closingAll {
			metadataCaches.closingAll = true
			metadataCaches.closeDone = make(chan struct{})
			shutdownDone = metadataCaches.closeDone
			metadataCaches.Unlock()
			break
		}
		pendingClose := metadataCaches.closeDone
		metadataCaches.Unlock()
		select {
		case <-pendingClose:
			continue
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	for {
		metadataCaches.Lock()
		opening := make([]<-chan struct{}, 0, len(metadataCaches.opening))
		for _, pending := range metadataCaches.opening {
			opening = append(opening, pending.done)
		}
		metadataCaches.Unlock()
		if len(opening) == 0 {
			break
		}
		for _, done := range opening {
			select {
			case <-done:
			case <-ctx.Done():
				metadataCaches.Lock()
				metadataCaches.closingAll = false
				metadataCaches.closeDone = nil
				close(shutdownDone)
				metadataCaches.Unlock()
				return ctx.Err()
			}
		}
	}
	metadataCaches.Lock()
	handles := make([]*metadataCacheHandle, 0, len(metadataCaches.handles))
	for path, handle := range metadataCaches.handles {
		metadataCaches.closing[path] = true
		handles = append(handles, handle)
		delete(metadataCaches.handles, path)
	}
	metadataCaches.Unlock()
	var result error
	for _, handle := range handles {
		result = errors.Join(result, closeMetadataCacheHandle(handle))
	}
	metadataCaches.Lock()
	for _, handle := range handles {
		delete(metadataCaches.closing, handle.path)
	}
	metadataCaches.closingAll = false
	metadataCaches.closeDone = nil
	close(shutdownDone)
	metadataCaches.Unlock()
	return result
}

type metadataCacheRemoval struct {
	path         string
	repositoryID string
	binding      string
}

func closeMetadataCacheForRemoval(db *sql.DB, repositoryID string) (metadataCacheRemoval, func(), error) {
	authority, err := LoadMetadataReadAuthority(context.Background(), db, repositoryID)
	if err != nil {
		return metadataCacheRemoval{}, nil, err
	}
	path, err := MetadataCachePath(db, repositoryID)
	if err != nil {
		return metadataCacheRemoval{}, nil, err
	}
	removal := metadataCacheRemoval{path: path, repositoryID: repositoryID, binding: authority.Binding}
	var handle *metadataCacheHandle
	for {
		metadataCaches.Lock()
		if metadataCaches.closingAll {
			metadataCaches.Unlock()
			return metadataCacheRemoval{}, nil, ErrMetadataIndexingUnavailable
		}
		if opening := metadataCaches.opening[path]; opening != nil {
			done := opening.done
			metadataCaches.Unlock()
			<-done
			continue
		}
		metadataCaches.closing[path] = true
		handle = metadataCaches.handles[path]
		delete(metadataCaches.handles, path)
		metadataCaches.Unlock()
		break
	}
	if handle != nil {
		if err := closeMetadataCacheHandle(handle); err != nil {
			metadataCaches.Lock()
			delete(metadataCaches.closing, path)
			metadataCaches.Unlock()
			return metadataCacheRemoval{}, nil, err
		}
	}
	var once sync.Once
	reopen := func() {
		once.Do(func() {
			metadataCaches.Lock()
			delete(metadataCaches.closing, path)
			metadataCaches.Unlock()
		})
	}
	return removal, reopen, nil
}

func retireMetadataCacheRemoval(removal metadataCacheRemoval) {
	metadataCaches.Lock()
	metadataCaches.retired[metadataCacheRetirement{path: removal.path, binding: removal.binding}] = struct{}{}
	metadataCaches.Unlock()
}

func removeClosedMetadataCache(removal metadataCacheRemoval) error {
	path := removal.path
	defer func() {
		metadataCaches.Lock()
		delete(metadataCaches.closing, path)
		metadataCaches.Unlock()
	}()
	initialize, err := preflightMetadataCache(path, removal.repositoryID, removal.binding)
	if err != nil {
		return err
	}
	// Removal may be the first cache operation after a crashed rebuild. Once
	// the vault row is gone, lazy opening can no longer reclaim its replacements.
	// Reuse the same exact-vault cleanup under the existing removal admission.
	var result error
	if !initialize {
		// A never-created cache may not even have a metadata directory. Only
		// a validated original establishes this crash-recovery cleanup scope.
		result = removeAbandonedMetadataReplacements(path)
	}
	// os.Remove never follows a link. When the cache file is a link, only the
	// link goes away. Its target and the -wal/-shm files SQLite keeps next to
	// the target are left alone, so the target stays a complete database for
	// whoever put it there; deleting just the target's WAL would silently drop
	// committed transactions from a file we aren't removing. The -wal/-shm
	// names next to the link are in Replicaro's own metadata folder and are
	// removed if anything is there. Missing files are fine; anything else is
	// reported.
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		if err := removeMetadataCacheFile(candidate); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, err)
		}
	}
	return result
}

// The File History version columns:
//
//   - metadata_cache_file_versions.mtime is the entry's native modified time in
//     whole Unix seconds, for files and folders: Restore browse and File
//     History show a folder's time as well as a file's. It is the time the
//     engine lists for the item; for a folder, Restic's is the folder's own
//     and Kopia's is the newest within it (see the Kopia directory reader,
//     engines/kopia_directory.go). object_id is
//     Kopia's object ID of a file (always empty for Restic and for folders,
//     which File History keeps per backup date, so a change inside a folder
//     doesn't add a row for the folder).
//   - metadata_cache_snapshot_marks lists, for one Restic snapshot, each file
//     `restic diff --metadata` marked with anything but a removal against the
//     snapshot in marks_base. A snapshot with marks_base NULL has no marks yet.
//     Removals aren't stored: a file missing from the snapshot already says so.
//     Marks belong to the snapshot row and go with it (ON DELETE CASCADE), so
//     re-indexing or dropping a snapshot always clears them.
//   - metadata_cache_snapshots.native_source_paths is the JSON array of the
//     source paths the snapshot's native header lists, sorted and without
//     repeats (Restic's `paths`, Kopia's one source path). Together with the
//     computer group it places the snapshot in its chain of File History
//     versions. It is not the indexing roots: an untagged Restic snapshot is
//     indexed beneath "/" whatever folders it holds.
const metadataCacheSchema = `
CREATE TABLE repositories (
	database_id INTEGER PRIMARY KEY CHECK (database_id=1),
	id TEXT NOT NULL UNIQUE,
	metadata_cache_binding TEXT NOT NULL UNIQUE
);
CREATE TABLE metadata_cache_entry_sets (
	entry_set_id INTEGER PRIMARY KEY AUTOINCREMENT,
	repository_database_id INTEGER NOT NULL CHECK (repository_database_id=1),
	engine TEXT NOT NULL,
	native_identity TEXT NOT NULL,
	root_scope TEXT NOT NULL,
	block_list_id INTEGER REFERENCES metadata_cache_block_lists(block_list_id),
	UNIQUE (repository_database_id,engine,native_identity,root_scope),
	UNIQUE (repository_database_id,entry_set_id),
	FOREIGN KEY (repository_database_id) REFERENCES repositories(database_id) ON DELETE CASCADE
);
CREATE TABLE metadata_cache_snapshots (
 native_root_type TEXT NOT NULL DEFAULT '' CHECK (native_root_type IN ('','d','f')),
	repository_database_id INTEGER NOT NULL CHECK (repository_database_id=1),
	snapshot_id TEXT NOT NULL,
	entry_set_id INTEGER,
	timestamp TEXT NOT NULL,
	size TEXT NOT NULL DEFAULT '',
	total_file_count INTEGER CHECK(total_file_count>=0),
	duration TEXT NOT NULL DEFAULT '',
	source TEXT NOT NULL DEFAULT '',
	marker_status TEXT NOT NULL DEFAULT 'missing' CHECK (marker_status IN ('missing','valid','malformed','duplicate','conflicting')),
	marker_profile_uuid TEXT NOT NULL DEFAULT '',
	marker_job_uuid TEXT NOT NULL DEFAULT '',
	presentation_client_uuid TEXT NOT NULL DEFAULT '',
	presentation_machine_label TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','ready','failed')),
	indexed_at TEXT NOT NULL DEFAULT '',
	error TEXT NOT NULL DEFAULT '',
	marks_base TEXT,
	native_source_paths TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (repository_database_id,snapshot_id),
	FOREIGN KEY (repository_database_id) REFERENCES repositories(database_id) ON DELETE CASCADE,
	FOREIGN KEY (repository_database_id,entry_set_id) REFERENCES metadata_cache_entry_sets(repository_database_id,entry_set_id)
);
CREATE TABLE metadata_cache_snapshot_roots (
	repository_database_id INTEGER NOT NULL CHECK (repository_database_id=1),
	snapshot_id TEXT NOT NULL,
	root_ordinal INTEGER NOT NULL CHECK (root_ordinal>=0),
	path TEXT NOT NULL,
	normalized_root TEXT NOT NULL,
	native_user TEXT NOT NULL DEFAULT '',
	native_host TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (repository_database_id,snapshot_id,root_ordinal),
	UNIQUE (repository_database_id,snapshot_id,normalized_root),
	UNIQUE (repository_database_id,snapshot_id,path,native_user,native_host),
	FOREIGN KEY (repository_database_id,snapshot_id) REFERENCES metadata_cache_snapshots(repository_database_id,snapshot_id) ON DELETE CASCADE
);
CREATE TABLE metadata_cache_files (
	file_id INTEGER PRIMARY KEY AUTOINCREMENT,
	repository_database_id INTEGER NOT NULL CHECK (repository_database_id=1),
	root TEXT NOT NULL,
	parent_path TEXT NOT NULL DEFAULT '',
	name TEXT NOT NULL,
	UNIQUE (repository_database_id,root,parent_path,name),
	UNIQUE (repository_database_id,file_id),
	FOREIGN KEY (repository_database_id) REFERENCES repositories(database_id) ON DELETE CASCADE
);
CREATE TABLE metadata_cache_file_versions (
	file_version_id INTEGER PRIMARY KEY AUTOINCREMENT,
	file_id INTEGER NOT NULL,
	mode TEXT NOT NULL DEFAULT '',
	size TEXT NOT NULL DEFAULT '',
	is_dir INTEGER NOT NULL CHECK (is_dir IN (0,1)),
	mtime INTEGER NOT NULL DEFAULT 0,
	object_id TEXT NOT NULL DEFAULT '',
	UNIQUE (file_id,mode,size,is_dir,mtime,object_id),
	UNIQUE (file_id,file_version_id),
	FOREIGN KEY (file_id) REFERENCES metadata_cache_files(file_id) ON DELETE CASCADE
);
CREATE TABLE metadata_cache_snapshot_marks (
	repository_database_id INTEGER NOT NULL CHECK (repository_database_id=1),
	snapshot_id TEXT NOT NULL,
	file_id INTEGER NOT NULL REFERENCES metadata_cache_files(file_id) ON DELETE CASCADE,
	PRIMARY KEY (repository_database_id,snapshot_id,file_id),
	FOREIGN KEY (repository_database_id,snapshot_id) REFERENCES metadata_cache_snapshots(repository_database_id,snapshot_id) ON DELETE CASCADE
) WITHOUT ROWID;
CREATE TABLE metadata_cache_blocks (
 block_id INTEGER PRIMARY KEY AUTOINCREMENT,
 digest BLOB NOT NULL
);
CREATE TABLE metadata_cache_block_entries (
 block_id INTEGER NOT NULL REFERENCES metadata_cache_blocks(block_id) ON DELETE CASCADE,
 file_id INTEGER NOT NULL,
 file_version_id INTEGER NOT NULL,
 PRIMARY KEY(block_id,file_id),
 FOREIGN KEY(file_id,file_version_id) REFERENCES metadata_cache_file_versions(file_id,file_version_id)
) WITHOUT ROWID;
CREATE TABLE metadata_cache_block_lists (
 block_list_id INTEGER PRIMARY KEY AUTOINCREMENT,
 digest BLOB NOT NULL,
 content BLOB NOT NULL,
 UNIQUE(digest,content)
);
CREATE TABLE metadata_cache_block_list_items (
 block_list_id INTEGER NOT NULL REFERENCES metadata_cache_block_lists(block_list_id) ON DELETE CASCADE,
 bucket INTEGER NOT NULL CHECK(bucket>=0),
 block_id INTEGER NOT NULL REFERENCES metadata_cache_blocks(block_id),
 PRIMARY KEY(block_list_id,bucket)
) WITHOUT ROWID;
CREATE VIEW metadata_cache_entries AS
 SELECT s.entry_set_id,e.file_id,e.file_version_id
 FROM metadata_cache_entry_sets s
 JOIN metadata_cache_block_list_items l ON l.block_list_id=s.block_list_id
 JOIN metadata_cache_block_entries e ON e.block_id=l.block_id;
CREATE TABLE metadata_cache_state (
	repository_database_id INTEGER PRIMARY KEY CHECK (repository_database_id=1),
	bucket_count INTEGER NOT NULL DEFAULT 100 CHECK(bucket_count>=100),
	last_reconciled TEXT NOT NULL DEFAULT '',
	applied_generation INTEGER NOT NULL DEFAULT 0 CHECK (applied_generation>=0),
	FOREIGN KEY (repository_database_id) REFERENCES repositories(database_id) ON DELETE CASCADE
);
CREATE TABLE metadata_cache_failures (
	repository_database_id INTEGER PRIMARY KEY CHECK (repository_database_id=1),
	attempted_at TEXT NOT NULL,
	error TEXT NOT NULL DEFAULT '',
	FOREIGN KEY (repository_database_id) REFERENCES repositories(database_id) ON DELETE CASCADE
);
CREATE TABLE metadata_cache_revisions (
	repository_database_id INTEGER PRIMARY KEY CHECK (repository_database_id=1),
	revision INTEGER NOT NULL DEFAULT 0 CHECK (revision>=0),
	FOREIGN KEY (repository_database_id) REFERENCES repositories(database_id) ON DELETE CASCADE
);
CREATE INDEX metadata_cache_blocks_digest ON metadata_cache_blocks(digest);
CREATE INDEX metadata_cache_block_entries_file_version ON metadata_cache_block_entries(file_id,file_version_id);
CREATE INDEX metadata_cache_block_list_items_block ON metadata_cache_block_list_items(block_id);
CREATE INDEX metadata_cache_entry_sets_list ON metadata_cache_entry_sets(block_list_id);
CREATE INDEX metadata_cache_snapshots_entry_set ON metadata_cache_snapshots(repository_database_id,entry_set_id);
CREATE INDEX metadata_cache_snapshots_status ON metadata_cache_snapshots(repository_database_id,status);
CREATE INDEX metadata_cache_snapshot_marks_file ON metadata_cache_snapshot_marks(file_id);
`
