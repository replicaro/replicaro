package main

import (
	"fmt"
	"strconv"
	"strings"
)

// containerUser is the numeric user and group the container package runs as.
type containerUser struct {
	uid int
	gid int
}

// defaultNoRootUser is the first regular account on most Linux hosts, so
// files Replicaro creates on bind mounts belong to that host user.
var defaultNoRootUser = containerUser{uid: 1000, gid: 1000}

func (user containerUser) String() string {
	return fmt.Sprintf("%d:%d", user.uid, user.gid)
}

// parseContainerUser reads the <uid>:<gid> value of --no-root. Both numbers
// must be canonical decimals. UID 0 is refused because leaving out --no-root
// already means root, and 4294967295 is refused because Linux reserves it as
// "no ID".
func parseContainerUser(value string) (containerUser, error) {
	uidText, gidText, found := strings.Cut(value, ":")
	uid, uidErr := parseContainerID(uidText)
	gid, gidErr := parseContainerID(gidText)
	if !found || uidErr != nil || gidErr != nil {
		return containerUser{}, fmt.Errorf("must be <uid>:<gid>, for example --no-root=1000:1000")
	}
	if uid == 0 {
		return containerUser{}, fmt.Errorf("UID 0 is root; leave out --no-root to run as root")
	}
	return containerUser{uid: uid, gid: gid}, nil
}

func parseContainerID(text string) (int, error) {
	id, err := strconv.ParseUint(text, 10, 32)
	if err != nil || text != strconv.FormatUint(id, 10) || id == 4294967295 {
		return 0, fmt.Errorf("invalid ID %q", text)
	}
	return int(id), nil
}
