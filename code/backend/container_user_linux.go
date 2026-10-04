//go:build linux

package main

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/local/replicaro/storageidentity"
)

// containerFiles holds the paths the container user setup reads and changes.
// They are fixed by the image; tests point them at a scratch tree.
type containerFiles struct {
	stateVolume string
	// instanceLock is the single-instance lock file inside the state volume.
	// It is the path instanceguard locks for the image's XDG_STATE_HOME.
	instanceLock string
	passwd       string
	group        string
	mountInfo    string
	// mountCheckTimeout bounds the mount access warnings, because a call on
	// an unresponsive network mount can block for minutes or forever. It is
	// usually long enough for a NAS or USB disk to wake from sleep.
	mountCheckTimeout time.Duration
}

var imageContainerFiles = containerFiles{
	stateVolume:       "/var/lib/replicaro",
	instanceLock:      "/var/lib/replicaro/state/replicaro/replicaro.instance",
	passwd:            "/etc/passwd",
	group:             "/etc/group",
	mountInfo:         "/proc/self/mountinfo",
	mountCheckTimeout: 15 * time.Second,
}

// settleContainerUser decides which user the container package runs as and
// switches to it. The image starts as root. Without --no-root Replicaro stays
// root; with it Replicaro switches to the requested user, or 1000:1000. When
// Docker's --user already started it as someone else, that user is kept and
// treated as --no-root.
//
// This runs before app data, the instance lock, or any worker exists, so
// nothing has opened a file that the ownership change or the user switch
// could leave behind with the wrong owner.
func settleContainerUser(noRoot bool, requested *containerUser) error {
	return imageContainerFiles.settleUser(noRoot, requested)
}

func (files containerFiles) settleUser(noRoot bool, requested *containerUser) error {
	started := containerUser{uid: os.Geteuid(), gid: os.Getegid()}
	if started.uid != 0 {
		return files.keepDockerUser(started, requested)
	}
	target := containerUser{}
	if noRoot {
		target = defaultNoRootUser
		if requested != nil {
			target = *requested
		}
	}
	// A second Replicaro started inside a running container (docker exec runs
	// as root now) must not re-own the state the running one is using, so it
	// skips every change here. Asking for the running user, it then stops at
	// the instance lock; otherwise it stops earlier, during app-data setup.
	running, err := files.instanceRunning()
	if err != nil {
		return err
	}
	if running {
		log.Printf("another Replicaro is running with the state folder %s, so its owners are left unchanged", files.stateVolume)
	} else {
		if target.uid != 0 {
			// OpenSSH exits with "No user exists for uid" when the running
			// UID has no passwd entry, which would break SFTP vaults.
			if err := files.addAccount(target); err != nil {
				return err
			}
		}
		if err := files.ownStateVolume(target); err != nil {
			return err
		}
	}
	if target.uid == 0 {
		return nil
	}
	if err := switchToUser(target); err != nil {
		return err
	}
	if running {
		return nil
	}
	log.Printf("running as %s (--no-root)", target)
	// Keep this after switchToUser. Go changes the user on every thread, and
	// a thread left blocked on an unresponsive mount would make that wait
	// forever.
	files.warnAboutMountAccess(target, false)
	return nil
}

// instanceRunning reports whether another process holds the single-instance
// lock, using the same flock as instanceguard. It never creates the file.
func (files containerFiles) instanceRunning() (bool, error) {
	// O_NONBLOCK keeps a FIFO planted at this path from blocking the open.
	file, err := os.OpenFile(files.instanceLock, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check whether Replicaro is already running: %w", err)
	}
	defer file.Close()
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() {
		return false, fmt.Errorf("check whether Replicaro is already running: %s is not a regular file", files.instanceLock)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return true, nil
		}
		return false, fmt.Errorf("check whether Replicaro is already running: %w", err)
	}
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return false, nil
}

