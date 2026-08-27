package messaging

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fastclaw-ai/weclaw/agent"
	"github.com/fastclaw-ai/weclaw/ilink"
	"github.com/google/uuid"
)

// AgentFactory creates an agent by config name. Returns nil if the name is unknown.
type AgentFactory func(ctx context.Context, name string) agent.Agent

// SaveDefaultFunc persists the default agent name to config file.
type SaveDefaultFunc func(name string) error

// AgentMeta holds static config info about an agent (for /status display).
type AgentMeta struct {
	Name    string
	Type    string // "acp", "cli", "http"
	Command string // binary path or endpoint
	Model   string
}

// Handler processes incoming WeChat messages and dispatches replies.
type Handler struct {
	mu            sync.RWMutex
	defaultName   string
	agents        map[string]agent.Agent // name -> running agent
	agentMetas    []AgentMeta            // all configured agents (for /status)
	agentWorkDirs map[string]string      // agent name -> configured/runtime cwd
	customAliases map[string]string      // custom alias -> agent name (from config)
	factory       AgentFactory
	saveDefault   SaveDefaultFunc
	contextTokens   sync.Map   // map[userID]contextToken
	saveDir         string     // directory to save images/files to
	seenMsgs        sync.Map   // map[int64]time.Time — dedup by message_id
	logFilePath     string     // path to weclaw.log for /log command
}

// NewHandler creates a new message handler.
func NewHandler(factory AgentFactory, saveDefault SaveDefaultFunc) *Handler {
	return &Handler{
		agents:        make(map[string]agent.Agent),
		agentWorkDirs: make(map[string]string),
		factory:       factory,
		saveDefault:   saveDefault,
	}
}

// SetSaveDir sets the directory for saving images and files.
func (h *Handler) SetSaveDir(dir string) {
	h.saveDir = dir
}

// SetLogFile sets the path to the weclaw log file for the /log command.
func (h *Handler) SetLogFile(path string) {
	h.logFilePath = path
}

// cleanSeenMsgs removes entries older than 5 minutes from the dedup cache.
func (h *Handler) cleanSeenMsgs() {
	cutoff := time.Now().Add(-5 * time.Minute)
	h.seenMsgs.Range(func(key, value any) bool {
		if t, ok := value.(time.Time); ok && t.Before(cutoff) {
			h.seenMsgs.Delete(key)
		}
		return true
	})
}

// SetCustomAliases sets custom alias mappings from config.
func (h *Handler) SetCustomAliases(aliases map[string]string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.customAliases = aliases
}

// SetAgentMetas sets the list of all configured agents (for /status).
func (h *Handler) SetAgentMetas(metas []AgentMeta) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.agentMetas = metas
}

// SetAgentWorkDirs sets the configured working directory for each agent.
func (h *Handler) SetAgentWorkDirs(workDirs map[string]string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.agentWorkDirs = make(map[string]string, len(workDirs))
	for name, dir := range workDirs {
		h.agentWorkDirs[name] = dir
	}
}

// SetDefaultAgent sets the default agent (already started).
func (h *Handler) SetDefaultAgent(name string, ag agent.Agent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.defaultName = name
	h.agents[name] = ag
	log.Printf("[handler] default agent ready: %s (%s)", name, ag.Info())
}

