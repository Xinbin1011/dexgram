//go:build !windows

package codex

import "os/exec"

type appServerProcessGuard struct{}

func guardAppServerProcess(_ *exec.Cmd) (appServerProcessGuard, error) {
	return appServerProcessGuard{}, nil
}

func (appServerProcessGuard) Close() error {
	return nil
}
