package main

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const latestReleaseURL = "https://api.github.com/repos/yashau/dexgram/releases/latest"

type githubRelease struct {
	TagName string        `json:"tag_name"`
	Assets  []githubAsset `json:"assets"`
}

type githubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

func runUpdateCommand() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	release, cmp, err := latestUpdateComparison(ctx, appVersion)
	if err != nil {
		return err
	}
	if cmp >= 0 {
		fmt.Printf("Dexgram is already up to date (%s).\n", appVersion)
		fmt.Println()
		return nil
	}

	fmt.Printf("Updating Dexgram %s -> %s...\n\n", appVersion, strings.TrimPrefix(release.TagName, "v"))
	return runUpdateWorkflow(ctx, release, os.Stdout)
}

func runUpdateHelperCommand(args []string) error {
	fs := flag.NewFlagSet("__update-helper", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var parentPID int
	fs.IntVar(&parentPID, "parent-pid", 0, "process id that launched the helper")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if parentPID > 0 {
		stopProcess(parentPID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	release, cmp, err := latestUpdateComparison(ctx, appVersion)
	if err != nil {
		return err
	}
	if cmp >= 0 {
		fmt.Printf("Dexgram is already up to date (%s).\n", appVersion)
		return nil
	}
	fmt.Printf("Updating Dexgram %s -> %s...\n\n", appVersion, strings.TrimPrefix(release.TagName, "v"))
	return runUpdateWorkflow(ctx, release, os.Stdout)
}

func latestUpdateComparison(ctx context.Context, current string) (githubRelease, int, error) {
	release, err := latestRelease(ctx)
	if err != nil {
		return githubRelease{}, 0, err
	}
	cmp, err := compareVersions(current, release.TagName)
	if err != nil {
		return githubRelease{}, 0, err
	}
	return release, cmp, nil
}

func startUpdateProcess(logPath string) error {
	helperPath, err := copySelfToUpdateHelper()
	if err != nil {
		return err
	}
	cmd := exec.Command(helperPath, "__update-helper", "-parent-pid", strconv.Itoa(os.Getpid()))
	detachLongRunningCommand(cmd)
	var logFile *os.File
	if strings.TrimSpace(logPath) != "" {
		if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
			return fmt.Errorf("create update log directory: %w", err)
		}
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return fmt.Errorf("open update log: %w", err)
		}
		logFile = f
		cmd.Stdout = f
		cmd.Stderr = f
	} else {
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
	}
	if err := cmd.Start(); err != nil {
		if logFile != nil {
			_ = logFile.Close()
		}
		return fmt.Errorf("start updater: %w", err)
	}
	if err := cmd.Process.Release(); err != nil {
		if logFile != nil {
			_ = logFile.Close()
		}
		return fmt.Errorf("release updater process: %w", err)
	}
	if logFile != nil {
		_ = logFile.Close()
	}
	return nil
}

func copySelfToUpdateHelper() (string, error) {
	current, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve current executable: %w", err)
	}
	helperDir := filepath.Join(os.TempDir(), "dexgram-run-"+strconv.Itoa(os.Getpid()))
	if err := os.MkdirAll(helperDir, 0o755); err != nil {
		return "", fmt.Errorf("create updater helper directory: %w", err)
	}
	helperPath := filepath.Join(helperDir, "dexgram-task.exe")
	if err := copyFile(current, helperPath); err != nil {
		return "", fmt.Errorf("copy updater helper: %w", err)
	}
	return helperPath, nil
}

func runUpdateWorkflow(ctx context.Context, release githubRelease, w io.Writer) error {
	asset, ok := selectReleaseAsset(release.Assets)
	if !ok {
		return fmt.Errorf("could not find a .exe or .zip asset in the latest release")
	}
	tmpDir, err := os.MkdirTemp("", "dexgram-update-*")
	if err != nil {
		return fmt.Errorf("create update temp directory: %w", err)
	}
	defer func() {
		_ = os.RemoveAll(tmpDir)
	}()

	fmt.Fprintf(w, "Downloading %s from %s...\n", asset.Name, release.TagName)
	sourceExe, err := downloadReleaseExecutable(ctx, asset, tmpDir)
	if err != nil {
		return err
	}

	fmt.Fprintln(w, "Stopping running Dexgram...")
	if err := stopDexgramForUpdate(w); err != nil {
		return err
	}

	fmt.Fprintf(w, "Replacing %s...\n", mustServiceExePath())
	if err := replaceDexgramExecutable(sourceExe, mustServiceExePath()); err != nil {
		return err
	}

	fmt.Fprintln(w, "Starting Dexgram service...")
	if err := serviceStart(); err != nil {
		return fmt.Errorf("start Dexgram service after update: %w", err)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Dexgram updated to %s.\n", cleanVersion(release.TagName))
	return nil
}

func selectReleaseAsset(assets []githubAsset) (githubAsset, bool) {
	for _, asset := range assets {
		if strings.EqualFold(filepath.Ext(asset.Name), ".exe") {
			return asset, true
		}
	}
	for _, asset := range assets {
		name := strings.ToLower(asset.Name)
		if filepath.Ext(name) == ".zip" &&
			(strings.Contains(name, "windows") || strings.Contains(name, "win") || strings.Contains(name, "amd64") || strings.Contains(name, "x64")) {
			return asset, true
		}
	}
	for _, asset := range assets {
		if strings.EqualFold(filepath.Ext(asset.Name), ".zip") {
			return asset, true
		}
	}
	return githubAsset{}, false
}

func downloadReleaseExecutable(ctx context.Context, asset githubAsset, tmpDir string) (string, error) {
	if strings.TrimSpace(asset.BrowserDownloadURL) == "" {
		return "", fmt.Errorf("release asset %q has no download URL", asset.Name)
	}
	downloadPath := filepath.Join(tmpDir, asset.Name)
	if err := downloadFile(ctx, asset.BrowserDownloadURL, downloadPath); err != nil {
		return "", err
	}
	if strings.EqualFold(filepath.Ext(asset.Name), ".exe") {
		return downloadPath, nil
	}
	extractDir := filepath.Join(tmpDir, "extract")
	if err := unzipFile(downloadPath, extractDir); err != nil {
		return "", err
	}
	var found string
	err := filepath.WalkDir(extractDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if found == "" && !entry.IsDir() && strings.EqualFold(entry.Name(), "dexgram.exe") {
			found = path
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("inspect release archive: %w", err)
	}
	if found == "" {
		return "", fmt.Errorf("release archive did not contain dexgram.exe")
	}
	return found, nil
}

func downloadFile(ctx context.Context, url, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "dexgram-updater")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("download release asset: HTTP %d", resp.StatusCode)
	}
	out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		_ = out.Close()
	}()
	if _, err := io.Copy(out, resp.Body); err != nil {
		return err
	}
	return nil
}