// getAgent returns a running agent by name, or starts it on demand via factory.
func (h *Handler) getAgent(ctx context.Context, name string) (agent.Agent, error) {
	// Fast path: already running
	h.mu.RLock()
	ag, ok := h.agents[name]
	h.mu.RUnlock()
	if ok {
		return ag, nil
	}

	// Slow path: create on demand
	if h.factory == nil {
		return nil, fmt.Errorf("agent %q not found and no factory configured", name)
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	// Double-check after acquiring write lock
	if ag, ok := h.agents[name]; ok {
		return ag, nil
	}

	log.Printf("[handler] starting agent %q on demand...", name)
	ag = h.factory(ctx, name)
	if ag == nil {
		return nil, fmt.Errorf("agent %q not available", name)
	}

	h.agents[name] = ag
	log.Printf("[handler] agent started on demand: %s (%s)", name, ag.Info())
	return ag, nil
}

// getDefaultAgent returns the default agent (may be nil if not ready yet).
func (h *Handler) getDefaultAgent() agent.Agent {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.defaultName == "" {
		return nil
	}
	return h.agents[h.defaultName]
}

// isKnownAgent checks if a name corresponds to a configured agent.
func (h *Handler) isKnownAgent(name string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	// Check running agents
	if _, ok := h.agents[name]; ok {
		return true
	}
	// Check configured agents (metas)
	for _, meta := range h.agentMetas {
		if meta.Name == name {
			return true
		}
	}
	return false
}

// agentAliases maps short aliases to agent config names.
var agentAliases = map[string]string{
	"cc":  "claude",
	"cx":  "codex",
	"oc":  "openclaw",
	"cs":  "cursor",
	"km":  "kimi",
	"gm":  "gemini",
	"ocd": "opencode",
	"pi":  "pi",
	"cp":  "copilot",
	"dr":  "droid",
	"if":  "iflow",
	"kr":  "kiro",
	"qw":  "qwen",
}

// resolveAlias returns the full agent name for an alias, or the original name if no alias matches.
// Checks custom aliases (from config) first, then built-in aliases.
func (h *Handler) resolveAlias(name string) string {
	h.mu.RLock()
	custom := h.customAliases
	h.mu.RUnlock()
	if custom != nil {
		if full, ok := custom[name]; ok {
			return full
		}
	}
	if full, ok := agentAliases[name]; ok {
		return full
	}
	return name
}

// parseCommand checks if text starts with "/" or "@" followed by agent name(s).
// Supports multiple agents: "@cc @cx hello" returns (["claude","codex"], "hello").
// Returns (agentNames, actualMessage). Aliases are resolved automatically.
// If no command prefix, returns (nil, originalText).
func (h *Handler) parseCommand(text string) ([]string, string) {
	if !strings.HasPrefix(text, "/") && !strings.HasPrefix(text, "@") {
		return nil, text
	}

	// Parse consecutive @name or /name tokens from the start
	var names []string
	rest := text
	for {
		rest = strings.TrimSpace(rest)
		if !strings.HasPrefix(rest, "/") && !strings.HasPrefix(rest, "@") {
			break
		}

		// Strip prefix
		after := rest[1:]
		idx := strings.IndexAny(after, " /@")
		var token string
		if idx < 0 {
			// Rest is just the name, no message
			token = after
			rest = ""
		} else if after[idx] == '/' || after[idx] == '@' {
			// Next token is another @name or /name
			token = after[:idx]
			rest = after[idx:]
		} else {
			// Space — name ends here
			token = after[:idx]
			rest = strings.TrimSpace(after[idx+1:])
		}

		if token != "" {
			names = append(names, h.resolveAlias(token))
		}

		if rest == "" {
			break
		}
	}

	// Deduplicate names preserving order
	seen := make(map[string]bool)
	unique := names[:0]
	for _, n := range names {
		if !seen[n] {
			seen[n] = true
			unique = append(unique, n)
		}
	}

	return unique, rest
}

// HandleMessage processes a single incoming message.
func (h *Handler) HandleMessage(ctx context.Context, client *ilink.Client, msg ilink.WeixinMessage) {
	// Only process user messages that are finished
	if msg.MessageType != ilink.MessageTypeUser {
		return
	}
	if msg.MessageState != ilink.MessageStateFinish {
		return
	}

	// Deduplicate by message_id to avoid processing the same message multiple times
	// (voice messages may trigger multiple finish-state updates)
	if msg.MessageID != 0 {
		if _, loaded := h.seenMsgs.LoadOrStore(msg.MessageID, time.Now()); loaded {
			return
		}
		// Clean up old entries periodically (fire-and-forget)
		go h.cleanSeenMsgs()
	}

	// Extract text from item list (text message or voice transcription)
	text := extractText(msg)
	if text == "" {
		if voiceText := extractVoiceText(msg); voiceText != "" {
			text = voiceText
			log.Printf("[handler] voice transcription from %s: %q", msg.FromUserID, truncate(text, 80))
		}
	}
	if text == "" {
		// Check for image message
		if img := extractImage(msg); img != nil && h.saveDir != "" {
			h.handleImageSave(ctx, client, msg, img)
			return
		}
		log.Printf("[handler] received non-text message from %s, skipping", msg.FromUserID)
		return
	}

	log.Printf("[handler] received from %s: %q", msg.FromUserID, truncate(text, 80))

	// Store context token for this user
	h.contextTokens.Store(msg.FromUserID, msg.ContextToken)

	// Generate a clientID for this reply (used to correlate typing → finish)
	clientID := NewClientID()

	// Intercept URLs: save to Linkhoard directly without AI agent
	trimmed := strings.TrimSpace(text)
	if h.saveDir != "" && IsURL(trimmed) {
		rawURL := ExtractURL(trimmed)
		if rawURL != "" {
			log.Printf("[handler] saving URL to linkhoard: %s", rawURL)
			title, err := SaveLinkToLinkhoard(ctx, h.saveDir, rawURL)
			var reply string
			if err != nil {
				log.Printf("[handler] link save failed: %v", err)
				reply = fmt.Sprintf("保存失败: %v", err)
			} else {
				reply = fmt.Sprintf("已保存: %s", title)
			}
			if err := SendTextReply(ctx, client, msg.FromUserID, reply, msg.ContextToken, clientID); err != nil {
				log.Printf("[handler] failed to send reply to %s: %v", msg.FromUserID, err)
			}
			return
		}
	}

	// Built-in commands (no typing needed)
	if trimmed == "/info" {
		reply := h.buildStatus()
		if err := SendTextReply(ctx, client, msg.FromUserID, reply, msg.ContextToken, clientID); err != nil {
			log.Printf("[handler] failed to send reply to %s: %v", msg.FromUserID, err)
		}
		return
	} else if trimmed == "/help" {
		reply := buildHelpText()
		if err := SendTextReply(ctx, client, msg.FromUserID, reply, msg.ContextToken, clientID); err != nil {
			log.Printf("[handler] failed to send reply to %s: %v", msg.FromUserID, err)
		}
		return
	} else if trimmed == "/new" || trimmed == "/clear" {
		reply := h.resetDefaultSession(ctx, msg.FromUserID)
		if err := SendTextReply(ctx, client, msg.FromUserID, reply, msg.ContextToken, clientID); err != nil {
			log.Printf("[handler] failed to send reply to %s: %v", msg.FromUserID, err)
		}
		return
	} else if strings.HasPrefix(trimmed, "/cwd") {
		reply := h.handleCwd(trimmed)
		if err := SendTextReply(ctx, client, msg.FromUserID, reply, msg.ContextToken, clientID); err != nil {
			log.Printf("[handler] failed to send reply to %s: %v", msg.FromUserID, err)
		}
		return
	} else if strings.HasPrefix(trimmed, "/log") {
		reply := h.handleLog(trimmed)
		if err := SendTextReply(ctx, client, msg.FromUserID, reply, msg.ContextToken, clientID); err != nil {
			log.Printf("[handler] failed to send reply to %s: %v", msg.FromUserID, err)
		}
		return
	} else if strings.HasPrefix(trimmed, "/ls") {
		reply := h.handleLs(trimmed)
		if err := SendTextReply(ctx, client, msg.FromUserID, reply, msg.ContextToken, clientID); err != nil {
			log.Printf("[handler] failed to send reply to %s: %v", msg.FromUserID, err)
		}
		return
	} else if strings.HasPrefix(trimmed, "/session") {
		reply := h.handleSession(trimmed, msg.FromUserID)
		if err := SendTextReply(ctx, client, msg.FromUserID, reply, msg.ContextToken, clientID); err != nil {
			log.Printf("[handler] failed to send reply to %s: %v", msg.FromUserID, err)
		}
		return
	}

	// Route: "/agentname message" or "@agent1 @agent2 message" -> specific agent(s)
	agentNames, message := h.parseCommand(text)

	// No command prefix -> send to default agent
	if len(agentNames) == 0 {
		h.sendToDefaultAgent(ctx, client, msg, text, clientID)
		return
	}

	// No message -> switch default agent (only first name)
	if message == "" {
		if len(agentNames) == 1 && h.isKnownAgent(agentNames[0]) {
			reply := h.switchDefault(ctx, agentNames[0])
			if err := SendTextReply(ctx, client, msg.FromUserID, reply, msg.ContextToken, clientID); err != nil {
				log.Printf("[handler] failed to send reply to %s: %v", msg.FromUserID, err)
			}
		} else if len(agentNames) == 1 && !h.isKnownAgent(agentNames[0]) {
			// Unknown agent -> forward to default
			h.sendToDefaultAgent(ctx, client, msg, text, clientID)
		} else {
			reply := "Usage: specify one agent to switch, or add a message to broadcast"
			if err := SendTextReply(ctx, client, msg.FromUserID, reply, msg.ContextToken, clientID); err != nil {
				log.Printf("[handler] failed to send reply to %s: %v", msg.FromUserID, err)
			}
		}
		return
	}

	// Filter to known agents; if single unknown agent -> forward to default
	var knownNames []string
	for _, name := range agentNames {
		if h.isKnownAgent(name) {
			knownNames = append(knownNames, name)
		}
	}
	if len(knownNames) == 0 {
		// No known agents -> forward entire text to default agent
		h.sendToDefaultAgent(ctx, client, msg, text, clientID)
		return
	}

	// Send typing indicator
	go func() {
		if typingErr := SendTypingState(ctx, client, msg.FromUserID, msg.ContextToken); typingErr != nil {
			log.Printf("[handler] failed to send typing state: %v", typingErr)
		}
	}()

	if len(knownNames) == 1 {
		// Single agent
		h.sendToNamedAgent(ctx, client, msg, knownNames[0], message, clientID)
	} else {
		// Multi-agent broadcast: parallel dispatch, send replies as they arrive
		h.broadcastToAgents(ctx, client, msg, knownNames, message)
	}
}

// sendToDefaultAgent sends the message to the default agent and replies.
func (h *Handler) sendToDefaultAgent(ctx context.Context, client *ilink.Client, msg ilink.WeixinMessage, text, clientID string) {
	go func() {
		if typingErr := SendTypingState(ctx, client, msg.FromUserID, msg.ContextToken); typingErr != nil {
			log.Printf("[handler] failed to send typing state: %v", typingErr)
		}
	}()

	h.mu.RLock()
	defaultName := h.defaultName
	h.mu.RUnlock()

	ag := h.getDefaultAgent()
	var reply string
	if ag != nil {
		var err error
		reply, err = h.chatWithAgent(ctx, ag, msg.FromUserID, text)
		if err != nil {
			reply = fmt.Sprintf("Error: %v", err)
		}
	} else {
		log.Printf("[handler] agent not ready, using echo mode for %s", msg.FromUserID)
		reply = "[echo] " + text
	}

	h.sendReplyWithMedia(ctx, client, msg, defaultName, reply, clientID)
}

// sendToNamedAgent sends the message to a specific agent and replies.
func (h *Handler) sendToNamedAgent(ctx context.Context, client *ilink.Client, msg ilink.WeixinMessage, name, message, clientID string) {
	ag, agErr := h.getAgent(ctx, name)
	if agErr != nil {
		log.Printf("[handler] agent %q not available: %v", name, agErr)
		reply := fmt.Sprintf("Agent %q is not available: %v", name, agErr)
		SendTextReply(ctx, client, msg.FromUserID, reply, msg.ContextToken, clientID)
		return
	}

	reply, err := h.chatWithAgent(ctx, ag, msg.FromUserID, message)
	if err != nil {
		reply = fmt.Sprintf("Error: %v", err)
	}
	h.sendReplyWithMedia(ctx, client, msg, name, reply, clientID)
}

// broadcastToAgents sends the message to multiple agents in parallel.
// Each reply is sent as a separate message with the agent name prefix.
func (h *Handler) broadcastToAgents(ctx context.Context, client *ilink.Client, msg ilink.WeixinMessage, names []string, message string) {
	type result struct {
		name  string
		reply string
	}

	ch := make(chan result, len(names))

	for _, name := range names {
		go func(n string) {
			ag, err := h.getAgent(ctx, n)
			if err != nil {
				ch <- result{name: n, reply: fmt.Sprintf("Error: %v", err)}
				return
			}
			reply, err := h.chatWithAgent(ctx, ag, msg.FromUserID, message)
			if err != nil {
				ch <- result{name: n, reply: fmt.Sprintf("Error: %v", err)}
				return
			}
			ch <- result{name: n, reply: reply}
		}(name)
	}

	// Send replies as they arrive
	for range names {
		r := <-ch
		reply := fmt.Sprintf("[%s] %s", r.name, r.reply)
		clientID := NewClientID()
		h.sendReplyWithMedia(ctx, client, msg, r.name, reply, clientID)
	}
}

// sendReplyWithMedia sends a text reply and any extracted image URLs.
func (h *Handler) sendReplyWithMedia(ctx context.Context, client *ilink.Client, msg ilink.WeixinMessage, agentName, reply, clientID string) {
	imageURLs := ExtractImageURLs(reply)
	attachmentPaths := extractLocalAttachmentPaths(reply)
	allowedRoots := h.allowedAttachmentRoots(agentName)

	var sentPaths []string
	var failedPaths []string
	for _, attachmentPath := range attachmentPaths {
		if !isAllowedAttachmentPath(attachmentPath, allowedRoots) {
			log.Printf("[handler] rejected attachment outside allowed roots for agent %q: %s", agentName, attachmentPath)
			failedPaths = append(failedPaths, attachmentPath)
			continue
		}
		if err := SendMediaFromPath(ctx, client, msg.FromUserID, attachmentPath, msg.ContextToken); err != nil {
			log.Printf("[handler] failed to send attachment to %s: %v", msg.FromUserID, err)
			failedPaths = append(failedPaths, attachmentPath)
			continue
		}
		sentPaths = append(sentPaths, attachmentPath)
	}

	reply = rewriteReplyWithAttachmentResults(reply, sentPaths, failedPaths)

	if err := SendTextReply(ctx, client, msg.FromUserID, reply, msg.ContextToken, clientID); err != nil {
		log.Printf("[handler] failed to send reply to %s: %v", msg.FromUserID, err)
	}

	for _, imgURL := range imageURLs {
		if err := SendMediaFromURL(ctx, client, msg.FromUserID, imgURL, msg.ContextToken); err != nil {
			log.Printf("[handler] failed to send image to %s: %v", msg.FromUserID, err)
		}
	}
}

func (h *Handler) allowedAttachmentRoots(agentName string) []string {
	roots := []string{defaultAttachmentWorkspace()}

	h.mu.RLock()
	agentDir := h.agentWorkDirs[agentName]
	h.mu.RUnlock()

	if agentDir != "" {
		roots = append(roots, agentDir)
	}

	return roots
}

// chatWithAgent sends a message to an agent and returns the reply, with logging.
func (h *Handler) chatWithAgent(ctx context.Context, ag agent.Agent, userID, message string) (string, error) {
	info := ag.Info()
	log.Printf("[handler] dispatching to agent (%s) for %s", info, userID)

	start := time.Now()
	reply, err := ag.Chat(ctx, userID, message)
	elapsed := time.Since(start)

	if err != nil {
		log.Printf("[handler] agent error (%s, elapsed=%s): %v", info, elapsed, err)
		return "", err
	}

	log.Printf("[handler] agent replied (%s, elapsed=%s): %q", info, elapsed, truncate(reply, 100))
	return reply, nil
}

// switchDefault switches the default agent. Starts it on demand if needed.
// The change is persisted to config file.
func (h *Handler) switchDefault(ctx context.Context, name string) string {
	ag, err := h.getAgent(ctx, name)
	if err != nil {
		log.Printf("[handler] failed to switch default to %q: %v", name, err)
		return fmt.Sprintf("Failed to switch to %q: %v", name, err)
	}

	h.mu.Lock()
	old := h.defaultName
	h.defaultName = name
	h.agents[name] = ag
	h.mu.Unlock()

	// Persist to config file
	if h.saveDefault != nil {
		if err := h.saveDefault(name); err != nil {
			log.Printf("[handler] failed to save default agent to config: %v", err)
		} else {
			log.Printf("[handler] saved default agent %q to config", name)
		}
	}

	info := ag.Info()
	log.Printf("[handler] switched default agent: %s -> %s (%s)", old, name, info)
	return fmt.Sprintf("switch to %s", name)
}

// resetDefaultSession resets the session for the given userID on the default agent.
func (h *Handler) resetDefaultSession(ctx context.Context, userID string) string {
	ag := h.getDefaultAgent()
	if ag == nil {
		return "No agent running."
	}
	name := ag.Info().Name
	sessionID, err := ag.ResetSession(ctx, userID)
	if err != nil {
		log.Printf("[handler] reset session failed for %s: %v", userID, err)
		return fmt.Sprintf("Failed to reset session: %v", err)
	}
	if sessionID != "" {
		return fmt.Sprintf("已创建新的%s会话\n%s", name, sessionID)
	}
	return fmt.Sprintf("已创建新的%s会话", name)
}

// handleCwd handles the /cwd command. It updates the working directory for all running agents.
func (h *Handler) handleCwd(trimmed string) string {
	arg := strings.TrimSpace(strings.TrimPrefix(trimmed, "/cwd"))
	if arg == "" {
		// No path provided — show current cwd of default agent
		ag := h.getDefaultAgent()
		if ag == nil {
			return "No agent running."
		}
		info := ag.Info()
		return fmt.Sprintf("cwd: (check agent config)\nagent: %s", info.Name)
	}

	// Expand ~ to home directory
	if arg == "~" {
		home, err := os.UserHomeDir()
		if err == nil {
			arg = home
		}
	} else if strings.HasPrefix(arg, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			arg = filepath.Join(home, arg[2:])
		}
	}

	// Resolve to absolute path
	absPath, err := filepath.Abs(arg)
	if err != nil {
		return fmt.Sprintf("Invalid path: %v", err)
	}

	// Verify directory exists
	info, err := os.Stat(absPath)
	if err != nil {
		return fmt.Sprintf("Path not found: %s", absPath)
	}
	if !info.IsDir() {
		return fmt.Sprintf("Not a directory: %s", absPath)
	}

	// Update cwd on all running agents
	h.mu.RLock()
	agents := make(map[string]agent.Agent, len(h.agents))
	for name, ag := range h.agents {
		agents[name] = ag
	}
	h.mu.RUnlock()

	for name, ag := range agents {
		ag.SetCwd(absPath)
		log.Printf("[handler] updated cwd for agent %s: %s", name, absPath)
	}

	h.mu.Lock()
	for name := range agents {
		h.agentWorkDirs[name] = absPath
	}
	h.mu.Unlock()

	return fmt.Sprintf("cwd: %s", absPath)
}

