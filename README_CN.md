# WeClaw

[English](README.md)

微信 AI Agent 桥接器 — 将微信消息接入 AI Agent（Claude、Codex、Gemini、Kimi 等）。

> 本项目参考 [@tencent-weixin/openclaw-weixin](https://npmx.dev/package/@tencent-weixin/openclaw-weixin) 实现，仅限个人学习，勿做他用。

|                                                 |                                                 |                                                 |
| :---------------------------------------------: | :---------------------------------------------: | :---------------------------------------------: |
| <img src="previews/preview1.png" width="280" /> | <img src="previews/preview2.png" width="280" /> | <img src="previews/preview3.png" width="280" /> |

## 快速开始

```bash
# 一键安装
curl -sSL https://raw.githubusercontent.com/fastclaw-ai/weclaw/main/install.sh | sh

# 启动（首次运行会弹出微信扫码登录）
weclaw start
```

就这么简单。首次启动时，WeClaw 会：

1. 显示二维码 — 用微信扫码登录
2. 自动检测已安装的 AI Agent（Claude、Codex、Gemini 等）
3. 保存配置到 `~/.weclaw/config.json`
4. 开始接收和回复微信消息

使用 `weclaw login` 可以添加更多微信账号。

### 其他安装方式

```bash
# 通过 Go 安装
go install github.com/fastclaw-ai/weclaw@latest

# 通过 Docker
docker run -it -v ~/.weclaw:/root/.weclaw ghcr.io/fastclaw-ai/weclaw start
```

## 架构

<p align="center">
  <img src="previews/architecture.png" width="600" />
</p>

### Agent 接入模式

| 模式 | 工作方式 | 支持的 Agent |
|------|---------|-------------|
| ACP  | 长驻子进程，通过 stdio JSON-RPC 通信。速度最快，复用进程和会话。 | Claude, Codex, Kimi, Gemini, Cursor, OpenCode, OpenClaw |
| CLI  | 每条消息启动一个新进程，支持通过 `--resume` 恢复会话。 | Claude (`claude -p`)、Codex (`codex exec`) |
| HTTP | OpenAI 兼容的 Chat Completions API。 | OpenClaw（HTTP 回退） |

同时存在 ACP 和 CLI 时，自动优先选择 ACP。

## 聊天命令

在微信中发送以下命令：

| 命令 | 说明 |
|------|------|
| `你好` | 发送给默认 Agent |
| `/codex 写一个排序函数` | 发送给指定 Agent |
| `/cc 解释一下这段代码` | 通过别名发送 |
| `@cc @cx 哪个更好` | 同时发给多个 Agent，对比回复 |
| `/claude` 或 `/cc` | 切换默认 Agent |
| `/new` 或 `/clear` | 开始新对话（清除会话） |
| `/cwd /path/to/project` | 切换工作目录 |
| `/info` | 查看当前 Agent 信息（名称、类型、模型） |
| `/help` | 查看帮助信息 |
| `/log` | 查看最近日志（默认 20 行） |
| `/log 50` | 查看最近 50 行日志（最大 200） |
| `/ls` | 列出当前工作目录文件（深度 1） |
| `/ls /path 3` | 列出指定路径，递归深度 3 |

### 快捷别名

| 别名 | Agent | 别名 | Agent |
|------|-------|------|-------|
| `/cc` | Claude | `/cx` | Codex |
| `/cs` | Cursor | `/km` | Kimi |
| `/gm` | Gemini | `/ocd` | OpenCode |
| `/oc` | OpenClaw | | |

也可以在配置文件中为每个 Agent 自定义触发命令：

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

然后 `/ai 你好` 或 `/c 你好` 就会路由到 claude。

切换默认 Agent 会写入配置文件，重启后仍然生效。

### 多 Agent 并行广播

同时向多个 Agent 提问，对比回复：

```
@cc @cx @gm 请解释 Go 的 goroutine 调度
```

每个 Agent 的回复会带上名称前缀（如 `[claude] ...`），依次发送到微信。

### 链接收藏

直接发送 URL 即可保存，无需经过 AI Agent：

```
https://mp.weixin.qq.com/s/xxx
```

支持直接抓取的站点：微信公众号、知乎、GitHub Issue/PR、小红书、YouTube。其余站点通过 Jina Reader 获取。保存为 markdown 文件到配置的目录。

## 富媒体消息

WeClaw 支持收发图片、视频、文件和语音消息。

### 语音消息

在微信中发送语音消息时，WeClaw 自动使用微信的语音转文字功能，将转写后的文本发送给 AI Agent。重复的语音消息事件会自动去重。

### 图片和文件

- **接收图片**：微信发来的图片自动保存到 `save_dir` 目录（默认 `~/.weclaw/workspace`），同时保存 sidecar 元数据文件（包含 message_id、发送者、时间戳等）。
- **接收文件**：当前仅记录日志，不自动处理。

### Agent 回复自动处理

- **图片提取**：Agent 回复中的 markdown 图片（`![](url)`）会自动提取 URL，下载后上传到微信 CDN（AES-128-ECB 加密），作为图片消息发送。
- **附件发送**：Agent 通过文件操作工具创建的文件，如果路径在允许目录下（工作目录或 `save_dir`），会作为文件消息自动发送。安全检查会拒绝路径外的文件。
- **Markdown 转换**：Agent 回复自动转为纯文本 — 代码块去掉围栏、链接只保留文字、加粗斜体标记去除等。

## 主动推送消息

无需等待用户发消息，主动向微信用户推送消息。

### 命令行

```bash
# 发送文本
weclaw send --to "user_id@im.wechat" --text "你好，来自 weclaw"

# 发送图片
weclaw send --to "user_id@im.wechat" --media "https://example.com/photo.png"

# 发送文本 + 图片
weclaw send --to "user_id@im.wechat" --text "看看这个" --media "https://example.com/photo.png"

# 发送文件
weclaw send --to "user_id@im.wechat" --media "https://example.com/report.pdf"
```

### HTTP API

`weclaw start` 运行时，默认监听 `127.0.0.1:18011`：

```bash
# 发送文本
curl -X POST http://127.0.0.1:18011/api/send \
  -H "Content-Type: application/json" \
  -d '{"to": "user_id@im.wechat", "text": "你好，来自 weclaw"}'

# 发送图片
curl -X POST http://127.0.0.1:18011/api/send \
  -H "Content-Type: application/json" \
  -d '{"to": "user_id@im.wechat", "media_url": "https://example.com/photo.png"}'

# 发送文本 + 媒体
curl -X POST http://127.0.0.1:18011/api/send \
  -H "Content-Type: application/json" \
  -d '{"to": "user_id@im.wechat", "text": "看看这个", "media_url": "https://example.com/photo.png"}'
```

支持的媒体类型：图片（png、jpg、gif、webp）、视频（mp4、mov）、文件（pdf、doc、zip 等）。

## 配置

配置文件路径：`~/.weclaw/config.json`

### 完整配置示例

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
  "system_prompt": "你是微信助手，回复简洁明了。",
  "max_history": 50
}
```

### Agent 配置字段

| 字段 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `type` | string | 是 | `acp`、`cli` 或 `http` |
| `command` | string | ACP/CLI | 可执行文件路径 |
| `args` | []string | 否 | 启动参数 |
| `env` | map | 否 | 环境变量（API Key 等） |
| `model` | string | 否 | 模型名称 |
| `endpoint` | string | HTTP | API 地址 |
| `api_key` | string | HTTP | API 密钥 |
| `headers` | map | 否 | 自定义请求头（HTTP 模式） |
| `aliases` | []string | 否 | 自定义触发命令 |
| `cwd` | string | 否 | 工作目录，默认 `~/.weclaw/workspace` |

### 全局配置字段

| 字段 | 说明 |
|------|------|
| `default_agent` | 默认 Agent 名称 |
| `save_dir` | 图片和文件保存目录，默认 `~/.weclaw/workspace` |
| `system_prompt` | 全局系统提示词，追加到 Agent 的默认提示后 |
| `max_history` | 最大历史消息轮数（默认 50，超出自动截断） |

### 环境变量

| 变量 | 说明 |
|------|------|
| `WECLAW_DEFAULT_AGENT` | 覆盖默认 Agent |
| `WECLAW_SAVE_DIR` | 覆盖文件保存目录 |
| `WECLAW_API_ADDR` | 覆盖 HTTP API 监听地址（默认 `127.0.0.1:18011`） |
| `OPENCLAW_GATEWAY_URL` | OpenClaw HTTP 回退地址 |
| `OPENCLAW_GATEWAY_TOKEN` | OpenClaw API Token |

### 权限配置

部分 Agent 默认需要交互式权限确认，在微信场景下无法操作会导致卡住。可通过 `args` 配置跳过：

| Agent | 参数 | 说明 |
|-------|------|------|
| Claude (CLI) | `--dangerously-skip-permissions` | 跳过所有工具权限确认 |
| Codex (CLI) | `--skip-git-repo-check` | 允许在非 git 仓库目录运行 |

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

> **注意：** 这些参数会跳过安全检查，请了解风险后再启用。ACP 模式的 Agent 会自动处理权限，无需配置。

## 后台运行

```bash
# 启动（默认后台运行）
weclaw start

