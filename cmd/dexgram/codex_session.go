package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"dexgram/internal/codex"
	"dexgram/internal/codexstate"
	"dexgram/internal/state"

	"github.com/go-telegram/bot"
)

var staleActiveTurnPattern = regexp.MustCompile(`(?i)but found\s+(?:[` + "`" + `']([^` + "`" + `']+)[` + "`" + `']|([^\s,.;]+))`)

func (a *app) startTopicSession(ctx context.Context, key string, chatID int64, messageThreadID int, titleHint string) (*activeTurn, error) {
	sessionCtx, cancel := context.WithCancel(ctx)
	conv := a.topicConversation(chatID, messageThreadID)
	var err error
	conv, err = prepareProjectlessWorkspace(conv, titleHint)
	if err != nil {
		cancel()
		return nil, err
	}
	seedExistingTurns := conv.CodexThreadID != "" && !conv.SideChat
	c, err := a.startCodexAppServer(ctx, codex.StartOptions{
		CLIPath:    a.cfg.Codex.CLIPath,
		WorkingDir: appServerWorkingDir(conv),
	}, "topic session key="+key)
	if err != nil {
		cancel()
		return nil, err
	}

	var resumedThread codex.Thread
	conv, resumedThread, err = a.prepareTopicConversation(ctx, c, conv)
	if err != nil {
		cancel()
		return nil, err
	}
	var activeTurns []codex.Turn
	if seedExistingTurns {
		activeTurns = a.activeCodexTurnsForResumedSession(conv.CodexThreadID, resumedThread)
	}

	session := &activeTurn{
		client:         c,
		threadID:       conv.CodexThreadID,
		ctx:            sessionCtx,
		cancel:         cancel,
		conv:           conv,
		turns:          map[string]*telegramTurn{},
		titleSyncItems: map[string]bool{},
		pendingEvents:  map[string][]codex.Event{},
	}
	seedSessionAutonomousTurns(session, activeTurns)
	if !a.registerSession(key, session) {
		cancel()
		return nil, fmt.Errorf("codex is already running in this topic")
	}
	if a.handlePendingCodexEvents(ctx, key, session) {
		return session, nil
	}
	if conv.CodexThreadID != "" {
		a.markDesktopMirrorActive(chatID, messageThreadID)
	}
	return session, nil
}

func (a *app) topicConversation(chatID int64, messageThreadID int) state.Conversation {
	conv, ok, err := a.store.Get(chatID, messageThreadID)
	if err != nil {
		log.Printf("read topic conversation: %v", err)
	}
	if !ok {
		return state.Conversation{
			ChatID:          chatID,
			MessageThreadID: messageThreadID,
			Projectless:     true,
		}
	}
	if conv.ProjectName == "" {
		conv.Projectless = true
	}
	return conv
}

func (a *app) prepareTopicConversation(ctx context.Context, c *codex.Client, conv state.Conversation) (state.Conversation, codex.Thread, error) {
	if conv.CodexThreadID == "" {
		threadID, cwd, err := a.startCodexThread(ctx, c, conv)
		if err != nil {
			return state.Conversation{}, codex.Thread{}, err
		}
		conv.CodexThreadID = threadID
		conv.CWD = cwd
		if conv.Projectless {
			if err := codexstate.RegisterProjectlessThread(threadID, projectlessRoot()); err != nil {
				log.Printf("register projectless Codex thread: %v", err)
			}
		}
		if err := a.store.Upsert(conv); err != nil {
			return state.Conversation{}, codex.Thread{}, err
		}
		log.Printf("mapped telegram thread %d:%d to codex thread %s", conv.ChatID, conv.MessageThreadID, threadID)
		return conv, codex.Thread{}, nil
	}
	if conv.SideChat {
		return conv, codex.Thread{}, nil
	}
	resume, err := a.resumeCodexThreadResult(ctx, c, conv.CodexThreadID)
	if err != nil {
		return state.Conversation{}, codex.Thread{}, err
	}
	return conv, resume.Thread, nil
}

