package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"dexgram/internal/codex"
)

func (a *app) setTopicGoal(ctx context.Context, chatID int64, messageThreadID int, objective string) error {
	key := fmt.Sprintf("%d:%d", chatID, messageThreadID)
	if session := a.activeSession(key); session != nil {
		return setThreadGoal(ctx, session.client, session.threadID, objective)
	}

	conv := a.topicConversation(chatID, messageThreadID)
	conv, err := prepareProjectlessWorkspace(conv, objective)
	if err != nil {
		return err
	}
	c, err := a.startCodexAppServer(ctx, codex.StartOptions{
		CLIPath:    a.cfg.Codex.CLIPath,
		WorkingDir: appServerWorkingDir(conv),
	}, "set goal")
	if err != nil {
		return err
	}
	done := make(chan struct{})
	defer close(done)
	defer func() {
		_ = c.Close()
	}()
	errs := c.Errors()
	go func() {
		for {
			select {
			case err, ok := <-errs:
				if !ok {
					return
				}
				log.Printf("codex app-server: %v", err)
			case <-done:
				return
			}
		}
	}()
	c.SetServerRequestHandler(func(_ context.Context, req codex.ServerRequest) (any, error) {
		return a.requestApproval(ctx, chatID, messageThreadID, req)
	})
	conv, err = a.prepareTopicConversation(ctx, c, conv)
	if err != nil {
		return err
	}
	return setThreadGoal(ctx, c, conv.CodexThreadID, objective)
}

func (a *app) clearTopicGoal(ctx context.Context, chatID int64, messageThreadID int) error {
	var clearedThreadID string
	if err := a.withExistingTopicCodexClient(ctx, chatID, messageThreadID, func(c *codex.Client, threadID string) error {
		clearedThreadID = threadID
		return clearThreadGoal(ctx, c, threadID)
	}); err != nil {
		return err
	}
	if clearedThreadID == "" {
		return nil
	}
	return a.store.DeletePausedGoal(clearedThreadID)
}

func (a *app) getTopicGoal(ctx context.Context, chatID int64, messageThreadID int) (*codex.ThreadGoal, error) {
	var goal *codex.ThreadGoal
	err := a.withExistingTopicCodexClient(ctx, chatID, messageThreadID, func(c *codex.Client, threadID string) error {
		var err error
		goal, err = getThreadGoal(ctx, c, threadID)
		return err
	})
	return goal, err
}

func (a *app) pauseTopicGoal(ctx context.Context, chatID int64, messageThreadID int) (string, error) {
	var objective string
	err := a.withExistingTopicCodexClient(ctx, chatID, messageThreadID, func(c *codex.Client, threadID string) error {
		goal, err := getThreadGoal(ctx, c, threadID)
		if err != nil {
			return err
		}
		if goal == nil || strings.TrimSpace(goal.Objective) == "" {
			return fmt.Errorf("no Codex goal is set for this thread")
		}
		objective = strings.TrimSpace(goal.Objective)
		if err := a.store.SavePausedGoal(threadID, objective); err != nil {
			return err
		}
		if err := clearThreadGoal(ctx, c, threadID); err != nil {
			if deleteErr := a.store.DeletePausedGoal(threadID); deleteErr != nil {
				log.Printf("rollback paused goal after clear failure thread_id=%s: %v", threadID, deleteErr)
			}
			return err
		}
		return nil
	})
	return objective, err
}

func (a *app) resumeTopicGoal(ctx context.Context, chatID int64, messageThreadID int) (string, error) {
	var objective string
	err := a.withExistingTopicCodexClient(ctx, chatID, messageThreadID, func(c *codex.Client, threadID string) error {
		paused, ok, err := a.store.GetPausedGoal(threadID)
		if err != nil {
			return err
		}
		if ok {
			objective = strings.TrimSpace(paused.Objective)
		}
		if objective == "" {
			goal, err := getThreadGoal(ctx, c, threadID)
			if err != nil {
				return err
			}
			objective = pausedGoalObjective(goal)
		}
		if objective == "" {
			return fmt.Errorf("no paused Codex goal is stored or active for this thread")
		}
		if err := setThreadGoal(ctx, c, threadID, objective); err != nil {
			return err
		}
		if ok {
			return a.store.DeletePausedGoal(threadID)
		}
		return nil
	})
	return objective, err
}

func pausedGoalObjective(goal *codex.ThreadGoal) string {
	if goal == nil || !strings.EqualFold(strings.TrimSpace(goal.Status), "paused") {
		return ""
	}
	return strings.TrimSpace(goal.Objective)
}

func (a *app) getPausedTopicGoal(chatID int64, messageThreadID int) (string, bool, error) {
	key := fmt.Sprintf("%d:%d", chatID, messageThreadID)
	if session := a.activeSession(key); session != nil {
		return a.getPausedThreadGoal(session.threadID)
	}
	conv, ok, err := a.store.Get(chatID, messageThreadID)
	if err != nil || !ok || conv.CodexThreadID == "" {
		return "", false, err
	}
	return a.getPausedThreadGoal(conv.CodexThreadID)
}

