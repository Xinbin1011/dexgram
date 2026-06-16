package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"dexgram/internal/codex"
	"dexgram/internal/state"
)

const codexAppServerStartAttempts = 4

func (a *app) startCodexAppServer(ctx context.Context, opts codex.StartOptions, logContext string) (*codex.Client, error) {
	if a == nil {
		return codex.StartStdioWithOptions(ctx, opts)
	}
	a.codexStartMu.Lock()
	defer a.codexStartMu.Unlock()

	if a.codexClient != nil {
		select {
		case <-a.codexClient.Done():
			a.codexClient = nil
		default:
			return a.codexClient, nil
		}
	}

	var lastErr error
	for attempt := 1; attempt <= codexAppServerStartAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c, err := codex.StartStdioWithOptions(ctx, opts)
		if err == nil {
			a.codexClient = c
			a.writeCodexAppServerPID(c)
			c.SetServerRequestHandler(func(reqCtx context.Context, req codex.ServerRequest) (any, error) {
				return a.routeCodexServerRequest(reqCtx, req)
			})
			go a.collectCodexEvents(ctx, c)
			go a.logCodexErrors(c)
			return c, nil
		}
		lastErr = err
		if !isTransientCodexAppServerStartError(err) || attempt == codexAppServerStartAttempts {
			return nil, err
		}
		delay := time.Duration(attempt) * 750 * time.Millisecond
		if logContext != "" {
			log.Printf("codex app-server start retry %s attempt=%d delay=%s: %v", logContext, attempt+1, delay, err)
		} else {
			log.Printf("codex app-server start retry attempt=%d delay=%s: %v", attempt+1, delay, err)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, lastErr
}

func (a *app) closeCodexAppServer() {
	if a == nil {
		return
	}
	a.codexStartMu.Lock()
	c := a.codexClient
	a.codexClient = nil
	a.codexStartMu.Unlock()
	if c != nil {
		_ = c.Close()
	}
	a.removeCodexAppServerPID()
}

func (a *app) logCodexErrors(c *codex.Client) {
	for err := range c.Errors() {
		log.Printf("codex app-server: %v", err)
	}
	a.codexStartMu.Lock()
	if a.codexClient == c {
		a.codexClient = nil
		a.removeCodexAppServerPID()
	}
	a.codexStartMu.Unlock()
}

func (a *app) cleanupStaleCodexAppServer() {
	path := a.codexAppServerPIDPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("read Codex app-server pid marker: %v", err)
		}
		return
	}
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n")
	pid, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil || pid <= 0 {
		_ = os.Remove(path)
		return
	}
	expectedPath := ""
	if len(lines) > 1 {
		expectedPath = strings.TrimSpace(lines[1])
	}
	if !isDexgramCodexAppServerProcess(pid, expectedPath) {
		_ = os.Remove(path)
		return
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		log.Printf("find stale Codex app-server pid=%d: %v", pid, err)
		_ = os.Remove(path)
		return
	}
	if err := process.Kill(); err != nil {
		log.Printf("kill stale Codex app-server pid=%d: %v", pid, err)
		return
	}
	log.Printf("killed stale Codex app-server pid=%d from previous Dexgram run", pid)
	_ = os.Remove(path)
}

func (a *app) writeCodexAppServerPID(c *codex.Client) {
	pid := c.ProcessID()
	if pid <= 0 {
		return
	}
	path := a.codexAppServerPIDPath()
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		log.Printf("create Codex app-server pid marker dir: %v", err)
		return
	}
	content := strconv.Itoa(pid) + "\n" + c.ExecutablePath() + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		log.Printf("write Codex app-server pid marker: %v", err)
	}
}

func (a *app) removeCodexAppServerPID() {
	path := a.codexAppServerPIDPath()
	if path != "" {
		_ = os.Remove(path)
	}
}

func (a *app) codexAppServerPIDPath() string {
	if a == nil || a.store == nil || a.store.Path() == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(a.store.Path()), "codex-app-server.pid")
}

func (a *app) collectCodexEvents(ctx context.Context, c *codex.Client) {
	for ev := range c.Events() {
		if isCodexUsageUpdateEvent(ev) {
			continue
		}
		key, session := a.sessionForCodexEvent(ev)
		if session == nil {
			if a.rememberPendingCodexEvent(ev) {
				continue
			}
			log.Printf("codex app-server event without active topic method=%s target=%s", ev.Method, codexEventTargetLog(ev))
			continue
		}
		select {
		case <-session.ctx.Done():
			continue
		default:
		}
		if a.deferUnknownTurnEvent(key, session, ev) {
			continue
		}
		if a.handleTopicSessionEvent(ctx, key, session, ev) {
			a.closeTopicSession(key, session)
		}
	}
	a.closeSessionsForCodexClient(c)
}

