package main

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dexgram/internal/codex"

	"github.com/go-telegram/bot/models"
)

func TestHandleUpdateGoalWithoutArgsShowsCurrentGoal(t *testing.T) {
	b, api := newTelegramTestBot(t)
	testApp := newHandlerTestApp(t, []int64{123})

	oldGetGoal := getTopicGoalFunc
	getTopicGoalFunc = func(_ *app, _ context.Context, chatID int64, messageThreadID int) (*codex.ThreadGoal, error) {
		if chatID != 123 || messageThreadID != 7 {
			t.Fatalf("get goal target = %d:%d", chatID, messageThreadID)
		}
		return &codex.ThreadGoal{
			Objective: "Ship the usage command",
			Status:    "active",
		}, nil
	}
	defer func() {
		getTopicGoalFunc = oldGetGoal
	}()

	testApp.handleUpdate(context.Background(), b, &models.Update{Message: &models.Message{
		ID:              5,
		MessageThreadID: 7,
		Chat:            models.Chat{ID: 123},
		Text:            "/goal",
	}})

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && !api.bodyContains("sendMessage", "Ship the usage command") {
		time.Sleep(10 * time.Millisecond)
	}
	if !api.bodyContains("sendMessage", "Status: active") {
		t.Fatalf("goal status was not sent: %#v", api.calls)
	}
	if api.bodyContains("sendMessage", "How should Dexgram use this message?") {
		t.Fatalf("/goal fell through to Codex prompt handling: %#v", api.calls)
	}
}

func TestHandleUpdateGoalPauseStoresAndClearsGoal(t *testing.T) {
	b, api := newTelegramTestBot(t)
	testApp := newHandlerTestApp(t, []int64{123})

	oldSetGoal := setTopicGoalFunc
	oldPauseGoal := pauseTopicGoalFunc
	setTopicGoalFunc = func(_ *app, _ context.Context, _ int64, _ int, objective string) error {
		t.Fatalf("pause should not set objective %q", objective)
		return nil
	}
	pauseCalled := false
	pauseTopicGoalFunc = func(_ *app, _ context.Context, chatID int64, messageThreadID int) (string, error) {
		pauseCalled = true
		if chatID != 123 || messageThreadID != 7 {
			t.Fatalf("pause goal target = %d:%d", chatID, messageThreadID)
		}
		return "Finish Dexgram goals", nil
	}
	defer func() {
		setTopicGoalFunc = oldSetGoal
		pauseTopicGoalFunc = oldPauseGoal
	}()

	testApp.handleUpdate(context.Background(), b, &models.Update{Message: &models.Message{
		ID:              5,
		MessageThreadID: 7,
		Chat:            models.Chat{ID: 123},
		Text:            "/goal pause",
	}})

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && !api.bodyContains("sendMessage", "Codex goal paused:") {
		time.Sleep(10 * time.Millisecond)
	}
	if !pauseCalled {
		t.Fatal("expected pause goal path to be called")
	}
	if !api.bodyContains("sendMessage", "Finish Dexgram goals") {
		t.Fatalf("goal pause confirmation was not sent: %#v", api.calls)
	}
	if api.bodyContains("sendMessage", "How should Dexgram use this message?") {
		t.Fatalf("/goal pause fell through to Codex prompt handling: %#v", api.calls)
	}
}

func TestHandleUpdateGoalResumeRestoresPausedGoal(t *testing.T) {
	b, api := newTelegramTestBot(t)
	testApp := newHandlerTestApp(t, []int64{123})

	oldSetGoal := setTopicGoalFunc
	oldResumeGoal := resumeTopicGoalFunc
	setTopicGoalFunc = func(_ *app, _ context.Context, _ int64, _ int, objective string) error {
		t.Fatalf("resume should not set a literal objective %q", objective)
		return nil
	}
	resumeCalled := false
	resumeTopicGoalFunc = func(_ *app, _ context.Context, chatID int64, messageThreadID int) (string, error) {
		resumeCalled = true
		if chatID != 123 || messageThreadID != 7 {
			t.Fatalf("resume goal target = %d:%d", chatID, messageThreadID)
		}
		return "Finish Dexgram goals", nil
	}
	defer func() {
		setTopicGoalFunc = oldSetGoal
		resumeTopicGoalFunc = oldResumeGoal
	}()

	testApp.handleUpdate(context.Background(), b, &models.Update{Message: &models.Message{
		ID:              5,
		MessageThreadID: 7,
		Chat:            models.Chat{ID: 123},
		Text:            "/goal resume",
	}})

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && !api.bodyContains("sendMessage", "Codex goal resumed:") {
		time.Sleep(10 * time.Millisecond)
	}
	if !resumeCalled {
		t.Fatal("expected resume goal path to be called")
	}
	if !api.bodyContains("sendMessage", "Finish Dexgram goals") {
		t.Fatalf("goal resume confirmation was not sent: %#v", api.calls)
	}
	if api.bodyContains("sendMessage", "How should Dexgram use this message?") {
		t.Fatalf("/goal resume fell through to Codex prompt handling: %#v", api.calls)
	}
}

func TestPausedGoalObjectiveRequiresLivePausedStatus(t *testing.T) {
	if got := pausedGoalObjective(&codex.ThreadGoal{Objective: "Resume me", Status: " paused "}); got != "Resume me" {
		t.Fatalf("pausedGoalObjective = %q", got)
	}
	for _, goal := range []*codex.ThreadGoal{
		nil,
		{Objective: "Active", Status: "active"},
		{Objective: "   ", Status: "paused"},
	} {
		if got := pausedGoalObjective(goal); got != "" {
			t.Fatalf("pausedGoalObjective(%#v) = %q", goal, got)
		}
	}
}

