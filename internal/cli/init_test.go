package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nicolaeser/dockpaum/internal/config"
)

func TestInitCommand(t *testing.T) {
	clearConfigEnv(t)
	def := config.DefaultConfig()
	code, out, errOut := execute(t, "init")
	if code != exitOK || out != string(def) || errOut != "" {
		t.Errorf("init: exit %d, stderr %q, stdout is the default config: %v", code, errOut, out == string(def))
	}
	if first, _, _ := strings.Cut(out, "\n"); first != "# yaml-language-server: $schema="+config.SchemaURL {
		t.Errorf("first line = %q, want the schema line", first)
	}
	if code, out, _ := execute(t, "init", "--output", "-"); code != exitOK || out != string(def) {
		t.Errorf("init --output -: exit %d", code)
	}

	target := filepath.Join(t.TempDir(), "conf", "dockpaum.yml")
	code, out, errOut = execute(t, "init", "--output", target)
	if code != exitOK || !strings.Contains(out, "wrote the default config to "+target) || !strings.Contains(out, "dockpaum validate --config "+target) {
		t.Fatalf("init --output: exit %d\n%s%s", code, out, errOut)
	}
	if got, err := os.ReadFile(target); err != nil || !bytes.Equal(got, def) {
		t.Fatalf("written file differs from the default config (%v)", err)
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(target); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("mode = %v (%v), want 0600", fi.Mode().Perm(), err)
		}
	}

	code, out, errOut = execute(t, "init", "--output", target)
	if code != exitFailure || out != "" || !strings.Contains(errOut, "already exists; pass --force") {
		t.Errorf("init over an existing file: exit %d\n%s%s", code, out, errOut)
	}
	if err := os.WriteFile(target, []byte("dry_run: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := execute(t, "init", "--output", target); code != exitFailure {
		t.Errorf("init over an edited file: exit %d", code)
	}
	if got, _ := os.ReadFile(target); string(got) != "dry_run: true\n" {
		t.Error("init without --force changed the existing file")
	}
	code, _, errOut = execute(t, "init", "--output", target, "--force")
	if code != exitOK {
		t.Fatalf("init --force: exit %d %s", code, errOut)
	}
	if got, _ := os.ReadFile(target); !bytes.Equal(got, def) {
		t.Error("init --force did not replace the file")
	}
	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(target); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("mode after --force = %v (%v), want 0600", fi.Mode().Perm(), err)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(target), ".dockpaum-init-*")); len(left) > 0 {
		t.Errorf("temporary files left: %v", left)
	}
	if code, _, errOut := execute(t, "init", "--output", filepath.Dir(target), "--force"); code != exitFailure || !strings.Contains(errOut, "is a directory") {
		t.Errorf("init --force onto a directory: exit %d %q", code, errOut)
	}

	if code, _, _ := execute(t, "init", "extra"); code != exitUsage {
		t.Errorf("init with an argument: exit %d, want 2", code)
	}
}
