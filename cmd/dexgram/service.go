package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const startupItemName = "Dexgram"

var (
	runningDexgramDirectProcessesFunc = runningDexgramDirectProcesses
	stopDexgramDirectProcessesFunc    = stopDexgramDirectProcesses
)

func runServiceCommand(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		printServiceHelp(os.Stdout, filepath.Base(os.Args[0]))
		return nil
	}

	switch args[0] {
	case "install":
		return serviceInstall()
	case "uninstall", "remove":
		return serviceUninstall()
	case "start":
		return serviceStart()
	case "stop":
		return serviceStop(false)
	case "restart":
		return serviceRestart()
	case "status":
		return serviceStatus()
	default:
		return fmt.Errorf("unknown service command %q; run %s service --help", args[0], filepath.Base(os.Args[0]))
	}
}

func printServiceHelp(w io.Writer, exe string) {
	fmt.Fprintf(w, `Dexgram Service Commands

  Dexgram service mode is a per-user Startup folder login item.
  It is not a Windows Service and it runs in the signed-in user's context.

Usage

  %[1]s service install
  %[1]s service start
  %[1]s service stop
  %[1]s service restart
  %[1]s service status
  %[1]s service uninstall

Paths

  Binary: %[2]s
  Config: %[3]s
  Logs:   %[4]s
  State:  %[5]s

Behavior

  install    creates or replaces the Startup login item and starts Dexgram
  start      starts Dexgram directly from the fixed service paths
  stop       stops the running Dexgram process
  restart    stops the process if needed, then starts it again
  status     prints Startup login item status and the fixed Dexgram paths
  uninstall  removes the Startup login item and stops Dexgram

The installer script can place dexgram.exe under %%LOCALAPPDATA%%\Dexgram.
Config, logs, media, and state live under %%APPDATA%%\Dexgram.
The service log keeps the newest %[6]d lines.

`, exe, mustServiceExePath(), mustServiceConfigPath(), mustServiceLogPath(), mustServiceStatePath(), logFileMaxLines)
}

func serviceInstall() error {
	exePath := mustServiceExePath()
	configPath := mustServiceConfigPath()
	logPath := mustServiceLogPath()
	if err := os.MkdirAll(filepath.Dir(exePath), 0o755); err != nil {
		return fmt.Errorf("create binary directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	warnings := servicePathWarnings(exePath, configPath)
	if err := installStartupItem(startupDirectCommand()); err != nil {
		return err
	}
	fmt.Printf("installed Startup login item: %s\n", mustStartupEntryPath())
	for _, warning := range warnings {
		fmt.Fprintln(os.Stderr, "warning:", warning)
	}
	if err := startDexgramDirect(); err != nil {
		return err
	}
	fmt.Printf("binary: %s\n", exePath)
	fmt.Printf("config: %s\n", configPath)
	fmt.Printf("log:    %s\n", logPath)
	return nil
}

func serviceUninstall() error {
	_ = serviceStop(true)
	if err := removeStartupItem(); err != nil {
		return err
	}
	return nil
}

func serviceStart() error {
	if !startupItemInstalled() {
		return fmt.Errorf("service is not installed; run %s service install", filepath.Base(os.Args[0]))
	}
	if err := installStartupItem(startupDirectCommand()); err != nil {
		return err
	}
	return startDexgramDirect()
}

func serviceStop(ignoreNotRunning bool) error {
	return stopDexgramDirect(ignoreNotRunning)
}

func serviceRestart() error {
	if err := serviceStop(true); err != nil {
		return err
	}
	return serviceStart()
}

func serviceStatus() error {
	printServiceStatusHeader(os.Stdout)
	printDexgramRuntimeStatus()
	if startupItemInstalled() {
		fmt.Println("Startup login item is installed")
	} else {
		fmt.Println("Startup login item is not installed")
	}
	return nil
}

func printServiceStatusHeader(w io.Writer) {
	fmt.Fprintf(w, "version: %s\n", appVersion)
	fmt.Fprintf(w, "binary: %s\n", mustServiceExePath())
	fmt.Fprintf(w, "config: %s\n", mustServiceConfigPath())
	fmt.Fprintf(w, "log:    %s\n", mustServiceLogPath())
	fmt.Fprintf(w, "state:  %s\n", mustServiceStatePath())
	fmt.Fprintf(w, "startup login item: %s\n\n", mustStartupEntryPath())
}

func installStartupItem(command string) error {
	path := mustStartupEntryPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create startup directory: %w", err)
	}
	content := startupItemContent(command)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write Startup login item: %w", err)
	}
	return nil
}

