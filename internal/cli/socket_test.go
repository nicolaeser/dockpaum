//go:build unix

package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/nicolaeser/dockpaum/internal/config"
)

func TestIsPermissionDenied(t *testing.T) {
	dial := &net.OpError{Op: "dial", Net: "unix", Err: os.NewSyscallError("connect", syscall.EACCES)}
	cases := []struct {
		err  error
		want bool
	}{
		{fmt.Errorf("host local: ping: %w", dial), true},
		{errors.New("permission denied while trying to connect to the Docker daemon socket"), true},
		{fmt.Errorf("host local: ping: %w", &net.OpError{Op: "dial", Net: "unix", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}), false},
		{nil, false},
	}
	for _, c := range cases {
		if got := isPermissionDenied(c.err); got != c.want {
			t.Errorf("isPermissionDenied(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

func TestUnixSockets(t *testing.T) {
	hosts := []config.HostConfig{
		{Name: "local"},
		{Name: "explicit", Endpoint: "unix:///run/user/1000/docker.sock"},
		{Name: "remote", Endpoint: "tcp://nas:2376"},
		{Name: "dup", Endpoint: "unix:///var/run/docker.sock"},
	}
	got := unixSockets(hosts, "")
	want := []string{"/var/run/docker.sock", "/run/user/1000/docker.sock"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("unixSockets = %q, want %q", got, want)
	}
	if got := unixSockets([]config.HostConfig{{Name: "local"}}, "unix:///custom.sock"); fmt.Sprint(got) != "[/custom.sock]" {
		t.Errorf("DOCKER_HOST not honored: %q", got)
	}
	if got := unixSockets([]config.HostConfig{{Name: "local"}}, "tcp://127.0.0.1:2375"); len(got) != 0 {
		t.Errorf("tcp DOCKER_HOST yielded sockets %q", got)
	}
}

func TestInspectSocketReadsOwner(t *testing.T) {
	p := filepath.Join(t.TempDir(), "docker.sock")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	a := inspectSocket(p)
	if a.statErr != nil || !a.known {
		t.Fatalf("inspectSocket = %+v", a)
	}
	if a.ownerUID != os.Getuid() || a.uid != os.Getuid() || a.groups[0] != os.Getgid() {
		t.Errorf("owner/process identity wrong: %+v", a)
	}
	missing := inspectSocket(filepath.Join(t.TempDir(), "none.sock"))
	if missing.statErr == nil || missing.known {
		t.Errorf("missing socket = %+v", missing)
	}
}

func TestSocketHint(t *testing.T) {
	base := socketAccess{path: "/var/run/docker.sock", known: true, ownerUID: 0, ownerGID: 998, mode: 0o660, uid: 1000, groups: []int{1000}}
	desktop := base
	desktop.ownerGID = 0
	desktop.mode = 0o660
	member := base
	member.groups = []int{1000, 998}
	member.mode = 0o600
	blocked := base
	blocked.groups = []int{1000, 998}
	owner := base
	owner.uid = 0
	owner.mode = 0o060
	rootOutside := base
	rootOutside.uid = 0
	rootOutside.groups = []int{0}
	rootOutside.ownerUID = 1000
	rootOutside.mode = 0o600
	unknown := socketAccess{path: "/var/run/docker.sock", uid: 1000, groups: []int{1000}}
	statFailed := unknown
	statFailed.statErr = fs.ErrPermission

	cases := []struct {
		name string
		a    socketAccess
		want []string
		not  []string
	}{
		{"usual: not in the socket's group", base, []string{
			"/var/run/docker.sock", "belongs to group 998", "uid 1000 with groups 1000",
			"Run dockpaum as root", "remove user:", `group_add: ["998"]`, "--group-add 998", "SupplementaryGroups",
		}, []string{"Docker Desktop"}},
		{"docker desktop", desktop, []string{`group_add: ["0"]`, "Docker Desktop"}, nil},
		{"group member without group permission", member, []string{"mode 0600 gives group 998 no read/write access", "0660", "run dockpaum as root"}, []string{"group_add"}},
		{"permissions fine but denied", blocked, []string{"mode 0660 allow it", "security module", "label=disable"}, []string{"group_add"}},
		{"running as the owner without permission", owner, []string{"runs as its owner (uid 0)", "mode 0060", "0660"}, []string{"group_add"}},
		{"root outside the socket's group", rootOutside, []string{"uid 0", `group_add: ["998"]`}, []string{"as root"}},
		{"stat failed", statFailed, []string{"cannot inspect it", "Run dockpaum as root", "group_add", "SupplementaryGroups"}, nil},
		{"owner unknown", unknown, []string{"Run dockpaum as root", "the socket's group"}, nil},
	}
	for _, c := range cases {
		hint := c.a.hint()
		for _, w := range c.want {
			if !strings.Contains(hint, w) {
				t.Errorf("%s: hint lacks %q:\n%s", c.name, w, hint)
			}
		}
		for _, n := range c.not {
			if strings.Contains(hint, n) {
				t.Errorf("%s: hint contains %q:\n%s", c.name, n, hint)
			}
		}
	}
	args := base.logArgs()
	if fmt.Sprint(args) == "" || !strings.Contains(fmt.Sprint(args), "socket_gid 998") || !strings.Contains(fmt.Sprint(args), troubleshootingURL) {
		t.Errorf("log args = %v", args)
	}
}
