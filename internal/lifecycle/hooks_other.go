//go:build !unix

package lifecycle

import "os/exec"

func setupProcessGroup(_ *exec.Cmd) {}
