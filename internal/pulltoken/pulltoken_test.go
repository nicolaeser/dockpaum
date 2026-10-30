package pulltoken

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/moby/moby/api/types/container"

	"github.com/nicolaeser/dockpaum/internal/config"
	"github.com/nicolaeser/dockpaum/internal/docker"
	"github.com/nicolaeser/dockpaum/internal/docker/dockertest"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) take() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.b.String()
	s.b.Reset()
	return out
}

type fixture struct {
	t     *testing.T
	d     *dockertest.Daemon
	c     *docker.Client
	store *Store
	logs  *syncBuffer
	self  Self
}

func connect(t *testing.T, d *dockertest.Daemon) *docker.Client {
	t.Helper()
	clients, err := docker.OpenAll(context.Background(), []config.HostConfig{{Name: "box", Endpoint: d.Endpoint()}}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(clients.Close)
	return clients.List()[0]
}

func newFixture(t *testing.T, inContainer bool) *fixture {
	t.Helper()
	d := dockertest.New(t, "1.47")
	logs := &syncBuffer{}
	f := &fixture{t: t, d: d, c: connect(t, d), logs: logs}
	f.store = New(Options{
		Logger: slog.New(slog.NewTextHandler(logs, nil)), Instance: "test", InContainer: inContainer,
		Local: func(host string) bool { return host == "box" },
	})
	return f
}

func (f *fixture) project(content string) string {
	f.t.Helper()
	dir := f.t.TempDir()
	if content != "" {
		f.write(dir, content)
	}
	return dir
}

func (f *fixture) write(dir, content string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(dir, FileName), []byte(content), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) load(dirs ...string) map[string]*File {
	f.t.Helper()
	var all []docker.Container
	for _, d := range dirs {
		all = append(all, docker.Container{Labels: map[string]string{WorkingDirLabel: d}})
	}
	return f.store.Load(context.Background(), f.c, f.self, all, dirs)
}

func (f *fixture) logins() []string {
	var out []string
	for _, call := range f.d.Calls("POST", "/auth") {
		out = append(out, string(call.Body))
	}
	return out
}

func usernames(file *File) []string {
	var out []string
	for _, c := range file.Credentials {
		out = append(out, c.Auth.Username)
	}
	return out
}

func TestStoreLoadsEachProjectsOwnFile(t *testing.T) {
	f := newFixture(t, false)
	f.d.AcceptLogin("ghcr.io", "shop-bot", "good-shop")
	shop := f.project("ghcr.io shop-bot good-shop\nghcr.io/acme acme-bot expired-acme\n")
	blog := f.project("registry.example.com blog-ci good-blog\n")
	plain := f.project("")

	got := f.load(shop, blog, plain)
	if strings.Join(usernames(got[shop]), ",") != "shop-bot,acme-bot" || strings.Join(usernames(got[blog]), ",") != "blog-ci" {
		t.Fatalf("Load = %+v", got)
	}
	if _, ok := got[plain]; ok {
		t.Fatal("a project without a .pull_token gets no file")
	}
	if src := got[shop].Credentials[1].Source; src != filepath.Join(shop, FileName)+" line 2" {
		t.Fatalf("source %q", src)
	}
	out := f.logs.take()
	for _, want := range []string{
		`msg=".pull_token loaded; the project's containers pull with its credentials"`,
		"file=" + filepath.Join(shop, FileName),
		`msg="registry login with a .pull_token entry succeeded" host=box file=` + filepath.Join(shop, FileName) + " line=1 scope=ghcr.io username=shop-bot",
		`msg="registry login with a .pull_token entry failed; pulls with it fail until it is fixed" host=box file=` + filepath.Join(shop, FileName) + " line=2 scope=ghcr.io/acme username=acme-bot",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("logs lack %q:\n%s", want, out)
		}
	}
	for _, secret := range []string{"good-shop", "expired-acme", "good-blog"} {
		if strings.Contains(out, secret) {
			t.Fatalf("logs leak %s:\n%s", secret, out)
		}
	}
	if !got[shop].Credentials[1].MarkReported() == false {
		t.Fatal("an entry whose login was denied is reported already")
	}
	if got[shop].Credentials[0].MarkReported() == false {
		t.Fatal("an entry whose login succeeded is not reported yet")
	}
}

