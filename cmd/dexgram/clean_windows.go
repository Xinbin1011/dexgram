//go:build windows

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"dexgram/internal/state"
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
		fmt.Fprintln(w, "No stale Dexgram-owned Codex app-server processes found.")
		return 0, nil
	}
	for _, line := range lines {
		fmt.Fprintln(w, "killed Codex app-server "+line)
	}
	return len(lines), nil
}

func cleanDanglingCodexAppServerLines() ([]string, error) {
	target := mustServiceExePath()
	markerPath := defaultCodexAppServerPIDPath()
	if markerPath == "" {
		return nil, nil
	}
	script := fmt.Sprintf(`
$target = [System.IO.Path]::GetFullPath(%s)
$marker = %s
$current = %d
$dexgramPids = @(
  Get-CimInstance Win32_Process -Filter "Name = 'dexgram.exe'" | Where-Object {
    $_.ProcessId -ne $current -and
    $_.ExecutablePath -and
    ([System.IO.Path]::GetFullPath($_.ExecutablePath) -ieq $target)
  } | ForEach-Object { [int]$_.ProcessId }
)
$killed = @()
if (Test-Path -LiteralPath $marker) {
  $lines = Get-Content -LiteralPath $marker -ErrorAction SilentlyContinue
  if ($lines.Count -gt 0) {
    $id = 0
    if ([int]::TryParse(($lines[0].Trim()), [ref]$id) -and $id -gt 0) {
      $expectedPath = ""
      if ($lines.Count -gt 1) { $expectedPath = $lines[1].Trim() }
      $proc = Get-CimInstance Win32_Process -Filter "ProcessId = $id" -ErrorAction SilentlyContinue
      if ($null -ne $proc -and
          $proc.Name -ieq "codex.exe" -and
          $proc.CommandLine -match 'app-server\s+--listen\s+stdio://' -and
          ($expectedPath -eq "" -or ([System.IO.Path]::GetFullPath($proc.ExecutablePath) -ieq [System.IO.Path]::GetFullPath($expectedPath)))) {
        $parent = [int]$proc.ParentProcessId
        $parentProc = Get-CimInstance Win32_Process -Filter "ProcessId = $parent" -ErrorAction SilentlyContinue
        $parentIsRunningDexgram = $dexgramPids -contains $parent
        $parentIsDexgram = $false
        if ($null -ne $parentProc -and
            $parentProc.Name -ieq "dexgram.exe" -and
            $parentProc.ExecutablePath -and
            ([System.IO.Path]::GetFullPath($parentProc.ExecutablePath) -ieq $target)) {
          $parentIsDexgram = $true
        }
        if (-not $parentIsRunningDexgram -and ($null -eq $parentProc -or $parentIsDexgram)) {
          Stop-Process -Id $id -Force -ErrorAction SilentlyContinue
          $killed += [pscustomobject]@{ Id = $id; Parent = $parent; Path = $proc.ExecutablePath }
        }
      }
    }
  }
}
$killed | ConvertTo-Json -Compress
`, quotePowerShellString(target), quotePowerShellString(markerPath), os.Getpid())
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

func defaultCodexAppServerPIDPath() string {
	statePath, err := state.DefaultPath()
	if err == nil && strings.TrimSpace(statePath) != "" {
		return filepath.Join(filepath.Dir(statePath), "codex-app-server.pid")
	}
	appData := strings.TrimSpace(os.Getenv("APPDATA"))
	if appData == "" {
		return ""
	}
	return filepath.Join(appData, "Dexgram", "codex-app-server.pid")
}
