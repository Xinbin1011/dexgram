//go:build !windows

package main

import (
	"os/exec"
	"testing"
)

func TestDetachLongRunningCommandIsNoop(t *testing.T) {
	cmd := exec.Command("dexgram")

	detachLongRunningCommand(cmd)

	if cmd.SysProcAttr != nil {
		t.Fatalf("SysProcAttr=%#v, want nil", cmd.SysProcAttr)
	}
}