func TestStoreLogsInAgainOnlyWhenTheFileChanges(t *testing.T) {
	f := newFixture(t, false)
	dir := f.project("ghcr.io bot one\n")
	f.load(dir)
	f.load(dir)
	if n := len(f.logins()); n != 1 {
		t.Fatalf("%d logins for an unchanged file", n)
	}
	f.logs.take()
	f.write(dir, "ghcr.io bot two\n")
	got := f.load(dir)
	if n := len(f.logins()); n != 2 || got[dir].Credentials[0].Auth.Password != "two" {
		t.Fatalf("%d logins, file %+v", n, got[dir])
	}
	if out := f.logs.take(); !strings.Contains(out, `msg=".pull_token changed; the project's containers pull with its new credentials"`) {
		t.Fatalf("logs:\n%s", out)
	}
}

func TestStoreLogsInvalidLinesOncePerChange(t *testing.T) {
	f := newFixture(t, false)
	dir := f.project("ghcr.io bot good\nghcr.io lonely-field\n")
	for i := 0; i < 2; i++ {
		got := f.load(dir)
		if strings.Join(usernames(got[dir]), ",") != "bot" || !strings.Contains(got[dir].Problem, filepath.Join(dir, FileName)) ||
			!strings.Contains(got[dir].Problem, "line 2") {
			t.Fatalf("file %+v", got[dir])
		}
	}
	out := f.logs.take()
	if strings.Count(out, `msg="invalid line in .pull_token; the line is ignored"`) != 1 || !strings.Contains(out, "line=2") {
		t.Fatalf("logs:\n%s", out)
	}
}

func TestStoreKeepsCredentialsWhenTheFileCannotBeRead(t *testing.T) {
	f := newFixture(t, false)
	dir := f.project("ghcr.io bot good\n")
	f.load(dir)
	f.store.read = func(string) ([]byte, error) { return nil, fs.ErrPermission }
	for i := 0; i < 2; i++ {
		got := f.load(dir)
		if strings.Join(usernames(got[dir]), ",") != "bot" || !strings.Contains(got[dir].Problem, "cannot be read: permission denied") {
			t.Fatalf("file %+v", got[dir])
		}
	}
	out := f.logs.take()
	if strings.Count(out, `msg="cannot read .pull_token; the credentials read from it before are kept, if any"`) != 1 {
		t.Fatalf("logs:\n%s", out)
	}
	f.store.read = readLocal
	if got := f.load(dir); got[dir].Problem != "" {
		t.Fatalf("problem %q after the file is readable again", got[dir].Problem)
	}
	if n := len(f.logins()); n != 1 {
		t.Fatalf("%d logins: the content did not change", n)
	}
}

func TestStoreForgetsARemovedFile(t *testing.T) {
	f := newFixture(t, false)
	dir := f.project("ghcr.io bot good\n")
	f.load(dir)
	if err := os.Remove(filepath.Join(dir, FileName)); err != nil {
		t.Fatal(err)
	}
	if got := f.load(dir); got[dir] != nil {
		t.Fatalf("file %+v after removal", got[dir])
	}
	if out := f.logs.take(); !strings.Contains(out, `msg=".pull_token removed; the project's containers use the registry entries of the config again"`) {
		t.Fatalf("logs:\n%s", out)
	}
}

