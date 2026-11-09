package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dexgram/internal/state"

	"github.com/fsnotify/fsnotify"
)

func TestSessionFileEventMatches(t *testing.T) {
	path := filepath.Join("C:", "Users", "Yashau", ".codex", "sessions", "rollout-thread.jsonl")
	if !sessionFileEventMatches(fsnotify.Event{Name: path, Op: fsnotify.Write}, path) {
		t.Fatal("expected write event for session file to match")
	}
	if !sessionFileEventMatches(fsnotify.Event{Name: filepath.Dir(path), Op: fsnotify.Write}, path) {
		t.Fatal("expected directory write event for session directory to match")
	}
	if !sessionFileEventMatches(fsnotify.Event{Name: filepath.Join(filepath.Dir(path), "other.jsonl"), Op: fsnotify.Create}, path) {
		t.Fatal("expected sibling event in session directory to wake stat check")
	}
	if sessionFileEventMatches(fsnotify.Event{Name: path, Op: fsnotify.Chmod}, path) {
		t.Fatal("chmod should not trigger session mirror")
	}
	if sessionFileEventMatches(fsnotify.Event{Name: filepath.Join("C:", "other.jsonl"), Op: fsnotify.Write}, path) {
		t.Fatal("different file should not match")
	}
}

func TestSessionFileChangedDetectsSizeAndModTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	if _, changed := sessionFileChanged(path, sessionFileState{}); changed {
		t.Fatal("missing file should not be changed")
	}
	if err := os.WriteFile(path, []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	state, changed := sessionFileChanged(path, sessionFileState{})
	if !changed {
		t.Fatal("first existing state should be treated as changed")
	}
	if _, changed := sessionFileChanged(path, state); changed {
		t.Fatal("unchanged file should not be changed")
	}
	if err := os.WriteFile(path, []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, changed := sessionFileChanged(path, state); !changed {
		t.Fatal("updated file should be changed")
	}
}

func TestMirrorWorkingDirReportsMissingProjectCWD(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "deleted-project")
	path, missingDir, err := mirrorWorkingDir(state.Conversation{CWD: missing})
	if err != nil {
		t.Fatal(err)
	}
	if path != missing || !missingDir {
		t.Fatalf("mirrorWorkingDir missing project = %q, %v; want %q, true", path, missingDir, missing)
	}
}

func TestMirrorWorkingDirKeepsExistingProjectCWD(t *testing.T) {
	dir := t.TempDir()
	path, missingDir, err := mirrorWorkingDir(state.Conversation{CWD: dir})
	if err != nil {
		t.Fatal(err)
	}
	if path != dir || missingDir {
		t.Fatalf("mirrorWorkingDir existing project = %q, %v; want %q, false", path, missingDir, dir)
	}
}

func TestDesktopMirrorRecentlyActiveUsesTelegramActivity(t *testing.T) {
	conv := state.Conversation{
		LastTelegramMessageAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
	}
	if !desktopMirrorRecentlyActive(conv, time.Now()) {
		t.Fatal("expected mirror to be active before expiry")
	}
	conv.LastTelegramMessageAt = time.Now().Add(-desktopMirrorActiveWindow - time.Minute).UTC().Format(time.RFC3339)
	if desktopMirrorRecentlyActive(conv, time.Now()) {
		t.Fatal("expected mirror to expire")
	}
	conv.LastTelegramMessageAt = "not a time"
	if desktopMirrorRecentlyActive(conv, time.Now()) {
		t.Fatal("invalid activity timestamp should be inactive")
	}
}

func TestDesktopMirrorEligibleSkipsSideChats(t *testing.T) {
	conv := state.Conversation{
		SideChat:              true,
		LastTelegramMessageAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
	}
	if desktopMirrorEligible(conv, time.Now()) {
		t.Fatal("side chats should not use desktop mirror polling")
	}
	conv.SideChat = false
	if !desktopMirrorEligible(conv, time.Now()) {
		t.Fatal("recent non-side topic should be eligible")
	}
}

func TestIsMissingCodexRolloutError(t *testing.T) {
	err := errors.New("app-server returned error for thread/resume (code -32600): no rollout found for thread id 019e")
	if !isMissingCodexRolloutError(err) {
		t.Fatal("expected missing rollout error")
	}
	if isMissingCodexRolloutError(errors.New("temporary app-server failure")) {
		t.Fatal("unexpected missing rollout error")
	}
}