func activeCodexTurns(thread codex.Thread) []codex.Turn {
	var active []codex.Turn
	for _, turn := range thread.Turns {
		if turn.ID != "" && !isTerminalTurnStatus(turn.Status) {
			active = append(active, turn)
		}
	}
	return active
}

func (a *app) activeCodexTurnsForResumedSession(threadID string, thread codex.Thread) []codex.Turn {
	var active []codex.Turn
	for _, turn := range activeCodexTurns(thread) {
		if a.knownTelegramTurnBestEffort(threadID, turn.ID) {
			log.Printf("ignoring stale resumed Telegram turn thread_id=%s turn_id=%s status=%s", threadID, turn.ID, turn.Status)
			continue
		}
		active = append(active, turn)
	}
	return active
}

func (a *app) knownTelegramTurnBestEffort(threadID, turnID string) bool {
	if a == nil || a.store == nil || strings.TrimSpace(threadID) == "" || strings.TrimSpace(turnID) == "" {
		return false
	}
	ok, err := a.store.IsTelegramTurn(threadID, turnID)
	if err != nil {
		log.Printf("read Telegram turn marker thread_id=%s turn_id=%s: %v", threadID, turnID, err)
		return false
	}
	return ok
}

func (a *app) shouldQueuePrompt(ctx context.Context, key string, session *activeTurn) bool {
	if session == nil || session.client == nil || session.threadID == "" {
		return a.sessionTurnCount(key) > 0
	}
	current := a.currentTurnID(key)
	if current != "" {
		return true
	}
	active, err := latestActiveThreadTurn(ctx, session.client, session.threadID)
	if err != nil {
		log.Printf("read active Codex turn before prompt submit thread_id=%s: %v", session.threadID, err)
		return current != "" || a.sessionTurnCount(key) > 0
	}
	if active.ID == "" {
		a.removeActiveSessionTurns(key)
		return false
	}
	if a.knownTelegramTurnBestEffort(session.threadID, active.ID) {
		log.Printf("ignoring stale active Telegram turn before prompt submit thread_id=%s turn_id=%s status=%s", session.threadID, active.ID, active.Status)
		a.removeActiveSessionTurns(key)
		return false
	}
	if active.ID != current {
		a.replaceActiveSessionTurn(key, active)
	}
	return true
}

func seedSessionAutonomousTurns(session *activeTurn, turns []codex.Turn) {
	if session == nil || len(turns) == 0 {
		return
	}
	for _, turn := range turns {
		if turn.ID == "" {
			continue
		}
		if session.turns[turn.ID] != nil {
			continue
		}
		session.turns[turn.ID] = &telegramTurn{
			TurnID:          turn.ID,
			ChatID:          session.conv.ChatID,
			MessageThreadID: session.conv.MessageThreadID,
			Autonomous:      true,
			Buffers:         map[string]string{},
			SentFiles:       map[string]bool{},
		}
		session.order = append(session.order, turn.ID)
	}
}

func (a *app) startCodexThread(ctx context.Context, c *codex.Client, conv state.Conversation) (string, string, error) {
	params := map[string]any{
		"approvalPolicy": a.cfg.Codex.ApprovalPolicy,
		"sandbox":        a.cfg.Codex.Sandbox,
	}
	cwd := conv.CWD
	if cwd == "" {
		cwd = a.cfg.Codex.CWD
	}
	if cwd == "" {
		cwd = "."
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", "", err
	}
	params["cwd"] = abs
	var out codex.ThreadStartResponse
	if err := c.Call(ctx, "thread/start", params, &out); err != nil {
		return "", "", err
	}
	return out.Thread.ID, out.Cwd, nil
}

func (a *app) resumeCodexThread(ctx context.Context, c *codex.Client, threadID string) error {
	_, err := a.resumeCodexThreadResult(ctx, c, threadID)
	return err
}