// keepDockerUser handles a container started with Docker's --user. Replicaro
// is not root, so it cannot change the state volume's owner or add an account;
// it can only check them and explain what to change.
func (files containerFiles) keepDockerUser(started containerUser, requested *containerUser) error {
	if requested != nil && *requested != started {
		return fmt.Errorf("Docker --user runs Replicaro as %s, but --no-root asks for %s. Remove one of them or make them match", started, *requested)
	}
	info, err := os.Lstat(files.stateVolume)
	if err != nil {
		return fmt.Errorf("inspect the state folder %s: %w", files.stateVolume, err)
	}
	if owner := fileOwner(info); owner.uid != started.uid {
		return fmt.Errorf("Replicaro runs as %s (Docker --user), but its state folder %s is owned by %s. Remove --user and use --no-root=%s so Replicaro can fix this itself, or give %s ownership of the state folder", started, files.stateVolume, owner, started, started)
	}
	listed, err := idListed(files.passwd, started.uid)
	if err != nil {
		return err
	}
	if !listed {
		// --user without a group runs with group 0; don't suggest keeping it.
		suggestion := started.String()
		if started.gid == 0 {
			suggestion = fmt.Sprintf("%d:<gid>", started.uid)
		}
		log.Printf("warning: UID %d has no account in the container, so SFTP vaults that use OpenSSH will fail. Remove Docker --user and use --no-root=%s instead", started.uid, suggestion)
	}
	log.Printf("running as %s (Docker --user)", started)
	files.warnAboutMountAccess(started, true)
	return nil
}

// addAccount adds passwd and group entries for user when its IDs have none.
// The entries match the image's own replicaro account. They are written to
// the container's filesystem, not the state volume, so a recreated container
// starts from the image's files and adds them again.
func (files containerFiles) addAccount(user containerUser) error {
	listed, err := idListed(files.group, user.gid)
	if err != nil {
		return err
	}
	if !listed {
		if err := appendLine(files.group, fmt.Sprintf("replicaro-%d:x:%d:", user.gid, user.gid)); err != nil {
			return err
		}
	}
	listed, err = idListed(files.passwd, user.uid)
	if err != nil {
		return err
	}
	if !listed {
		return appendLine(files.passwd, fmt.Sprintf("replicaro-%d:x:%d:%d:Replicaro:/home/replicaro:/sbin/nologin", user.uid, user.uid, user.gid))
	}
	return nil
}

// idListed reports whether an entry in a passwd or group file has this
// numeric ID. Both formats keep the ID in the third field.
func idListed(path string, id int) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	want := strconv.Itoa(id)
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) >= 3 && fields[2] == want {
			return true, nil
		}
	}
	return false, nil
}

func appendLine(path, line string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		line = "\n" + line
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	_, writeErr := file.WriteString(line + "\n")
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return fmt.Errorf("add an account to %s: %w", path, err)
	}
	return nil
}

// changeOwnerAt changes the owner of name inside the open folder dir without
// following a symlink. Tests replace it to interrupt a walk.
var changeOwnerAt = func(dir int, name string, user containerUser) error {
	return unix.Fchownat(dir, name, user.uid, user.gid, unix.AT_SYMLINK_NOFOLLOW)
}

// beforeOpenFolder runs between inspecting a folder and opening it. It does
// nothing; tests replace it to swap the folder for a symlink at that moment.
var beforeOpenFolder = func(path string) {}

// ownStateVolume gives user ownership of everything in the state volume. It
// does nothing when the top folder already belongs to user, so it costs one
// stat on an ordinary start and walks the tree only after the user changed,
// for example on the first start after an upgrade from the 10001 image.
//
// The top folder is changed last: if the walk is interrupted, the top folder
// still has the old owner and the next start with the same user walks the
// whole tree again. The walk opens each folder relative to its already open
// parent and never follows a symlink, so a folder swapped for a symlink while
// it runs can't lead it outside the volume. Anything mounted inside the
// volume (say a host folder bind-mounted over its home folder) is skipped,
// because it is not Replicaro's to re-own.
func (files containerFiles) ownStateVolume(user containerUser) error {
	root := files.stateVolume
	dir, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
		return fmt.Errorf("the state folder %s is not a folder", root)
	}
	if err != nil {
		return fmt.Errorf("open the state folder %s: %w", root, err)
	}
	defer unix.Close(dir)
	var stat unix.Stat_t
	if err := unix.Fstat(dir, &stat); err != nil {
		return fmt.Errorf("inspect the state folder %s: %w", root, err)
	}
	if statOwner(stat) == user {
		return nil
	}
	mounted, err := files.mountsBelow(root)
	if err != nil {
		return err
	}
	log.Printf("giving %s ownership of the state folder %s", user, root)
	if err := reownChildren(dir, root, user, mounted); err != nil {
		return fmt.Errorf("give %s ownership of the state folder %s: %w", user, root, err)
	}
	if err := unix.Fchown(dir, user.uid, user.gid); err != nil {
		return fmt.Errorf("give %s ownership of the state folder %s: %w", user, root, err)
	}
	return nil
}