// handleLog handles the /log command — returns the last N lines from the log file.
func (h *Handler) handleLog(trimmed string) string {
	n := 20
	arg := strings.TrimSpace(strings.TrimPrefix(trimmed, "/log"))
	if arg != "" {
		if parsed, err := strconv.Atoi(arg); err == nil && parsed > 0 {
			n = parsed
		}
	}
	if n > 200 {
		n = 200
	}

	if h.logFilePath == "" {
		return "log file not configured"
	}

	f, err := os.Open(h.logFilePath)
	if err != nil {
		return fmt.Sprintf("open log file: %v", err)
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return fmt.Sprintf("read log file: %v", err)
	}

	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}

	return fmt.Sprintf("```\n%s\n```", strings.Join(lines, "\n"))
}

// handleLs handles the /ls command — lists files and directories at a given path with depth control.
func (h *Handler) handleLs(trimmed string) string {
	parts := strings.Fields(strings.TrimSpace(strings.TrimPrefix(trimmed, "/ls")))

	root := "."
	maxDepth := 1

	if len(parts) > 0 && parts[0] != "" {
		root = parts[0]
	}
	if len(parts) > 1 {
		if d, err := strconv.Atoi(parts[1]); err == nil && d > 0 {
			maxDepth = d
		}
	}
	if maxDepth > 5 {
		maxDepth = 5
	}

	// Expand ~
	if root == "~" {
		home, err := os.UserHomeDir()
		if err == nil {
			root = home
		}
	} else if strings.HasPrefix(root, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			root = filepath.Join(home, root[2:])
		}
	}

	absPath, err := filepath.Abs(root)
	if err != nil {
		return fmt.Sprintf("invalid path: %v", err)
	}

	info, err := os.Stat(absPath)
	if err != nil {
		return fmt.Sprintf("path not found: %s", absPath)
	}
	if !info.IsDir() {
		return fmt.Sprintf("not a directory: %s", absPath)
	}

	var buf strings.Builder
	n := 1
	walkDirFlat(&buf, absPath, maxDepth, 0, &n)

	out := strings.TrimSpace(buf.String())
	if out == "" {
		return "(empty)"
	}
	return out
}

