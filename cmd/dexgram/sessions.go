package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"dexgram/internal/codex"
	"dexgram/internal/state"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

const (
	sessionProjectsPerPage = 8
	sessionThreadsPerPage  = 8
	sessionListPageSize    = 100
)

func (a *app) handleSessionsCommand(ctx context.Context, b *bot.Bot, msg *models.Message, query string) {
	browser, token, err := a.newSessionBrowser(ctx, msg.Chat.ID, msg.MessageThreadID, "", query)
	if err != nil {
		_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:          msg.Chat.ID,
			MessageThreadID: msg.MessageThreadID,
			Text:            "Could not load Codex sessions:\n\n" + err.Error(),
		})
		return
	}
	text, markup := a.sessionBrowserView(token, browser)
	_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
		ChatID:              msg.Chat.ID,
		MessageThreadID:     msg.MessageThreadID,
		Text:                text,
		ReplyMarkup:         markup,
		DisableNotification: true,
	})
}

func (a *app) newSessionBrowser(ctx context.Context, chatID int64, messageThreadID int, pendingFreshKey, query string) (*sessionBrowser, string, error) {
	threads, err := a.fetchCodexThreads(ctx, strings.TrimSpace(query))
	if err != nil {
		return nil, "", err
	}
	browser := &sessionBrowser{
		chatID:          chatID,
		messageThreadID: messageThreadID,
		pendingFreshKey: pendingFreshKey,
		query:           strings.TrimSpace(query),
		threads:         threads,
		sectionIndex:    -1,
		projectIndex:    -1,
		createdAt:       time.Now(),
	}
	if err := a.loadSessionCatalog(browser); err != nil {
		return nil, "", err
	}
	token := strconv.FormatInt(a.sessionBrowserSeq.Add(1), 36)
	a.mu.Lock()
	if a.sessionBrowsers == nil {
		a.sessionBrowsers = map[string]*sessionBrowser{}
	}
	a.sessionBrowsers[token] = browser
	a.mu.Unlock()
	return browser, token, nil
}

func (a *app) sessionBrowser(token string) (*sessionBrowser, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	browser := a.sessionBrowsers[token]
	return browser, browser != nil
}

func (a *app) forgetSessionBrowser(token string) {
	a.mu.Lock()
	delete(a.sessionBrowsers, token)
	a.mu.Unlock()
}

func (a *app) fetchCodexThreads(ctx context.Context, query string) ([]codex.Thread, error) {
	c, err := a.startCodexAppServer(ctx, codex.StartOptions{
		CLIPath:    a.cfg.Codex.CLIPath,
		WorkingDir: a.cfg.Codex.CWD,
	}, "sessions")
	if err != nil {
		return nil, err
	}

	threads, err := fetchThreadListPages(ctx, c, query, true)
	if err != nil {
		threads, err = fetchThreadListPages(ctx, c, query, false)
		if err != nil {
			return nil, err
		}
	}
	if query != "" {
		threads = filterThreads(threads, query)
	}
	sort.SliceStable(threads, func(i, j int) bool {
		return threads[i].UpdatedAt > threads[j].UpdatedAt
	})
	return threads, nil
}

func fetchThreadListPages(ctx context.Context, c *codex.Client, query string, includeSourceKinds bool) ([]codex.Thread, error) {
	var threads []codex.Thread
	var cursor any
	seen := map[string]bool{}
	for {
		params := map[string]any{"limit": sessionListPageSize}
		if includeSourceKinds {
			params["sourceKinds"] = []string{"cli", "vscode", "appServer", "exec", "unknown"}
		}
		if query != "" {
			params["searchTerm"] = query
		}
		if cursor != nil {
			params["cursor"] = cursor
		}
		var out codex.ThreadListResponse
		if err := c.Call(ctx, "thread/list", params, &out); err != nil {
			return nil, err
		}
		threads = append(threads, threadListItems(out)...)
		next := threadListCursor(out)
		if next == nil {
			break
		}
		key := fmt.Sprint(next)
		if seen[key] {
			return nil, fmt.Errorf("Codex returned a repeated thread list cursor")
		}
		seen[key] = true
		cursor = next
	}
	return threads, nil
}

func threadListItems(out codex.ThreadListResponse) []codex.Thread {
	switch {
	case len(out.Data) > 0:
		return out.Data
	case len(out.Items) > 0:
		return out.Items
	default:
		return out.Threads
	}
}

