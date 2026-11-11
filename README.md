# Qoder Agent SDK for Go

`github.com/larbing/qodercn-agent-go`（包名 `qoder`）是 Qoder Agent 运行时（`qoderclicn` CLI）的 Go SDK，完整对齐官方 Python SDK（`qodercn-agent-sdk`）的公开 API 语义：

- 一次性查询 `Query`（流式迭代消息）与多轮会话 `Client`
- 双传输协议：`--sdk` daemon（v2）优先、direct worker 自动降级（`Options.TransportMode`）
- 五种认证方式（AccessToken / 环境变量 / qodercli 登录态 / 服务账号 / Job Token）
- 权限回调 `CanUseTool`（含动态权限更新 `PermissionUpdate`）
- 15 种生命周期 Hooks
- MCP 集成：stdio / SSE / HTTP / **进程内 SDK MCP server**（经控制协议桥接，无需额外传输）
- 自定义子代理 `AgentDefinition`
- 模型策略回调 `ResolveModel`（按用途解析模型与推理参数）
- 外置会话存储 `SessionStore`（含 fork / rename / tag / import 等辅助函数）
- SideQuestion 独立提问通道

**零第三方依赖**，仅使用 Go 标准库（Go ≥ 1.24）。

真实执行者是 `qoderclicn` 子进程：SDK 负责 spawn 进程、NDJSON stdin/stdout 流解析、双向控制协议与类型化回调。

## 安装

```bash
go get github.com/larbing/qodercn-agent-go
```

### 前置条件：qoderclicn

Go 版 SDK **不捆绑 CLI 二进制**（Go 不适合 wheel 式分发）。安装 Qoder CLI 后确认 `qoderclicn` 可用：

- `Options.CLIPath` 显式指定二进制路径（最高优先级）；
- 环境变量 `QODERCLI_PATH`；
- `PATH` 上的 `qoderclicn`（Windows 自动尝试 `.exe` 后缀）。

三者都不命中时返回 `ErrKindCLINotFound` 错误。连接时 SDK 会做一次版本握手，过老的 CLI 版本会收到警告（可用 `Options.SkipVersionCheck` 关闭）。

## 传输模式：daemon 与 direct

SDK 与 `qoderclicn` 之间有两种 NDJSON 传输协议，由 `Options.TransportMode` 选择：

| 模式 | 行为 |
|---|---|
| `TransportAuto`（默认，零值） | 优先 `--sdk` daemon 协议；握手失败（进程退出 / 协议版本不符）时自动降级 direct，并向 stderr 打一行警告 |
| `TransportDaemon` | 锁定 daemon 协议，握手失败直接报错 |
| `TransportDirect` | 锁定旧 worker 协议（单进程单会话） |

daemon 协议下 SDK spawn `qoderclicn --sdk` 并执行 `initialize` 握手（协议版本 2 + 能力协商，含 keep_alive 心跳看门狗），随后 `open_session` 打开会话；会话内的控制协议与 direct 完全一致（initialize / hooks / can_use_tool 等），仅每帧多盖一个路由 `session_id`。会话结束时发送 `close_session` 并等待 ACK 后才退出进程，保证 SessionEnd hooks 可达。当前每个 `Query` / `Client` 各自持有一个独立的 daemon 进程（共享多会话 daemon 连接的独立 API 见下文范围边界）。

**版本矩阵**：

| qoderclicn 版本 | 可用 TransportMode |
|---|---|
| ≥ 1.1.86 | auto / daemon（direct 已被 CLI 拒绝：`sdk_worker_removed`） |
| 1.1.80 – 1.1.85 | auto / daemon / direct 均可 |
| < 1.1.80 | 仅 direct |

**daemon 协议限制**（超出时创建会话直接报 `ErrKindUnsupportedCapability`）：

- `PermissionMode` 不支持 `dontAsk` / `plan`；
- 不支持 `ResumeSessionAt` / `ResumeDropsTurn`（恢复位点截断）；
- `ContinueConversation` 需要配合 `SessionStore`（经外置存储续聊）。

其余选项在两种传输下行为一致；auto 模式下建议保持默认，SDK 会自动选对。

## 认证

SDK 将认证 payload 写入 0600 权限的临时文件，并通过 `QODER_SDK_AUTH_PAYLOAD_FILE` 环境变量传给子进程。五种方式：

