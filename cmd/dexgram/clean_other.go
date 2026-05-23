//go:build !windows

package main

import (
	"fmt"
	"io"
)

func runCleanCommand(io.Writer) error {
	return fmt.Errorf("dexgram clean is only supported on Windows")
}

func cleanDanglingCodexAppServers(io.Writer) (int, error) {
	return 0, fmt.Errorf("Codex app-server cleanup is only supported on Windows")
}
