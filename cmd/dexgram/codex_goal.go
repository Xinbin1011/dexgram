package main

import (
	"context"
	"fmt"
	"strings"

	"dexgram/internal/codex"
)

func (a *app) setTopicGoal(ctx context.Context, chatID int64, messageThreadID int, objective string) error {
	session, err := a.ensureTopicLiveSession(ctx, chatID, messageThreadID, objective)
	if err != nil {
		return err
	}
	return setThreadGoal(ctx, session.client, session.threadID, objective)
}

func (a *app) clearTopicGoal(ctx context.Context, chatID int64, messageThreadID int) error {
	return a.withExistingTopicCodexClient(ctx, chatID, messageThreadID, false, func(c *codex.Client, threadID string) error {
		return clearThreadGoal(ctx, c, threadID)
	})
}

func (a *app) getTopicGoal(ctx context.Context, chatID int64, messageThreadID int) (*codex.ThreadGoal, error) {
	var goal *codex.ThreadGoal
	err := a.withExistingTopicCodexClient(ctx, chatID, messageThreadID, false, func(c *codex.Client, threadID string) error {
		var err error
		goal, err = getThreadGoal(ctx, c, threadID)
		return err
	})
	return goal, err
}

func (a *app) pauseTopicGoal(ctx context.Context, chatID int64, messageThreadID int) (string, error) {
	var objective string
	err := a.withExistingTopicCodexClient(ctx, chatID, messageThreadID, false, func(c *codex.Client, threadID string) error {
		goal, err := getThreadGoal(ctx, c, threadID)
		if err != nil {
			return err
		}
		if goal == nil || strings.TrimSpace(goal.Objective) == "" {
			return fmt.Errorf("no Codex goal is set for this thread")
		}
		objective = strings.TrimSpace(goal.Objective)
		return setThreadGoalStatus(ctx, c, threadID, "paused")
	})
	return objective, err
}

func (a *app) resumeTopicGoal(ctx context.Context, chatID int64, messageThreadID int) (string, error) {
	var objective string
	session, err := a.ensureExistingTopicLiveSession(ctx, chatID, messageThreadID)
	if err != nil {
		return "", err
	}
	goal, err := getThreadGoal(ctx, session.client, session.threadID)
	if err != nil {
		return "", err
	}
	objective = goalObjective(goal)
	if objective == "" {
		return "", fmt.Errorf("no paused Codex goal is set for this thread")
	}
	return objective, setThreadGoalStatus(ctx, session.client, session.threadID, "active")
}

func goalObjective(goal *codex.ThreadGoal) string {
	if goal == nil {
		return ""
	}
	return strings.TrimSpace(goal.Objective)
}

func (a *app) ensureTopicLiveSession(ctx context.Context, chatID int64, messageThreadID int, titleHint string) (*activeTurn, error) {
	key := fmt.Sprintf("%d:%d", chatID, messageThreadID)
	if session := a.activeSession(key); session != nil {
		return session, nil
	}
	return a.startTopicSession(ctx, key, chatID, messageThreadID, titleHint)
}

func (a *app) ensureExistingTopicLiveSession(ctx context.Context, chatID int64, messageThreadID int) (*activeTurn, error) {
	key := fmt.Sprintf("%d:%d", chatID, messageThreadID)
	if session := a.activeSession(key); session != nil {
		return session, nil
	}

	conv, ok, err := a.store.Get(chatID, messageThreadID)
	if err != nil {
		return nil, err
	}
	if !ok || conv.CodexThreadID == "" {
		return nil, fmt.Errorf("this Telegram topic has not started a Codex thread yet")
	}
	return a.startTopicSession(ctx, key, chatID, messageThreadID, "")
}

func (a *app) withExistingTopicCodexClient(ctx context.Context, chatID int64, messageThreadID int, resume bool, fn func(*codex.Client, string) error) error {
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
	if resume {
		if err := a.resumeCodexThread(ctx, c, conv.CodexThreadID); err != nil {
			return err
		}
	}
	if err := fn(c, conv.CodexThreadID); err != nil {
		return err
	}
	return nil
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

func setThreadGoalStatus(ctx context.Context, c *codex.Client, threadID, status string) error {
	var out codex.ThreadGoalGetResponse
	return c.Call(ctx, "thread/goal/set", map[string]any{
		"threadId": threadID,
		"status":   status,
	}, &out)
}

func getThreadGoal(ctx context.Context, c *codex.Client, threadID string) (*codex.ThreadGoal, error) {
	var out codex.ThreadGoalGetResponse
	if err := c.Call(ctx, "thread/goal/get", map[string]any{
		"threadId": threadID,
	}, &out); err != nil {
		if isCodexGoalMissingStorageError(err) {
			return nil, nil
		}
		return nil, err
	}
	return out.Goal, nil
}

func isCodexGoalMissingStorageError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "no such table: thread_goals")
}
