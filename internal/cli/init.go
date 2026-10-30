package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/nicolaeser/dockpaum/internal/config"
)

type initOptions struct {
	output string
	force  bool
}

func runInit(inv *invocation, stdout, stderr io.Writer) int {
	o := inv.initCfg
	body := config.DefaultConfig()
	if o.output == "" || o.output == "-" {
		if _, err := stdout.Write(body); err != nil {
			printf(stderr, "init: %v\n", err)
			return exitFailure
		}
		return exitOK
	}
	if err := writeNewConfig(o.output, body, o.force); err != nil {
		if errors.Is(err, fs.ErrExist) {
			printf(stderr, "init: %s already exists; pass --force to overwrite it\n", o.output)
		} else {
			printf(stderr, "init: %v\n", err)
		}
		return exitFailure
	}
	printf(stdout, "wrote the default config to %s\n", o.output)
	printf(stdout, "check it with: dockpaum validate --config %s\n", o.output)
	return exitOK
}

func writeNewConfig(target string, body []byte, force bool) error {
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	if !force {
		f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		if _, err := f.Write(body); err != nil {
			_ = f.Close()
			_ = os.Remove(target)
			return err
		}
		return f.Close()
	}
	if fi, err := os.Stat(target); err == nil && fi.IsDir() {
		return fmt.Errorf("%s is a directory", target)
	}
	tmp, err := os.CreateTemp(dir, ".dockpaum-init-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), target)
}
