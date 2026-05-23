//go:build windows

package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

func isDexgramCodexAppServerProcess(pid int, expectedPath string) bool {
	script := fmt.Sprintf(`
$proc = Get-CimInstance Win32_Process -Filter "ProcessId = %d" -ErrorAction SilentlyContinue
if ($null -eq $proc) { return }
[pscustomobject]@{
  Name = $proc.Name
  ExecutablePath = $proc.ExecutablePath
  CommandLine = $proc.CommandLine
} | ConvertTo-Json -Compress
`, pid)
	cmd := exec.Command("powershell.exe", "-NoProfile", "-Command", script)
	hideCommandWindow(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) == "" {
		return false
	}
	var record struct {
		Name           string
		ExecutablePath string
		CommandLine    string
	}
	if err := json.Unmarshal(out, &record); err != nil {
		return false
	}
	if !strings.EqualFold(record.Name, "codex.exe") {
		return false
	}
	if expectedPath != "" && !samePath(record.ExecutablePath, expectedPath) {
		return false
	}
	commandLine := strings.ToLower(record.CommandLine)
	return strings.Contains(commandLine, " app-server ") &&
		strings.Contains(commandLine, "--listen") &&
		strings.Contains(commandLine, "stdio://")
}

func samePath(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA == nil {
		a = absA
	}
	if errB == nil {
		b = absB
	}
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}
