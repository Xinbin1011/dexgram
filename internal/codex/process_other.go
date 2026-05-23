//go:build !windows

package codex

import "os/exec"

func configureCodexProcess(_ *exec.Cmd) {}
