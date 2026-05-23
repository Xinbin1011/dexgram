package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const maxAppServerStderrLineLen = 2000
const maxAppServerParseBufferLen = 1024 * 1024
const maxAppServerParseBufferLines = 1000

var ansiEscapePattern = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)
var codexVersionPattern = regexp.MustCompile(`\b(\d+)\.(\d+)\.(\d+)(?:-([0-9A-Za-z.-]+))?\b`)
var probeCodexVersion = readCodexVersion

type Client struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser
	guard  appServerProcessGuard

	nextID         atomic.Int64
	pending        map[int64]chan rpcMessage
	events         chan rpcMessage
	errs           chan error
	requestHandler ServerRequestHandler
	mu             sync.Mutex
	lastErr        error
	closed         chan struct{}
	readers        sync.WaitGroup
}

type rpcMessage struct {
	ID     *int64          `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type Event struct {
	Method string
	Params json.RawMessage
}

type ServerRequest struct {
	ID     int64
	Method string
	Params json.RawMessage
}

type ServerRequestHandler func(context.Context, ServerRequest) (any, error)

type StartOptions struct {
	CLIPath    string
	WorkingDir string
}

type pendingParse struct {
	text      string
	lineCount int
	firstErr  error
}

func StartStdio(ctx context.Context) (*Client, error) {
	return StartStdioWithPath(ctx, "")
}

func StartStdioWithPath(ctx context.Context, cliPath string) (*Client, error) {
	return StartStdioWithOptions(ctx, StartOptions{CLIPath: cliPath})
}

func StartStdioWithOptions(ctx context.Context, opts StartOptions) (*Client, error) {
	codexPath, err := resolveCodexPath(opts.CLIPath)
	if err != nil {
		return nil, err
	}
	cmd := appServerCommand(ctx, codexPath)
	if opts.WorkingDir != "" {
		cmd.Dir = opts.WorkingDir
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	guard, err := guardAppServerProcess(cmd)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, err
	}

	c := &Client{
		cmd:     cmd,
		stdin:   stdin,
		stdout:  stdout,
		stderr:  stderr,
		guard:   guard,
		pending: make(map[int64]chan rpcMessage),
		events:  make(chan rpcMessage, 256),
		errs:    make(chan error, 8),
		closed:  make(chan struct{}),
	}
	c.readers.Add(2)
	go c.readStdout()
	go c.readStderr()
	go c.wait()

	if err := c.Initialize(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

func resolveCodexPath(cliPath string) (string, error) {
	if cliPath != "" {
		return cliPath, nil
	}
	localAppData := os.Getenv("LOCALAPPDATA")
	if localAppData != "" {
		if candidate, ok := latestLocalCodexInstall(filepath.Join(localAppData, "OpenAI", "Codex", "bin")); ok {
			return candidate, nil
		}
	}
	if p, err := exec.LookPath("codex"); err == nil {
		return p, nil
	}
	return "codex", nil
}

type codexVersion struct {
	major      int
	minor      int
	patch      int
	prerelease string
}

type codexInstallCandidate struct {
	path       string
	version    codexVersion
	hasVersion bool
	modTime    time.Time
}

func latestLocalCodexInstall(binDir string) (string, bool) {
	candidates := localCodexCandidates(binDir)
	if len(candidates) == 0 {
		return "", false
	}
	best := candidates[0]
	for _, candidate := range candidates[1:] {
		if betterCodexCandidate(candidate, best) {
			best = candidate
		}
	}
	return best.path, true
}

func localCodexCandidates(binDir string) []codexInstallCandidate {
	paths := []string{filepath.Join(binDir, "codex.exe")}
	entries, err := os.ReadDir(binDir)
	if err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				paths = append(paths, filepath.Join(binDir, entry.Name(), "codex.exe"))
			}
		}
	}

	var candidates []codexInstallCandidate
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		version, hasVersion := probeCodexVersion(path)
		candidates = append(candidates, codexInstallCandidate{
			path:       path,
			version:    version,
			hasVersion: hasVersion,
			modTime:    info.ModTime(),
		})
	}
	return candidates
}

func betterCodexCandidate(candidate, best codexInstallCandidate) bool {
	if candidate.hasVersion && best.hasVersion {
		if cmp := compareCodexVersion(candidate.version, best.version); cmp != 0 {
			return cmp > 0
		}
	}
	if candidate.hasVersion != best.hasVersion {
		return candidate.hasVersion
	}
	if !candidate.modTime.Equal(best.modTime) {
		return candidate.modTime.After(best.modTime)
	}
	return candidate.path < best.path
}

func readCodexVersion(path string) (codexVersion, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	output, err := codexCommand(ctx, path, "--version").Output()
	if err != nil {
		return codexVersion{}, false
	}
	return parseCodexVersion(string(output))
}

func parseCodexVersion(output string) (codexVersion, bool) {
	matches := codexVersionPattern.FindStringSubmatch(output)
	if matches == nil {
		return codexVersion{}, false
	}
	major, err := strconv.Atoi(matches[1])
	if err != nil {
		return codexVersion{}, false
	}
	minor, err := strconv.Atoi(matches[2])
	if err != nil {
		return codexVersion{}, false
	}
	patch, err := strconv.Atoi(matches[3])
	if err != nil {
		return codexVersion{}, false
	}
	return codexVersion{
		major:      major,
		minor:      minor,
		patch:      patch,
		prerelease: matches[4],
	}, true
}

func compareCodexVersion(a, b codexVersion) int {
	for _, pair := range [][2]int{
		{a.major, b.major},
		{a.minor, b.minor},
		{a.patch, b.patch},
	} {
		if pair[0] > pair[1] {
			return 1
		}
		if pair[0] < pair[1] {
			return -1
		}
	}
	if a.prerelease == "" && b.prerelease != "" {
		return 1
	}
	if a.prerelease != "" && b.prerelease == "" {
		return -1
	}
	return 0
}

func (c *Client) Initialize(ctx context.Context) error {
	params := map[string]any{
		"clientInfo": map[string]any{
			"name":    "dexgram",
			"title":   nil,
			"version": "0.0.1",
		},
		"capabilities": map[string]any{
			"experimentalApi": true,
		},
	}
	var result map[string]any
	if err := c.Call(ctx, "initialize", params, &result); err != nil {
		return err
	}
	return c.Notify("initialized", nil)
}

func (c *Client) Events() <-chan Event {
	out := make(chan Event)
	go func() {
		defer close(out)
		for msg := range c.events {
			out <- Event{Method: msg.Method, Params: msg.Params}
		}
	}()
	return out
}

func (c *Client) Errors() <-chan error {
	return c.errs
}

func (c *Client) Done() <-chan struct{} {
	return c.closed
}

func (c *Client) ProcessID() int {
	if c == nil || c.cmd == nil || c.cmd.Process == nil {
		return 0
	}
	return c.cmd.Process.Pid
}

func (c *Client) ExecutablePath() string {
	if c == nil || c.cmd == nil {
		return ""
	}
	return c.cmd.Path
}

func (c *Client) SetServerRequestHandler(handler ServerRequestHandler) {
	c.mu.Lock()
	c.requestHandler = handler
	c.mu.Unlock()
}

func (c *Client) Call(ctx context.Context, method string, params any, out any) error {
	id := c.nextID.Add(1)
	ch := make(chan rpcMessage, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()

	req := map[string]any{
		"id":     id,
		"method": method,
		"params": params,
		"trace":  nil,
	}
	if err := c.writeJSON(req); err != nil {
		c.forget(id)
		return err
	}

	select {
	case msg := <-ch:
		if msg.Error != nil {
			return fmt.Errorf("app-server returned error for %s (code %d): %s", method, msg.Error.Code, msg.Error.Message)
		}
		if out == nil {
			return nil
		}
		if len(msg.Result) == 0 || string(msg.Result) == "null" {
			return nil
		}
		return json.Unmarshal(msg.Result, out)
	case <-ctx.Done():
		c.forget(id)
		return ctx.Err()
	case <-c.closed:
		return c.closedError()
	}
}

func (c *Client) Notify(method string, params any) error {
	req := map[string]any{"method": method}
	if params != nil {
		req["params"] = params
	}
	return c.writeJSON(req)
}

func (c *Client) Close() error {
	select {
	case <-c.closed:
		return nil
	default:
	}
	_ = c.stdin.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	<-c.closed
	return nil
}

func (c *Client) forget(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *Client) writeJSON(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = c.stdin.Write(b)
	return err
}

func (c *Client) readStdout() {
	defer c.readers.Done()
	scanner := bufio.NewScanner(c.stdout)
	buf := make([]byte, 0, 1024*1024)
	scanner.Buffer(buf, 16*1024*1024)
	var pending *pendingParse
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if pending != nil {
			msg, nextPending, err, ok := parsePendingAppServerLine(pending, line)
			pending = nextPending
			if err != nil {
				c.reportError(err)
			}
			if !ok {
				continue
			}
			c.handleStdoutMessage(msg)
			continue
		}
		msg, nextPending, err, ok := parseAppServerLine(line)
		pending = nextPending
		if err != nil {
			c.reportError(err)
		}
		if !ok {
			continue
		}
		c.handleStdoutMessage(msg)
	}
	if err := scanner.Err(); err != nil {
		if !isExpectedAppServerPipeClose(err) {
			select {
			case <-c.closed:
			default:
				c.reportError(fmt.Errorf("read app-server stdout: %w", err))
			}
		}
	}
	close(c.events)
}

func parseAppServerLine(line string) (rpcMessage, *pendingParse, error, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return rpcMessage{}, nil, nil, false
	}
	var msg rpcMessage
	if err := json.Unmarshal([]byte(trimmed), &msg); err != nil {
		if shouldBufferAppServerParseFailure(trimmed, err) {
			return rpcMessage{}, &pendingParse{text: trimmed, lineCount: 1, firstErr: err}, nil, false
		}
		return rpcMessage{}, nil, fmt.Errorf("decode app-server line: %w", err), false
	}
	return msg, nil, nil, true
}

func parsePendingAppServerLine(pending *pendingParse, line string) (rpcMessage, *pendingParse, error, bool) {
	candidate := pending.text + `\n` + line
	var msg rpcMessage
	if err := json.Unmarshal([]byte(candidate), &msg); err != nil {
		lineCount := pending.lineCount + 1
		if len(candidate) <= maxAppServerParseBufferLen && lineCount <= maxAppServerParseBufferLines {
			return rpcMessage{}, &pendingParse{text: candidate, lineCount: lineCount, firstErr: pending.firstErr}, nil, false
		}
		return rpcMessage{}, nil, fmt.Errorf("decode buffered app-server line after %d fragments: %w", lineCount, err), false
	}
	return msg, nil, nil, true
}

func shouldBufferAppServerParseFailure(line string, err error) bool {
	if !strings.HasPrefix(line, "{") && !strings.HasPrefix(line, "[") {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "unexpected end of JSON input") ||
		strings.Contains(msg, "invalid character") && strings.Contains(msg, "in string literal")
}

func (c *Client) handleStdoutMessage(msg rpcMessage) {
	if msg.ID != nil {
		if msg.Method != "" {
			c.handleServerRequest(*msg.ID, msg.Method, msg.Params)
			return
		}
		c.mu.Lock()
		ch := c.pending[*msg.ID]
		delete(c.pending, *msg.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- msg
		}
		return
	}
	if msg.Method != "" {
		c.events <- msg
	}
}

func isExpectedAppServerPipeClose(err error) bool {
	return errors.Is(err, os.ErrClosed) ||
		errors.Is(err, io.ErrClosedPipe) ||
		strings.Contains(strings.ToLower(err.Error()), "file already closed")
}

func normalizeAppServerStderr(line string) string {
	line = strings.TrimSpace(ansiEscapePattern.ReplaceAllString(line, ""))
	if line == "" {
		return "app-server stderr: <blank>"
	}

	var entry struct {
		Level  string `json:"level"`
		Target string `json:"target"`
		Fields struct {
			Message string `json:"message"`
			Error   string `json:"error"`
		} `json:"fields"`
	}
	if json.Unmarshal([]byte(line), &entry) == nil && (entry.Level != "" || entry.Target != "") {
		msg := entry.Fields.Message
		if msg == "" {
			msg = entry.Fields.Error
		}
		if msg == "" {
			msg = line
		}
		if len(msg) > maxAppServerStderrLineLen {
			msg = msg[:maxAppServerStderrLineLen] + fmt.Sprintf("... [truncated %d bytes]", len(msg)-maxAppServerStderrLineLen)
		}
		level := strings.ToLower(entry.Level)
		if level == "" {
			level = "stderr"
		}
		if entry.Target == "" {
			return fmt.Sprintf("app-server %s: %s", level, msg)
		}
		return fmt.Sprintf("app-server %s %s: %s", level, entry.Target, msg)
	}

	if len(line) > maxAppServerStderrLineLen {
		line = line[:maxAppServerStderrLineLen] + fmt.Sprintf("... [truncated %d bytes]", len(line)-maxAppServerStderrLineLen)
	}
	if strings.Contains(line, "codex_core::tools::router") && strings.Contains(line, "ERROR") {
		if _, after, ok := strings.Cut(line, "error="); ok {
			return "app-server tool error: " + strings.TrimSpace(after)
		}
		return "app-server tool error: " + line
	}
	return "app-server stderr: " + line
}

func (c *Client) reportError(err error) {
	c.mu.Lock()
	c.lastErr = err
	c.mu.Unlock()
	select {
	case <-c.closed:
		select {
		case c.errs <- err:
		default:
		}
	default:
		select {
		case c.errs <- err:
		default:
		}
	}
}

func (c *Client) closedError() error {
	c.mu.Lock()
	lastErr := c.lastErr
	c.mu.Unlock()
	if lastErr == nil {
		return errors.New("codex app-server closed")
	}
	return fmt.Errorf("codex app-server closed: %w", lastErr)
}

func (c *Client) handleServerRequest(id int64, method string, params json.RawMessage) {
	c.mu.Lock()
	handler := c.requestHandler
	c.mu.Unlock()
	if handler == nil {
		c.replyUnsupported(id, method)
		return
	}
	go func() {
		result, err := handler(context.Background(), ServerRequest{
			ID:     id,
			Method: method,
			Params: params,
		})
		if err != nil {
			c.replyError(id, -32000, err.Error())
			return
		}
		c.replyResult(id, result)
	}()
}

func (c *Client) replyUnsupported(id int64, method string) {
	c.replyError(id, -32601, "dexgram does not handle server request: "+method)
}

func (c *Client) replyError(id int64, code int, message string) {
	_ = c.writeJSON(map[string]any{
		"id": id,
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	})
}

func (c *Client) replyResult(id int64, result any) {
	_ = c.writeJSON(map[string]any{
		"id":     id,
		"result": result,
	})
}

func (c *Client) readStderr() {
	defer c.readers.Done()
	scanner := bufio.NewScanner(c.stderr)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 16*1024*1024)
	for scanner.Scan() {
		c.reportError(errors.New(normalizeAppServerStderr(scanner.Text())))
	}
	if err := scanner.Err(); err != nil && !isExpectedAppServerPipeClose(err) {
		select {
		case <-c.closed:
		default:
			c.reportError(fmt.Errorf("read app-server stderr: %w", err))
		}
	}
}

func (c *Client) wait() {
	_ = c.cmd.Wait()
	_ = c.guard.Close()
	c.readers.Wait()
	close(c.closed)
	close(c.errs)
}