// handleSession handles the /session command — manages opencode sessions.
func (h *Handler) handleSession(trimmed string, userID string) string {
	arg := strings.TrimSpace(strings.TrimPrefix(trimmed, "/session"))

	// No argument: show current session ID
	if arg == "" {
		return h.getCurrentSessionID(userID)
	}

	// Find opencode binary
	opencodePath, err := findOpencodeBinary()
	if err != nil {
		return "opencode 未安装或不在 PATH 中"
	}

	if arg == "list" {
		return listOpencodeSessions(opencodePath)
	}

	if strings.HasPrefix(arg, "delete ") || strings.HasPrefix(arg, "del ") {
		sessionID := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(arg, "delete "), "del "))
		if sessionID == "" {
			return "用法: /session delete <sessionID>"
		}
		return deleteOpencodeSession(opencodePath, sessionID)
	}

	if strings.HasPrefix(arg, "fork ") {
		sessionID := strings.TrimSpace(strings.TrimPrefix(arg, "fork "))
		if sessionID == "" {
			return "用法: /session fork <sessionID>"
		}
		return forkOpencodeSession(opencodePath, sessionID)
	}

	if strings.HasPrefix(arg, "rename ") {
		parts := strings.SplitN(strings.TrimPrefix(arg, "rename "), " ", 2)
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			return "用法: /session rename <sessionID> <新标题>"
		}
		return renameOpencodeSession(opencodePath, parts[0], parts[1])
	}

	return "用法:\n/session - 查看当前会话 ID\n/session list - 列出所有会话\n/session delete <id> - 删除会话\n/session fork <id> - 分叉会话（显示最近对话）\n/session rename <id> <标题> - 重命名会话（显示最近对话）"
}

