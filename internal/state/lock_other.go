//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package state

import (
	"os"
	"time"
)

func lockFile(path string, _ time.Duration) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	return func() { _ = f.Close() }, nil
}