func threadListCursor(out codex.ThreadListResponse) any {
	for _, raw := range []json.RawMessage{out.NextCursor, out.NextCursorAlt} {
		trimmed := strings.TrimSpace(string(raw))
		if trimmed == "" || trimmed == "null" {
			continue
		}
		var value any
		if err := json.Unmarshal(raw, &value); err == nil && value != nil {
			return value
		}
		return trimmed
	}
	return nil
}

func filterThreads(threads []codex.Thread, query string) []codex.Thread {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return threads
	}
	out := make([]codex.Thread, 0, len(threads))
	for _, thread := range threads {
		haystack := strings.ToLower(strings.Join([]string{
			thread.ID,
			threadTitle(thread),
			thread.Preview,
			thread.Cwd,
		}, "\n"))
		if strings.Contains(haystack, query) {
			out = append(out, thread)
		}
	}
	return out
}

func normalizeSessionPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	cleaned := filepath.Clean(path)
	return strings.ToLower(strings.TrimRight(cleaned, `\/`))
}

func (a *app) sessionBrowserView(token string, browser *sessionBrowser) (string, *models.InlineKeyboardMarkup) {
	if browser.sectionIndex < 0 {
		order := browser.orderedIndices(indicesOf(len(browser.sections)), func(i int) string {
			return "section:" + browser.sections[i].ID
		})
		start, end := pageBounds(browser.page, sessionProjectsPerPage, len(order))
		var rows [][]models.InlineKeyboardButton
		for _, i := range order[start:end] {
			section := browser.sections[i]
			key := "section:" + section.ID
			rows = append(rows, browser.pinRow(token, section.Name, fmt.Sprintf("sess:%s:section:%d", token, i), "s", i, key))
		}
		rows = append(rows, sessionNavRows(token, browser.page, sessionProjectsPerPage, len(order), "sections")...)
		title := "Codex sections"
		if browser.query != "" {
			title += "\nFilter: " + browser.query
		}
		return title, &models.InlineKeyboardMarkup{InlineKeyboard: rows}
	}
	if browser.projectIndex < 0 {
		return a.sessionBrowserProjectView(token, browser)
	}
	return a.sessionBrowserThreadView(token, browser)
}

func (b *sessionBrowser) pinRow(token, label, action, kind string, index int, key string) []models.InlineKeyboardButton {
	pin, value := "Pin", 1
	if b.isPinned(key) {
		label = "📌 " + label
		pin, value = "Unpin", 0
	}
	return []models.InlineKeyboardButton{
		{Text: shortButtonLabel(label), CallbackData: action},
		{Text: pin, CallbackData: fmt.Sprintf("sess:%s:pin:%s:%d:%d", token, kind, index, value)},
	}
}

