package main

import (
	"context"
	"log"
	"strings"
	"time"

	"dexgram/internal/codex"
)

const codexAppServerStartAttempts = 4

func (a *app) startCodexAppServer(ctx context.Context, opts codex.StartOptions, logContext string) (*codex.Client, error) {
	if a == nil {
		return codex.StartStdioWithOptions(ctx, opts)
	}
	var lastErr error
	for attempt := 1; attempt <= codexAppServerStartAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		a.codexStartMu.Lock()
		c, err := codex.StartStdioWithOptions(ctx, opts)
		a.codexStartMu.Unlock()
		if err == nil {
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

func isTransientCodexAppServerStartError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "database is locked") ||
		strings.Contains(text, "failed to initialize sqlite state db")
}
