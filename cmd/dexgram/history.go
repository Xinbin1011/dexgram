package main

import (
	"context"
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"dexgram/internal/codex"
	"dexgram/internal/state"
	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

const historyPageSize = 6
const historyReadTimeout = 20 * time.Second

type historyBrowser struct {
	mu       sync.Mutex
	token    string
	threadID string
	cursors  []string
	page     int
	turns    []codex.Turn
	next     string
	created  time.Time
}

func (a *app) readHistoryPage(ctx context.Context, conv state.Conversation, cursor string) ([]codex.Turn, string, error) {
	client, err := a.startCodexAppServer(ctx, codex.StartOptions{
		CLIPath: a.cfg.Codex.CLIPath, WorkingDir: appServerWorkingDir(conv),
	}, "read history")
	if err != nil {
		return nil, "", err
	}
	return readHistoryPage(ctx, client, conv.CodexThreadID, cursor)
}

type historyRPC interface {
	Call(context.Context, string, any, any) error
}

func readHistoryPage(ctx context.Context, client historyRPC, threadID, cursor string) ([]codex.Turn, string, error) {
	return readTurnHistoryPage(ctx, client, threadID, cursor, "full")
}

func readTurnHistoryPage(ctx context.Context, client historyRPC, threadID, cursor, itemsView string) ([]codex.Turn, string, error) {
	ctx, cancel := context.WithTimeout(ctx, historyReadTimeout)
	defer cancel()
	anchor, legacy := strings.CutPrefix(cursor, "legacy:")
	if !legacy {
		params := map[string]any{"threadId": threadID, "limit": historyPageSize, "itemsView": itemsView, "sortDirection": "desc"}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var page struct {
			Data []codex.Turn `json:"data"`
			Next string       `json:"nextCursor"`
		}
		err := client.Call(ctx, "thread/turns/list", params, &page)
		if err == nil {
			if page.Next != "" && page.Next == cursor {
				return nil, "", fmt.Errorf("Codex repeated a history cursor")
			}
			return page.Data, page.Next, nil
		}
		if cursor != "" || !strings.Contains(err.Error(), "code -32601") {
			return nil, "", err
		}
	}
	var read codex.ThreadReadResponse
	if err := client.Call(ctx, "thread/read", map[string]any{"threadId": threadID, "includeTurns": true}, &read); err != nil {
		return nil, "", err
	}
	return legacyHistoryPage(read.Thread.Turns, anchor)
}

type syncHistory struct {
	latest      string
	turns       []codex.Turn
	foundMarker bool
}

// A zero limit mirrors every new turn, or only establishes a baseline when
// there is no marker. Manual sync without a marker sends the latest turn.
func readSyncHistory(ctx context.Context, client historyRPC, threadID, marker string, limit int) (syncHistory, error) {
	ctx, cancel := context.WithTimeout(ctx, historyReadTimeout)
	defer cancel()
	var history syncHistory
	for cursor := ""; ; {
		page, next, err := readTurnHistoryPage(ctx, client, threadID, cursor, "notLoaded")
		if err != nil {
			return syncHistory{}, err
		}
		var wanted []string
		for _, turn := range page {
			if history.latest == "" && turn.Status == "completed" {
				history.latest = turn.ID
			}
			if turn.ID == marker && marker != "" {
				history.foundMarker = true
				break
			}
			if turn.Status != "completed" {
				continue
			}
			if (limit > 0 && len(history.turns)+len(wanted) < limit) || (limit == 0 && marker != "") {
				wanted = append(wanted, turn.ID)
			}
			if marker == "" {
				break
			}
		}
		if len(wanted) > 0 {
			full, _, err := readHistoryPage(ctx, client, threadID, cursor)
			if err != nil {
				return syncHistory{}, err
			}
			before := len(history.turns)
			for _, turn := range full {
				if turn.Status == "completed" && slices.Contains(wanted, turn.ID) {
					history.turns = append(history.turns, turn)
				}
			}
			if len(history.turns)-before != len(wanted) {
				return syncHistory{}, fmt.Errorf("Codex history changed while syncing")
			}
		}
		if history.foundMarker || next == "" || (marker == "" && history.latest != "") {
			break
		}
		cursor = next
	}
	if marker != "" && !history.foundMarker {
		history.turns = nil
	}
	slices.Reverse(history.turns)
	return history, nil
}

func legacyHistoryPage(turns []codex.Turn, anchor string) ([]codex.Turn, string, error) {
	end := len(turns)
	if anchor != "" {
		end = slices.IndexFunc(turns, func(t codex.Turn) bool { return t.ID == anchor })
		if end < 0 {
			return nil, "", fmt.Errorf("History changed. Run /history again.")
		}
	}
	start := max(0, end-historyPageSize)
	page := append([]codex.Turn(nil), turns[start:end]...)
	next := ""
	if start > 0 {
		next = "legacy:" + turns[start].ID
	}
	slices.Reverse(page)
	return page, next, nil
}

func (a *app) handleHistoryCommand(ctx context.Context, b *bot.Bot, msg *models.Message) {
	conv, ok, err := a.store.Get(msg.Chat.ID, msg.MessageThreadID)
	if err != nil || !ok || conv.CodexThreadID == "" {
		a.sendOrEditPlain(ctx, b, msg.Chat.ID, msg.MessageThreadID, 0, "Attach or start a Codex chat in this topic first.", nil)
		return
	}
	turns, next, err := a.readHistoryPage(ctx, conv, "")
	if err != nil {
		log.Printf("read Codex history: %v", err)
		a.sendOrEditPlain(ctx, b, msg.Chat.ID, msg.MessageThreadID, 0, "Could not read Codex history. Try /history again.", nil)
		return
	}
	h := &historyBrowser{token: strconv.FormatInt(a.sessionBrowserSeq.Add(1), 36), threadID: conv.CodexThreadID, cursors: []string{""}, turns: turns, next: next, created: time.Now()}
	key := fmt.Sprintf("%d:%d", msg.Chat.ID, msg.MessageThreadID)
	a.mu.Lock()
	if a.historyBrowsers == nil {
		a.historyBrowsers = map[string]*historyBrowser{}
	}
	for key, old := range a.historyBrowsers {
		if time.Since(old.created) > 30*time.Minute {
			delete(a.historyBrowsers, key)
		}
	}
	a.historyBrowsers[key] = h
	a.mu.Unlock()
	text, markup := h.view()
	a.sendOrEditPlain(ctx, b, msg.Chat.ID, msg.MessageThreadID, 0, text, markup)
}

func (h *historyBrowser) view() (string, *models.InlineKeyboardMarkup) {
	var rows [][]models.InlineKeyboardButton
	for i, turn := range h.turns {
		if turn.Status != "completed" {
			continue
		}
		label := strings.Join(strings.Fields(turnUserPrompt(turn)), " ")
		if label == "" {
			label = "Read turn"
		}
		rows = append(rows, []models.InlineKeyboardButton{{Text: shortButtonLabel(label), CallbackData: fmt.Sprintf("hist:%s:%d:read:%d", h.token, h.page, i)}})
	}
	var nav []models.InlineKeyboardButton
	if h.page > 0 {
		nav = append(nav, models.InlineKeyboardButton{Text: "Newer", CallbackData: fmt.Sprintf("hist:%s:%d:newer", h.token, h.page)})
	}
	if h.next != "" {
		nav = append(nav, models.InlineKeyboardButton{Text: "Older", CallbackData: fmt.Sprintf("hist:%s:%d:older", h.token, h.page)})
	}
	if len(nav) > 0 {
		rows = append(rows, nav)
	}
	title := fmt.Sprintf("Codex history · Page %d\nChoose a turn to read it.", h.page+1)
	if len(h.turns) == 0 {
		title = "No saved Codex history in this topic."
	}
	return title, &models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

func (a *app) handleHistoryCallback(ctx context.Context, b *bot.Bot, q *models.CallbackQuery) {
	msg := q.Message.Message
	if msg == nil {
		return
	}
	parts := strings.Split(q.Data, ":")
	if len(parts) < 4 {
		return
	}
	key := fmt.Sprintf("%d:%d", msg.Chat.ID, msg.MessageThreadID)
	a.mu.Lock()
	h := a.historyBrowsers[key]
	a.mu.Unlock()
	answer := func(text string) {
		_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: q.ID, Text: text, ShowAlert: text != ""})
	}
	if h == nil || h.token != parts[1] || time.Since(h.created) > 30*time.Minute {
		answer("Run /history again.")
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if strconv.Itoa(h.page) != parts[2] {
		answer("That page has changed.")
		return
	}
	conv, ok, err := a.store.Get(msg.Chat.ID, msg.MessageThreadID)
	if err != nil || !ok || conv.CodexThreadID != h.threadID {
		answer("This topic changed. Run /history again.")
		return
	}
	switch parts[3] {
	case "read":
		if len(parts) != 5 {
			return
		}
		i, err := strconv.Atoi(parts[4])
		if err != nil || i < 0 || i >= len(h.turns) || h.turns[i].Status != "completed" {
			return
		}
		answer("")
		turn := h.turns[i]
		_, _, final := summarizeTurn(turn)
		text := "User\n" + turnUserPrompt(turn) + "\n\nCodex\n" + stripAssistantAppDirectives(final)
		if err := sendRichMessageNotify(ctx, b, msg.Chat.ID, msg.MessageThreadID, text, false); err != nil {
			log.Printf("send Codex history: %v", err)
		}
		return
	case "newer", "older":
		page := h.page - 1
		if parts[3] == "older" {
			page = h.page + 1
		}
		if page < 0 || page > len(h.cursors) || (page > h.page && h.next == "") {
			return
		}
		cursor := h.next
		if page < len(h.cursors) {
			cursor = h.cursors[page]
		}
		answer("")
		turns, next, err := a.readHistoryPage(ctx, conv, cursor)
		if err != nil {
			a.sendOrEditPlain(ctx, b, msg.Chat.ID, msg.MessageThreadID, 0, "Could not load that history page. Run /history again.", nil)
			return
		}
		if page == len(h.cursors) {
			h.cursors = append(h.cursors, cursor)
		}
		h.page, h.turns, h.next = page, turns, next
		text, markup := h.view()
		a.sendOrEditPlain(ctx, b, msg.Chat.ID, msg.MessageThreadID, msg.ID, text, markup)
	}
}