func (a *app) resumeCodexThreadResult(ctx context.Context, c *codex.Client, threadID string) (codex.ThreadResumeResponse, error) {
	var out codex.ThreadResumeResponse
	err := c.Call(ctx, "thread/resume", map[string]any{
		"threadId":       threadID,
		"approvalPolicy": a.cfg.Codex.ApprovalPolicy,
		"sandbox":        a.cfg.Codex.Sandbox,
	}, &out)
	return out, err
}

func (a *app) forkSideTopicThread(ctx context.Context, chatID int64, messageThreadID int, conv state.Conversation) (string, string, *codex.Client, error) {
	if conv.CodexThreadID == "" {
		return "", "", nil, fmt.Errorf("this Telegram topic has not started a Codex thread yet")
	}
	c, err := a.startForkAppServer(ctx, chatID, messageThreadID, conv)
	if err != nil {
		return "", "", nil, err
	}
	threadID, cwd, err := a.forkCodexThread(ctx, c, conv)
	if err != nil {
		return "", "", nil, err
	}
	return threadID, cwd, c, nil
}

func (a *app) startForkAppServer(ctx context.Context, chatID int64, messageThreadID int, conv state.Conversation) (*codex.Client, error) {
	c, err := a.startCodexAppServer(ctx, codex.StartOptions{
		CLIPath:    a.cfg.Codex.CLIPath,
		WorkingDir: appServerWorkingDir(conv),
	}, "side fork")
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (a *app) forkCodexThread(ctx context.Context, c *codex.Client, conv state.Conversation) (string, string, error) {
	params := map[string]any{
		"threadId":               conv.CodexThreadID,
		"approvalPolicy":         a.cfg.Codex.ApprovalPolicy,
		"sandbox":                a.cfg.Codex.Sandbox,
		"ephemeral":              true,
		"persistExtendedHistory": false,
	}
	cwd := conv.CWD
	if cwd == "" {
		cwd = a.cfg.Codex.CWD
	}
	if cwd == "" {
		cwd = "."
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", "", err
	}
	params["cwd"] = abs
	var out codex.ThreadForkResponse
	if err := c.Call(ctx, "thread/fork", params, &out); err != nil {
		return "", "", err
	}
	threadID := out.Thread.ID
	if threadID == "" {
		return "", "", fmt.Errorf("Codex returned an empty forked thread id")
	}
	if out.Cwd != "" {
		return threadID, out.Cwd, nil
	}
	if out.Thread.Cwd != "" {
		return threadID, out.Thread.Cwd, nil
	}
	return threadID, abs, nil
}

func (a *app) syncTopicTitle(ctx context.Context, conv *state.Conversation, c *codex.Client) error {
	if conv.TopicNamed {
		return nil
	}
	var read codex.ThreadReadResponse
	if err := c.Call(ctx, "thread/read", map[string]any{"threadId": conv.CodexThreadID}, &read); err != nil {
		return err
	}
	title := threadTitle(read.Thread)
	if title == "" {
		return nil
	}
	topicName := topicTitle(conv.ProjectName, title)
	if conv.TopicTitle == topicName {
		conv.TopicNamed = true
		return a.store.Upsert(*conv)
	}
	if _, err := a.bot.EditForumTopic(ctx, &bot.EditForumTopicParams{
		ChatID:          conv.ChatID,
		MessageThreadID: conv.MessageThreadID,
		Name:            topicName,
	}); err != nil {
		if !isTelegramNoopTopicEdit(err) {
			return err
		}
	}
	conv.TopicTitle = topicName
	conv.TopicNamed = true
	return a.store.Upsert(*conv)
}

func (a *app) syncTopicTitleBestEffort(ctx context.Context, session *activeTurn) {
	if session.conv.TopicNamed {
		return
	}
	if err := a.syncTopicTitle(ctx, &session.conv, session.client); err != nil {
		log.Printf("rename telegram topic: %v", err)
	}
}

func (a *app) syncTopicTitleForDelta(ctx context.Context, session *activeTurn, itemID, delta string) {
	if strings.TrimSpace(delta) == "" || session.conv.TopicNamed || session.titleSyncItems[itemID] {
		return
	}
	session.titleSyncItems[itemID] = true
	a.syncTopicTitleBestEffort(ctx, session)
}

func isTelegramNoopTopicEdit(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "not modified") || strings.Contains(text, "topic_not_modified")
}

