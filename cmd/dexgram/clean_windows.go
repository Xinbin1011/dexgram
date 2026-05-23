//go:build windows

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

func runCleanCommand(w io.Writer) error {
	_, err := cleanDanglingCodexAppServers(w)
	return err
}

func cleanDanglingCodexAppServers(w io.Writer) (int, error) {
	lines, err := cleanDanglingCodexAppServerLines()
	if err != nil {
		return 0, err
	}
	if len(lines) == 0 {
		fmt.Fprintln(w, "No dangling Codex app-server processes found.")
		return 0, nil
	}
	for _, line := range lines {
		fmt.Fprintln(w, "killed Codex app-server "+line)
	}
	return len(lines), nil
}

func cleanDanglingCodexAppServerLines() ([]string, error) {
	target := mustServiceExePath()
	script := fmt.Sprintf(`
$target = [System.IO.Path]::GetFullPath(%s)
$current = %d
$dexgramPids = @(
  Get-CimInstance Win32_Process -Filter "Name = 'dexgram.exe'" | Where-Object {
    $_.ProcessId -ne $current -and
    $_.ExecutablePath -and
    ([System.IO.Path]::GetFullPath($_.ExecutablePath) -ieq $target)
  } | ForEach-Object { [int]$_.ProcessId }
)
$matches = Get-CimInstance Win32_Process -Filter "Name = 'codex.exe'" | Where-Object {
  $_.CommandLine -match 'app-server\s+--listen\s+stdio://'
}
$killed = @()
foreach ($proc in $matches) {
  $id = [int]$proc.ProcessId
  $parent = [int]$proc.ParentProcessId
  if ($dexgramPids -contains $parent) {
    continue
  }
  Stop-Process -Id $id -Force -ErrorAction SilentlyContinue
  $killed += [pscustomobject]@{ Id = $id; Parent = $parent; Path = $proc.ExecutablePath }
}
$killed | ConvertTo-Json -Compress
`, quotePowerShellString(target), os.Getpid())
	cmd := exec.Command("powershell.exe", "-NoProfile", "-Command", script)
	hideCommandWindow(cmd)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text == "" {
			return nil, fmt.Errorf("clean Codex app-server processes: %w", err)
		}
		return nil, fmt.Errorf("clean Codex app-server processes: %w\n%s", err, text)
	}
	if text == "" {
		return nil, nil
	}
	var raw any
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return nil, fmt.Errorf("parse cleaned Codex app-server processes: %w", err)
	}
	var records []map[string]any
	switch value := raw.(type) {
	case []any:
		for _, item := range value {
			if record, ok := item.(map[string]any); ok {
				records = append(records, record)
			}
		}
	case map[string]any:
		records = append(records, value)
	}
	var lines []string
	for _, record := range records {
		id := fmt.Sprint(record["Id"])
		parent := fmt.Sprint(record["Parent"])
		path := strings.TrimSpace(fmt.Sprint(record["Path"]))
		if path == "" || path == "<nil>" {
			path = "path unavailable"
		}
		lines = append(lines, "pid="+id+" parent="+parent+" path="+path)
	}
	return lines, nil
}