# 查看状态
weclaw status

# 停止
weclaw stop

# 前台运行（调试用）
weclaw start -f
```

日志输出到 `~/.weclaw/weclaw.log`。

### 系统服务（开机自启）

**macOS (launchd)：**

```bash
cp service/com.fastclaw.weclaw.plist ~/Library/LaunchAgents/
launchctl load ~/Library/LaunchAgents/com.fastclaw.weclaw.plist
```

**Linux (systemd)：**

```bash
sudo cp service/weclaw.service /etc/systemd/system/
sudo systemctl enable --now weclaw
```

## Docker

```bash
# 构建
docker build -t weclaw .

# 登录（交互式，扫描二维码）
docker run -it -v ~/.weclaw:/root/.weclaw weclaw login

# 使用 HTTP Agent 启动
docker run -d --name weclaw \
  -v ~/.weclaw:/root/.weclaw \
  -e OPENCLAW_GATEWAY_URL=https://api.example.com \
  -e OPENCLAW_GATEWAY_TOKEN=sk-xxx \
  weclaw

# 查看日志
docker logs -f weclaw
```

> 注意：ACP 和 CLI 模式需要容器内有对应的 Agent 二进制文件。
> 默认镜像只包含 WeClaw 本体。如需使用 ACP/CLI Agent，请挂载二进制文件或构建自定义镜像。
> HTTP 模式开箱即用。

## 多账号

使用 `weclaw login` 可添加多个微信账号：

```bash
# 添加第一个账号（首次启动时自动触发）
weclaw start