```go
opts := &qoder.Options{}

// 1. Personal Access Token
opts.Auth = qoder.AccessToken("qd_pat_xxx")

// 2. 从环境变量读取 PAT（默认变量名可覆盖；未设置/为空时报错）
opts.Auth = qoder.AccessTokenFromEnv()             // QODER_ACCESS_TOKEN
opts.Auth = qoder.AccessTokenFromEnv("MY_PAT_VAR") // 自定义变量名

// 3. 使用本地 qoderclicn login 登录态（交互式开发推荐）
opts.Auth = qoder.QoderCLIAuth()

// 4. 服务账号密钥（或由宿主进程提供短期 token）
opts.Auth = qoder.ServiceAccountKey("sa-key")
opts.Auth = qoder.ServiceAccountToken(func() (qoder.ServiceAccountTokenResult, error) {
    return qoder.ServiceAccountTokenResult{
        Token: "short-lived", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
    }, nil
})

// 5. Job Token（一方任务令牌，按需拉取）
opts.Auth = qoder.JobToken(func(req qoder.JobTokenRequest, ctx *qoder.JobTokenContext) (qoder.JobTokenResult, error) {
    return qoder.JobTokenResult{Token: "job-token", ExpiresAt: ...}, nil
})
```

CLI 以退出码 41 报告认证失败时，SDK 返回的 `*qoder.Error` 满足 `qoder.IsProcessExitWithCode(err, 41)`。

## 一次性查询：Query

```go
stream, err := qoder.Query(ctx, "帮我审查这段代码", &qoder.Options{
    Auth:  qoder.QoderCLIAuth(),
    Model: "glm-4.7",
})
if err != nil { ... }
defer stream.Close()

for {
    msg, err := stream.Next(ctx) // io.EOF 表示流结束
    if err == io.EOF { break }
    if err != nil { ... }

    switch m := msg.(type) {
    case *qoder.AssistantMessage:
        for _, block := range m.Content {
            if t, ok := block.(*qoder.TextBlock); ok {
                fmt.Println(t.Text)
            }
        }
    case *qoder.SystemMessage: // Subtype "init" 携带 SessionID
    case *qoder.ResultMessage: // 终态：Subtype/IsError/Result/NumTurns/Usage
    }
}
```

`Options.IncludePartialMessages: true` 时还会收到 `*qoder.StreamEvent`（增量流事件）。

## 多轮会话：Client

`Client` 保持一个 CLI 进程，跨轮次复用会话并支持运行时控制：

```go
client, err := qoder.NewClient(&qoder.Options{
    Auth: qoder.QoderCLIAuth(),
    PermissionMode: qoder.PermissionAcceptEdits,
})
if err != nil { ... }
defer client.Disconnect(context.Background())

if err := client.Connect(ctx, "开始吧"); err != nil { ... }

// 发送新一轮输入（后台投递）
_ = client.Query(ctx, "把结果写入 report.md")

// 消费流：ReceiveMessages 无界缓冲；ReceiveResponse 阻塞到本轮 result
result, err := client.ReceiveResponse(ctx)

// 运行时控制
_ = client.Interrupt(ctx)
_ = client.SetModel(ctx, "glm-4.7")
_ = client.SetPermissionMode(ctx, qoder.PermissionPlan)
_ = client.SetProxy(ctx, "http://proxy:8080")
_ = client.ApplyFlagSettings(ctx, qoder.Settings{...})

// MCP 运维
status, _ := client.MCPStatus(ctx)
res, _ := client.SetMCPServers(ctx, map[string]qoder.MCPServerConfig{...}) // 增删热更新
_ = client.ReconnectMCPServer(ctx, "fetch")
_ = client.ToggleMCPServer(ctx, "fetch", false)

// 模型目录与用量
models, _ := client.AvailableModels(ctx)
usage, _ := client.UsageInfo(ctx) // 账号配额；CLI 不支持时返回 (nil, nil)

// 后台任务与异步消息
_ = client.StopTask(ctx, taskID)
cancelled, _ := client.CancelAsyncMessage(ctx, messageUUID)

// SideQuestion：不进入主对话上下文的独立提问
answer, _ := client.SideQuestion(ctx, "cfg.yaml 在哪个目录？", nil)

// 元信息
_ = client.ServerInfo()      // initialize 响应
_ = client.SupportedAgents() // CLI 支持的代理类型
plugins, _ := client.ListPlugins(ctx)
```

## 权限回调：CanUseTool

以 `--permission-prompt-tool stdio` 启动（设置 `CanUseTool` 自动启用）。CLI 每次要执行工具时回调宿主：

