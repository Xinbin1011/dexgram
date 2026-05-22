//go:build !windows

package codex

import (
	"os/exec"
	"testing"
)

func assertAppServerCommandIsolated(t *testing.T, _ *exec.Cmd) {
	t.Helper()
}
