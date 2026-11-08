package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"

	"dexgram/internal/codex"
	"dexgram/internal/state"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

const (
	defaultSyncTurnLimit = 1
	maxSyncTurnLimit     = 5
	maxSyncTextRunes     = 6000
)

func (a *app) handleSyncCommand(ctx context.Context, b *bot.Bot, msg *models.Message, arg string) {
	limit, err := parseSyncLimit(arg)
	if err != nil {
		_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:          msg.Chat.ID,
			MessageThreadID: msg.MessageThreadID,
			Text:            err.Error(),
		})
		return
	}
	conv, ok, err := a.store.Get(msg.Chat.ID, msg.MessageThreadID)
	if err != nil {
		log.Printf("read conversation for sync: %v", err)
		_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:          msg.Chat.ID,
			MessageThreadID: msg.MessageThreadID,
			Text:            "Could not read Dexgram state: " + err.Error(),
		})
		return
	}
	if !ok || conv.CodexThreadID == "" {
		_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:          msg.Chat.ID,
			MessageThreadID: msg.MessageThreadID,
			Text:            "This topic is not mapped to a Codex thread yet.",
		})
		return
	}
	conv = a.topicConversation(msg.Chat.ID, msg.MessageThreadID)
	c, err := a.startCodexAppServer(ctx, codex.StartOptions{
		CLIPath:    a.cfg.Codex.CLIPath,
		WorkingDir: appServerWorkingDir(conv),
	}, "manual sync")
	if err != nil {
		_, _ = b.SendMessage(ctx, &bot.SendMessageParams{ChatID: msg.Chat.ID, MessageThreadID: msg.MessageThreadID, Text: "Could not start Codex app-server: " + err.Error()})
		return
	}
	resume, err := a.resumeCodexThreadResult(ctx, c, conv.CodexThreadID)
	if err != nil {
		_, _ = b.SendMessage(ctx, &bot.SendMessageParams{ChatID: msg.Chat.ID, MessageThreadID: msg.MessageThreadID, Text: "Could not resume Codex thread: " + err.Error()})
		return
	}
	thread := resume.Thread
	if len(thread.Turns) == 0 {
		var read codex.ThreadReadResponse
		if err := c.Call(ctx, "thread/read", map[string]any{"threadId": conv.CodexThreadID}, &read); err != nil {
			_, _ = b.SendMessage(ctx, &bot.SendMessageParams{ChatID: msg.Chat.ID, MessageThreadID: msg.MessageThreadID, Text: "Could not read Codex thread: " + err.Error()})
			return
		}
		thread = read.Thread
	}
	if len(thread.Turns) == 0 {
		_, _ = b.SendMessage(ctx, &bot.SendMessageParams{ChatID: msg.Chat.ID, MessageThreadID: msg.MessageThreadID, Text: "No completed Codex turns to sync."})
		return
	}

	turns := unsyncedTurns(thread.Turns, conv.LastSyncedTurnID)
	if len(turns) == 0 {
		_, _ = b.SendMessage(ctx, &bot.SendMessageParams{ChatID: msg.Chat.ID, MessageThreadID: msg.MessageThreadID, Text: "Already synced."})
		return
	}
	if len(turns) > limit {
		turns = turns[len(turns)-limit:]
	}
	for _, turn := range turns {
		if a.shouldSkipTelegramOriginTurn(conv.CodexThreadID, turn) {
			conv.LastSyncedTurnID = turn.ID
			continue
		}
		if err := renderHistoricalTurn(ctx, b, msg.Chat.ID, msg.MessageThreadID, turn); err != nil {
			log.Printf("sync turn render failed chat_id=%d thread_id=%d turn_id=%s: %v", msg.Chat.ID, msg.MessageThreadID, turn.ID, err)
			return
		}
		conv.LastSyncedTurnID = turn.ID
	}
	if err := a.store.Upsert(conv); err != nil {
		log.Printf("store sync marker: %v", err)
	}
}

func parseSyncLimit(arg string) (int, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return defaultSyncTurnLimit, nil
	}
	n, err := strconv.Atoi(arg)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("Usage: /sync [1-%d]", maxSyncTurnLimit)
	}
	if n > maxSyncTurnLimit {
		return maxSyncTurnLimit, nil
	}
	return n, nil
}

func unsyncedTurns(turns []codex.Turn, lastID string) []codex.Turn {
	if lastID == "" {
		for i := len(turns) - 1; i >= 0; i-- {
			if turns[i].Status == "completed" {
				return []codex.Turn{turns[i]}
			}
		}
		return nil
	}
	var out []codex.Turn
	seen := false
	for _, turn := range turns {
		if seen && turn.Status == "completed" {
			out = append(out, turn)
		}
		if turn.ID == lastID {
			seen = true
		}
	}
	return out
}

func renderHistoricalTurn(ctx context.Context, b *bot.Bot, chatID int64, messageThreadID int, turn codex.Turn) error {
	return renderHistoricalTurnNotify(ctx, b, chatID, messageThreadID, turn, turnDesktopPrompt(turn), true)
}

func renderHistoricalTurnSilent(ctx context.Context, b *bot.Bot, chatID int64, messageThreadID int, turn codex.Turn) error {
	return renderHistoricalTurnNotify(ctx, b, chatID, messageThreadID, turn, turnDesktopPrompt(turn), false)
}

