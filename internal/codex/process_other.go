//go:build !windows

package codex

import "os/exec"

func configureAppServerProcess(_ *exec.Cmd) {}