func (a *app) routeCodexServerRequest(ctx context.Context, req codex.ServerRequest) (any, error) {
	target := codexRequestTarget(req)
	if _, session := a.sessionForCodexTarget(target.threadID, target.turnID); session != nil {
		return a.requestApproval(ctx, session.conv.ChatID, session.conv.MessageThreadID, req)
	}
	if target.threadID != "" && a.store != nil {
		if conv, ok := a.conversationForCodexThread(target.threadID); ok {
			return a.requestApproval(ctx, conv.ChatID, conv.MessageThreadID, req)
		}
	}
	return nil, fmt.Errorf("could not route Codex server request %s for thread_id=%q turn_id=%q", req.Method, target.threadID, target.turnID)
}

func (a *app) closeTopicSession(key string, session *activeTurn) {
	if session == nil {
		return
	}
	a.mu.Lock()
	if a.active[key] == session {
		delete(a.active, key)
	}
	a.mu.Unlock()
	session.cancel()
}

func (a *app) closeSessionsForCodexClient(c *codex.Client) {
	if c == nil {
		return
	}
	var sessions []struct {
		key     string
		session *activeTurn
	}
	a.mu.Lock()
	for key, session := range a.active {
		if session != nil && session.client == c {
			delete(a.active, key)
			sessions = append(sessions, struct {
				key     string
				session *activeTurn
			}{key: key, session: session})
		}
	}
	a.mu.Unlock()
	for _, entry := range sessions {
		entry.session.cancel()
	}
}

func (a *app) rememberPendingCodexEvent(ev codex.Event) bool {
	target := codexEventTarget(ev)
	if target.threadID == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pendingCodexEvents == nil {
		a.pendingCodexEvents = map[string][]codex.Event{}
	}
	events := append(a.pendingCodexEvents[target.threadID], ev)
	if len(events) > 64 {
		events = events[len(events)-64:]
	}
	a.pendingCodexEvents[target.threadID] = events
	return true
}

func (a *app) takePendingCodexEvents(threadID string) []codex.Event {
	if threadID == "" {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	events := append([]codex.Event(nil), a.pendingCodexEvents[threadID]...)
	delete(a.pendingCodexEvents, threadID)
	return events
}

func (a *app) handlePendingCodexEvents(ctx context.Context, key string, session *activeTurn) bool {
	for _, ev := range a.takePendingCodexEvents(session.threadID) {
		if a.deferUnknownTurnEvent(key, session, ev) {
			continue
		}
		if a.handleTopicSessionEvent(ctx, key, session, ev) {
			a.closeTopicSession(key, session)
			return true
		}
	}
	return false
}

func (a *app) sessionForCodexEvent(ev codex.Event) (string, *activeTurn) {
	target := codexEventTarget(ev)
	return a.sessionForCodexTarget(target.threadID, target.turnID)
}

func (a *app) sessionForCodexTarget(threadID, turnID string) (string, *activeTurn) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for key, session := range a.active {
		if session == nil {
			continue
		}
		if threadID != "" && session.threadID == threadID {
			return key, session
		}
		if turnID != "" && session.turns[turnID] != nil {
			return key, session
		}
	}
	return "", nil
}

func (a *app) conversationForCodexThread(threadID string) (state.Conversation, bool) {
	convs, err := a.store.ListConversations()
	if err != nil {
		log.Printf("list conversations for Codex request routing: %v", err)
		return state.Conversation{}, false
	}
	for _, conv := range convs {
		if conv.CodexThreadID == threadID {
			return conv, true
		}
	}
	return state.Conversation{}, false
}

type codexTarget struct {
	threadID string
	turnID   string
}

func codexRequestTarget(req codex.ServerRequest) codexTarget {
	return decodeCodexTarget(req.Params)
}

func codexEventTarget(ev codex.Event) codexTarget {
	return decodeCodexTarget(ev.Params)
}

func codexEventTargetLog(ev codex.Event) string {
	target := codexEventTarget(ev)
	return fmt.Sprintf("thread_id=%q turn_id=%q", target.threadID, target.turnID)
}

func decodeCodexTarget(raw json.RawMessage) codexTarget {
	var out struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
		Turn     struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	_ = json.Unmarshal(raw, &out)
	if out.TurnID == "" {
		out.TurnID = out.Turn.ID
	}
	return codexTarget{threadID: out.ThreadID, turnID: out.TurnID}
}

func isTransientCodexAppServerStartError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "database is locked") ||
		strings.Contains(text, "failed to initialize sqlite state db")
}

func isCodexClientClosedError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "codex app-server closed") ||
		strings.Contains(text, "file already closed") ||
		strings.Contains(text, "broken pipe") ||
		strings.Contains(text, "pipe is being closed")
}