func (a *app) getPausedThreadGoal(threadID string) (string, bool, error) {
	paused, ok, err := a.store.GetPausedGoal(threadID)
	if err != nil || !ok {
		return "", false, err
	}
	objective := strings.TrimSpace(paused.Objective)
	return objective, objective != "", nil
}

func (a *app) withExistingTopicCodexClient(ctx context.Context, chatID int64, messageThreadID int, fn func(*codex.Client, string) error) error {
	key := fmt.Sprintf("%d:%d", chatID, messageThreadID)
	if session := a.activeSession(key); session != nil {
		return fn(session.client, session.threadID)
	}

	conv, ok, err := a.store.Get(chatID, messageThreadID)
	if err != nil {
		return err
	}
	if !ok || conv.CodexThreadID == "" {
		return fmt.Errorf("this Telegram topic has not started a Codex thread yet")
	}

	c, err := a.startCodexAppServer(ctx, codex.StartOptions{
		CLIPath:    a.cfg.Codex.CLIPath,
		WorkingDir: appServerWorkingDir(conv),
	}, "existing topic")
	if err != nil {
		return err
	}
	done := make(chan struct{})
	defer close(done)
	defer func() {
		_ = c.Close()
	}()
	errs := c.Errors()
	go func() {
		for {
			select {
			case err, ok := <-errs:
				if !ok {
					return
				}
				log.Printf("codex app-server: %v", err)
			case <-done:
				return
			}
		}
	}()
	c.SetServerRequestHandler(func(_ context.Context, req codex.ServerRequest) (any, error) {
		return a.requestApproval(ctx, chatID, messageThreadID, req)
	})
	if err := a.resumeCodexThread(ctx, c, conv.CodexThreadID); err != nil {
		return err
	}
	return fn(c, conv.CodexThreadID)
}

func setThreadGoal(ctx context.Context, c *codex.Client, threadID, objective string) error {
	var out map[string]any
	return c.Call(ctx, "thread/goal/set", map[string]any{
		"threadId":  threadID,
		"objective": objective,
	}, &out)
}

func clearThreadGoal(ctx context.Context, c *codex.Client, threadID string) error {
	var out map[string]any
	return c.Call(ctx, "thread/goal/clear", map[string]any{
		"threadId": threadID,
	}, &out)
}

func getThreadGoal(ctx context.Context, c *codex.Client, threadID string) (*codex.ThreadGoal, error) {
	var out codex.ThreadGoalGetResponse
	if err := c.Call(ctx, "thread/goal/get", map[string]any{
		"threadId": threadID,
	}, &out); err != nil {
		if isCodexGoalMissingStorageError(err) {
			goal, fallbackErr := getThreadGoalFromSQLite(threadID)
			if fallbackErr != nil {
				log.Printf("read Codex goal from SQLite fallback thread_id=%s: %v", threadID, fallbackErr)
			}
			return goal, nil
		}
		return nil, err
	}
	return out.Goal, nil
}

func getThreadGoalFromSQLite(threadID string) (*codex.ThreadGoal, error) {
	threadID = strings.TrimSpace(threadID)
	if threadID == "" {
		return nil, nil
	}
	paths, err := codexGoalSQLitePaths()
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, path := range paths {
		goal, ok, err := readThreadGoalFromSQLite(path, threadID)
		if err != nil {
			lastErr = err
			continue
		}
		if ok {
			return goal, nil
		}
	}
	return nil, lastErr
}

func codexGoalSQLitePaths() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	if home == "" {
		return nil, nil
	}
	paths, err := filepath.Glob(filepath.Join(home, ".codex", "goals_*.sqlite"))
	if err != nil {
		return nil, err
	}
	return paths, nil
}

func readThreadGoalFromSQLite(path, threadID string) (*codex.ThreadGoal, bool, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, false, err
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			log.Printf("close Codex goals SQLite fallback db: %v", closeErr)
		}
	}()
	if _, err := db.Exec(`PRAGMA busy_timeout = 5000`); err != nil {
		return nil, false, err
	}
	row := db.QueryRow(`
SELECT thread_id, objective, status, token_budget, tokens_used, time_used_seconds, created_at_ms, updated_at_ms
FROM thread_goals
WHERE thread_id = ?`, threadID)
	var goal codex.ThreadGoal
	var tokenBudget sql.NullInt64
	var tokensUsed, timeUsedSeconds, createdAtMS, updatedAtMS int64
	if err := row.Scan(
		&goal.ThreadID,
		&goal.Objective,
		&goal.Status,
		&tokenBudget,
		&tokensUsed,
		&timeUsedSeconds,
		&createdAtMS,
		&updatedAtMS,
	); err != nil {
		if err == sql.ErrNoRows || isCodexGoalMissingStorageError(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if tokenBudget.Valid {
		budget := float64(tokenBudget.Int64)
		goal.TokenBudget = &budget
	}
	goal.TokensUsed = float64(tokensUsed)
	goal.TimeUsedSeconds = float64(timeUsedSeconds)
	goal.CreatedAt = float64(createdAtMS) / 1000
	goal.UpdatedAt = float64(updatedAtMS) / 1000
	return &goal, true, nil
}

func isCodexGoalMissingStorageError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "no such table: thread_goals")
}