func (a *app) sessionBrowserProjectView(token string, browser *sessionBrowser) (string, *models.InlineKeyboardMarkup) {
	section := browser.sections[browser.sectionIndex]
	order := browser.orderedIndices(indicesOf(len(browser.projects)), func(i int) string { return "project:" + browser.projects[i].ID })
	start, end := pageBounds(browser.page, sessionProjectsPerPage, len(order))
	title := section.Name
	if len(order) == 0 {
		title += "\nNo chats or projects match this view."
	}
	var rows [][]models.InlineKeyboardButton
	for _, i := range order[start:end] {
		project := browser.projects[i]
		label := fmt.Sprintf("%s (%d)", project.Name, project.ThreadCount)
		action := fmt.Sprintf("sess:%s:proj:%d", token, i)
		if project.ID == "" {
			rows = append(rows, []models.InlineKeyboardButton{{Text: label, CallbackData: action}})
		} else {
			rows = append(rows, browser.pinRow(token, label, action, "p", i, "project:"+project.ID))
		}
	}
	rows = append(rows, sessionNavRows(token, browser.page, sessionProjectsPerPage, len(order), "projects")...)
	rows = append(rows, []models.InlineKeyboardButton{{Text: "Back to sections", CallbackData: "sess:" + token + ":sections"}})
	return title, &models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

func (a *app) sessionBrowserThreadView(token string, browser *sessionBrowser) (string, *models.InlineKeyboardMarkup) {
	project := browser.projects[browser.projectIndex]
	order := browser.orderedIndices(project.ThreadIDs, func(i int) string { return "chat:" + browser.threads[i].ID })
	start, end := pageBounds(browser.page, sessionThreadsPerPage, len(order))
	var rows [][]models.InlineKeyboardButton
	for _, i := range order[start:end] {
		thread := browser.threads[i]
		title := threadTitle(thread)
		if title == "" {
			title = "Untitled session"
		}
		rows = append(rows, browser.pinRow(token, title, fmt.Sprintf("sess:%s:attach:%d", token, i), "t", i, "chat:"+thread.ID))
	}
	if project.ID != "" && project.CWD != "" {
		rows = append(rows, []models.InlineKeyboardButton{{Text: "New chat in this project", CallbackData: fmt.Sprintf("sess:%s:new:%d", token, browser.projectIndex)}})
	}
	rows = append(rows, sessionNavRows(token, browser.page, sessionThreadsPerPage, len(order), "threads")...)
	rows = append(rows, []models.InlineKeyboardButton{{Text: "Back to projects", CallbackData: "sess:" + token + ":back"}})
	return fmt.Sprintf("%s\n%d chats", project.Name, len(order)), &models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

func pageBounds(page, perPage, total int) (int, int) {
	if perPage <= 0 {
		perPage = 1
	}
	maxPage := max((total-1)/perPage, 0)
	page = min(max(page, 0), maxPage)
	start := page * perPage
	end := min(start+perPage, total)
	return start, end
}

func sessionNavRows(token string, page, perPage, total int, scope string) [][]models.InlineKeyboardButton {
	var rows [][]models.InlineKeyboardButton
	maxPage := max((total-1)/perPage, 0)
	var nav []models.InlineKeyboardButton
	if page > 0 {
		nav = append(nav, models.InlineKeyboardButton{Text: "Prev", CallbackData: fmt.Sprintf("sess:%s:page:%s:%d", token, scope, page-1)})
	}
	if page < maxPage {
		nav = append(nav, models.InlineKeyboardButton{Text: "Next", CallbackData: fmt.Sprintf("sess:%s:page:%s:%d", token, scope, page+1)})
	}
	if len(nav) > 0 {
		rows = append(rows, nav)
	}
	rows = append(rows, []models.InlineKeyboardButton{{Text: "Refresh", CallbackData: "sess:" + token + ":refresh"}})
	return rows
}

func (a *app) handleFreshTopicCallback(ctx context.Context, b *bot.Bot, query *models.CallbackQuery) {
	parts := strings.Split(query.Data, ":")
	if len(parts) != 3 {
		return
	}
	token, action := parts[1], parts[2]
	switch action {
	case "new":
		pending, ok := a.takeFreshTopic(token)
		if !ok {
			_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: query.ID, Text: "That pending message expired.", ShowAlert: true})
			return
		}
		if _, ok := pendingFreshTopicCommandName(pending); ok {
			if query.Message.Message != nil {
				_, _ = b.EditMessageReplyMarkup(ctx, &bot.EditMessageReplyMarkupParams{
					ChatID:    query.Message.Message.Chat.ID,
					MessageID: query.Message.Message.ID,
				})
			}
			_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: query.ID})
			return
		}
		if query.Message.Message != nil {
			_, _ = b.EditMessageText(ctx, &bot.EditMessageTextParams{
				ChatID:    query.Message.Message.Chat.ID,
				MessageID: query.Message.Message.ID,
				Text:      "Starting a new Codex chat.",
			})
		}
		a.submitBuiltPrompt(ctx, b, pending.chatID, pending.messageThreadID, pending.replyMessageID, pending.input, pending.displayText, pending.collaborationMode)
		_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: query.ID, Text: "Starting new chat."})
	case "sessions":
		pending, ok := a.freshTopic(token)
		if !ok {
			_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: query.ID, Text: "That pending message expired.", ShowAlert: true})
			return
		}
		browser, browserToken, err := a.newSessionBrowser(ctx, pending.chatID, pending.messageThreadID, token, "")
		if err != nil {
			_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: query.ID, Text: "Could not load sessions.", ShowAlert: true})
			return
		}
		text, markup := a.sessionBrowserView(browserToken, browser)
		if query.Message.Message != nil {
			_, _ = b.EditMessageText(ctx, &bot.EditMessageTextParams{
				ChatID:      query.Message.Message.Chat.ID,
				MessageID:   query.Message.Message.ID,
				Text:        text,
				ReplyMarkup: markup,
			})
		}
		_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: query.ID})
	case "project":
		_, _ = a.takeFreshTopic(token)
		if query.Message.Message != nil {
			_, _ = b.EditMessageText(ctx, &bot.EditMessageTextParams{
				ChatID:    query.Message.Message.Chat.ID,
				MessageID: query.Message.Message.ID,
				Text:      "Run /project <project name>, then send your message again.",
			})
		}
		_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: query.ID})
	}
}

