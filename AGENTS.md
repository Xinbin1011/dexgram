# AGENTS.md

Guidance for agents working on Dexgram.

## Project Overview

Dexgram is a Windows-only Telegram bridge for Codex Desktop. It runs as a Go
binary, listens through a Telegram bot with threaded topics enabled, and maps
Telegram topics to Codex app-server threads.

Dexgram owns one shared Codex `app-server --listen stdio://` process per
Dexgram runtime. Topic sessions are routing records over that shared client, not
one app-server per Telegram topic. Events and approval/user-input requests must
be routed by Codex `threadId`/`turnId` to the mapped Telegram topic. The shared
app-server is tracked with a small PID marker and guarded by a Windows Job
Object so stale children are cleaned up after crashes or restarts.

Side chats are Telegram topics created with `/side` or `/btw`. They are backed
by native Codex `thread/fork`, keep the same project/cwd as the parent, and use
a `↳N` topic-name prefix so they do not get confused with the main topic.

Existing Codex sessions can be attached through /sessions [query]. The menu
uses registered Codex Desktop sections and projects, with a separate view for
unassigned chats. Pins and per-topic model choices live in Dexgram SQLite.
/history pages through saved turns using read-only app-server calls. /sendfile
sends a file explicitly chosen from the current private topic’s workspace.

Fresh unbound topics must not silently become projectless chats on the first
prompt. Dexgram should first ask whether to resume an existing session, start a
new chat, or set a project first.

The main command lives in `cmd/dexgram`. Shared packages live under `internal/`:

- `internal/codex`: Codex app-server JSON-RPC client.
- `internal/codexprojects`: Codex Desktop project discovery.
- `internal/codexstate`: projectless workspace helpers.
- `internal/config`: TOML config loading/defaults.
- `internal/state`: SQLite-backed mapping, sync, settings, and pairing-code
  state.

Keep changes small and aligned with the current layout. Prefer local helpers and
existing package boundaries over adding new framework-like abstractions.

## Development Commands

Run the shared local/CI check before handing work back:

```powershell
.\scripts\check.ps1
```

This is the same entry point GitHub Actions uses after setting up Go from
`go.mod`. It verifies `gofmt`, runs `go test ./...`, runs
`go test -cover ./...`, runs `golangci-lint run ./...`, and rejects tracked Go
files over 1000 lines.

The repo linter config is `.golangci.yml`; `errcheck` and `staticcheck` are
explicitly enabled there. The check script pins `golangci-lint` to `v1.64.8`
and installs it with the active Go toolchain if needed, so the linter binary is
compatible with the repo's declared Go version.

If the check script cannot install or run `golangci-lint`, note that clearly in
your final response. Do not silently skip it.

GitHub CI lives at:

```text
.github/workflows/ci.yml
```

To build the Windows binary:

```powershell
go build -o dexgram.exe .\cmd\dexgram
```

## Windows Resource and Binary Version

Dexgram embeds Windows file metadata through `goversioninfo`.

The entrypoint contains:

```go
//go:generate go run genversion.go
//go:generate goversioninfo -64 -o resource.syso
```

The metadata source is:

```text
cmd/dexgram/versioninfo.json
```

The generated files are:

```text
cmd/dexgram/version.go
cmd/dexgram/resource.syso
```

To change the binary version or author metadata:

1. Edit `cmd/dexgram/versioninfo.json`.
2. Update both numeric versions under `FixedFileInfo`:
   `FileVersion` and `ProductVersion`.
3. Update matching strings under `StringFileInfo`:
   `FileVersion` and `ProductVersion`.
4. Update author/copyright fields there as needed.
5. Regenerate generated version files:

```powershell
go generate ./cmd/dexgram
```

If `goversioninfo` is missing, install it with:

```powershell
go install github.com/josephspurrier/goversioninfo/cmd/goversioninfo@latest
```

