package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStartupItemContentStartsHiddenCommand(t *testing.T) {
	command := `powershell.exe -NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -Command "Start-Process -FilePath 'C:\Users\Yashau\AppData\Local\Dexgram\dexgram.exe' -ArgumentList @('-config', 'C:\Users\Yashau\AppData\Roaming\Dexgram\dexgram.toml', '-log', 'C:\Users\Yashau\AppData\Roaming\Dexgram\dexgram.log') -WindowStyle Hidden"`

	got := startupItemContent(command)

	if strings.Contains(got, "cmd.exe /d /c") {
		t.Fatalf("startup item should not wrap hidden command in cmd.exe: %q", got)
	}
	if strings.Contains(got, `\"`) {
		t.Fatalf("startup item uses C-style quote escaping: %q", got)
	}
	wantLine := `start "" /min powershell.exe -NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -Command "Start-Process -FilePath 'C:\Users\Yashau\AppData\Local\Dexgram\dexgram.exe' -ArgumentList @('-config', 'C:\Users\Yashau\AppData\Roaming\Dexgram\dexgram.toml', '-log', 'C:\Users\Yashau\AppData\Roaming\Dexgram\dexgram.log') -WindowStyle Hidden"`
	if !strings.Contains(got, wantLine+"\r\n") {
		t.Fatalf("startup item content = %q, want line %q", got, wantLine)
	}
}

func TestStartupDirectCommandStartsDexgramHidden(t *testing.T) {
	root := t.TempDir()
	local := filepath.Join(root, "Local")
	roaming := filepath.Join(root, "Roaming")
	t.Setenv("LOCALAPPDATA", local)
	t.Setenv("APPDATA", roaming)

	got := startupDirectCommand()

	for _, want := range []string{
		"powershell.exe -NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -Command ",
		"Start-Process -FilePath ",
		" -ArgumentList @('-config', ",
		", '-log', ",
		") -WindowStyle Hidden",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("startup direct command missing %q: %q", want, got)
		}
	}
}

func TestPrintServiceHelpIncludesStartupItemAndPaths(t *testing.T) {
	var out bytes.Buffer
	printServiceHelp(&out, "dexgram.exe")
	text := out.String()
	for _, want := range []string{"dexgram.exe service install", "Startup folder login item", "Binary:", "Config:", "Logs:"} {
		if !strings.Contains(text, want) {
			t.Fatalf("service help missing %q: %q", want, text)
		}
	}
}

func TestRunServiceCommandHelpAndUnknownCommand(t *testing.T) {
	if err := runServiceCommand([]string{"help"}); err != nil {
		t.Fatalf("service help command: %v", err)
	}
	if err := runServiceCommand([]string{"bogus"}); err == nil {
		t.Fatal("expected unknown service command to fail")
	}
}

func TestTopLevelStatusUsesServiceStatus(t *testing.T) {
	roaming := filepath.Join(t.TempDir(), "Roaming")
	t.Setenv("APPDATA", roaming)
	oldArgs := os.Args
	oldRunningDirect := runningDexgramDirectProcessesFunc
	defer func() {
		os.Args = oldArgs
		runningDexgramDirectProcessesFunc = oldRunningDirect
	}()

	os.Args = []string{"dexgram.exe", "status"}
	runningDexgramDirectProcessesFunc = func() ([]string, error) {
		return nil, nil
	}

	if err := run(); err != nil {
		t.Fatalf("top-level status: %v", err)
	}
}

func TestQuoteCmdArgDoublesEmbeddedQuotes(t *testing.T) {
	got := quoteCmdArg(`C:\Users\Name "Quoted"\dexgram.log`)
	want := `"C:\Users\Name ""Quoted""\dexgram.log"`
	if got != want {
		t.Fatalf("quoteCmdArg() = %q, want %q", got, want)
	}
}

func TestQuotePowerShellStringEscapesSingleQuotes(t *testing.T) {
	got := quotePowerShellString(`C:\Users\O'Brien\AppData\Local\Dexgram\dexgram.exe`)
	want := `'C:\Users\O''Brien\AppData\Local\Dexgram\dexgram.exe'`
	if got != want {
		t.Fatalf("quotePowerShellString() = %q, want %q", got, want)
	}
}

func TestServiceStartDoesNotDuplicateExistingStartupItemProcess(t *testing.T) {
	roaming := filepath.Join(t.TempDir(), "Roaming")
	t.Setenv("APPDATA", roaming)
	if err := installStartupItem(`"C:\Dexgram\dexgram.exe"`); err != nil {
		t.Fatalf("install startup item: %v", err)
	}

	oldRunningDirect := runningDexgramDirectProcessesFunc
	defer func() {
		runningDexgramDirectProcessesFunc = oldRunningDirect
	}()

	runningDexgramDirectProcessesFunc = func() ([]string, error) {
		return []string{`pid=123 path=C:\Dexgram\dexgram.exe`}, nil
	}

	if err := serviceStart(); err != nil {
		t.Fatalf("serviceStart() = %v", err)
	}
}

func TestServicePathsAndWarningsUseConfiguredAppData(t *testing.T) {
	root := t.TempDir()
	local := filepath.Join(root, "Local")
	roaming := filepath.Join(root, "Roaming")
	t.Setenv("LOCALAPPDATA", local)
	t.Setenv("APPDATA", roaming)

	exePath := mustServiceExePath()
	configPath := mustServiceConfigPath()
	if !strings.HasPrefix(exePath, local) {
		t.Fatalf("service exe path = %q", exePath)
	}
	if !strings.HasPrefix(configPath, roaming) {
		t.Fatalf("service config path = %q", configPath)
	}

	warnings := servicePathWarnings(exePath, configPath)
	if len(warnings) != 2 {
		t.Fatalf("warnings for missing paths = %#v", warnings)
	}
	if err := os.MkdirAll(filepath.Dir(exePath), 0o755); err != nil {
		t.Fatalf("create exe dir: %v", err)
	}
	if err := os.WriteFile(exePath, []byte("binary"), 0o600); err != nil {
		t.Fatalf("write exe: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("create config dir: %v", err)
	}
	if err := os.WriteFile(configPath, []byte("config"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if warnings := servicePathWarnings(exePath, configPath); len(warnings) != 0 {
		t.Fatalf("warnings for present paths = %#v", warnings)
	}
}

func TestStartupItemInstallStatusAndRemove(t *testing.T) {
	roaming := filepath.Join(t.TempDir(), "Roaming")
	t.Setenv("APPDATA", roaming)

	if startupItemInstalled() {
		t.Fatal("startup item should not exist before install")
	}
	if err := installStartupItem(`"C:\Dexgram\dexgram.exe"`); err != nil {
		t.Fatalf("install startup item: %v", err)
	}
	if !startupItemInstalled() {
		t.Fatal("startup item should exist after install")
	}
	content, err := os.ReadFile(mustStartupEntryPath())
	if err != nil {
		t.Fatalf("read startup item: %v", err)
	}
	if !strings.Contains(string(content), "Dexgram user-login startup item") {
		t.Fatalf("unexpected startup item content: %q", content)
	}
	if err := removeStartupItem(); err != nil {
		t.Fatalf("remove startup item: %v", err)
	}
	if startupItemInstalled() {
		t.Fatal("startup item should be removed")
	}
	if err := removeStartupItem(); err != nil {
		t.Fatalf("remove missing startup item: %v", err)
	}
}
