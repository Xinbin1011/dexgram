package main

import (
	"encoding/json"
	"testing"

	"dexgram/internal/codex"
	"dexgram/internal/state"
)

func TestCodexEventTargetExtractsThreadAndTurn(t *testing.T) {
	ev := codex.Event{
		Method: "turn/completed",
		Params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1"}}`),
	}
	target := codexEventTarget(ev)
	if target.threadID != "thread-1" || target.turnID != "turn-1" {
		t.Fatalf("target = %#v", target)
	}

	ev = codex.Event{
		Method: "item/completed",
		Params: json.RawMessage(`{"threadId":"thread-2","turnId":"turn-2"}`),
	}
	target = codexEventTarget(ev)
	if target.threadID != "thread-2" || target.turnID != "turn-2" {
		t.Fatalf("item target = %#v", target)
	}
}

func TestSessionForCodexTargetRoutesByThreadOrTurn(t *testing.T) {
	app := &app{active: map[string]*activeTurn{}}
	first := &activeTurn{
		threadID: "thread-1",
		turns:    map[string]*telegramTurn{"turn-1": {TurnID: "turn-1"}},
		conv:     state.Conversation{ChatID: 1, MessageThreadID: 10},
	}
	second := &activeTurn{
		threadID: "thread-2",
		turns:    map[string]*telegramTurn{"turn-2": {TurnID: "turn-2"}},
		conv:     state.Conversation{ChatID: 2, MessageThreadID: 20},
	}
	if !app.registerSession("1:10", first) || !app.registerSession("2:20", second) {
		t.Fatal("register sessions")
	}

	key, got := app.sessionForCodexTarget("thread-2", "")
	if key != "2:20" || got != second {
		t.Fatalf("thread route = %q %#v", key, got)
	}
	key, got = app.sessionForCodexTarget("", "turn-1")
	if key != "1:10" || got != first {
		t.Fatalf("turn route = %q %#v", key, got)
	}
}

func TestPendingCodexEventsBufferByThread(t *testing.T) {
	app := &app{}
	if app.rememberPendingCodexEvent(codex.Event{Method: "noise", Params: json.RawMessage(`{"turnId":"turn-1"}`)}) {
		t.Fatal("event without thread id should not be buffered")
	}

	ev := codex.Event{Method: "turn/started", Params: json.RawMessage(`{"threadId":"thread-1","turn":{"id":"turn-1"}}`)}
	if !app.rememberPendingCodexEvent(ev) {
		t.Fatal("thread event was not buffered")
	}
	got := app.takePendingCodexEvents("thread-1")
	if len(got) != 1 || got[0].Method != "turn/started" {
		t.Fatalf("pending events = %#v", got)
	}
	if got := app.takePendingCodexEvents("thread-1"); len(got) != 0 {
		t.Fatalf("pending events were not consumed: %#v", got)
	}
}
