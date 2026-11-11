// Command claude-code-agent is an interactive terminal agent that mirrors
// the Claude Code experience on top of this SDK: a multi-turn REPL with
// streaming typewriter output, per-tool terminal approval (y/n/always),
// tool activity lines, and Ctrl+C to interrupt the current turn.
//
// Requires qoderclicn on PATH (or QODERCLI_PATH) and authentication via
// QoderCLIAuth (local `qoderclicn login` state).
//
// Try: `go run ./examples/claude-code-agent` then ask it to inspect or edit
// files in the current directory; approve its tool calls from the terminal.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"

	qoder "github.com/larbing/qodercn-agent-go"
)

// ANSI colors (Windows Terminal / pwsh / modern terminals all support VT).
const (
	cReset  = "\033[0m"
	cBold   = "\033[1m"
	cCyan   = "\033[36m"
	cGray   = "\033[90m"
	cYellow = "\033[33m"
	cRed    = "\033[31m"
	cGreen  = "\033[32m"
)

var (
	// printMu serializes terminal writes coming from the stream consumer,
	// the permission callback, and hook callbacks.
	printMu sync.Mutex
	// stdinMu guards the single shared stdin reader: the REPL and the
	// modal approval prompt both read from it.
	stdinMu sync.Mutex
	stdin   = bufio.NewReader(os.Stdin)
)

// streamingText tracks whether the current turn already printed streamed
// text deltas, so the final AssistantMessage is not printed twice. Only
// accessed from the consumer goroutine.
var streamingText bool

// curModel mirrors the model reported by the system init message; read and
// write are both done under printMu.
var curModel string

type agent struct {
	client *qoder.Client
	busy   atomic.Bool // a turn is in flight; SIGINT means interrupt
}

func main() {
	a := &agent{}

	opts := &qoder.Options{
		Auth: qoder.QoderCLIAuth(),
		// Token-level deltas for the typewriter effect.
		IncludePartialMessages: true,
		// Every tool call is approved from this terminal.
		CanUseTool: approveTool,
		// Hook demo: surface tool failures as terminal warnings.
		Hooks: map[qoder.HookEvent][]qoder.HookMatcher{
			qoder.HookPostToolUseFailure: {{
				Hooks: []qoder.HookCallback{onToolFailure},
			}},
		},
	}

	client, err := qoder.NewClient(opts)
	if err != nil {
		fatal("create client: %v", err)
	}
	a.client = client

	ctx := context.Background()
	if err := client.Connect(ctx); err != nil {
		fatal("connect: %v", err)
	}
	defer client.Disconnect(context.Background())

	printBanner()

	// Background consumer: renders the live message stream; the terminal
	// REPL only waits for the per-turn result channel.
	resultCh := make(chan *qoder.ResultMessage, 4)
	go consume(ctx, client, resultCh)

	// Ctrl+C: interrupt the running turn, or exit when idle.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	go func() {
		for range sigCh {
			if a.busy.CompareAndSwap(true, false) {
				interruptCtx, cancel := context.WithTimeout(context.Background(), 5e9)
				_ = client.Interrupt(interruptCtx)
				cancel()
				printMu.Lock()
				fmt.Printf("\n%s(中断当前轮…)%s\n", cGray, cReset)
				printMu.Unlock()
				continue
			}
			printMu.Lock()
			fmt.Printf("\n%s再见！%s\n", cGray, cReset)
			printMu.Unlock()
			os.Exit(0)
		}
	}()

	repl(ctx, a, resultCh)
}

// repl reads prompts and slash commands until EOF or /exit.
func repl(ctx context.Context, a *agent, resultCh chan *qoder.ResultMessage) {
	for {
		printMu.Lock()
		fmt.Printf("%s>%s ", cCyan+cBold, cReset)
		printMu.Unlock()

		stdinMu.Lock()
		line, err := stdin.ReadString('\n')
		stdinMu.Unlock()
		if err != nil {
			return // EOF
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "/") {
			if slashCommand(ctx, a.client, line) {
				return
			}
			continue
		}

		a.busy.Store(true)
		if err := a.client.Query(ctx, line); err != nil {
			printMu.Lock()
			fmt.Printf("%s发送失败: %v%s\n", cRed, err, cReset)
			printMu.Unlock()
			a.busy.Store(false)
			continue
		}
		// One ResultMessage per turn lands on the channel; interrupting
		// also produces a result, so this always returns. A nil value means
		// the stream died (consume printed the cause) — stop the REPL.
		if <-resultCh == nil {
			return
		}
		a.busy.Store(false)
	}
}

