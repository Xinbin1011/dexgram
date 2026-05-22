//go:build windows

package codex

import (
	"os/exec"
	"syscall"
)

const createNewProcessGroup = 0x00000200

func configureAppServerProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: createNewProcessGroup,
	}
}
