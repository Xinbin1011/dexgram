package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"dexgram/internal/codex"
	"dexgram/internal/codexprojects"
	"dexgram/internal/config"
	"dexgram/internal/state"
	"github.com/go-telegram/bot/models"
)

func TestCatalogUsesRegisteredProjectsAndActualMembership(t *testing.T) {
	root := t.TempDir()
	sidebar := codexprojects.Sidebar{
		Projects: []codexprojects.Project{{ID: "p", Name: "Real project", Path: root}},
		Sections: []codexprojects.Section{{ID: "s", Name: "Section"}},
		Threads: map[string]codexprojects.Placement{
			"t": {ProjectID: "p", SectionID: "s"},
			"u": {},
		},
	}
	threads := []codex.Thread{
		{ID: "t", Cwd: filepath.Join(root, "worktree")},
		{ID: "u", Cwd: root},
		{ID: "v", Cwd: filepath.Join(root, "arbitrary-folder")},
		{ID: "w", Cwd: root},
	}
	mappings := []state.Conversation{{CodexThreadID: "w", ProjectName: "Real project", CWD: root}}
	sections := buildSessionCatalog(threads, sidebar, mappings, "")
	if len(sections) != 3 || !slices.Equal(sections[0].Projects[0].ThreadIDs, []int{0}) ||
		sections[1].Projects[0].Name != "Real project" || !slices.Equal(sections[1].Projects[0].ThreadIDs, []int{0, 3}) ||
		len(sections[2].Projects) != 1 || sections[2].Projects[0].ID != "" || !slices.Equal(sections[2].Projects[0].ThreadIDs, []int{1, 2}) {
		t.Fatalf("wrong catalog: %#v", sections)
	}
}

