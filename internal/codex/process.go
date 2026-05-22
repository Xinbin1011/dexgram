package codex

import (
	"context"
	"os/exec"
)

func appServerCommand(ctx context.Context, codexPath string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, codexPath, "app-server", "--listen", "stdio://")
	configureAppServerProcess(cmd)
	return cmd
}
