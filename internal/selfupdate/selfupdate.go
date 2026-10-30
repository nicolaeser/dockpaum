package selfupdate

import (
	"bufio"
	"context"
	"os"
	"path"
	"regexp"
	"strings"

	"github.com/nicolaeser/dockpaum/internal/docker"
)

const (
	SourceCgroup    = "cgroup"
	SourceMountinfo = "mountinfo"
	SourceHostname  = "hostname"
)

type Candidate struct {
	ID     string
	Source string
}

func Candidates() []Candidate {
	return candidates("/proc/self/cgroup", "/proc/self/mountinfo", os.Getenv("HOSTNAME"))
}

func candidates(cgroupPath, mountinfoPath, hostname string) []Candidate {
	var out []Candidate
	add := func(id, source string) {
		if id == "" {
			return
		}
		for _, c := range out {
			if IsSelf(c.ID, id) {
				return
			}
		}
		out = append(out, Candidate{ID: id, Source: source})
	}
	add(readCgroupContainerID(cgroupPath), SourceCgroup)
	add(readMountinfoContainerID(mountinfoPath), SourceMountinfo)
	if hostnameIDRE.MatchString(hostname) {
		add(hostname, SourceHostname)
	}
	return out
}

func WhoAmI() string {
	if c := Candidates(); len(c) > 0 {
		return c[0].ID
	}
	return ""
}

func InContainer() bool {
	for _, marker := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(marker); err == nil {
			return true
		}
	}
	for _, c := range Candidates() {
		if c.Source != SourceHostname {
			return true
		}
	}
	return false
}

var hostnameIDRE = regexp.MustCompile(`^[0-9a-f]{12,64}$`)

func readCgroupContainerID(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if id := extractDockerID(sc.Text()); id != "" {
			return id
		}
	}
	return ""
}

var containerDirRE = regexp.MustCompile(`(?:^|/)(?:overlay-)?containers/([0-9a-f]{64})(?:/|$)`)

var containerFiles = map[string]bool{"/etc/hostname": true, "/etc/hosts": true, "/etc/resolv.conf": true}

func readMountinfoContainerID(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 || !containerFiles[fields[4]] {
			continue
		}
		if m := containerDirRE.FindStringSubmatch(fields[3]); m != nil {
			return m[1]
		}
	}
	return ""
}

func extractDockerID(line string) string {
	for _, marker := range []string{"docker/", "docker-", "libpod-", "containers/"} {
		if i := strings.LastIndex(line, marker); i >= 0 {
			rest := line[i+len(marker):]
			if id := readHex(rest); len(id) == 64 {
				return id
			}
		}
	}
	return ""
}

func readHex(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			b.WriteRune(r)
			continue
		}
		break
	}
	return b.String()
}

func IsSelf(self, other string) bool {
	if self == "" || other == "" {
		return false
	}
	if self == other {
		return true
	}
	a, b := self, other
	if len(a) > len(b) {
		a, b = b, a
	}
	return strings.HasPrefix(b, a)
}

const imageTitleLabel = "org.opencontainers.image.title"

func IsDockpaum(labels map[string]string, image string) bool {
	if labels[imageTitleLabel] == "dockpaum" {
		return true
	}
	ref := image
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		ref = ref[:i]
	}
	return ref != "" && path.Base(ref) == "dockpaum"
}

func runsDockpaum(fc *docker.FullContainer) bool {
	if fc.Config == nil {
		return false
	}
	if IsDockpaum(fc.Config.Labels, fc.Config.Image) {
		return true
	}
	return len(fc.Config.Entrypoint) > 0 && path.Base(fc.Config.Entrypoint[0]) == "dockpaum"
}

type Identity struct {
	ID     string
	Host   string
	Source string
}

func Identify(ctx context.Context, clients []*docker.Client, cands []Candidate) (Identity, bool) {
	for _, cand := range cands {
		for _, c := range clients {
			fc, err := c.Inspect(ctx, cand.ID)
			if err != nil {
				continue
			}
			if cand.Source == SourceCgroup || runsDockpaum(fc) {
				return Identity{ID: fc.ID, Host: c.Name, Source: cand.Source}, true
			}
			if id, ok := dockpaumJoiner(ctx, c, fc.ID); ok {
				return Identity{ID: id, Host: c.Name, Source: cand.Source}, true
			}
		}
	}
	return Identity{}, false
}

func dockpaumJoiner(ctx context.Context, c *docker.Client, ownerID string) (string, bool) {
	list, err := c.ListContainers(ctx, false)
	if err != nil {
		return "", false
	}
	var found []string
	for _, ct := range docker.NetworkDependents(list, ownerID) {
		if IsDockpaum(ct.Labels, ct.Image) {
			found = append(found, ct.ID)
		}
	}
	if len(found) != 1 {
		return "", false
	}
	return found[0], true
}