func (a *app) handleSessionBrowserCallback(ctx context.Context, b *bot.Bot, query *models.CallbackQuery) {
	msg := query.Message.Message
	if msg == nil {
		return
	}
	parts := strings.Split(query.Data, ":")
	if len(parts) < 3 {
		return
	}
	token, action := parts[1], parts[2]
	browser, ok := a.sessionBrowser(token)
	if !ok || browser.chatID != msg.Chat.ID || browser.messageThreadID != msg.MessageThreadID {
		_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: query.ID, Text: "Open /sessions again in this topic.", ShowAlert: true})
		return
	}
	browser.mu.Lock()
	defer browser.mu.Unlock()
	if _, valid := a.sessionBrowser(token); !valid {
		return
	}
	switch action {
	case "section", "proj", "attach", "new":
		if len(parts) != 4 {
			return
		}
		index, err := strconv.Atoi(parts[3])
		if err != nil || index < 0 {
			return
		}
		switch action {
		case "section":
			if index >= len(browser.sections) {
				return
			}
			browser.sectionIndex, browser.projectIndex, browser.page = index, -1, 0
			browser.projects = browser.sections[index].Projects
			if len(browser.projects) == 1 && browser.projects[0].ID == "" {
				browser.projectIndex = 0
			}
		case "proj":
			if index >= len(browser.projects) {
				return
			}
			browser.projectIndex, browser.page = index, 0
		case "attach":
			if index >= len(browser.threads) {
				return
			}
			a.attachSessionFromBrowser(ctx, b, query, token, browser, browser.threads[index])
			return
		case "new":
			if index >= len(browser.projects) {
				return
			}
			project := browser.projects[index]
			if project.ID == "" || project.CWD == "" {
				return
			}
			_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: query.ID})
			a.createChatTopic(ctx, b, msg, project.Name, project.CWD, "New chat", false)
			return
		}
	case "sections":
		browser.sectionIndex, browser.projectIndex, browser.page = -1, -1, 0
		browser.projects = nil
	case "back":
		if browser.sectionIndex < 0 {
			return
		}
		if browser.sections[browser.sectionIndex].ID == "other" {
			browser.sectionIndex, browser.projects = -1, nil
		}
		browser.projectIndex, browser.page = -1, 0
	case "page":
		if len(parts) != 5 {
			return
		}
		page, err := strconv.Atoi(parts[4])
		if err != nil || page < 0 {
			return
		}
		view := "sections"
		if browser.sectionIndex >= 0 {
			view = "projects"
		}
		if browser.projectIndex >= 0 {
			view = "threads"
		}
		if parts[3] != view {
			return
		}
		browser.page = page
	case "pin":
		if err := a.applySessionPin(browser, parts); err != nil {
			_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: query.ID, Text: "Could not update this pin.", ShowAlert: true})
			return
		}
	case "refresh":
		fresh, newToken, err := a.newSessionBrowser(ctx, browser.chatID, browser.messageThreadID, browser.pendingFreshKey, browser.query)
		if err != nil {
			_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: query.ID, Text: "Refresh failed.", ShowAlert: true})
			return
		}
		a.forgetSessionBrowser(token)
		token, browser = newToken, fresh
	default:
		return
	}
	text, markup := a.sessionBrowserView(token, browser)
	_, _ = b.EditMessageText(ctx, &bot.EditMessageTextParams{ChatID: msg.Chat.ID, MessageID: msg.ID, Text: text, ReplyMarkup: markup})
	_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: query.ID})
}