# 添加额外账号
weclaw login
```

每个账号的凭证独立存储在 `~/.weclaw/accounts/<id>.json`，互不影响。

## 更新与发版

```bash
# 更新到最新版本（运行中会自动重启）
weclaw update

# 查看当前版本
weclaw version
```

### 发版

```bash
# 打 tag 触发 GitHub Actions 自动构建发版
git tag v0.1.0
git push origin v0.1.0
```

自动构建 `darwin/linux/windows` x `amd64/arm64` 的二进制，创建 GitHub Release 并上传所有产物和校验文件。

## 常见问题

**Q：发送消息后没有回复？**
- 检查默认 Agent 是否已启动：发送 `/info` 查看状态
- 如果显示 `[echo]` 前缀，说明 Agent 尚未初始化完成，等待片刻再试
- 发送 `/log` 查看最近日志排查问题

**Q：语音消息收不到回复？**
- 确认 Agent 已正常运行（发送 `/info`）
- 语音转文字后作为普通文本处理，检查文本是否正确

**Q：图片没有自动发送给对方？**
- Agent 回复中的图片 URL 需要能被 WeClaw 访问到
- 检查日志中是否有 CDN 上传失败的错误

**Q：如何切换 Agent 的工作目录？**
- 发送 `/cwd /path/to/project`，路径支持 `~` 展开
- 所有运行中的 Agent 工作目录会同步更新

**Q：ACP Agent 启动失败？**
- 检查 `command` 路径是否正确
- 查看日志中 `[acp-stderr]` 前缀的输出，通常包含具体错误信息
- 确认 Agent 二进制文件已正确安装且有执行权限

**Q：Docker 中无法使用 Claude？**
- 默认镜像不包含 Agent 二进制，需要挂载或自定义镜像
- HTTP 模式无需额外二进制，可直接使用

## 开发

```bash
# 热重载
make dev

# 编译
go build -o weclaw .

# 运行
./weclaw start

# 运行测试
go test ./... -count=1 -race
```

## 贡献者

<a href="https://github.com/fastclaw-ai/weclaw/graphs/contributors">
  <img src="https://contrib.rocks/image?repo=fastclaw-ai/weclaw" />
</a>

## Star 趋势

[![Star History Chart](https://api.star-history.com/svg?repos=fastclaw-ai/weclaw&type=Timeline)](https://star-history.com/#fastclaw-ai/weclaw&Timeline)

## 许可证

[MIT](LICENSE)
