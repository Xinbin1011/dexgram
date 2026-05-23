package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"dexgram/internal/codex"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

func (a *app) handleGoalCommand(ctx context.Context, b *bot.Bot, msg *models.Message, objective string) {
	objective = strings.TrimSpace(objective)
	if objective == "" {
		a.handleGoalStatus(ctx, b, msg)
		return
	}
	if isGoalStatusCommand(objective) {
		a.handleGoalStatus(ctx, b, msg)
		return
	}
	if isGoalHelpCommand(objective) {
		_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:          msg.Chat.ID,
			MessageThreadID: msg.MessageThreadID,
			Text:            goalUsageText(),
		})
		return
	}
	if isGoalPauseCommand(objective) {
		paused, err := pauseTopicGoalFunc(a, ctx, msg.Chat.ID, msg.MessageThreadID)
		if err != nil {
			log.Printf("pause codex goal failed: %v", err)
			text := "Dexgram could not pause the Codex goal:\n\n" + err.Error()
			if isCodexGoalsDisabledError(err) {
				text = codexGoalsDisabledMessage()
			}
			_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID:          msg.Chat.ID,
				MessageThreadID: msg.MessageThreadID,
				Text:            text,
			})
			return
		}
		_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:          msg.Chat.ID,
			MessageThreadID: msg.MessageThreadID,
			Text:            "Codex goal paused:\n" + paused,
		})
		return
	}
	if isGoalResumeCommand(objective) {
		resumed, err := resumeTopicGoalFunc(a, ctx, msg.Chat.ID, msg.MessageThreadID)
		if err != nil {
			log.Printf("resume codex goal failed: %v", err)
			text := "Dexgram could not resume the Codex goal:\n\n" + err.Error()
			if isCodexGoalsDisabledError(err) {
				text = codexGoalsDisabledMessage()
			}
			_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID:          msg.Chat.ID,
				MessageThreadID: msg.MessageThreadID,
				Text:            text,
			})
			return
		}
		_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:          msg.Chat.ID,
			MessageThreadID: msg.MessageThreadID,
			Text:            "Codex goal resumed:\n" + resumed,
		})
		return
	}
	if isGoalClearCommand(objective) {
		if err := clearTopicGoalFunc(a, ctx, msg.Chat.ID, msg.MessageThreadID); err != nil {
			log.Printf("clear codex goal failed: %v", err)
			text := "Dexgram could not clear the Codex goal:\n\n" + err.Error()
			if isCodexGoalsDisabledError(err) {
				text = codexGoalsDisabledMessage()
			}
			_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
				ChatID:          msg.Chat.ID,
				MessageThreadID: msg.MessageThreadID,
				Text:            text,
			})
			return
		}
		_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:          msg.Chat.ID,
			MessageThreadID: msg.MessageThreadID,
			Text:            "Codex goal cleared.",
		})
		return
	}
	if err := setTopicGoalFunc(a, ctx, msg.Chat.ID, msg.MessageThreadID, objective); err != nil {
		log.Printf("set codex goal failed: %v", err)
		text := "Dexgram could not set the Codex goal:\n\n" + err.Error()
		if isCodexGoalsDisabledError(err) {
			text = codexGoalsDisabledMessage()
		}
		_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:          msg.Chat.ID,
			MessageThreadID: msg.MessageThreadID,
			Text:            text,
		})
		return
	}
	_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:          msg.Chat.ID,
		MessageThreadID: msg.MessageThreadID,
		Text:            "Codex goal set:\n" + objective,
	})
}

func (a *app) handleGoalStatus(ctx context.Context, b *bot.Bot, msg *models.Message) {
	goal, err := getTopicGoalFunc(a, ctx, msg.Chat.ID, msg.MessageThreadID)
	if err != nil {
		log.Printf("get codex goal failed: %v", err)
		text := "Dexgram could not read the Codex goal:\n\n" + err.Error()
		if isCodexGoalsDisabledError(err) {
			text = codexGoalsDisabledMessage()
		}
		_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:          msg.Chat.ID,
			MessageThreadID: msg.MessageThreadID,
			Text:            text,
		})
		return
	}
	_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:          msg.Chat.ID,
		MessageThreadID: msg.MessageThreadID,
		Text:            formatGoalStatus(goal, time.Now()),
	})
}

func goalUsageText() string {
	return "Usage: /goal <objective>\n" +
		"Show current: /goal\n" +
		"Pause: /goal pause\n" +
		"Resume: /goal resume\n" +
		"Clear: /goal clear"
}

func isGoalStatusCommand(text string) bool {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(text)))
	if len(fields) != 1 {
		return false
	}
	switch fields[0] {
	case "status", "show", "current", "get":
		return true
	default:
		return false
	}
}

func isGoalHelpCommand(text string) bool {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(text)))
	if len(fields) != 1 {
		return false
	}
	return fields[0] == "help" || fields[0] == "?"
}

func isGoalPauseCommand(text string) bool {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(text)))
	return len(fields) == 1 && fields[0] == "pause"
}

func isGoalResumeCommand(text string) bool {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(text)))
	return len(fields) == 1 && fields[0] == "resume"
}

func isGoalClearCommand(text string) bool {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(text)))
	if len(fields) != 1 {
		return false
	}
	switch fields[0] {
	case "clear", "delete", "remove", "stop", "off", "none":
		return true
	default:
		return false
	}
}

func formatGoalStatus(goal *codex.ThreadGoal, now time.Time) string {
	if goal == nil || strings.TrimSpace(goal.Objective) == "" {
		return "No Codex goal is set for this topic.\n\n" + goalUsageText()
	}
	lines := []string{"Codex goal", "Status: " + emptyAs(goal.Status, "unknown"), "Objective:\n" + goal.Objective}
	if goal.TokenBudget != nil {
		lines = append(lines, fmt.Sprintf("Tokens: %s / %s", formatNumber(goal.TokensUsed), formatNumber(*goal.TokenBudget)))
	} else if goal.TokensUsed > 0 {
		lines = append(lines, "Tokens used: "+formatNumber(goal.TokensUsed))
	}
	if goal.TimeUsedSeconds > 0 {
		lines = append(lines, "Time used: "+compactDuration(time.Duration(goal.TimeUsedSeconds)*time.Second))
	}
	if goal.UpdatedAt > 0 {
		updated := time.Unix(int64(goal.UpdatedAt), 0).Local()
		age := now.Sub(updated)
		if age > 0 {
			lines = append(lines, "Updated: "+compactDuration(age)+" ago ("+updated.Format("2006-01-02 15:04")+")")
		} else {
			lines = append(lines, "Updated: "+updated.Format("2006-01-02 15:04"))
		}
	}
	return strings.Join(lines, "\n")
}

func isCodexGoalsDisabledError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "goals feature is disabled")
}

func codexGoalsDisabledMessage() string {
	return "Codex goals are not enabled yet.\n\n" +
		"To enable /goal, edit:\n" +
		"%USERPROFILE%\\.codex\\config.toml\n\n" +
		"Add this section, or add the goals line under your existing [features] section:\n\n" +
		"[features]\n" +
		"goals = true\n\n" +
		"Then restart Codex/Dexgram and run /goal again."
}
