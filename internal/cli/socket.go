package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/nicolaeser/dockpaum/internal/config"
)

const defaultDockerSocket = "/var/run/docker.sock"

func isPermissionDenied(err error) bool {
	return err != nil && (errors.Is(err, fs.ErrPermission) || strings.Contains(err.Error(), "permission denied"))
}

func unixSockets(hosts []config.HostConfig, dockerHost string) []string {
	var out []string
	for _, h := range hosts {
		ep := h.Endpoint
		if ep == "" {
			ep = dockerHost
		}
		if ep == "" {
			ep = "unix://" + defaultDockerSocket
		}
		if p, ok := strings.CutPrefix(ep, "unix://"); ok && p != "" && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}

type socketAccess struct {
	path     string
	statErr  error
	known    bool
	ownerUID int
	ownerGID int
	mode     fs.FileMode
	uid      int
	groups   []int
}

func inspectSocket(path string) socketAccess {
	a := socketAccess{path: path, uid: os.Getuid(), groups: []int{os.Getgid()}}
	if extra, err := os.Getgroups(); err == nil {
		for _, g := range extra {
			if !slices.Contains(a.groups, g) {
				a.groups = append(a.groups, g)
			}
		}
	}
	fi, err := os.Stat(path)
	if err != nil {
		a.statErr = err
		return a
	}
	a.mode = fi.Mode().Perm()
	a.ownerUID, a.ownerGID, a.known = fileOwner(fi)
	return a
}

func (a socketAccess) hint() string {
	who := fmt.Sprintf("dockpaum runs as uid %d with groups %s", a.uid, joinInts(a.groups))
	switch {
	case a.statErr != nil:
		return fmt.Sprintf("cannot access the Docker socket %s and cannot inspect it (%v); %s. %s",
			a.path, a.statErr, who, a.grantGroup("the socket's group with group_add"))
	case !a.known:
		return fmt.Sprintf("cannot access the Docker socket %s; %s. %s", a.path, who, a.grantGroup("the socket's group with group_add"))
	}
	inGroup := slices.Contains(a.groups, a.ownerGID)
	switch {
	case a.uid == a.ownerUID && a.mode&0o600 == 0o600, inGroup && a.mode&0o060 == 0o060:
		return fmt.Sprintf("cannot access the Docker socket %s although its owner %d:%d and mode %04o allow it (%s): a security module is likely blocking it; on SELinux hosts add security_opt: [\"label=disable\"] to the dockpaum service",
			a.path, a.ownerUID, a.ownerGID, uint32(a.mode), who)
	case a.uid == a.ownerUID:
		return fmt.Sprintf("cannot access the Docker socket %s: dockpaum runs as its owner (uid %d) but its mode %04o grants the owner no read/write access; restore the socket's usual mode 0660",
			a.path, a.uid, uint32(a.mode))
	case inGroup:
		msg := fmt.Sprintf("cannot access the Docker socket %s: its mode %04o gives group %d no read/write access (%s). Restore the socket's usual mode 0660",
			a.path, uint32(a.mode), a.ownerGID, who)
		if a.uid != 0 {
			msg += ", or run dockpaum as root, the image's default user (remove user: from the service)"
		}
		return msg
	}
	msg := fmt.Sprintf("cannot access the Docker socket %s: it belongs to group %d and %s. %s",
		a.path, a.ownerGID, who, a.grantGroup(fmt.Sprintf("group %d: group_add: [\"%d\"] (docker run: --group-add %d)", a.ownerGID, a.ownerGID, a.ownerGID)))
	if a.ownerGID == 0 {
		msg += ". Group 0 is the root group, which owns the socket on Docker Desktop"
	}
	return msg
}

func (a socketAccess) grantGroup(group string) string {
	add := "Add " + group + "; under systemd, add it to SupplementaryGroups"
	if a.uid == 0 {
		return add
	}
	return "Run dockpaum as root, the image's default user (remove user: from the service), or a" + add[1:]
}

func (a socketAccess) logArgs() []any {
	args := []any{"socket", a.path, "uid", a.uid, "groups", a.groups, "help", troubleshootingURL}
	if a.known {
		args = append(args, "socket_gid", a.ownerGID, "socket_uid", a.ownerUID, "socket_mode", fmt.Sprintf("%04o", uint32(a.mode)))
	}
	return args
}

func joinInts(xs []int) string {
	s := make([]string, len(xs))
	for i, x := range xs {
		s[i] = strconv.Itoa(x)
	}
	return strings.Join(s, ",")
}