```go
opts.CanUseTool = func(ctx context.Context, toolName string, input map[string]any,
    pctx *qoder.ToolPermissionContext) (qoder.PermissionResult, error) {

    if toolName == "Bash" && input["command"] == "rm -rf /" {
        return &qoder.PermissionResultDeny{
            Message:   "危险命令已拦截",
            Interrupt: true, // 同时中断本轮
        }, nil
    }
    // allow 可选改写输入、追加动态权限规则
    return &qoder.PermissionResultAllow{
        UpdatedInput: input,
        UpdatedPermissions: []qoder.PermissionUpdate{{
            Type:        qoder.PermUpdateAddRules,
            Rules:       []qoder.PermissionRuleValue{{ToolName: "Bash", RuleContent: "git *"}},
            Behavior:    qoder.PermissionAllow,
            Destination: qoder.DestinationSession,
        }},
    }, nil
}
```

`pctx` 携带拦截上下文（`Title`/`Suggestions`/`BlockedPath`/`ExitPlanMode` 等）；退出计划模式审批用 `PermissionResultAllow` + `pctx.ExitPlanMode`。

## Hooks

注册期 SDK 为每个回调分配稳定 ID 并写入 initialize 握手；CLI 触发时回调宿主。输出经自动转换（`continue_`→`continue`、`stop_reason`→`stopReason`、`hook_specific_output`→`hookSpecificOutput`）：

```go
opts.Hooks = map[qoder.HookEvent][]qoder.HookMatcher{
    qoder.HookPreToolUse: {{
        Matcher: "Bash|Write",
        Timeout: 30, // 秒
        Hooks: []qoder.HookCallback{
            func(ctx context.Context, in *qoder.HookInput, toolUseID string) (*qoder.HookJSONOutput, error) {
                fmt.Println("about to run tool:", toolUseID, in.ToolName)
                cont := true
                return &qoder.HookJSONOutput{Continue: &cont}, nil
            },
        },
    }},
    qoder.HookPostToolUse: {{Hooks: []qoder.HookCallback{auditLog}}},
}
```

支持的事件：`PreToolUse` / `PostToolUse` / `PostToolUseFailure` / `UserPromptSubmit` / `SessionStart` / `SessionEnd` / `Stop` / `SubagentStart` / `SubagentStop` / `PreCompact` / `PostCompact` / `CwdChanged` / `InstructionsLoaded` / `FileChanged` / `PermissionRequest`。`HookInput` 聚合所有事件字段，未识别字段保留在 `Raw`。

## MCP 集成

四种服务端类型 + 工具级权限策略：

```go
opts.MCPServers = map[string]qoder.MCPServerConfig{
    // 1. stdio 子进程
    "local": &qoder.StdioServerConfig{
        Command: "npx", Args: []string{"-y", "@modelcontextprotocol/server-filesystem", "/tmp"},
        Env: map[string]string{"K": "V"},
    },
    // 2. SSE
    "remote": &qoder.SSEServerConfig{URL: "https://mcp.example/sse", Headers: map[string]string{"Auth": "..."}},
    // 3. Streamable HTTP + 工具策略
    "api": &qoder.HTTPServerConfig{
        URL: "https://mcp.example/mcp",
        Tools: []qoder.McpToolPolicy{
            {Name: "fetch", PermissionPolicy: qoder.ToolPolicyAlwaysAllow},
            {Name: "deploy", PermissionPolicy: qoder.ToolPolicyAlwaysDeny},
        },
    },
    // 4. 进程内 SDK MCP server（见下节）
    "calc": qoder.NewSDKMCPServer("calc", "1.0.0", tools...),
}
```

工具策略（`McpToolPolicy`）会编译进 `--settings` 的 `permissions.allow/ask/deny` 规则（`mcp__server__tool`）。

### 进程内 SDK MCP Server

工具以 Go 函数实现、随宿主进程运行；CLI 经控制协议调用，无需额外传输：

```go
calc := qoder.NewSDKMCPServer("calc", "1.0.0",
    qoder.NewTool("add", "两数相加",
        map[string]any{
            "type": "object",
            "properties": map[string]any{
                "a": map[string]any{"type": "number"},
                "b": map[string]any{"type": "number"},
            },
            "required": []string{"a", "b"},
        },
        func(ctx context.Context, args map[string]any, extra *qoder.ToolInvocationContext) (map[string]any, error) {
            sum := args["a"].(float64) + args["b"].(float64)
            return map[string]any{"sum": sum}, nil // 自动包装为 text content block
        },
        nil),
)

opts.MCPServers = map[string]qoder.MCPServerConfig{"calc": calc}
opts.AllowedTools = []string{"mcp__calc__add"} // 完整工具名 mcp__{server}__{tool}
```

handler 返回 `{"content": [...], "is_error": true}` 形状时按完整 CallToolResult 透传；返回 error 时按 MCP 语义置 `isError: true`。

## 自定义子代理