// getCurrentSessionID returns the current session ID for the user.
func (h *Handler) getCurrentSessionID(userID string) string {
	ag := h.getDefaultAgent()
	if ag == nil {
		return "没有运行中的 Agent"
	}

	sessionID := ag.GetSessionID(userID)
	if sessionID == "" {
		return "当前会话尚未创建。发送消息后会自动创建。"
	}

	info := ag.Info()
	return fmt.Sprintf("当前会话:\nAgent: %s\n会话 ID: %s\n(完整 ID 可用于 fork/rename)", info.Name, sessionID)
}

// findOpencodeBinary finds the opencode binary in PATH.
func findOpencodeBinary() (string, error) {
	if p, err := exec.LookPath("opencode"); err == nil {
		return p, nil
	}
	// Fallback: try login shell
	shell := "zsh"
	if runtime.GOOS != "darwin" {
		shell = "bash"
	}
	out, err := exec.Command(shell, "-lic", "which opencode").Output()
	if err != nil {
		return "", fmt.Errorf("opencode not found")
	}
	p := strings.TrimSpace(string(out))
	if p == "" {
		return "", fmt.Errorf("opencode not found")
	}
	return p, nil
}

// listOpencodeSessions runs `opencode session list` and returns formatted output.
func listOpencodeSessions(opencodePath string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, opencodePath, "session", "list", "--format", "json")
	out, err := cmd.Output()
	if err != nil {
		return fmt.Sprintf("获取会话列表失败: %v", err)
	}

	var sessions []struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		CreatedAt string `json:"createdAt"`
		UpdatedAt string `json:"updatedAt"`
	}
	if err := json.Unmarshal(out, &sessions); err != nil {
		// Fallback: return raw output
		output := strings.TrimSpace(string(out))
		if output == "" {
			return "没有会话"
		}
		return output
	}

	if len(sessions) == 0 {
		return "没有会话"
	}

	var buf strings.Builder
	buf.WriteString("会话列表:\n")
	for i, s := range sessions {
		if i >= 20 {
			buf.WriteString(fmt.Sprintf("... 还有 %d 个会话\n", len(sessions)-20))
			break
		}
		title := s.Title
		if title == "" {
			title = "(无标题)"
		}
		// Truncate long titles
		if len(title) > 30 {
			title = title[:27] + "..."
		}
		buf.WriteString(fmt.Sprintf("%s - %s\n", s.ID[:8], title))
	}
	return buf.String()
}

