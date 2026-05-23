package codex

import (
	"context"
	"os/exec"
)

func codexCommand(ctx context.Context, codexPath string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, codexPath, args...)
	configureCodexProcess(cmd)
	return cmd
}

func appServerCommand(ctx context.Context, codexPath string) *exec.Cmd {
	return codexCommand(ctx, codexPath, "app-server", "--listen", "stdio://")
}