// slashCommand implements the built-in commands. It returns true to exit.
func slashCommand(ctx context.Context, client *qoder.Client, line string) (exit bool) {
	fields := strings.Fields(line)
	cmd, arg := fields[0], ""
	if len(fields) > 1 {
		arg = fields[1]
	}
	switch cmd {
	case "/exit", "/quit", "/q":
		return true
	case "/help":
		out := []string{
			"直接输入任务与 agent 对话（读写文件、执行命令等工具调用会请求审批）",
			"/model [名称]   查看或切换模型",
			"/mode  [模式]   查看或切换权限模式 (default/acceptEdits/plan/bypassPermissions)",
			"/usage          查询账号用量",
			"/mcp            查看 MCP server 状态",
			"/agents         列出可用子代理",
			"/exit           退出（Ctrl+C 中断当前轮 / 空闲时退出）",
		}
		printMu.Lock()
		for _, s := range out {
			fmt.Println(cGray + s + cReset)
		}
		printMu.Unlock()
	case "/model":
		if arg == "" {
			printMu.Lock()
			model := curModel
			printMu.Unlock()
			if model == "" {
				model = "unknown"
			}
			printMu.Lock()
			fmt.Printf("当前模型: %s\n", model)
			printMu.Unlock()
			break
		}
		if err := client.SetModel(ctx, arg); err != nil {
			printMu.Lock()
			fmt.Printf("%s切换模型失败: %v%s\n", cRed, err, cReset)
			printMu.Unlock()
		} else {
			printMu.Lock()
			fmt.Printf("模型已切换: %s\n", arg)
			printMu.Unlock()
		}
	case "/mode":
		if arg == "" {
			printMu.Lock()
			fmt.Println("用法: /mode default|acceptEdits|plan|bypassPermissions")
			printMu.Unlock()
			break
		}
		if err := client.SetPermissionMode(ctx, qoder.PermissionMode(arg)); err != nil {
			printMu.Lock()
			fmt.Printf("%s切换权限模式失败: %v%s\n", cRed, err, cReset)
			printMu.Unlock()
		} else {
			printMu.Lock()
			fmt.Printf("权限模式已切换: %s\n", arg)
			printMu.Unlock()
		}
	case "/usage":
		usage, err := client.UsageInfo(ctx)
		printMu.Lock()
		if err != nil {
			fmt.Printf("%s查询失败: %v%s\n", cRed, err, cReset)
		} else if b, merr := json.MarshalIndent(usage, "", "  "); merr == nil {
			fmt.Println(string(b))
		}
		printMu.Unlock()
	case "/mcp":
		status, err := client.MCPStatus(ctx)
		printMu.Lock()
		if err != nil {
			fmt.Printf("%s查询失败: %v%s\n", cRed, err, cReset)
		} else if b, merr := json.MarshalIndent(status, "", "  "); merr == nil {
			fmt.Println(string(b))
		}
		printMu.Unlock()
	case "/agents":
		agents := client.SupportedAgents()
		printMu.Lock()
		if len(agents) == 0 {
			fmt.Println(cGray + "（无）" + cReset)
		}
		for _, ag := range agents {
			fmt.Printf("- %v: %v\n", ag["name"], ag["description"])
		}
		printMu.Unlock()
	default:
		printMu.Lock()
		fmt.Printf("未知命令 %s（/help 查看帮助）\n", cmd)
		printMu.Unlock()
	}
	return false
}

// consume renders the live stream until the client disconnects. It closes
// resultCh on exit so the REPL never waits forever on a dead session.
func consume(ctx context.Context, client *qoder.Client, resultCh chan<- *qoder.ResultMessage) {
	defer close(resultCh)
	stream, err := client.ReceiveMessages(ctx)
	if err != nil {
		printMu.Lock()
		fmt.Printf("%s消息流不可用: %v%s\n", cRed, err, cReset)
		printMu.Unlock()
		return
	}
	for {
		msg, err := stream.Next(ctx)
		if err != nil {
			if ctx.Err() == nil {
				printMu.Lock()
				fmt.Printf("%s会话已断开: %v%s\n", cRed, err, cReset)
				printMu.Unlock()
			}
			return
		}
		switch m := msg.(type) {
		case *qoder.StreamEvent:
			renderDelta(m)
		case *qoder.AssistantMessage:
			renderAssistant(m)
		case *qoder.UserMessage:
			renderToolResult(m)
		case *qoder.SystemMessage:
			renderSystem(m)
		case *qoder.ResultMessage:
			renderResult(m)
			resultCh <- m
		}
	}
}

func renderDelta(m *qoder.StreamEvent) {
	if t, _ := m.Event["type"].(string); t != "content_block_delta" {
		return
	}
	delta, _ := m.Event["delta"].(map[string]any)
	switch dt, _ := delta["type"].(string); dt {
	case "text_delta":
		s, _ := delta["text"].(string)
		printMu.Lock()
		fmt.Print(s)
		printMu.Unlock()
		streamingText = true
	case "thinking_delta":
		s, _ := delta["thinking"].(string)
		printMu.Lock()
		fmt.Print(cGray + s + cReset)
		printMu.Unlock()
	}
}

func renderAssistant(m *qoder.AssistantMessage) {
	for _, block := range m.Content {
		switch b := block.(type) {
		case *qoder.TextBlock:
			if streamingText {
				continue // already rendered via deltas
			}
			printMu.Lock()
			fmt.Println(b.Text)
			printMu.Unlock()
		case *qoder.ToolUseBlock:
			printMu.Lock()
			fmt.Printf("%s⚙ %s%s %s\n", cYellow, b.Name, cReset, summarizeInput(b.Input))
			printMu.Unlock()
		}
	}
}