// deleteOpencodeSession runs `opencode session delete <id>`.
func deleteOpencodeSession(opencodePath, sessionID string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, opencodePath, "session", "delete", sessionID)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Sprintf("删除会话失败: %v\n%s", err, strings.TrimSpace(string(out)))
	}
	return fmt.Sprintf("已删除会话 %s", sessionID)
}

// forkOpencodeSession forks an existing session via the OpenCode server API.
func forkOpencodeSession(opencodePath, sessionID string) string {
	serverURL := findOpencodeServer()
	if serverURL == "" {
		// Fallback: try using CLI with run command
		return forkOpencodeSessionCLI(opencodePath, sessionID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	body := `{}`
	req, err := http.NewRequestWithContext(ctx, "POST", serverURL+"/session/"+sessionID+"/fork", strings.NewReader(body))
	if err != nil {
		return fmt.Sprintf("创建请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Sprintf("分叉会话失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Sprintf("分叉会话失败 (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var session struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&session); err != nil {
		return "已分叉会话（无法获取新会话信息）"
	}

	title := session.Title
	if title == "" {
		title = "(无标题)"
	}

	result := fmt.Sprintf("已分叉会话\n新会话: %s\n标题: %s", session.ID[:8], title)

	// Get last 5 messages from the new session
	if history := getSessionHistory(opencodePath, session.ID, 5); history != "" {
		result += "\n\n" + history
	}

	return result
}

// forkOpencodeSessionCLI forks a session using the opencode run command.
func forkOpencodeSessionCLI(opencodePath, sessionID string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Use run command with --session and --fork flags
	cmd := exec.CommandContext(ctx, opencodePath, "run", "--session", sessionID, "--fork", "--format", "json", "")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Sprintf("分叉会话失败: %v\n%s", err, strings.TrimSpace(string(out)))
	}

	// Parse the output to get the new session ID
	var result struct {
		SessionID string `json:"sessionID"`
	}
	if err := json.Unmarshal(out, &result); err == nil && result.SessionID != "" {
		reply := fmt.Sprintf("已分叉会话\n新会话: %s", result.SessionID[:8])

		// Get last 5 messages from the new session
		if history := getSessionHistory(opencodePath, result.SessionID, 5); history != "" {
			reply += "\n\n" + history
		}

		return reply
	}

	return "已分叉会话"
}

// renameOpencodeSession renames a session via the OpenCode server API.
func renameOpencodeSession(opencodePath, sessionID, newTitle string) string {
	serverURL := findOpencodeServer()
	if serverURL == "" {
		return "OpenCode 服务器未运行。请先启动: opencode serve"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	body := fmt.Sprintf(`{"title": %s}`, jsonEscapeString(newTitle))
	req, err := http.NewRequestWithContext(ctx, "PATCH", serverURL+"/session/"+sessionID, strings.NewReader(body))
	if err != nil {
		return fmt.Sprintf("创建请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Sprintf("重命名会话失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Sprintf("重命名会话失败 (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	result := fmt.Sprintf("已将会话 %s 重命名为: %s", sessionID[:8], newTitle)

	// Get last 5 messages from the session
	if history := getSessionHistory(opencodePath, sessionID, 5); history != "" {
		result += "\n\n" + history
	}

	return result
}

// jsonEscapeString escapes a string for use in JSON.
func jsonEscapeString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// getSessionHistory returns the last N messages from a session.
func getSessionHistory(opencodePath, sessionID string, maxMessages int) string {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, opencodePath, "export", sessionID)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}

	// Parse the export output - skip the first line "Exporting session: ..."
	lines := strings.SplitN(string(out), "\n", 2)
	if len(lines) < 2 {
		return ""
	}

	var export struct {
		Messages []struct {
			Info struct {
				Role string `json:"role"`
			} `json:"info"`
			Parts []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"messages"`
	}

	if err := json.Unmarshal([]byte(lines[1]), &export); err != nil {
		return ""
	}

	if len(export.Messages) == 0 {
		return ""
	}

	// Get the last N messages
	start := 0
	if len(export.Messages) > maxMessages {
		start = len(export.Messages) - maxMessages
	}
	recentMessages := export.Messages[start:]

	var buf strings.Builder
	buf.WriteString("最近对话:\n")

	for _, msg := range recentMessages {
		role := msg.Info.Role
		if role == "user" {
			// Extract text from parts
			for _, part := range msg.Parts {
				if part.Type == "text" && part.Text != "" {
					text := part.Text
					if len(text) > 50 {
						text = text[:47] + "..."
					}
					buf.WriteString(fmt.Sprintf("用户: %s\n", text))
					break
				}
			}
		} else if role == "assistant" {
			// Extract text from parts
			for _, part := range msg.Parts {
				if part.Type == "text" && part.Text != "" {
					text := part.Text
					if len(text) > 100 {
						text = text[:97] + "..."
					}
					buf.WriteString(fmt.Sprintf("助手: %s\n", text))
					break
				}
			}
		}
	}

	return buf.String()
}

// findOpencodeServer finds a running OpenCode server.
// Returns the base URL (e.g., "http://127.0.0.1:4096") or empty string.
func findOpencodeServer() string {
	// Try default port first
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://127.0.0.1:4096/global/health")
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return "http://127.0.0.1:4096"
		}
	}

	// Try common alternative ports
	for _, port := range []int{4097, 4098, 4099, 8080, 8081} {
		url := fmt.Sprintf("http://127.0.0.1:%d/global/health", port)
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return fmt.Sprintf("http://127.0.0.1:%d", port)
			}
		}
	}

	return ""
}

// walkDirFlat recursively walks a directory and writes full paths one per line.
func walkDirFlat(buf *strings.Builder, path string, maxDepth, currentDepth int, n *int) {
	if currentDepth >= maxDepth {
		return
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return
	}

	for _, entry := range entries {
		fullPath := filepath.Join(path, entry.Name())
		if entry.IsDir() {
			buf.WriteString(fmt.Sprintf("%d. %s/\n", *n, fullPath))
			*n++
			walkDirFlat(buf, fullPath, maxDepth, currentDepth+1, n)
		} else {
			buf.WriteString(fmt.Sprintf("%d. %s\n", *n, fullPath))
			*n++
		}
	}
}

// buildStatus returns a short status string showing the current default agent.
func (h *Handler) buildStatus() string {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if h.defaultName == "" {
		return "agent: none (echo mode)"
	}

	ag, ok := h.agents[h.defaultName]
	if !ok {
		return fmt.Sprintf("agent: %s (not started)", h.defaultName)
	}

	info := ag.Info()
	return fmt.Sprintf("agent: %s\ntype: %s\nmodel: %s", h.defaultName, info.Type, info.Model)
}

func buildHelpText() string {
	return `WeClaw 命令列表

对话
  直接发文字       发送给默认 Agent
  /agent msg       发送给指定 Agent
  @agent msg       同上
  @a @b msg        同时发给多个 Agent

切换
  /agent           切换默认 Agent
  /cc              切换到 Claude
  /cx              切换到 Codex
  /cs              切换到 Cursor
  /km              切换到 Kimi
  /gm              切换到 Gemini
  /ocd             切换到 OpenCode
  /oc              切换到 OpenClaw
  /pi              切换到 Pi
  /cp              切换到 Copilot

会话
  /new             开始新对话
  /clear           同上
  /session list    列出 OpenCode 会话
  /session delete <id>  删除 OpenCode 会话
  /session fork <id>    分叉 OpenCode 会话
  /session rename <id> <标题>  重命名 OpenCode 会话

工作目录
  /cwd /path       切换工作目录

系统
  /info            查看当前 Agent 信息
  /log [N]         查看最近日志（默认 20 行，最大 200）
  /ls [path] [d]   列出文件（默认深度 1，最大 5）
  /help            显示此帮助`
}

func extractText(msg ilink.WeixinMessage) string {
	for _, item := range msg.ItemList {
		if item.Type == ilink.ItemTypeText && item.TextItem != nil {
			return item.TextItem.Text
		}
	}
	return ""
}

func extractImage(msg ilink.WeixinMessage) *ilink.ImageItem {
	for _, item := range msg.ItemList {
		if item.Type == ilink.ItemTypeImage && item.ImageItem != nil {
			return item.ImageItem
		}
	}
	return nil
}

func extractVoiceText(msg ilink.WeixinMessage) string {
	for _, item := range msg.ItemList {
		if item.Type == ilink.ItemTypeVoice && item.VoiceItem != nil && item.VoiceItem.Text != "" {
			return item.VoiceItem.Text
		}
	}
	return ""
}

func (h *Handler) handleImageSave(ctx context.Context, client *ilink.Client, msg ilink.WeixinMessage, img *ilink.ImageItem) {
	clientID := NewClientID()
	log.Printf("[handler] received image from %s, saving to %s", msg.FromUserID, h.saveDir)

	// Download image data
	var data []byte
	var err error

	if img.URL != "" {
		// Direct URL download
		data, _, err = downloadFile(ctx, img.URL)
	} else if img.Media != nil && img.Media.EncryptQueryParam != "" {
		// CDN encrypted download
		data, err = DownloadFileFromCDN(ctx, img.Media.EncryptQueryParam, img.Media.AESKey)
	} else {
		log.Printf("[handler] image has no URL or media info from %s", msg.FromUserID)
		return
	}

	if err != nil {
		log.Printf("[handler] failed to download image from %s: %v", msg.FromUserID, err)
		reply := fmt.Sprintf("Failed to save image: %v", err)
		_ = SendTextReply(ctx, client, msg.FromUserID, reply, msg.ContextToken, clientID)
		return
	}

	// Detect extension from content
	ext := detectImageExt(data)

	// Generate filename with timestamp
	ts := time.Now().Format("20060102-150405")
	fileName := fmt.Sprintf("%s%s", ts, ext)
	filePath := filepath.Join(h.saveDir, fileName)

	// Ensure save directory exists
	if err := os.MkdirAll(h.saveDir, 0o755); err != nil {
		log.Printf("[handler] failed to create save dir: %v", err)
		return
	}

	// Write image file
	if err := os.WriteFile(filePath, data, 0o644); err != nil {
		log.Printf("[handler] failed to write image: %v", err)
		reply := fmt.Sprintf("Failed to save image: %v", err)
		_ = SendTextReply(ctx, client, msg.FromUserID, reply, msg.ContextToken, clientID)
		return
	}

	// Write sidecar file
	sidecarPath := filePath + ".sidecar.md"
	sidecarContent := fmt.Sprintf("---\nid: %s\n---\n", uuid.New().String())
	if err := os.WriteFile(sidecarPath, []byte(sidecarContent), 0o644); err != nil {
		log.Printf("[handler] failed to write sidecar: %v", err)
	}

	log.Printf("[handler] saved image to %s (%d bytes)", filePath, len(data))
	reply := fmt.Sprintf("Saved: %s", fileName)
	if err := SendTextReply(ctx, client, msg.FromUserID, reply, msg.ContextToken, clientID); err != nil {
		log.Printf("[handler] failed to send reply to %s: %v", msg.FromUserID, err)
	}
}

func detectImageExt(data []byte) string {
	if len(data) < 4 {
		return ".bin"
	}
	// PNG: 89 50 4E 47
	if data[0] == 0x89 && data[1] == 0x50 && data[2] == 0x4E && data[3] == 0x47 {
		return ".png"
	}
	// JPEG: FF D8 FF
	if data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF {
		return ".jpg"
	}
	// GIF: 47 49 46
	if data[0] == 0x47 && data[1] == 0x49 && data[2] == 0x46 {
		return ".gif"
	}
	// WebP: 52 49 46 46 ... 57 45 42 50
	if len(data) >= 12 && data[0] == 0x52 && data[1] == 0x49 && data[8] == 0x57 && data[9] == 0x45 {
		return ".webp"
	}
	// BMP: 42 4D
	if data[0] == 0x42 && data[1] == 0x4D {
		return ".bmp"
	}
	return ".jpg" // default to jpg for WeChat images
}