func TestPinsPersistPerChatAndCanOverrideDesktopPin(t *testing.T) {
	store, err := state.Open(filepath.Join(t.TempDir(), "dexgram.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestStateStore(t, store)
	if err = store.SetWorkspacePin(41, "section:one", true); err != nil {
		t.Fatal(err)
	}
	if err = store.SetWorkspacePin(41, "chat:t", false); err != nil {
		t.Fatal(err)
	}
	if err = store.SetWorkspacePin(42, "section:two", true); err != nil {
		t.Fatal(err)
	}
	pins, err := store.WorkspacePins(41)
	if err != nil {
		t.Fatal(err)
	}
	browser := sessionBrowser{pins: pins, desktopPins: map[string]bool{"t": true}}
	if !browser.isPinned("section:one") || browser.isPinned("section:two") || browser.isPinned("chat:t") {
		t.Fatalf("wrong pin state: %#v", pins)
	}
}

func TestLegacyHistoryPagesReachOldestWithoutDuplicates(t *testing.T) {
	var history []codex.Turn
	for i := 0; i < 71; i++ {
		history = append(history, codex.Turn{ID: fmt.Sprint(i)})
	}
	var ids []string
	anchor := ""
	for {
		turns, next, err := legacyHistoryPage(history, anchor)
		if err != nil {
			t.Fatal(err)
		}
		for _, turn := range turns {
			ids = append(ids, turn.ID)
		}
		if next == "" {
			break
		}
		anchor = strings.TrimPrefix(next, "legacy:")
	}
	if len(ids) != 71 {
		t.Fatalf("got %d turns", len(ids))
	}
	for i, id := range ids {
		if id != fmt.Sprint(70-i) {
			t.Fatalf("turn %d: got %s", i, id)
		}
	}
}

func TestTopicModelAppliesToOrdinaryTurnAndIsIsolated(t *testing.T) {
	store, err := state.Open(filepath.Join(t.TempDir(), "dexgram.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestStateStore(t, store)
	a := app{store: store, cfg: &config.Config{Codex: config.CodexConfig{ApprovalPolicy: "on-request", Sandbox: "read-only"}}}
	if err = a.saveTopicSetting(41, 7, codexModelSettingKey, "selected"); err != nil {
		t.Fatal(err)
	}
	if err = a.saveTopicSetting(41, 7, codexReasoningSettingKey, "low"); err != nil {
		t.Fatal(err)
	}
	opts, err := a.turnOptions(context.Background(), nil, "", 41, 7)
	if err != nil || opts.Model != "selected" || opts.ReasoningEffort != "low" || opts.Sandbox != "read-only" {
		t.Fatalf("wrong options: %#v %v", opts, err)
	}
	other, err := a.turnOptions(context.Background(), nil, "", 41, 8)
	if err != nil || other.Model != "" || other.ReasoningEffort != "" {
		t.Fatalf("model leaked to other topic: %#v %v", other, err)
	}
}

func TestSendFileStaysInTopicWorkspace(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "project")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "result.txt")
	outside := filepath.Join(dir, "outside.txt")
	for _, p := range []string{file, outside} {
		if err := os.WriteFile(p, []byte("test"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{file, "result.txt"} {
		f, err := openWorkspaceFile(root, p)
		if err != nil {
			t.Fatal(err)
		}
		if err = f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{outside, filepath.Join("..", "outside.txt"), ".", "result.txt:stream"} {
		if f, err := openWorkspaceFile(root, p); err == nil {
			_ = f.Close()
			t.Errorf("allowed %q", p)
		}
	}
}

func TestPrivateTopicFileRoundTrip(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	t.Setenv("APPDATA", root)
	t.Setenv("XDG_CONFIG_HOME", root)
	b, api := newTelegramTestBot(t)
	a := newHandlerTestApp(t, []int64{41})
	msg := &models.Message{ID: 5, MessageThreadID: 7, Chat: models.Chat{ID: 41, Type: models.ChatTypePrivate}, Document: &models.Document{FileID: "fake", FileName: "round-trip.txt"}}
	in, _, err := a.buildTurnInput(ctx, b, msg, "Read this")
	if err != nil || len(in) != 2 {
		t.Fatalf("input: %#v %v", in, err)
	}
	path, ok := in[1]["path"].(string)
	if !ok {
		t.Fatalf("no local attachment: %#v", in)
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "synthetic file round trip" {
		t.Fatalf("wrong attachment: %q %v", content, err)
	}
	if err = a.store.Upsert(state.Conversation{ChatID: 41, MessageThreadID: 7, CWD: root}); err != nil {
		t.Fatal(err)
	}
	a.handleSendFileCommand(ctx, b, msg, path)
	if api.count("sendDocument") != 1 || !api.bodyContains("sendDocument", "synthetic file round trip") {
		t.Fatalf("file did not arrive: %#v", api.calls)
	}
}

func TestNewProjectTopicsKeepSeparateMappings(t *testing.T) {
	b, _ := newTelegramTestBot(t)
	a := newHandlerTestApp(t, []int64{41})
	msg := &models.Message{MessageThreadID: 7, Chat: models.Chat{ID: 41, Type: models.ChatTypePrivate}}
	root := t.TempDir()
	a.createChatTopic(context.Background(), b, msg, "Project", root, "First", false)
	a.createChatTopic(context.Background(), b, msg, "Project", root, "Second", false)
	mappings, err := a.store.ListConversations()
	if err != nil || len(mappings) != 2 || mappings[0].CWD != root || mappings[1].CWD != root ||
		mappings[0].MessageThreadID == mappings[1].MessageThreadID {
		t.Fatalf("wrong project topic mappings: %#v %v", mappings, err)
	}
}

func TestCatalogSeparatesSidebarPinsFromNamedPinnedSection(t *testing.T) {
	root := t.TempDir()
	sidebar := codexprojects.Sidebar{
		Projects: []codexprojects.Project{{ID: "p", Name: "Project", Path: root}},
		Sections: []codexprojects.Section{{ID: "s", Name: "Pinned"}},
		Threads: map[string]codexprojects.Placement{
			"desktop-pin":   {ProjectID: "p"},
			"named-section": {SectionID: "s"},
			"sqlite-pin":    {Pinned: true},
		},
		PinnedThreadIDs: []string{"desktop-pin", "remote-host-pin", "desktop-pin"},
		RemotePinTitles: map[string]string{"remote-host-pin": "Remote pinned chat"},
	}
	threads := []codex.Thread{{ID: "desktop-pin", Name: "First pinned chat"}, {ID: "named-section"}, {ID: "sqlite-pin", Name: "Second pinned chat"}}
	got := buildSessionCatalog(threads, sidebar, nil, "")
	if len(got) != 4 || got[0].Name != "Pinned chats" || got[1].Name != "Pinned" {
		t.Fatalf("wrong sections: %#v", got)
	}
	pins := got[0].PinnedChats
	if len(got[0].Projects) != 0 || len(pins) != 3 || pins[0].ThreadIndex != 0 ||
		pins[0].ProjectName != "Project" || pins[1].ThreadIndex != 2 || pins[2].RemoteTitle != "Remote pinned chat" ||
		!slices.Equal(got[1].Projects[0].ThreadIDs, []int{1}) {
		t.Fatalf("wrong pin placement: %#v", got)
	}
	_, view := sessionBrowserPinnedView("token", 0, got[0], threads)
	for i, want := range []string{"First pinned chat", "Second pinned chat", "Remote pinned chat (remote)"} {
		row := view.InlineKeyboard[i]
		if len(row) != 1 || row[0].Text != want || !strings.Contains(row[0].CallbackData, ":openpin:") {
			t.Fatalf("pin %d: got %v, want direct chat button %q", i, row, want)
		}
	}
}