func renderToolResult(m *qoder.UserMessage) {
	for _, block := range m.ContentBlocks {
		if tr, ok := block.(*qoder.ToolResultBlock); ok && tr.IsError {
			printMu.Lock()
			fmt.Printf("%s✗ 工具返回错误 (tool_use_id=%s)%s\n", cRed, tr.ToolUseID, cReset)
			printMu.Unlock()
		}
	}
}

func renderSystem(m *qoder.SystemMessage) {
	switch m.Subtype {
	case "init":
		// The CLI reports model/session/tools once the session is ready.
		model, _ := m.Data["model"].(string)
		sid, _ := m.Data["session_id"].(string)
		tools, _ := m.Data["tools"].([]any)
		printMu.Lock()
		curModel = model
		fmt.Printf("%smodel=%s session=%s tools=%d%s\n", cGray, model, truncate(sid, 12), len(tools), cReset)
		printMu.Unlock()
	case "mirror_error":
		printMu.Lock()
		fmt.Printf("%s会话镜像写入失败: %v%s\n", cRed, m.Data["error"], cReset)
		printMu.Unlock()
	}
}

func renderResult(m *qoder.ResultMessage) {
	printMu.Lock()
	defer printMu.Unlock()
	if streamingText {
		fmt.Println()
		streamingText = false
	}
	cost := ""
	if m.TotalCostUSD != nil {
		cost = fmt.Sprintf(", cost=$%.4f", *m.TotalCostUSD)
	}
	fmt.Printf("%s--- %s (turns=%d, %.1fs%s) ---%s\n",
		cGray, m.Subtype, m.NumTurns, float64(m.DurationMS)/1000, cost, cReset)
	if m.IsError {
		fmt.Printf("%s%s%s\n", cRed, m.Result, cReset)
	}
}

// approveTool asks the user in the terminal for every tool call.
func approveTool(ctx context.Context, toolName string, input map[string]any, pctx *qoder.ToolPermissionContext) (qoder.PermissionResult, error) {
	if ctx.Err() != nil {
		return &qoder.PermissionResultDeny{Message: "请求已被 CLI 取消"}, nil
	}
	printMu.Lock()
	fmt.Printf("\n%s⚙ 工具审批%s %s\n", cYellow, cReset, toolName)
	fmt.Printf("  %s\n", summarizeInput(input))
	if pctx.Description != "" {
		fmt.Printf("  %s%s%s\n", cGray, pctx.Description, cReset)
	}
	fmt.Printf("%s允许? [y] 是 / [n] 否 / [a] 本会话总是允许:%s ", cCyan, cReset)
	printMu.Unlock()

	stdinMu.Lock()
	line, err := stdin.ReadString('\n')
	stdinMu.Unlock()
	if err != nil {
		return &qoder.PermissionResultDeny{Message: "终端输入不可用，拒绝工具调用"}, nil
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes", "是":
		return &qoder.PermissionResultAllow{}, nil
	case "a", "always":
		// Session-scoped allow rule for this tool, like "don't ask again".
		return &qoder.PermissionResultAllow{
			UpdatedPermissions: []qoder.PermissionUpdate{{
				Type:        qoder.PermUpdateAddRules,
				Rules:       []qoder.PermissionRuleValue{{ToolName: toolName}},
				Behavior:    qoder.PermissionAllow,
				Destination: qoder.DestinationSession,
			}},
		}, nil
	default:
		printMu.Lock()
		fmt.Printf("%s已拒绝%s\n", cGray, cReset)
		printMu.Unlock()
		return &qoder.PermissionResultDeny{Message: "用户在终端拒绝了该工具调用"}, nil
	}
}

// onToolFailure prints tool failures reported through the hook channel.
func onToolFailure(ctx context.Context, in *qoder.HookInput, _ string) (*qoder.HookJSONOutput, error) {
	printMu.Lock()
	fmt.Printf("%s✗ %s 失败: %s%s\n", cRed, in.ToolName, in.Error, cReset)
	printMu.Unlock()
	return &qoder.HookJSONOutput{}, nil
}

// summarizeInput renders the interesting part of a tool call input.
func summarizeInput(input map[string]any) string {
	for _, key := range []string{"command", "file_path", "path", "pattern", "query", "url", "prompt", "description"} {
		if v, ok := input[key].(string); ok && v != "" {
			return truncate(v, 160)
		}
	}
	b, err := json.Marshal(input)
	if err != nil {
		return "(无法序列化的输入)"
	}
	return truncate(string(b), 160)
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", "\\n")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func printBanner() {
	printMu.Lock()
	defer printMu.Unlock()
	fmt.Printf("%sqoder-code%s — 仿 Claude Code 的终端 agent 示例 (Go SDK)\n", cBold, cReset)
	fmt.Printf("%s输入任务开始对话；/help 查看命令；Ctrl+C 中断当前轮（空闲时退出）%s\n", cGray, cReset)
	fmt.Printf("%s试试: %s看看当前目录结构，总结这个仓库是做什么的%s\n", cGray, cGreen, cReset)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "fatal: "+format+"\n", args...)
	os.Exit(1)
}