On Windows, ensure `%USERPROFILE%\go\bin` is on `PATH` before running
`go generate`.

## Runtime Notes

Dexgram is intentionally Windows-specific:

- Service mode uses a per-user Startup folder login item and is not a Windows
  Service. `dexgram service start|stop|restart` manage that login item and the
  running Dexgram process.
- `dexgram clean` removes only clearly stale Dexgram-owned
  `codex.exe app-server --listen stdio://` processes from Dexgram's PID marker.
  It must not clean app-server processes launched by Codex Desktop or Codex
  tool runtimes. The update workflow runs the same cleanup after restarting
  Dexgram.
- Default paths use `%APPDATA%` and `%LOCALAPPDATA%`.
- The Codex CLI is auto-discovered at
  `%LOCALAPPDATA%\OpenAI\Codex\bin\codex.exe`.
- Telegram authorization is based on `[telegram].chat_ids`, an array of
  explicit `int64` chat IDs. Negative Telegram group IDs are valid. `0` is not
  an authorization mechanism.
- Unauthorized Telegram chats must not reach Codex and must not get slash
  commands. They receive only discovery/setup text with a short-lived pairing
  code, shown as `XXX-XXX`.
- Users should not edit TOML by hand for Telegram pairing. Use
  `dexgram telegram chatid add <chat_id_or_pairing_code>`,
  `dexgram telegram chatid del <chat_id>`, and
  `dexgram telegram chatid clear`.
- Pairing codes are stored in SQLite state, expire quickly, are consumed once,
  and should be accepted as `XXX-XXX` or `XXXXXX`, case-insensitively.
- `dexgram telegram token update` prompts for a replacement bot token so the
  token does not land in shell history.
- Running Dexgram hot-reloads config from disk. Reload all config values, not
  only `chat_ids`; if `telegram.bot_token` changes, the Telegram poller should
  reconnect with the new token.
- Manual `/sync [limit]` mirrors unsynced completed turns, defaults to one turn,
  caps at five turns, and truncates oversized historical output.
- Manual sync and Desktop mirroring read saved turns without resuming the
  Codex thread or claiming its writer.
- Desktop-to-Telegram live mirroring is scoped to recently active mapped
  Telegram topics. Dexgram persists `last_telegram_message_at` in SQLite when a
  user sends a message in a mapped topic, then only runs mirror workers for
  topics inside that activity window. Old mappings stay available for explicit
  commands like `/status`, `/sessions`, and `/sync`.
- Telegram-origin prompts are also inserted into Codex session JSONL as passive
  `Telegram: ...` transcript entries when a safe completed-turn boundary exists.
  These records use normal Codex `response_item message/user` plus `event_msg
  user_message` shapes and include a top-level `dexgram` metadata object. Codex
  Desktop renders these only after the thread/session is reloaded, not as a live
  UI update.

Avoid replacing this Windows behavior with cross-platform assumptions unless
the user explicitly asks for that.

## Editing Guidelines

- Preserve existing user configuration files such as `dexgram.toml`.
- Do not commit or depend on generated local binaries like `dexgram.exe`.
- Keep Telegram command behavior documented in `README.md` when user-facing
  commands change.
- Keep `AGENTS.md` current when behavior affects future agent assumptions.
- Use structured parsing for TOML, JSON, and SQLite data instead of ad hoc
  string manipulation.
- Keep logs and errors useful for a user running Dexgram from PowerShell or as
  a Startup login item.

## Quick Smoke Checks

For config-only or documentation-only changes, `gofmt` may not alter anything,
but still run tests when Go behavior could be affected.

For changes touching Windows version metadata, run:

```powershell
go generate ./cmd/dexgram
go build -o "$env:TEMP\dexgram-version-test.exe" .\cmd\dexgram
```

Then inspect the built file with PowerShell:

```powershell
[System.Diagnostics.FileVersionInfo]::GetVersionInfo("$env:TEMP\dexgram-version-test.exe")
```