func unzipFile(zipPath, destDir string) error {
	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("open release archive: %w", err)
	}
	defer func() {
		_ = reader.Close()
	}()
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	for _, file := range reader.File {
		cleanName := filepath.Clean(file.Name)
		if strings.HasPrefix(cleanName, "..") || filepath.IsAbs(cleanName) {
			return fmt.Errorf("release archive contains unsafe path %q", file.Name)
		}
		target := filepath.Join(destDir, cleanName)
		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		in, err := file.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, file.FileInfo().Mode())
		if err != nil {
			_ = in.Close()
			return err
		}
		_, copyErr := io.Copy(out, in)
		closeErr := out.Close()
		inErr := in.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if inErr != nil {
			return inErr
		}
	}
	return nil
}

func stopDexgramForUpdate(w io.Writer) error {
	_, _ = runSchtasks("/End", "/TN", serviceTaskName)
	stopped, err := stopDexgramDirectProcessesFunc()
	if err != nil {
		return err
	}
	for _, line := range stopped {
		fmt.Fprintln(w, "stopped Dexgram process "+line)
	}
	return nil
}

func replaceDexgramExecutable(source, destination string) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return fmt.Errorf("create binary directory: %w", err)
	}
	backup := destination + ".old-" + time.Now().Format("20060102150405")
	if err := removeOldDexgramBackups(filepath.Dir(destination)); err != nil {
		return err
	}
	if err := os.Rename(destination, backup); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("move existing executable aside: %w", err)
	}
	if err := copyFile(source, destination); err != nil {
		if _, statErr := os.Stat(destination); errors.Is(statErr, os.ErrNotExist) {
			_ = os.Rename(backup, destination)
		}
		return fmt.Errorf("copy replacement executable: %w", err)
	}
	_ = os.Remove(backup)
	return nil
}

func removeOldDexgramBackups(dir string) error {
	matches, err := filepath.Glob(filepath.Join(dir, "dexgram.exe.old-*"))
	if err != nil {
		return err
	}
	for _, match := range matches {
		_ = os.Remove(match)
	}
	return nil
}

func copyFile(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() {
		_ = in.Close()
	}()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func stopProcess(pid int) {
	process, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = process.Kill()
	_, _ = process.Wait()
}

func latestRelease(ctx context.Context) (githubRelease, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latestReleaseURL, nil)
	if err != nil {
		return githubRelease{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "dexgram-updater")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return githubRelease{}, err
	}
	defer func() {
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return githubRelease{}, fmt.Errorf("check latest release: GitHub returned HTTP %d", resp.StatusCode)
	}
	var release githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return githubRelease{}, err
	}
	if strings.TrimSpace(release.TagName) == "" {
		return githubRelease{}, fmt.Errorf("check latest release: latest release has no tag")
	}
	return release, nil
}

func compareVersions(current, latest string) (int, error) {
	currentParts, err := versionParts(current)
	if err != nil {
		return 0, fmt.Errorf("parse current version %q: %w", current, err)
	}
	latestParts, err := versionParts(latest)
	if err != nil {
		return 0, fmt.Errorf("parse latest version %q: %w", latest, err)
	}
	for i := 0; i < 3; i++ {
		switch {
		case currentParts[i] > latestParts[i]:
			return 1, nil
		case currentParts[i] < latestParts[i]:
			return -1, nil
		}
	}
	return 0, nil
}

func versionParts(version string) ([3]int, error) {
	version = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(version, "v"), "V"))
	if before, _, ok := strings.Cut(version, "-"); ok {
		version = before
	}
	if before, _, ok := strings.Cut(version, "+"); ok {
		version = before
	}
	fields := strings.Split(version, ".")
	if len(fields) < 2 || len(fields) > 3 {
		return [3]int{}, fmt.Errorf("expected major.minor.patch")
	}
	var out [3]int
	for i, field := range fields {
		n, err := strconv.Atoi(field)
		if err != nil {
			return [3]int{}, err
		}
		out[i] = n
	}
	return out, nil
}