func (a *app) applySessionPin(browser *sessionBrowser, parts []string) error {
	if len(parts) != 6 || (parts[5] != "0" && parts[5] != "1") {
		return fmt.Errorf("invalid pin request")
	}
	index, err := strconv.Atoi(parts[4])
	if err != nil || index < 0 {
		return fmt.Errorf("invalid pin index")
	}
	key := ""
	switch parts[3] {
	case "s":
		if index < len(browser.sections) {
			key = "section:" + browser.sections[index].ID
		}
	case "p":
		if index < len(browser.projects) && browser.projects[index].ID != "" {
			key = "project:" + browser.projects[index].ID
		}
	case "t":
		if index < len(browser.threads) {
			key = "chat:" + browser.threads[index].ID
		}
	}
	if key == "" {
		return fmt.Errorf("pin target missing")
	}
	pinned := parts[5] == "1"
	if err := a.store.SetWorkspacePin(browser.chatID, key, pinned); err != nil {
		return err
	}
	browser.pins[key] = pinned
	return nil
}

func (a *app) attachSessionFromBrowser(ctx context.Context, b *bot.Bot, query *models.CallbackQuery, token string, browser *sessionBrowser, thread codex.Thread) {
	key := fmt.Sprintf("%d:%d", browser.chatID, browser.messageThreadID)
	if a.activeSession(key) != nil {
		_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: query.ID, Text: "This topic already has an active Codex session.", ShowAlert: true})
		return
	}
	title := threadTitle(thread)
	if title == "" {
		title = "Codex session"
	}
	projectName := ""
	if browser.projectIndex >= 0 {
		project := browser.projects[browser.projectIndex]
		if project.ID != "" {
			projectName = project.Name
		}
	}
	conv := state.Conversation{
		ChatID:          browser.chatID,
		MessageThreadID: browser.messageThreadID,
		CodexThreadID:   thread.ID,
		ProjectName:     projectName,
		CWD:             thread.Cwd,
		Projectless:     projectName == "",
		TopicTitle:      topicTitle(projectName, title),
		TopicNamed:      false,
	}
	if err := a.store.Upsert(conv); err != nil {
		_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: query.ID, Text: "Could not save mapping.", ShowAlert: true})
		return
	}
	if conv.TopicTitle != "" {
		if _, err := b.EditForumTopic(ctx, &bot.EditForumTopicParams{
			ChatID:          browser.chatID,
			MessageThreadID: browser.messageThreadID,
			Name:            conv.TopicTitle,
		}); err != nil {
			log.Printf("rename attached session topic: %v", err)
		} else {
			conv.TopicNamed = true
			_ = a.store.Upsert(conv)
		}
	}
	a.forgetSessionBrowser(token)
	if query.Message.Message != nil {
		_, _ = b.EditMessageText(ctx, &bot.EditMessageTextParams{
			ChatID:    query.Message.Message.Chat.ID,
			MessageID: query.Message.Message.ID,
			Text:      fmt.Sprintf("Attached Codex session: %s\nSyncing recent history...", title),
		})
	}
	_, _ = b.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: query.ID, Text: "Session attached."})
	syncText := "Synced recent history. Use /history for older turns."
	if err := a.syncRecentAttachedHistory(ctx, b, &conv); err != nil {
		log.Printf("sync attached session history: %v", err)
		syncText = "Recent history sync failed; live Desktop replies will still be mirrored from now on."
		_, _ = b.SendMessage(ctx, &bot.SendMessageParams{
			ChatID:              browser.chatID,
			MessageThreadID:     browser.messageThreadID,
			Text:                "Attached, but recent history sync failed:\n\n" + err.Error(),
			DisableNotification: true,
		})
	}
	if query.Message.Message != nil {
		_, _ = b.EditMessageText(ctx, &bot.EditMessageTextParams{
			ChatID:    query.Message.Message.Chat.ID,
			MessageID: query.Message.Message.ID,
			Text:      fmt.Sprintf("Attached Codex session: %s\n%s Live Desktop replies will be mirrored here.", title, syncText),
		})
	}
	a.markDesktopMirrorActive(browser.chatID, browser.messageThreadID)
	if browser.pendingFreshKey == "" {
		return
	}
	pending, ok := a.takeFreshTopic(browser.pendingFreshKey)
	if !ok {
		return
	}
	if _, ok := pendingFreshTopicCommandName(pending); ok {
		return
	}
	a.submitBuiltPrompt(ctx, b, pending.chatID, pending.messageThreadID, pending.replyMessageID, pending.input, pending.displayText, pending.collaborationMode)
}