func TestIsCodexGoalMissingStorageError(t *testing.T) {
	err := mockError("app-server returned error for thread/goal/get (code -32603): failed to read thread goal: error returned from database: (code: 1) no such table: thread_goals")
	if !isCodexGoalMissingStorageError(err) {
		t.Fatal("expected missing thread_goals table error to be recognized")
	}
	if isCodexGoalMissingStorageError(mockError("goals feature is disabled")) {
		t.Fatal("goals disabled should not be treated as missing storage")
	}
}

func TestReadThreadGoalFromSQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "goals_1.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close sqlite: %v", err)
		}
	}()
	_, err = db.Exec(`
CREATE TABLE thread_goals (
    thread_id TEXT PRIMARY KEY NOT NULL,
    goal_id TEXT NOT NULL,
    objective TEXT NOT NULL,
    status TEXT NOT NULL,
    token_budget INTEGER,
    tokens_used INTEGER NOT NULL DEFAULT 0,
    time_used_seconds INTEGER NOT NULL DEFAULT 0,
    created_at_ms INTEGER NOT NULL,
    updated_at_ms INTEGER NOT NULL
);
INSERT INTO thread_goals (
    thread_id, goal_id, objective, status, token_budget, tokens_used,
    time_used_seconds, created_at_ms, updated_at_ms
) VALUES (
    'thread-sqlbro', 'goal-1', 'Audit backups', 'active', 5000, 1234, 55, 1700000000000, 1700000012345
);`)
	if err != nil {
		t.Fatalf("seed sqlite: %v", err)
	}

	goal, ok, err := readThreadGoalFromSQLite(path, "thread-sqlbro")
	if err != nil {
		t.Fatalf("read goal: %v", err)
	}
	if !ok {
		t.Fatal("expected goal row")
	}
	if goal.ThreadID != "thread-sqlbro" || goal.Objective != "Audit backups" || goal.Status != "active" {
		t.Fatalf("unexpected goal: %#v", goal)
	}
	if goal.TokenBudget == nil || *goal.TokenBudget != 5000 {
		t.Fatalf("token budget = %#v", goal.TokenBudget)
	}
	if goal.TokensUsed != 1234 || goal.TimeUsedSeconds != 55 {
		t.Fatalf("usage fields = %#v", goal)
	}
	if goal.CreatedAt != 1700000000 || goal.UpdatedAt != 1700000012.345 {
		t.Fatalf("time fields = %#v", goal)
	}
}

func TestGetThreadGoalFromSQLiteFindsCodexGoalsDB(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	codexDir := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		t.Fatalf("create codex dir: %v", err)
	}
	path := filepath.Join(codexDir, "goals_1.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	_, err = db.Exec(`
CREATE TABLE thread_goals (
    thread_id TEXT PRIMARY KEY NOT NULL,
    goal_id TEXT NOT NULL,
    objective TEXT NOT NULL,
    status TEXT NOT NULL,
    token_budget INTEGER,
    tokens_used INTEGER NOT NULL DEFAULT 0,
    time_used_seconds INTEGER NOT NULL DEFAULT 0,
    created_at_ms INTEGER NOT NULL,
    updated_at_ms INTEGER NOT NULL
);
INSERT INTO thread_goals (
    thread_id, goal_id, objective, status, token_budget, tokens_used,
    time_used_seconds, created_at_ms, updated_at_ms
) VALUES (
    'thread-desktop', 'goal-1', 'Desktop goal', 'active', NULL, 42, 7, 1700000000000, 1700000001000
);`)
	if err != nil {
		t.Fatalf("seed sqlite: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close sqlite: %v", err)
	}

	goal, err := getThreadGoalFromSQLite("thread-desktop")
	if err != nil {
		t.Fatalf("fallback goal: %v", err)
	}
	if goal == nil || goal.Objective != "Desktop goal" || goal.Status != "active" {
		t.Fatalf("fallback goal = %#v", goal)
	}
}

func TestHandleUpdateGoalClearPhraseSetsObjective(t *testing.T) {
	b, api := newTelegramTestBot(t)
	testApp := newHandlerTestApp(t, []int64{123})

	oldSetGoal := setTopicGoalFunc
	oldClearGoal := clearTopicGoalFunc
	setCalled := false
	setTopicGoalFunc = func(_ *app, _ context.Context, chatID int64, messageThreadID int, objective string) error {
		setCalled = true
		if chatID != 123 || messageThreadID != 7 || objective != "clear database migration" {
			t.Fatalf("set goal args = %d:%d %q", chatID, messageThreadID, objective)
		}
		return nil
	}
	clearTopicGoalFunc = func(_ *app, _ context.Context, _ int64, _ int) error {
		t.Fatal("multi-word goal should not clear current goal")
		return nil
	}
	defer func() {
		setTopicGoalFunc = oldSetGoal
		clearTopicGoalFunc = oldClearGoal
	}()

	testApp.handleUpdate(context.Background(), b, &models.Update{Message: &models.Message{
		ID:              5,
		MessageThreadID: 7,
		Chat:            models.Chat{ID: 123},
		Text:            "/goal clear database migration",
	}})

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && !api.bodyContains("sendMessage", "Codex goal set:") {
		time.Sleep(10 * time.Millisecond)
	}
	if !setCalled {
		t.Fatal("expected set goal path to be called")
	}
	if !api.bodyContains("sendMessage", "clear database migration") {
		t.Fatalf("goal set confirmation was not sent: %#v", api.calls)
	}
}