func startupItemContent(command string) string {
	return "@echo off\r\n" +
		"rem Dexgram user-login startup item\r\n" +
		"start \"\" /min " + command + "\r\n"
}

func startupDirectCommand() string {
	script := fmt.Sprintf(
		"Start-Process -FilePath %s -ArgumentList @('-config', %s, '-log', %s) -WindowStyle Hidden",
		quotePowerShellString(mustServiceExePath()),
		quotePowerShellString(mustServiceConfigPath()),
		quotePowerShellString(mustServiceLogPath()),
	)
	return "powershell.exe -NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -Command " + quoteCmdArg(script)
}

func removeStartupItem() error {
	path := mustStartupEntryPath()
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("remove Startup login item: %w", err)
	}
	fmt.Printf("removed Startup login item %q\n", path)
	return nil
}

func startupItemInstalled() bool {
	_, err := os.Stat(mustStartupEntryPath())
	return err == nil
}

func startDexgramDirect() error {
	running, err := runningDexgramDirectProcessesFunc()
	if err != nil {
		return err
	}
	if len(running) > 0 {
		for _, line := range running {
			fmt.Println("Dexgram process already running " + line)
		}
		return nil
	}

	cmd := exec.Command(mustServiceExePath(), "-config", mustServiceConfigPath(), "-log", mustServiceLogPath())
	logFile, err := os.OpenFile(mustServiceLogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open service log: %w", err)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	detachLongRunningCommand(cmd)
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return fmt.Errorf("start Dexgram: %w", err)
	}
	_ = logFile.Close()
	fmt.Printf("started Dexgram process pid=%d\n", cmd.Process.Pid)
	return cmd.Process.Release()
}

func stopDexgramDirect(ignoreNotRunning bool) error {
	stopped, err := stopDexgramDirectProcessesFunc()
	if err != nil {
		return err
	}
	if len(stopped) == 0 {
		if ignoreNotRunning {
			return nil
		}
		return fmt.Errorf("Dexgram process is not running")
	}
	for _, line := range stopped {
		fmt.Println("stopped Dexgram process " + line)
	}
	return nil
}

func runningDexgramDirectProcesses() ([]string, error) {
	return dexgramDirectProcessLines(false)
}

func stopDexgramDirectProcesses() ([]string, error) {
	return dexgramDirectProcessLines(true)
}