func projectlessRoot() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, "Documents", "Codex")
}

func prepareProjectlessWorkspace(conv state.Conversation, titleHint string) (state.Conversation, error) {
	if !conv.Projectless || conv.CodexThreadID != "" || conv.CWD != "" {
		return conv, nil
	}
	root := projectlessRoot()
	if root == "" {
		return state.Conversation{}, fmt.Errorf("could not resolve Codex projectless workspace root")
	}
	dateDir := filepath.Join(root, time.Now().Format("2006-01-02"))
	slug := projectlessSlug(titleHint)
	if slug == "" {
		slug = "chat"
	}
	dir := filepath.Join(dateDir, slug)
	for i := 2; pathExists(dir); i++ {
		dir = filepath.Join(dateDir, fmt.Sprintf("%s-%d", slug, i))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return state.Conversation{}, fmt.Errorf("prepare projectless Codex workspace: %w", err)
	}
	conv.CWD = dir
	return conv, nil
}

func projectlessSlug(text string) string {
	text = strings.ToLower(strings.TrimSpace(text))
	var b strings.Builder
	lastDash := false
	for _, r := range text {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			lastDash = false
		case unicode.IsSpace(r) || r == '-' || r == '_' || r == '.':
			if b.Len() > 0 && !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
		if b.Len() >= 56 {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func appServerWorkingDir(conv state.Conversation) string {
	if conv.Projectless {
		if conv.CWD != "" {
			return conv.CWD
		}
		return projectlessRoot()
	}
	if conv.CWD != "" {
		return conv.CWD
	}
	return projectlessRoot()
}

type codexTurnOptions struct {
	ApprovalPolicy    string
	Sandbox           string
	CollaborationMode string
	Model             string
	ReasoningEffort   string
}

func startTurn(ctx context.Context, c *codex.Client, threadID string, input []map[string]any, opts codexTurnOptions) (string, error) {
	var out codex.TurnStartResponse
	params := map[string]any{
		"threadId":       threadID,
		"input":          input,
		"approvalPolicy": opts.ApprovalPolicy,
		"sandbox":        opts.Sandbox,
	}
	if mode := normalizeCollaborationMode(opts.CollaborationMode); mode != "" {
		model := strings.TrimSpace(opts.Model)
		if model == "" {
			return "", fmt.Errorf("codex model is required for %s mode; set one with /model", mode)
		}
		settings := map[string]any{
			"model":                  model,
			"reasoning_effort":       nil,
			"developer_instructions": nil,
		}
		if effort := normalizeReasoningEffort(opts.ReasoningEffort); effort != "" {
			settings["reasoning_effort"] = effort
		}
		params["collaborationMode"] = map[string]any{
			"mode":     mode,
			"settings": settings,
		}
	}
	if err := c.Call(ctx, "turn/start", params, &out); err != nil {
		return "", err
	}
	return out.Turn.ID, nil
}

func steerTurn(ctx context.Context, c *codex.Client, threadID, expectedTurnID string, input []map[string]any) error {
	if len(input) == 0 {
		input = []map[string]any{{
			"type":          "text",
			"text":          "",
			"text_elements": []any{},
		}}
	}
	activeTurnID := expectedTurnID
	for attempt := 0; attempt < 3; attempt++ {
		if err := callSteerTurn(ctx, c, threadID, activeTurnID, input); err != nil {
			nextTurnID := staleActiveTurnID(err)
			if nextTurnID != "" && nextTurnID != activeTurnID {
				activeTurnID = nextTurnID
				continue
			}
			return err
		}
		return nil
	}
	return callSteerTurn(ctx, c, threadID, activeTurnID, input)
}

func callSteerTurn(ctx context.Context, c *codex.Client, threadID, expectedTurnID string, input []map[string]any) error {
	var out struct {
		TurnID string `json:"turnId"`
	}
	return c.Call(ctx, "turn/steer", map[string]any{
		"threadId":       threadID,
		"expectedTurnId": expectedTurnID,
		"input":          input,
	}, &out)
}

func staleActiveTurnID(err error) string {
	if err == nil {
		return ""
	}
	match := staleActiveTurnPattern.FindStringSubmatch(err.Error())
	if len(match) < 2 {
		return ""
	}
	for _, group := range match[1:] {
		if id := strings.TrimSpace(group); id != "" {
			return id
		}
	}
	return ""
}

func textInput(prompt string) []map[string]any {
	input := []map[string]any{{
		"type":          "text",
		"text":          prompt,
		"text_elements": []any{},
	}}
	return input
}

func interruptTurn(ctx context.Context, c *codex.Client, threadID, turnID string) error {
	activeTurnID := turnID
	if activeTurnID == "" {
		var err error
		activeTurnID, err = latestActiveThreadTurnID(ctx, c, threadID)
		if err != nil {
			return err
		}
		if activeTurnID == "" {
			return fmt.Errorf("no active Codex turn in this thread")
		}
	}
	for attempt := 0; attempt < 3; attempt++ {
		if err := callInterruptTurn(ctx, c, threadID, activeTurnID); err != nil {
			nextTurnID := staleActiveTurnID(err)
			if nextTurnID != "" && nextTurnID != activeTurnID {
				activeTurnID = nextTurnID
				continue
			}
			return err
		}
		return nil
	}
	return callInterruptTurn(ctx, c, threadID, activeTurnID)
}

func callInterruptTurn(ctx context.Context, c *codex.Client, threadID, turnID string) error {
	var out map[string]any
	return c.Call(ctx, "turn/interrupt", map[string]any{
		"threadId": threadID,
		"turnId":   turnID,
	}, &out)
}

func latestActiveThreadTurnID(ctx context.Context, c *codex.Client, threadID string) (string, error) {
	turn, err := latestActiveThreadTurn(ctx, c, threadID)
	if err != nil {
		return "", err
	}
	return turn.ID, nil
}

func latestActiveThreadTurn(ctx context.Context, c *codex.Client, threadID string) (codex.Turn, error) {
	var read codex.ThreadReadResponse
	if err := c.Call(ctx, "thread/read", map[string]any{"threadId": threadID}, &read); err != nil {
		return codex.Turn{}, err
	}
	for i := len(read.Thread.Turns) - 1; i >= 0; i-- {
		turn := read.Thread.Turns[i]
		if turn.ID != "" && !isTerminalTurnStatus(turn.Status) {
			return turn, nil
		}
	}
	return codex.Turn{}, nil
}

func isTerminalTurnStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "completed", "failed", "cancelled", "canceled", "interrupted":
		return true
	default:
		return false
	}
}

func (a *app) startNextQueuedTurn(ctx context.Context, key string, session *activeTurn) bool {
	for {
		queued := a.nextQueuedSessionTurn(key)
		if queued == nil {
			return false
		}

		opts, err := a.turnOptions(ctx, session.client, queued.CollaborationMode)
		if err != nil {
			log.Printf("codex queued turn options failed: %v", err)
			a.removeSessionTurn(key, queued.TurnID)
			a.forgetTurnAction(key, queued.TurnID)
			if queued.StatusMessageID != 0 {
				_, _ = a.bot.EditMessageText(ctx, &bot.EditMessageTextParams{
					ChatID:    queued.ChatID,
					MessageID: queued.StatusMessageID,
					Text:      "Dexgram could not prepare the queued message for Codex:\n\n" + err.Error(),
				})
			}
			continue
		}
		a.syncTelegramPromptTranscript(queued.ChatID, queued.MessageThreadID, queued.SourceMessageID, session.threadID, queued.Text)
		a.beginSessionStartingTurn(key, session)
		turnID, err := startTurn(ctx, session.client, session.threadID, queued.Input, opts)
		if err != nil {
			a.endSessionStartingTurn(key, session)
			log.Printf("codex queued turn start failed: %v", err)
			a.removeSessionTurn(key, queued.TurnID)
			a.forgetTurnAction(key, queued.TurnID)
			if queued.StatusMessageID != 0 {
				_, _ = a.bot.EditMessageText(ctx, &bot.EditMessageTextParams{
					ChatID:    queued.ChatID,
					MessageID: queued.StatusMessageID,
					Text:      "Dexgram could not submit the queued message to Codex:\n\n" + err.Error(),
				})
			}
			continue
		}
		a.markTelegramTurn(session.threadID, turnID, queued.ChatID, queued.MessageThreadID, queued.SourceMessageID)

		tgTurn := a.promoteSessionTurn(key, queued.TurnID, turnID)
		if tgTurn == nil {
			a.endSessionStartingTurn(key, session)
			_ = interruptTurn(ctx, session.client, session.threadID, turnID)
			continue
		}
		a.forgetTurnAction(key, queued.TurnID)
		if tgTurn.StatusMessageID != 0 {
			_, _ = a.bot.EditMessageText(ctx, &bot.EditMessageTextParams{
				ChatID:      tgTurn.ChatID,
				MessageID:   tgTurn.StatusMessageID,
				Text:        "Dexgram is thinking...",
				ReplyMarkup: turnControlMarkup(a.rememberTurnAction(key, tgTurn.TurnID), false),
			})
		}
		for _, ev := range a.takePendingTurnEvents(key, turnID) {
			if a.handleTopicSessionEvent(ctx, key, session, ev) {
				a.endSessionStartingTurn(key, session)
				return false
			}
		}
		a.endSessionStartingTurn(key, session)
		a.startTypingIndicator(key, tgTurn.ChatID, tgTurn.MessageThreadID)
		return true
	}
}

func (a *app) handleTopicSessionEvent(ctx context.Context, key string, session *activeTurn, ev codex.Event) bool {
	switch ev.Method {
	case "turn/started":
		var started struct {
			ThreadID string     `json:"threadId"`
			Turn     codex.Turn `json:"turn"`
		}
		if json.Unmarshal(ev.Params, &started) == nil {
			tgTurn := a.sessionTurn(key, started.Turn.ID)
			if tgTurn == nil && started.ThreadID == session.threadID && started.Turn.ID != "" && a.sessionStartingTurn(key, session) {
				tgTurn = a.promoteStartingSessionTurn(key, "", started.Turn.ID)
				a.markTelegramTurn(session.threadID, started.Turn.ID, session.conv.ChatID, session.conv.MessageThreadID, 0)
			}
			if tgTurn == nil && started.ThreadID == session.threadID && started.Turn.ID != "" {
				tgTurn = &telegramTurn{
					TurnID:          started.Turn.ID,
					ChatID:          session.conv.ChatID,
					MessageThreadID: session.conv.MessageThreadID,
					Autonomous:      true,
					Buffers:         map[string]string{},
					SentFiles:       map[string]bool{},
				}
				a.addSessionTurn(key, tgTurn)
			}
			if tgTurn != nil && tgTurn.StatusMessageID != 0 {
				if err := waitTelegramQueue(ctx, "edit status message", tgTurn.ChatID, tgTurn.MessageThreadID); err != nil {
					return false
				}
				_, _ = a.bot.EditMessageText(ctx, &bot.EditMessageTextParams{
					ChatID:      tgTurn.ChatID,
					MessageID:   tgTurn.StatusMessageID,
					Text:        "Dexgram is thinking...",
					ReplyMarkup: turnControlMarkup(a.rememberTurnAction(key, tgTurn.TurnID), false),
				})
			}
			if tgTurn != nil {
				for _, pending := range a.takePendingTurnEvents(key, started.Turn.ID) {
					if a.handleTopicSessionEvent(ctx, key, session, pending) {
						return true
					}
				}
			}
		}
	case "item/started":
		var item codex.ItemStartedNotification
		if json.Unmarshal(ev.Params, &item) == nil {
			tgTurn := a.sessionTurn(key, item.TurnID)
			if tgTurn == nil {
				return false
			}
			if isCompactionNoticeItem(item.Item) {
				tgTurn.startCompactionDraft(ctx, a.bot, item.Item.ID)
			} else if isRunLogItem(item.Item) {
				tgTurn.ensureRunLog(ctx, a.bot)
				tgTurn.RunLog.start(item.Item)
			}
		}
	case "item/commandExecution/outputDelta", "item/fileChange/outputDelta":
		var delta codex.CommandOutputDeltaNotification
		if json.Unmarshal(ev.Params, &delta) == nil {
			if tgTurn := a.sessionTurn(key, delta.TurnID); tgTurn != nil && tgTurn.RunLog != nil {
				text := delta.Delta
				if text == "" {
					text = delta.Output
				}
				tgTurn.RunLog.output(delta.ItemID, text)
			}
		}
	case "item/agentMessage/delta", "item/plan/delta":
		var delta codex.AgentMessageDeltaNotification
		if json.Unmarshal(ev.Params, &delta) == nil {
			if tgTurn := a.sessionTurn(key, delta.TurnID); tgTurn != nil {
				if !tgTurn.Autonomous {
					a.syncTopicTitleForDelta(ctx, session, delta.ItemID, delta.Delta)
				}
				tgTurn.Buffers[delta.ItemID] += delta.Delta
				if tgTurn.isCompactionItemID(delta.ItemID) {
					tgTurn.startCompactionDraft(ctx, a.bot, delta.ItemID)
					return false
				}
				if tgTurn.CompactionCancel != nil {
					tgTurn.stopCompactionDraft()
				}
				tgTurn.ensureInitial(ctx, a.bot)
				tgTurn.Initial.draft(tgTurn.Buffers[delta.ItemID])
			}
		}
	case "item/completed":
		var item codex.ItemCompletedNotification
		if json.Unmarshal(ev.Params, &item) != nil {
			return false
		}
		tgTurn := a.sessionTurn(key, item.TurnID)
		if tgTurn == nil {
			return false
		}
		// Title sync is driven by user-started turns. Goal/autonomous turns
		// can run indefinitely and should not keep renaming the topic.
		if !tgTurn.Autonomous && (item.Item.Type == "agentMessage" || item.Item.Type == "plan") && strings.TrimSpace(item.Item.Text) != "" {
			a.syncTopicTitleBestEffort(ctx, session)
		}
		if tgTurn.isCompactionItemID(item.Item.ID) || isCompactionNoticeItem(item.Item) {
			tgTurn.stopCompactionDraft()
			return false
		}
		switch item.Item.Type {
		case "agentMessage":
			tgTurn.LastAgent = item.Item.Text
			if item.Item.Phase != nil && *item.Item.Phase == "final_answer" {
				tgTurn.FinalAnswer = item.Item.Text
			} else if strings.TrimSpace(item.Item.Text) != "" {
				tgTurn.ensureInitial(ctx, a.bot)
				tgTurn.Initial.set(item.Item.Text)
			}
		case "plan":
			if strings.TrimSpace(item.Item.Text) != "" {
				tgTurn.ensureInitial(ctx, a.bot)
				tgTurn.Initial.set(item.Item.Text)
			}
		default:
			if isRunLogItem(item.Item) {
				tgTurn.ensureRunLog(ctx, a.bot)
				tgTurn.RunLog.complete(item.Item)
			}
		}
	case "turn/completed":
		var done codex.TurnCompletedNotification
		if json.Unmarshal(ev.Params, &done) != nil {
			return false
		}
		tgTurn := a.sessionTurn(key, done.Turn.ID)
		if tgTurn == nil {
			return false
		}
		answer := strings.TrimSpace(tgTurn.FinalAnswer)
		if answer == "" {
			answer = strings.TrimSpace(tgTurn.LastAgent)
		}
		answer = stripAssistantAppDirectives(answer)
		tgTurn.stopCompactionDraft()
		if tgTurn.RunLog != nil {
			tgTurn.RunLog.finish()
		}
		if answer == "" && tgTurn.Autonomous {
			session.conv.LastSyncedTurnID = done.Turn.ID
			if err := a.store.Upsert(session.conv); err != nil {
				log.Printf("store sync marker: %v", err)
			}
			a.forgetTurnAction(key, tgTurn.TurnID)
			a.removeSessionTurn(key, tgTurn.TurnID)
			if !a.startNextQueuedTurn(ctx, key, session) && a.sessionTurnCount(key) == 0 && !a.shouldKeepSessionOpenForGoal(ctx, session) {
				return true
			}
			return false
		}
		if answer == "" {
			answer = "Codex completed without a final text answer."
		}
		if tgTurn.Initial != nil && sameTelegramText(tgTurn.Initial.text, answer) {
			tgTurn.Initial.delete()
		}
		replyText := answer
		sentAsNew := false
		finalTextDelivered := false
		if err := sendRichMessageReply(ctx, a.bot, tgTurn.ChatID, tgTurn.MessageThreadID, tgTurn.SourceMessageID, replyText); err != nil {
			log.Printf("send final message: %v", err)
			if tgTurn.StatusMessageID != 0 {
				if editErr := editRichMessage(ctx, a.bot, tgTurn.ChatID, tgTurn.StatusMessageID, replyText); editErr != nil {
					log.Printf("edit fallback final message: %v", editErr)
				} else {
					finalTextDelivered = true
				}
			}
		} else {
			sentAsNew = true
			finalTextDelivered = true
		}
		if finalTextDelivered && a.cfg.Telegram.UploadFinalAnswerFiles {
			a.sendFinalAnswerFiles(ctx, tgTurn, session.conv.CWD, answer)
		}
		if sentAsNew && tgTurn.StatusMessageID != 0 {
			if err := waitTelegramQueue(ctx, "delete status message", tgTurn.ChatID, tgTurn.MessageThreadID); err != nil {
				return false
			}
			if _, err := a.bot.DeleteMessage(ctx, &bot.DeleteMessageParams{
				ChatID:    tgTurn.ChatID,
				MessageID: tgTurn.StatusMessageID,
			}); err != nil {
				log.Printf("delete status message: %v", err)
			}
		}
		if !tgTurn.Autonomous {
			a.syncTopicTitleBestEffort(ctx, session)
		}
		session.conv.LastSyncedTurnID = done.Turn.ID
		if err := a.store.Upsert(session.conv); err != nil {
			log.Printf("store sync marker: %v", err)
		}
		a.forgetTurnAction(key, tgTurn.TurnID)
		a.removeSessionTurn(key, tgTurn.TurnID)
		if !a.startNextQueuedTurn(ctx, key, session) && a.sessionTurnCount(key) == 0 && !a.shouldKeepSessionOpenForGoal(ctx, session) {
			return true
		}
	case "error":
		log.Printf("codex app-server event error: %s", string(ev.Params))
	}
	return false
}

func (a *app) shouldKeepSessionOpenForGoal(ctx context.Context, session *activeTurn) bool {
	if session == nil || session.client == nil || session.threadID == "" {
		return false
	}
	if session.conv.SideChat {
		return false
	}
	goal, err := getThreadGoal(ctx, session.client, session.threadID)
	if err != nil {
		log.Printf("read Codex goal after turn completion: %v", err)
		return false
	}
	return goal != nil &&
		strings.TrimSpace(goal.Objective) != "" &&
		strings.EqualFold(strings.TrimSpace(goal.Status), "active")
}
