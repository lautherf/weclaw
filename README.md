# WeClaw

[中文文档](README_CN.md)

WeChat AI Agent Bridge — connect WeChat to AI agents (Claude, Codex, Gemini, Kimi, etc.).

> This project is inspired by [@tencent-weixin/openclaw-weixin](https://npmx.dev/package/@tencent-weixin/openclaw-weixin). For personal learning only, not for commercial use.

| | | |
|:---:|:---:|:---:|
| <img src="previews/preview1.png" width="280" /> | <img src="previews/preview2.png" width="280" /> | <img src="previews/preview3.png" width="280" /> |

## Quick Start

```bash
# One-line install
curl -sSL https://raw.githubusercontent.com/fastclaw-ai/weclaw/main/install.sh | sh

# Start (first run will prompt QR code login)
weclaw start
```

That's it. On first start, WeClaw will:

1. Show a QR code — scan with WeChat to login
2. Auto-detect installed AI agents (Claude, Codex, Gemini, etc.)
3. Save config to `~/.weclaw/config.json`
4. Start receiving and replying to WeChat messages

Use `weclaw login` to add additional WeChat accounts.

### Other install methods

```bash
# Via Go
go install github.com/fastclaw-ai/weclaw@latest

# Via Docker
docker run -it -v ~/.weclaw:/root/.weclaw ghcr.io/fastclaw-ai/weclaw start
```

## Architecture

<p align="center">
  <img src="previews/architecture.png" width="600" />
</p>

### Agent Modes

| Mode | How it works | Examples |
|------|-------------|----------|
| ACP  | Long-running subprocess, JSON-RPC over stdio. Fastest — reuses process and sessions. | Claude, Codex, Kimi, Gemini, Cursor, OpenCode, OpenClaw |
| CLI  | Spawns a new process per message. Supports session resume via `--resume`. | Claude (`claude -p`), Codex (`codex exec`) |
| HTTP | OpenAI-compatible chat completions API. | OpenClaw (HTTP fallback) |

Auto-detection picks ACP over CLI when both are available.

## Chat Commands

Send these as WeChat messages:

| Command | Description |
|---------|-------------|
| `hello` | Send to default agent |
| `/codex write a function` | Send to a specific agent |
| `/cc explain this code` | Send to agent by alias |
| `@cc @cx which is better` | Broadcast to multiple agents, compare replies |
| `/claude` or `/cc` | Switch default agent |
| `/new` or `/clear` | Start a new conversation (clear session) |
| `/cwd /path/to/project` | Switch workspace directory |
| `/info` | Show current agent info (name, type, model) |
| `/help` | Show help message |
| `/log` | Show recent logs (default 20 lines) |
| `/log 50` | Show last 50 lines (max 200) |
| `/ls` | List files in current workspace (depth 1) |
| `/ls /path 3` | List files at path, recursive depth 3 |

### Aliases

| Alias | Agent | Alias | Agent |
|-------|-------|-------|-------|
| `/cc` | Claude | `/cx` | Codex |
| `/cs` | Cursor | `/km` | Kimi |
| `/gm` | Gemini | `/ocd` | OpenCode |
| `/oc` | OpenClaw | | |

You can also define custom aliases per agent in config:

```json
{
  "agents": {
    "claude": {
      "type": "acp",
      "aliases": ["ai", "c"]
    }
  }
}
```

Then `/ai hello` or `/c hello` will route to claude.

Switching default agent is persisted to config — survives restarts.

### Multi-Agent Broadcast

Ask multiple agents the same question and compare replies:

```
@cc @cx @gm explain Go's goroutine scheduling
```

Each agent's reply is prefixed with its name (e.g. `[claude] ...`) and sent to WeChat separately.

### Link Hoard

Send a URL directly to save it without going through an AI agent:

```
https://mp.weixin.qq.com/s/xxx
```

Supported for direct fetching: WeChat articles, Zhihu, GitHub Issues/PRs, Xiaohongshu, YouTube. Others use Jina Reader. Saved as markdown files to the configured directory.

## Media Messages

WeClaw supports sending images, videos, files, and voice messages to/from WeChat.

### Voice Messages

When you send a voice message in WeChat, WeClaw automatically uses WeChat's speech-to-text transcription and forwards the text to the AI agent. Duplicate voice message events are automatically deduplicated.

### Images and Files

- **Receiving images:** Images from WeChat are saved to the `save_dir` directory (default `~/.weclaw/workspace`), along with a sidecar metadata file (message_id, sender, timestamp, etc.).
- **Receiving files:** Currently logged only, not processed automatically.

### Agent Reply Processing

- **Image extraction:** Images in agent markdown replies (`![](url)`) are automatically extracted, downloaded, uploaded to WeChat CDN (AES-128-ECB encrypted), and sent as image messages.
- **Attachment sending:** Files created by agent file operation tools are sent as file messages if their path is within allowed directories (workspace or `save_dir`). Security checks reject paths outside allowed directories.
- **Markdown conversion:** Agent replies are automatically converted to plain text — code fences stripped, links show display text only, bold/italic markers removed, etc.

## Proactive Messaging

Send messages to WeChat users without waiting for them to message first.

### CLI

```bash
# Send text
weclaw send --to "user_id@im.wechat" --text "Hello from weclaw"

# Send image
weclaw send --to "user_id@im.wechat" --media "https://example.com/photo.png"

# Send text + image
weclaw send --to "user_id@im.wechat" --text "Check this out" --media "https://example.com/photo.png"

# Send file
weclaw send --to "user_id@im.wechat" --media "https://example.com/report.pdf"
```

### HTTP API

Runs on `127.0.0.1:18011` when `weclaw start` is running:

```bash
# Send text
curl -X POST http://127.0.0.1:18011/api/send \
  -H "Content-Type: application/json" \
  -d '{"to": "user_id@im.wechat", "text": "Hello from weclaw"}'

# Send image
curl -X POST http://127.0.0.1:18011/api/send \
  -H "Content-Type: application/json" \
  -d '{"to": "user_id@im.wechat", "media_url": "https://example.com/photo.png"}'

# Send text + media
curl -X POST http://127.0.0.1:18011/api/send \
  -H "Content-Type: application/json" \
  -d '{"to": "user_id@im.wechat", "text": "See this", "media_url": "https://example.com/photo.png"}'
```

Supported media types: images (png, jpg, gif, webp), videos (mp4, mov), files (pdf, doc, zip, etc.).

## Configuration

Config file: `~/.weclaw/config.json`

### Full config example

```json
{
  "default_agent": "claude",
  "agents": {
    "claude": {
      "type": "acp",
      "command": "/usr/local/bin/claude-agent-acp",
      "env": {
        "ANTHROPIC_API_KEY": "sk-ant-xxx"
      },
      "model": "sonnet",
      "aliases": ["ai", "c"]
    },
    "codex": {
      "type": "acp",
      "command": "/usr/local/bin/codex-acp",
      "env": {
        "OPENAI_API_KEY": "sk-xxx"
      }
    },
    "openclaw": {
      "type": "http",
      "endpoint": "https://api.example.com/v1/chat/completions",
      "api_key": "sk-xxx",
      "model": "openclaw:main",
      "headers": {
        "X-Custom": "value"
      }
    }
  },
  "save_dir": "/home/user/wechat-files",
  "system_prompt": "You are a WeChat assistant. Reply concisely.",
  "max_history": 50
}
```

### Agent config fields

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `type` | string | Yes | `acp`, `cli`, or `http` |
| `command` | string | ACP/CLI | Path to executable |
| `args` | []string | No | Startup arguments |
| `env` | map | No | Environment variables (API keys, etc.) |
| `model` | string | No | Model name |
| `endpoint` | string | HTTP | API endpoint URL |
| `api_key` | string | HTTP | API key |
| `headers` | map | No | Custom request headers (HTTP mode) |
| `aliases` | []string | No | Custom trigger commands |
| `cwd` | string | No | Working directory, defaults to `~/.weclaw/workspace` |

### Global config fields

| Field | Description |
|-------|-------------|
| `default_agent` | Default agent name |
| `save_dir` | Directory for saving images and files, defaults to `~/.weclaw/workspace` |
| `system_prompt` | Global system prompt, appended after agent's default prompt |
| `max_history` | Max conversation history rounds (default 50, older entries auto-truncated) |

### Environment variables

| Variable | Description |
|----------|-------------|
| `WECLAW_DEFAULT_AGENT` | Override default agent |
| `WECLAW_SAVE_DIR` | Override file save directory |
| `WECLAW_API_ADDR` | Override HTTP API listen address (default `127.0.0.1:18011`) |
| `OPENCLAW_GATEWAY_URL` | OpenClaw HTTP fallback endpoint |
| `OPENCLAW_GATEWAY_TOKEN` | OpenClaw API token |

### Permission bypass

By default, some agents require interactive permission approval which doesn't work in WeChat. Add `args` to your agent config to bypass:

| Agent | Flag | What it does |
|-------|------|-------------|
| Claude (CLI) | `--dangerously-skip-permissions` | Skip all tool permission prompts |
| Codex (CLI) | `--skip-git-repo-check` | Allow running outside git repos |

```json
{
  "claude": {
    "type": "cli",
    "command": "/usr/local/bin/claude",
    "cwd": "/home/user/my-project",
    "args": ["--dangerously-skip-permissions"]
  }
}
```

> **Warning:** These flags disable safety checks. Only enable them if you understand the risks. ACP agents handle permissions automatically and don't need these flags.

## Background Mode

```bash
# Start (runs in background by default)
weclaw start

# Check if running
weclaw status

# Stop
weclaw stop

# Run in foreground (for debugging)
weclaw start -f
```

Logs are written to `~/.weclaw/weclaw.log`.

### System service (auto-start on boot)

**macOS (launchd):**

```bash
cp service/com.fastclaw.weclaw.plist ~/Library/LaunchAgents/
launchctl load ~/Library/LaunchAgents/com.fastclaw.weclaw.plist
```

**Linux (systemd):**

```bash
sudo cp service/weclaw.service /etc/systemd/system/
sudo systemctl enable --now weclaw
```

## Docker

```bash
# Build
docker build -t weclaw .

# Login (interactive — scan QR code)
docker run -it -v ~/.weclaw:/root/.weclaw weclaw login

# Start with HTTP agent
docker run -d --name weclaw \
  -v ~/.weclaw:/root/.weclaw \
  -e OPENCLAW_GATEWAY_URL=https://api.example.com \
  -e OPENCLAW_GATEWAY_TOKEN=sk-xxx \
  weclaw

# View logs
docker logs -f weclaw
```

> Note: ACP and CLI agents require the agent binary inside the container.
> The Docker image ships only WeClaw itself. For ACP/CLI agents, mount
> the binary or build a custom image. HTTP agents work out of the box.

## Multi-Account

Add multiple WeChat accounts with `weclaw login`:

```bash
# First account (auto-triggered on first start)
weclaw start

# Add additional accounts
weclaw login
```

Each account's credentials are stored independently in `~/.weclaw/accounts/<id>.json`.

## Update & Release

```bash
# Update to the latest version (auto-restarts if running)
weclaw update

# Check current version
weclaw version
```

### Release

```bash
# Tag a new version to trigger GitHub Actions build & release
git tag v0.1.0
git push origin v0.1.0
```

The workflow builds binaries for `darwin/linux/windows` x `amd64/arm64`, creates a GitHub Release, and uploads all artifacts with checksums.

## Troubleshooting

**Q: No reply after sending a message?**
- Check if the default agent is running: send `/info`
- If you see `[echo]` prefix, the agent hasn't initialized yet — wait a moment
- Send `/log` to check recent logs

**Q: Voice messages not getting replies?**
- Confirm the agent is running (`/info`)
- Voice is converted to text then forwarded as a normal message — check if transcription is correct

**Q: Images not sent from agent replies?**
- Image URLs in agent replies must be accessible by WeClaw
- Check logs for CDN upload failures

**Q: How to change agent workspace?**
- Send `/cwd /path/to/project` — `~` expansion is supported
- All running agents' working directories update simultaneously

**Q: ACP agent fails to start?**
- Check the `command` path is correct
- Look for `[acp-stderr]` prefixed output in logs for specific errors
- Ensure the agent binary is installed and executable

**Q: Claude not working in Docker?**
- Default image doesn't include agent binaries — mount them or build a custom image
- HTTP mode works out of the box

## Development

```bash
# Hot reload
make dev

# Build
go build -o weclaw .

# Run
./weclaw start

# Run tests
go test ./... -count=1 -race
```

## Contributors

<a href="https://github.com/fastclaw-ai/weclaw/graphs/contributors">
  <img src="https://contrib.rocks/image?repo=fastclaw-ai/weclaw" />
</a>

## Star History

[![Star History Chart](https://api.star-history.com/svg?repos=fastclaw-ai/weclaw&type=Timeline)](https://star-history.com/#fastclaw-ai/weclaw&Timeline)

## License

[MIT](LICENSE)