// reownChildren gives user ownership of everything inside the open folder
// dir, whose path is path. Each folder's contents are changed before the
// folder itself.
func reownChildren(dir int, path string, user containerUser, mounted map[string]bool) error {
	names, err := folderNames(dir)
	if err != nil {
		return fmt.Errorf("list %s: %w", path, err)
	}
	for _, name := range names {
		child := path + "/" + name
		if mounted[child] {
			log.Printf("leaving %s and everything in it unchanged, because it is a separate mount", child)
			continue
		}
		var stat unix.Stat_t
		if err := unix.Fstatat(dir, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return fmt.Errorf("inspect %s: %w", child, err)
		}
		if stat.Mode&unix.S_IFMT == unix.S_IFDIR {
			beforeOpenFolder(child)
			sub, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
			if err != nil {
				return fmt.Errorf("open %s: %w", child, err)
			}
			err = reownChildren(sub, child, user, mounted)
			unix.Close(sub)
			if err != nil {
				return err
			}
		}
		if statOwner(stat) == user {
			continue
		}
		if err := changeOwnerAt(dir, name, user); err != nil {
			return fmt.Errorf("change the owner of %s: %w", child, err)
		}
	}
	return nil
}

// folderNames lists the open folder dir without closing it.
func folderNames(dir int) ([]string, error) {
	copied, err := unix.Dup(dir)
	if err != nil {
		return nil, err
	}
	folder := os.NewFile(uintptr(copied), "")
	defer folder.Close()
	return folder.Readdirnames(-1)
}

// mountsBelow returns the mount points strictly inside root.
func (files containerFiles) mountsBelow(root string) (map[string]bool, error) {
	mounts, err := files.readMounts()
	if err != nil {
		return nil, err
	}
	below := map[string]bool{}
	for _, mount := range mounts {
		if strings.HasPrefix(mount.MountPoint, root+"/") {
			below[mount.MountPoint] = true
		}
	}
	return below, nil
}

func (files containerFiles) readMounts() ([]storageidentity.LinuxMount, error) {
	file, err := os.Open(files.mountInfo)
	if err != nil {
		return nil, fmt.Errorf("list the container's mounts: %w", err)
	}
	defer file.Close()
	mounts, err := storageidentity.ParseLinuxMountInfo(file)
	if err != nil {
		return nil, fmt.Errorf("list the container's mounts: %w", err)
	}
	return mounts, nil
}

// switchToUser drops root for good: supplementary groups first, then the
// group, then the user, because only root may change the first two. On Linux,
// Go applies these to every thread of the process, and the programs Replicaro
// starts later inherit the new IDs.
func switchToUser(user containerUser) error {
	if err := syscall.Setgroups([]int{}); err != nil {
		return fmt.Errorf("clear supplementary groups before switching to %s: %w", user, err)
	}
	if err := syscall.Setgid(user.gid); err != nil {
		return fmt.Errorf("switch to group %d: %w", user.gid, err)
	}
	if err := syscall.Setuid(user.uid); err != nil {
		return fmt.Errorf("switch to user %d: %w", user.uid, err)
	}
	if os.Getuid() != user.uid || os.Geteuid() != user.uid || os.Getgid() != user.gid || os.Getegid() != user.gid {
		return fmt.Errorf("switching to %s did not take effect", user)
	}
	if err := syscall.Setuid(0); err == nil {
		return fmt.Errorf("switched to %s but could still switch back to root", user)
	}
	// The kernel marks a process that changed its IDs as not dumpable, which
	// hides its /proc entries even from its own user. Restore the state of a
	// process started directly as this user, as the image did before 1.0.5.
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("mark the process as %s's own: %w", user, err)
	}
	return nil
}