```go
opts.Agents = map[string]qoder.AgentDefinition{
    "code-reviewer": {
        Description:     "审查代码质量",
        Prompt:          "你是严格的代码审查员……",
        Tools:           []string{"Read", "Grep", "Glob"},
        Model:           "glm-4.7",
        MaxTurns:        10,
        PermissionMode:  qoder.PermissionAcceptEdits,
    },
}
opts.AllowedTools = []string{"Agent"} // 会话需暴露内置 Agent 工具才能委派
```

## 模型策略：ResolveModel

CLI 每次发起 LLM 调用前询问宿主使用哪个模型（含用途与候选目录）。超时（默认 500ms，`Options.ResolveModelTimeoutMS`）直接失败本轮，不静默降级：

```go
opts.ResolveModel = func(ctx context.Context, pctx *qoder.ModelPolicyContext) (qoder.ModelPolicyResult, error) {
    if pctx.Purpose == qoder.PurposeTitle {
        return qoder.ModelPolicyResult{Model: "glm-4.7-air"}, nil // 轻任务用小模型
    }
    return qoder.ModelPolicyResult{
        Model: "glm-4.7",
        Parameters: &qoder.ModelPolicyParameters{
            ContextWindow: 200000, ReasoningEffort: "high",
        },
    }, nil
}
```

## 外置会话存储：SessionStore

把会话记录镜像到宿主管理的存储（数据库等），并可通过辅助函数读取/管理：

```go
store := qoder.NewInMemorySessionStore() // 生产环境实现 qoder.SessionStore 接口
opts.SessionStore = store
opts.SessionStoreFlush = qoder.FlushBatched // FlushEager 立即写；批式默认 300ms/64 条
opts.LoadTimeoutMS = 60000
```

```go
// 自定义存储：Append/Load 必需；ListSessions/Delete/ListSubkeys 可选能力，
// 缺失时辅助函数按 Python SDK 语义降级（能力缺失报 UnsupportedCapability 或跳过）。
type MyStore struct{ /* ... */ }
func (s *MyStore) Append(ctx context.Context, key qoder.SessionKey, entries []qoder.SessionStoreEntry) error
func (s *MyStore) Load(ctx context.Context, key qoder.SessionKey) ([]qoder.SessionStoreEntry, error) // 未命中返回 nil, nil
```

辅助函数（`opts.Directory` 缺省取当前目录）：

```go
sessions, _ := qoder.ListSessionsFromStore(ctx, store, qoder.StoreQueryOptions{Directory: "/proj"})
info, _ := qoder.GetSessionInfoFromStore(ctx, store, sessionID, qopts)
msgs, _ := qoder.GetSessionMessagesFromStore(ctx, store, sessionID, qopts) // Offset/Limit 分页
subs, _ := qoder.ListSubagentsFromStore(ctx, store, sessionID, qopts)
subMsgs, _ := qoder.GetSubagentMessagesFromStore(ctx, store, sessionID, agentID, qopts)
_ = qoder.RenameSessionViaStore(ctx, store, sessionID, "新标题", qopts)
_ = qoder.TagSessionViaStore(ctx, store, sessionID, "release", qopts)
_ = qoder.DeleteSessionViaStore(ctx, store, sessionID, qopts) // 级联删除子代理记录
fork, _ := qoder.ForkSessionViaStore(ctx, store, sessionID, qoder.StoreQueryOptions{
    UpToMessageID: msgUUID, Title: "forked",
})
_ = qoder.ImportSessionToStore(ctx, sessionID, store, qoder.ImportSessionOptions{}) // 从本地 CLI 载入
```

配合 `Options.Resume`（session id）可从外置存储延续会话；`Options.ResumeSessionAt` / `Options.ResumeDropsTurn` 控制恢复位点。

## 错误处理

所有错误都是 `*qoder.Error`，`Kind` 判别失败类别，支持 `errors.Is/As` 与解包链：

```go
_, err := qoder.Query(ctx, "hi", opts)
var qerr *qoder.Error
if errors.As(err, &qerr) {
    switch qerr.Kind {
    case qoder.ErrKindCLINotFound:          // 未找到 qoderclicn
    case qoder.ErrKindCLIConnection:        // 进程失联/流中断
    case qoder.ErrKindProcess:              // CLI 非零退出（qerr.ExitCode）
    case qoder.ErrKindJSONDecode:           // CLI 输出无法解析
    case qoder.ErrKindAuthNotConfigured:    // 未配置认证
    case qoder.ErrKindModelPolicyTimeout:   // ResolveModel 超时
    case qoder.ErrKindUnsupportedCapability:// CLI 不支持该能力
    case qoder.ErrKindControl:              // 控制协议失败/超时
    }
}
qoder.IsProcessExitWithCode(err, 41) // 认证失败退出码
qoder.AsCLINotFound(err)
```