func renderHistoricalTurnNotify(ctx context.Context, b *bot.Bot, chatID int64, messageThreadID int, turn codex.Turn, prompt string, notify bool) error {
	_, _, final := summarizeTurn(turn)
	final = truncateSyncText(stripAssistantAppDirectives(final))
	if strings.TrimSpace(final) != "" {
		final = prefixQuotedPrompt(prompt, final)
		return sendRichMessageNotify(ctx, b, chatID, messageThreadID, final, notify)
	}
	return nil
}

func (a *app) syncRecentAttachedHistory(ctx context.Context, b *bot.Bot, conv *state.Conversation) error {
	turns, _, err := a.readHistoryPage(ctx, *conv, "")
	if err != nil {
		return err
	}
	for i, j := 0, len(turns)-1; i < j; i, j = i+1, j-1 {
		turns[i], turns[j] = turns[j], turns[i]
	}
	for _, turn := range turns {
		if turn.Status != "completed" {
			continue
		}
		if a.shouldSkipTelegramOriginTurn(conv.CodexThreadID, turn) {
			conv.LastSyncedTurnID = turn.ID
			continue
		}
		if err := renderHistoricalTurn(ctx, b, conv.ChatID, conv.MessageThreadID, turn); err != nil {
			return err
		}
		conv.LastSyncedTurnID = turn.ID
	}
	if conv.LastSyncedTurnID != "" {
		if err := a.store.Upsert(*conv); err != nil {
			log.Printf("store attach sync marker: %v", err)
		}
	}
	return nil
}

func (a *app) shouldSkipTelegramOriginTurn(codexThreadID string, turn codex.Turn) bool {
	if turnHasTelegramTranscriptPrompt(turn) {
		return true
	}
	synced, err := a.store.IsTelegramTurn(codexThreadID, turn.ID)
	if err != nil {
		log.Printf("read telegram turn marker thread_id=%s turn_id=%s: %v", codexThreadID, turn.ID, err)
		return false
	}
	return synced
}

func truncateSyncText(text string) string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) <= maxSyncTextRunes {
		return string(runes)
	}
	return string(runes[:maxSyncTextRunes]) + "\n\n... truncated by /sync limit"
}

func stripAssistantAppDirectives(text string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "::") {
			continue
		}
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func summarizeTurn(turn codex.Turn) (string, []string, string) {
	var initial []string
	var runLines []string
	var lastAgent string
	var final string
	for _, item := range turn.Items {
		switch item.Type {
		case "agentMessage":
			lastAgent = item.Text
			if item.Phase != nil && *item.Phase == "final_answer" {
				final = item.Text
			} else if strings.TrimSpace(item.Text) != "" {
				initial = append(initial, item.Text)
			}
		case "plan":
			if strings.TrimSpace(item.Text) != "" {
				initial = append(initial, item.Text)
			}
		default:
			if line := runLogLine(item); line != "" {
				runLines = append(runLines, line)
			}
		}
	}
	if final == "" {
		final = lastAgent
	}
	return strings.Join(initial, "\n\n"), runLines, final
}

func turnUserPrompt(turn codex.Turn) string {
	var prompts []string
	for _, item := range turn.Items {
		if item.Type != "userMessage" {
			continue
		}
		if text := strings.TrimSpace(item.Text); text != "" {
			prompts = append(prompts, text)
			continue
		}
		var content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if len(item.Content) == 0 || json.Unmarshal(item.Content, &content) != nil {
			continue
		}
		for _, part := range content {
			if part.Type == "text" || part.Type == "input_text" {
				if text := strings.TrimSpace(part.Text); text != "" {
					prompts = append(prompts, text)
				}
			}
		}
	}
	return strings.Join(prompts, "\n\n")
}

func turnHasTelegramTranscriptPrompt(turn codex.Turn) bool {
	for _, item := range turn.Items {
		if item.Type != "userMessage" {
			continue
		}
		for _, text := range itemTextParts(item) {
			if isTelegramTranscriptPrompt(text) {
				return true
			}
		}
	}
	return false
}

func turnDesktopPrompt(turn codex.Turn) string {
	if turnHasTelegramTranscriptPrompt(turn) {
		return ""
	}
	return turnUserPrompt(turn)
}

func itemTextParts(item codex.ThreadItem) []string {
	var parts []string
	if text := strings.TrimSpace(item.Text); text != "" {
		parts = append(parts, text)
	}
	var content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if len(item.Content) == 0 || json.Unmarshal(item.Content, &content) != nil {
		return parts
	}
	for _, part := range content {
		if part.Type == "text" || part.Type == "input_text" {
			if text := strings.TrimSpace(part.Text); text != "" {
				parts = append(parts, text)
			}
		}
	}
	return parts
}

func isTelegramTranscriptPrompt(text string) bool {
	return strings.HasPrefix(strings.TrimSpace(text), telegramTranscriptPrefix)
}

func prefixQuotedPrompt(prompt, body string) string {
	prompt = truncatePromptQuote(prompt)
	if prompt == "" {
		return body
	}
	return markdownQuote(prompt) + "\n\n" + body
}

func markdownQuote(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			lines[i] = ">"
		} else {
			lines[i] = "> " + line
		}
	}
	return strings.Join(lines, "\n")
}

func truncatePromptQuote(text string) string {
	runes := []rune(strings.TrimSpace(text))
	const maxPromptQuoteRunes = 1200
	if len(runes) <= maxPromptQuoteRunes {
		return string(runes)
	}
	return string(runes[:maxPromptQuoteRunes]) + "\n\n... prompt truncated"
}
