//go:build windows

package main

import (
	"os/exec"
	"testing"
)

func TestDetachLongRunningCommandUsesDetachedProcessGroup(t *testing.T) {
	cmd := exec.Command("dexgram.exe")

	detachLongRunningCommand(cmd)

	if cmd.SysProcAttr == nil {
		t.Fatal("expected SysProcAttr to be set")
	}
	if !cmd.SysProcAttr.HideWindow {
		t.Fatal("expected hidden detached process window")
	}
	flags := cmd.SysProcAttr.CreationFlags
	for _, want := range []uint32{detachedProcess, createNewProcessGroup, createNoWindow} {
		if flags&want == 0 {
			t.Fatalf("CreationFlags=%#x missing %#x", flags, want)
		}
	}
}