// mountAccessWarning returns the warning for one mounted folder, or "" when
// user can use it. Tests replace it to simulate a mount that never answers.
var mountAccessWarning = func(point string, user containerUser, dockerUser bool) string {
	info, err := os.Stat(point)
	if err != nil || !info.IsDir() {
		return ""
	}
	owner := fileOwner(info)
	if err := unix.Access(point, unix.R_OK|unix.X_OK); err != nil {
		return fmt.Sprintf("warning: %s is owned by %s and %s can't read it. %s", point, owner, user, mountAdvice(owner, dockerUser))
	}
	// A read-only mount is expected for a backup source. access(2) checks
	// permissions before the read-only flag, so ask statfs instead.
	var filesystem unix.Statfs_t
	if err := unix.Statfs(point, &filesystem); err == nil && filesystem.Flags&unix.ST_RDONLY != 0 {
		return ""
	}
	if err := unix.Access(point, unix.W_OK); err != nil {
		return fmt.Sprintf("warning: %s is owned by %s and %s can't write to it. If it is only a backup source, mount it read-only. %s", point, owner, user, mountAdvice(owner, dockerUser))
	}
	return ""
}

// warnAboutMountAccess logs every folder mounted into the container that user
// cannot read, and every writable one it cannot write to. It checks only the
// mounted folders themselves, not what is inside them. These are warnings
// only: a mount may never be used, and a backup or restore that does use it
// still fails with its own error.
//
// Each folder is checked in its own goroutine and startup waits at most
// mountCheckTimeout for all of them. A folder that hasn't answered by then is
// reported and left behind; its call stays blocked in the kernel and its
// result is discarded, as the folder picker does with a folder that doesn't
// respond.
func (files containerFiles) warnAboutMountAccess(user containerUser, dockerUser bool) {
	mounts, err := files.readMounts()
	if err != nil {
		log.Printf("warning: could not check which mounted folders %s can use: %v", user, err)
		return
	}
	var points []string
	for _, mount := range mounts {
		point := mount.MountPoint
		if point == "/" || point == files.stateVolume || strings.HasPrefix(point, files.stateVolume+"/") ||
			underAny(point, "/proc", "/sys", "/dev") {
			continue
		}
		points = append(points, point)
	}
	results := make([]chan string, len(points))
	for index, point := range points {
		result := make(chan string, 1)
		results[index] = result
		go func() { result <- mountAccessWarning(point, user, dockerUser) }()
	}
	deadline := time.NewTimer(files.mountCheckTimeout)
	defer deadline.Stop()
	expired := false
	for index, result := range results {
		if !expired {
			select {
			case warning := <-result:
				logIfSet(warning)
				continue
			case <-deadline.C:
				expired = true
			}
		}
		select {
		case warning := <-result:
			logIfSet(warning)
		default:
			log.Printf("warning: %s did not answer within %s, so Replicaro could not check whether %s can use it", points[index], files.mountCheckTimeout, user)
		}
	}
}

func logIfSet(message string) {
	if message != "" {
		log.Print(message)
	}
}

func mountAdvice(owner containerUser, dockerUser bool) string {
	switch {
	case owner.uid == 0 && dockerUser:
		return "Remove Docker --user to run as root, or give this user access to it."
	case owner.uid == 0:
		return "Remove --no-root to run as root, or give this user access to it."
	case dockerUser:
		return fmt.Sprintf("Use --no-root=%s instead of Docker --user, or remove --user to run as root.", owner)
	default:
		return fmt.Sprintf("Start the container with --no-root=%s, or remove --no-root to run as root.", owner)
	}
}

func underAny(path string, roots ...string) bool {
	for _, root := range roots {
		if path == root || strings.HasPrefix(path, root+"/") {
			return true
		}
	}
	return false
}

func fileOwner(info fs.FileInfo) containerUser {
	stat := info.Sys().(*syscall.Stat_t)
	return containerUser{uid: int(stat.Uid), gid: int(stat.Gid)}
}

func statOwner(stat unix.Stat_t) containerUser {
	return containerUser{uid: int(stat.Uid), gid: int(stat.Gid)}
}