func TestStoreFollowsASymlinkOnTheHost(t *testing.T) {
	f := newFixture(t, false)
	shared := filepath.Join(t.TempDir(), "acme-token")
	if err := os.WriteFile(shared, []byte("ghcr.io/acme acme-bot shared\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := f.project("")
	if err := os.Symlink(shared, filepath.Join(dir, FileName)); err != nil {
		t.Fatal(err)
	}
	if got := f.load(dir); strings.Join(usernames(got[dir]), ",") != "acme-bot" {
		t.Fatalf("file %+v", got[dir])
	}
}

func TestStoreCannotReadARemoteHostFromOutsideAContainer(t *testing.T) {
	f := newFixture(t, false)
	f.store.opts.Local = func(string) bool { return false }
	dir := f.project("ghcr.io bot good\n")
	if got := f.load(dir); len(got) != 0 {
		t.Fatalf("Load = %+v", got)
	}
	f.load(dir)
	if out := f.logs.take(); strings.Count(out, "cannot read .pull_token files on this Docker host") != 1 {
		t.Fatalf("logs:\n%s", out)
	}
}

func runScript(t *testing.T) func(dockertest.RunRequest) (int, string, string, bool) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh to run the file reader script")
	}
	return func(r dockertest.RunRequest) (int, string, string, bool) {
		if len(r.Entrypoint) < 3 {
			return 0, "", "", false
		}
		args := append(append([]string{"-c"}, r.Entrypoint[2:]...), r.Cmd...)
		for i := 4; i < len(args); i++ {
			for _, m := range r.Mounts {
				if rest, ok := strings.CutPrefix(args[i], m.Target); ok && (rest == "" || strings.HasPrefix(rest, "/")) {
					src := m.Source
					if resolved, err := filepath.EvalSymlinks(src); err == nil && rest == "" {
						src = resolved
					}
					args[i] = src + rest
				}
			}
		}
		var stdout, stderr bytes.Buffer
		cmd := exec.Command(sh, args...) // #nosec G204 -- test runs the reader script with fixed arguments
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return ee.ExitCode(), stdout.String(), stderr.String(), true
			}
			return 127, "", err.Error(), true
		}
		return 0, stdout.String(), stderr.String(), true
	}
}

func TestStoreReadsThroughItsOwnImageInAContainer(t *testing.T) {
	f := newFixture(t, true)
	f.d.AddImage(dockertest.Image{ID: "self", Run: runScript(t)}, "example.com/dockpaum:1")
	f.d.CheckBindSources(func(p string) bool { _, err := os.Stat(p); return err == nil })
	dir := f.project("ghcr.io bot good\n")

	if got := f.load(dir); len(got) != 0 {
		t.Fatalf("Load without knowing its own container = %+v", got)
	}
	if out := f.logs.take(); !strings.Contains(out, "dockpaum's own container was not found") {
		t.Fatalf("logs:\n%s", out)
	}

	selfID := f.d.Run("dockpaum", &container.Config{Image: "example.com/dockpaum:1"}, nil, nil)
	f.self = Self{ID: selfID, Clients: []*docker.Client{f.c}}
	got := f.load(dir)
	if strings.Join(usernames(got[dir]), ",") != "bot" {
		t.Fatalf("Load = %+v", got)
	}
	if out := f.logs.take(); !strings.Contains(out, ".pull_token files can be read on this Docker host again") {
		t.Fatalf("logs:\n%s", out)
	}
	for _, n := range f.d.Names() {
		if strings.HasPrefix(n, docker.FileReaderPrefix) {
			t.Fatalf("reader %s left behind", n)
		}
	}

	other := dockertest.New(t, "1.47")
	if got := f.store.Load(context.Background(), connect(t, other), f.self, nil, []string{dir}); len(got) != 0 {
		t.Fatalf("Load on a host without dockpaum's image = %+v", got)
	}
	if out := f.logs.take(); !strings.Contains(out, "dockpaum's image is not present on this Docker host") {
		t.Fatalf("logs:\n%s", out)
	}
}

func TestStoreRemovesLeftoverReaders(t *testing.T) {
	f := newFixture(t, false)
	f.d.AddImage(dockertest.Image{ID: "self"}, "example.com/dockpaum:1")
	leftover := f.d.Create(docker.FileReaderPrefix+"1", &container.Config{Image: "example.com/dockpaum:1", Labels: map[string]string{docker.FileReaderLabel: "test"}}, nil)
	foreign := f.d.Create(docker.FileReaderPrefix+"2", &container.Config{Image: "example.com/dockpaum:1", Labels: map[string]string{docker.FileReaderLabel: "other"}}, nil)
	all, err := f.c.ListContainers(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	f.store.Load(context.Background(), f.c, Self{}, all, nil)
	if _, ok := f.d.Inspect(leftover); ok {
		t.Fatal("a leftover reader of this instance must be removed")
	}
	if _, ok := f.d.Inspect(foreign); !ok {
		t.Fatal("a reader another instance just created must be left alone")
	}
}