func dexgramDirectProcessLines(stop bool) ([]string, error) {
	target := mustServiceExePath()
	stopBlock := ""
	if stop {
		stopBlock = `
    Stop-Process -Id $id -Force -ErrorAction Stop
`
	}
	script := fmt.Sprintf(`
$target = [System.IO.Path]::GetFullPath(%s)
$current = %d
$matches = Get-CimInstance Win32_Process -Filter "Name = 'dexgram.exe'" | Where-Object {
    $_.ProcessId -ne $current -and
    $_.ExecutablePath -and
    ([System.IO.Path]::GetFullPath($_.ExecutablePath) -ieq $target)
}
$stopped = @()
foreach ($proc in $matches) {
    $id = [int]$proc.ProcessId
%s
    $stopped += [pscustomobject]@{ Id = $id; Path = $proc.ExecutablePath }
}
$stopped | ConvertTo-Json -Compress
`, quotePowerShellString(target), os.Getpid(), stopBlock)
	cmd := exec.Command("powershell.exe", "-NoProfile", "-Command", script)
	hideCommandWindow(cmd)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text == "" {
			return nil, fmt.Errorf("inspect Dexgram process: %w", err)
		}
		return nil, fmt.Errorf("inspect Dexgram process: %w\n%s", err, text)
	}
	if text == "" {
		return nil, nil
	}
	var raw any
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return nil, fmt.Errorf("parse stopped Dexgram processes: %w", err)
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
		path := strings.TrimSpace(fmt.Sprint(record["Path"]))
		if path == "" || path == "<nil>" {
			path = "path unavailable"
		}
		lines = append(lines, "pid="+id+" path="+path)
	}
	return lines, nil
}

func quoteCmdArg(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func quotePowerShellString(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func printDexgramRuntimeStatus() {
	lines, err := runningDexgramProcesses()
	if err != nil {
		fmt.Printf("runtime: unknown (%v)\n\n", err)
		return
	}
	if len(lines) == 0 {
		fmt.Println("runtime: stopped")
		fmt.Println()
		return
	}
	fmt.Println("runtime: running")
	for _, line := range lines {
		fmt.Println("  " + line)
	}
	fmt.Println()
}

func runningDexgramProcesses() ([]string, error) {
	script := fmt.Sprintf(`Get-Process -Name dexgram -ErrorAction SilentlyContinue | Where-Object { $_.Id -ne %d } | Select-Object Id,Path | ConvertTo-Json -Compress`, os.Getpid())
	cmd := exec.Command("powershell.exe", "-NoProfile", "-Command", script)
	hideCommandWindow(cmd)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil && text != "" {
		return nil, err
	}
	if text == "" {
		return nil, nil
	}
	var raw any
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return nil, err
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
		path := strings.TrimSpace(fmt.Sprint(record["Path"]))
		if path == "" || path == "<nil>" {
			path = "path unavailable"
		}
		lines = append(lines, "pid="+id+" path="+path)
	}
	return lines, nil
}

func servicePathWarnings(exePath, configPath string) []string {
	var warnings []string
	if _, err := os.Stat(exePath); errors.Is(err, os.ErrNotExist) {
		warnings = append(warnings, fmt.Sprintf("binary does not exist yet at %s", exePath))
	}
	if _, err := os.Stat(configPath); errors.Is(err, os.ErrNotExist) {
		warnings = append(warnings, fmt.Sprintf("config does not exist yet at %s", configPath))
	}
	return warnings
}

func mustServiceExePath() string {
	return filepath.Join(mustLocalAppData(), "Dexgram", "dexgram.exe")
}

func mustServiceConfigPath() string {
	return filepath.Join(mustRoamingAppData(), "Dexgram", "dexgram.toml")
}

func mustServiceLogPath() string {
	return filepath.Join(mustRoamingAppData(), "Dexgram", "dexgram.log")
}

func mustServiceStatePath() string {
	return filepath.Join(mustRoamingAppData(), "Dexgram", "dexgram.db")
}

func mustStartupEntryPath() string {
	return filepath.Join(mustRoamingAppData(), "Microsoft", "Windows", "Start Menu", "Programs", "Startup", startupItemName+".cmd")
}

func mustLocalAppData() string {
	if dir := os.Getenv("LOCALAPPDATA"); dir != "" {
		return dir
	}
	dir, err := os.UserCacheDir()
	if err == nil && dir != "" {
		return dir
	}
	return filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Local")
}

func mustRoamingAppData() string {
	if dir := os.Getenv("APPDATA"); dir != "" {
		return dir
	}
	dir, err := os.UserConfigDir()
	if err == nil && dir != "" {
		return dir
	}
	return filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Roaming")
}
