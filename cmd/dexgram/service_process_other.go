//go:build !windows

package main

import "os/exec"

func detachLongRunningCommand(cmd *exec.Cmd) {
}

func hideCommandWindow(cmd *exec.Cmd) {
}
