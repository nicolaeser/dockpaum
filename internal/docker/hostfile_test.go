package docker

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nicolaeser/dockpaum/internal/docker/dockertest"
)

func runLocally(t *testing.T) func(dockertest.RunRequest) (int, string, string, bool) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh to run the file reader script")
	}
	return func(r dockertest.RunRequest) (int, string, string, bool) {
		if len(r.Entrypoint) < 3 || r.Entrypoint[0] != "/bin/sh" || r.Entrypoint[1] != "-c" {
			return 127, "", "unexpected entrypoint", true
		}
		args := append(append([]string{"-c"}, r.Entrypoint[2:]...), r.Cmd...)
		for i := 4; i < len(args); i++ {
			for _, m := range r.Mounts {
				if !m.ReadOnly {
					return 126, "", "writable mount", true
				}
				rest, ok := strings.CutPrefix(args[i], m.Target)
				if !ok || (rest != "" && !strings.HasPrefix(rest, "/")) {
					continue
				}
				src := m.Source
				if info, err := os.Lstat(src); err == nil && !info.IsDir() {
					if resolved, err := filepath.EvalSymlinks(src); err == nil {
						src = resolved
					}
				}
				args[i] = src + rest
			}
		}
		var stdout, stderr bytes.Buffer
		cmd := exec.Command(sh, args...) // #nosec G204 -- test runs the reader script with fixed arguments
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		code := 0
		if err := cmd.Run(); err != nil {
			code = 127
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				code = ee.ExitCode()
			}
		}
		return code, stdout.String(), stderr.String(), true
	}
}

func readerDaemon(t *testing.T, run func(dockertest.RunRequest) (int, string, string, bool)) (*dockertest.Daemon, *Client) {
	t.Helper()
	d := dockertest.New(t, "1.47")
	d.AddImage(dockertest.Image{ID: "self", Run: run}, "example.com/dockpaum:1")
	d.CheckBindSources(func(p string) bool {
		_, err := os.Stat(p)
		return err == nil
	})
	return d, testClient(t, d)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReadHostFilesThroughAHelperContainer(t *testing.T) {
	d, c := readerDaemon(t, runLocally(t))
	root := t.TempDir()
	dir := func(name string) string {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(p, 0o750); err != nil {
			t.Fatal(err)
		}
		return p
	}
	plain, empty, absLink, relLink, dangling, isDir, large, tricky := dir("plain"), dir("empty"), dir("abs"), dir("rel"),
		dir("dangling"), dir("isdir"), dir("large"), dir("tricky")
	gone := filepath.Join(root, "gone")
	writeFile(t, filepath.Join(plain, ".pull_token"), "ghcr.io bot secret-plain\n")
	writeFile(t, filepath.Join(root, "shared", "token"), "ghcr.io/acme acme secret-shared\n")
	if err := os.Symlink(filepath.Join(root, "shared", "token"), filepath.Join(absLink, ".pull_token")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../shared/token", filepath.Join(relLink, ".pull_token")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "nowhere"), filepath.Join(dangling, ".pull_token")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(isDir, ".pull_token"), 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(large, ".pull_token"), strings.Repeat("x", 65))
	trickyContent := "line one\nffff file\n\nno newline at the end"
	writeFile(t, filepath.Join(tricky, ".pull_token"), trickyContent)

	got, err := c.ReadHostFiles(context.Background(), FileReader{
		Image: "sha256:self", Name: ".pull_token", MaxSize: 64,
		Labels: map[string]string{FileReaderLabel: "test", "dockpaum.enable": "false"},
	}, []string{plain, empty, gone, absLink, relLink, dangling, isDir, large, tricky, plain})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{plain: "ghcr.io bot secret-plain\n", absLink: "ghcr.io/acme acme secret-shared\n",
		relLink: "ghcr.io/acme acme secret-shared\n", tricky: trickyContent}
	for d, content := range want {
		if got[d].Err != nil || string(got[d].Data) != content {
			t.Errorf("%s = %q, %v; want %q", d, got[d].Data, got[d].Err, content)
		}
	}
	for _, d := range []string{empty, gone, dangling} {
		if !errors.Is(got[d].Err, fs.ErrNotExist) {
			t.Errorf("%s: err = %v, want not exist", d, got[d].Err)
		}
	}
	if err := got[isDir].Err; err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("directory: err = %v", err)
	}
	if err := got[large].Err; err == nil || !strings.Contains(err.Error(), "larger than the 64 bytes") {
		t.Errorf("large file: err = %v", err)
	}

	for _, n := range d.Names() {
		if strings.HasPrefix(n, FileReaderPrefix) {
			t.Errorf("file reader container %s was left behind", n)
		}
	}
	creates := d.Calls("POST", "/containers/create")
	if len(creates) < 2 {
		t.Fatalf("%d creates, want a directory pass and a symlink pass", len(creates))
	}
	for _, call := range creates {
		req := createBody(t, call)
		if req.HostConfig.NetworkMode != "none" || !req.HostConfig.ReadonlyRootfs || req.User != "0" ||
			req.Labels[FileReaderLabel] != "test" || req.Labels["dockpaum.enable"] != "false" {
			t.Errorf("reader container %+v %+v", req.Config, req.HostConfig)
		}
		for _, m := range req.HostConfig.Mounts {
			if !m.ReadOnly || string(m.Type) != "bind" {
				t.Errorf("mount %+v must be a read-only bind", m)
			}
		}
	}
}

func TestReadHostFilesReportsAFailingHelper(t *testing.T) {
	_, c := readerDaemon(t, func(dockertest.RunRequest) (int, string, string, bool) {
		return 127, "", "exec /bin/sh: no such file or directory\n", true
	})
	dir := t.TempDir()
	_, err := c.ReadHostFiles(context.Background(), FileReader{Image: "sha256:self", Name: ".pull_token", MaxSize: 64}, []string{dir})
	if err == nil || !strings.Contains(err.Error(), "exited with code 127") || !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseReaderOutputIgnoresForeignMarkers(t *testing.T) {
	nonce := "0123456789abcdef0123456789abcdef"
	other := "fedcba9876543210fedcba9876543210"
	out := nonce + "\n" + "a\n" + other + " missing\n\n" + nonce + " file\n" + nonce + "\n\n" + nonce + " missing\n"
	got, err := parseReaderOutput([]byte(out), nonce, []string{"/a", "/b"})
	if err != nil {
		t.Fatal(err)
	}
	if got["/a"].kind != "file" || string(got["/a"].data) != "a\n"+other+" missing\n" || got["/b"].kind != "missing" {
		t.Fatalf("got %+v", got)
	}
	if _, err := parseReaderOutput([]byte(nonce+"\nhalf"), nonce, []string{"/a"}); err == nil {
		t.Fatal("truncated output must be an error")
	}
}

func TestHostPath(t *testing.T) {
	for _, tc := range [][3]string{
		{"/srv/app", ".pull_token", "/srv/app/.pull_token"},
		{"/srv/app/", ".pull_token", "/srv/app/.pull_token"},
		{`C:\Users\me\app`, ".pull_token", `C:\Users\me\app\.pull_token`},
	} {
		if got := HostPath(tc[0], tc[1]); got != tc[2] {
			t.Errorf("HostPath(%q, %q) = %q, want %q", tc[0], tc[1], got, tc[2])
		}
	}
}