运行时可感知认证过期：`Options.OnAuthExpired func()` 在 CLI 报告 `auth_status` 错误或 assistant `authentication_failed` 时触发（每次会话至多一次）。

## Options 速览

| 字段 | 说明 |
|---|---|
| `Auth` | 认证配置（必填，见上文） |
| `Model` / `ResolveModel` | 静态模型 / 动态模型策略回调 |
| `SystemPrompt` / `AppendSystemPrompt` | 覆盖 / 追加系统提示词 |
| `Tools` / `AllowedTools` / `DisallowedTools` | 工具面控制；`ToolsAllDefault`→`--tools default` |
| `PermissionMode` | `default` / `acceptEdits` / `plan` / `bypassPermissions`（需 `AllowDangerouslySkipPermissions`）/ `yolo` / `dontAsk` / `auto` |
| `CanUseTool` | 权限回调（与 `PermissionPromptToolName` 互斥） |
| `Hooks` | 生命周期回调 |
| `MCPServers` | MCP 配置（4 型） |
| `Agents` / `Skills` / `SettingSources` / `AddDirs` / `Plugins` | 子代理 / 技能白名单 / 设置来源 / 额外目录 / 插件 |
| `Cwd` / `Env` / `CLIPath` | 进程环境 |
| `MaxTurns` / `IncludePartialMessages` / `ForkSession` | 会话行为 |
| `SessionID` / `Resume` / `ResumeSessionAt` / `ResumeDropsTurn` / `ContinueConversation` | 会话延续 |
| `SessionStore` / `SessionStoreFlush` / `LoadTimeoutMS` | 外置会话存储 |
| `ControlRequestTimeoutMS` | 控制请求默认超时（0=不限时） |
| `InitializeTimeoutSec` | initialize 握手超时（默认 60s） |
| `SkipVersionCheck` | 跳过 CLI 版本检查 |
| `TransportMode` | `auto`（默认）/ `daemon` / `direct`，见上文传输模式章节 |
| `Settings` / `StrictMCPConfig` | 透传 settings JSON / 严格 MCP 模式 |

完整字段见 [`options.go`](options.go)。

## 与 Python / TypeScript SDK 的差异

- **不捆绑 CLI**：Go 模块无法携带二进制，需自行安装 `qoderclicn`（见上文）。
- **零依赖**：SDK MCP 经控制协议桥接，未引入 mcp-go；JSON 处理全部使用 `encoding/json`。
- **流式消费语义**：`Query` 模式消息队列容量 512，消费者跟不上时流以 `ErrKindCLIConnection` 失败（对齐 Python 慢消费者语义）；`Client.ReceiveMessages` 无界缓冲。
- **ctx 全链路取消**：Python 的 `asyncio` 取消对应 Go `context`；控制请求取消时 SDK 自动发送 `control_cancel_request`。
- **elicitation**：未提供宿主交互回调，收到请求时按 TS/Python 默认行为回复 `cancel`。
- **范围边界**：共享多会话 daemon 连接的独立 API（Python SDK 的多会话 daemon 模式；当前每个 Query/Client 各自持有一个 daemon 进程）、memory 演进、skill-evolution、云代理（cloud agents）、`rewind`/`rewind_files` 不在 Python SDK 参考范围内，未实现。
- **API 风格**：异常 → `*qoder.Error` + `Kind`；`async def` → 普通 goroutine + channel/回调；`dataclass` 参数 → 单一 `Options` struct（JSON tag 对齐 wire 协议）。

## 示例

[`examples/quickstart`](examples/quickstart/main.go) 是 Python SDK `main.py` 的等价实现：一次查询、流式打印助手输出与最终结果。

运行前确保 `qoderclicn` 在 PATH 上并已完成 `qoderclicn login`：

```bash
cd examples/quickstart
go run .
```

[`examples/claude-code-agent`](examples/claude-code-agent/main.go) 是一个仿 Claude Code 的交互式终端 agent：多轮 REPL、流式打字机输出、每个工具调用在终端审批（y / n / a = 本会话总是允许）、内置 `/model` `/mode` `/usage` `/mcp` `/agents` 命令、Ctrl+C 中断当前轮（空闲时退出），并演示了 `CanUseTool` 审批回调与 `Hooks`（工具失败告警）的用法。

```bash
go run ./examples/claude-code-agent
```

## 开发

```bash
go build ./...
go vet ./...
go test ./...
```
