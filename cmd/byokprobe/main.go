// Command byokprobe 决定性实验：验证 daemon 在 QODER_SDK_CUSTOM_BASE_URL_BYOK=1
// 下能否真正执行 BYOK 自定义 provider 的模型。两种模式：
//
//	byokprobe <model-id> [prompt]          --model 启动参数直通
//	byokprobe -set <model-id> [prompt]     长连接 SetModel 中途切换（server 链路）
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	qoder "github.com/larbing/qodercn-agent-go"
)

func main() {
	args := os.Args[1:]
	var (
		setMode   bool
		warmup    bool
		resume    string
		cwdFlag   string
		authSel   string
		modelFlag string
		setModel  string
		rest      []string
	)
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-set":
			setMode = true
		case "-warmup":
			warmup = true
		case "-resume":
			if i+1 < len(args) {
				i++
				resume = args[i]
			}
		case "-cwd":
			if i+1 < len(args) {
				i++
				cwdFlag = args[i]
			}
		case "-auth":
			if i+1 < len(args) {
				i++
				authSel = args[i]
			}
		case "-model":
			if i+1 < len(args) {
				i++
				modelFlag = args[i]
			}
		case "-setmodel":
			if i+1 < len(args) {
				i++
				setModel = args[i]
			}
		default:
			rest = append(rest, args[i])
		}
	}
	if !warmup && len(rest) < 1 {
		fmt.Fprintln(os.Stderr, "usage: byokprobe [-warmup [-resume <id>] [-cwd <dir>] [-auth pat|cli]] | [-set] <model-id> [prompt]")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	opts := &qoder.Options{}
	switch authSel {
	case "cli":
		opts.Auth = qoder.QoderCLIAuth()
	case "pat":
		opts.Auth = qoder.AccessTokenFromEnv()
	default:
		opts.Auth = qoder.AccessTokenFromEnv()
		if opts.Auth == nil {
			opts.Auth = qoder.QoderCLIAuth()
		}
	}
	if opts.Auth == nil {
		fmt.Fprintln(os.Stderr, "no auth: set QODERCN_PERSONAL_ACCESS_TOKEN or run `qoderclicn login`")
		os.Exit(2)
	}
	if resume != "" {
		opts.Resume = resume
	}
	if cwdFlag != "" {
		opts.Cwd = cwdFlag
	}
	if modelFlag != "" {
		opts.Model = modelFlag // --model 启动参数（运行配置，覆盖 transcript 恢复的模型）
	}

	model := ""
	prompt := "用一个词回答：你是什么模型"
	if len(rest) > 0 {
		model = rest[0]
	}
	if len(rest) > 1 {
		prompt = rest[1]
	}

	if warmup {
		// 探针：Connect = daemon 握手 + openSession（含 resume 的
		// loadResumeMetadata 模型验证），成功后可选 SetModel 再断开。
		// 零 LLM 调用。
		client, err := qoder.NewClient(opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "WARMUP FAILED (new client): %v\n", err)
			os.Exit(1)
		}
		if err := client.Connect(ctx); err != nil {
			_ = client.Disconnect(context.Background())
			fmt.Fprintf(os.Stderr, "WARMUP FAILED (connect): %v\n", err)
			os.Exit(1)
		}
		if msgs, err := client.ReceiveMessages(ctx); err == nil {
			// 读 init 确认起步模型（--model 是否被接受 vs 被忽略回落）
			deadline := time.Now().Add(8 * time.Second)
			for time.Now().Before(deadline) {
				mctx, mcancel := context.WithTimeout(ctx, 2*time.Second)
				msg, nerr := msgs.Next(mctx)
				mcancel()
				if nerr != nil {
					break
				}
				if sm, ok := msg.(*qoder.SystemMessage); ok && sm.Subtype == "init" {
					fmt.Fprintf(os.Stderr, "init model=%v session=%v\n", sm.Data["model"], sm.Data["session_id"])
					break
				}
			}
		}
		if setModel != "" {
			if err := client.SetModel(ctx, setModel); err != nil {
				_ = client.Disconnect(context.Background())
				fmt.Fprintf(os.Stderr, "WARMUP setmodel FAILED: %v\n", err)
				os.Exit(1)
			}
			fmt.Fprintf(os.Stderr, "WARMUP setmodel OK: %s\n", setModel)
		}
		_ = client.Disconnect(context.Background())
		fmt.Fprintln(os.Stderr, "WARMUP OK")
		return
	}

	var stream *qoder.MessageStream
	var client *qoder.Client
	var err error
	if setMode {
		// server 链路：默认模型建会话，SetModel 中途切换到自定义模型。
		client, err = qoder.NewClient(opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "new client: %v\n", err)
			os.Exit(1)
		}
		if err := client.Connect(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "connect: %v\n", err)
			os.Exit(1)
		}
		defer func() { _ = client.Disconnect(context.Background()) }()
		msgs, err := client.ReceiveMessages(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "receive setup: %v\n", err)
			os.Exit(1)
		}
		stream = msgs
		if err := client.SetModel(ctx, model); err != nil {
			fmt.Fprintf(os.Stderr, "setmodel FAILED: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "setmodel OK for %s\n", model)
		if err := client.Query(ctx, prompt); err != nil {
			fmt.Fprintf(os.Stderr, "query failed: %v\n", err)
			os.Exit(1)
		}
	} else {
		// --model 直通：Options.Model 透传 CLI 启动参数。
		opts.Model = model
		stream, err = qoder.Query(ctx, prompt, opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "query failed: %v\n", err)
			os.Exit(1)
		}
		defer stream.Close()
	}

	for {
		msg, err := stream.Next(ctx)
		if err == io.EOF {
			break
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "stream error: %v\n", err)
			os.Exit(1)
		}
		switch m := msg.(type) {
		case *qoder.SystemMessage:
			if m.Subtype == "init" {
				fmt.Fprintf(os.Stderr, "init: model=%v session=%v\n", m.Data["model"], m.Data["session_id"])
			}
		case *qoder.AssistantMessage:
			for _, block := range m.Content {
				if t, ok := block.(*qoder.TextBlock); ok && t.Text != "" {
					fmt.Printf("assistant[%s]: %s\n", m.Model, t.Text)
				}
			}
		case *qoder.ResultMessage:
			for k := range m.ModelUsage {
				fmt.Printf("result.model_usage: %s\n", k)
			}
			fmt.Printf("result: subtype=%s turns=%d error=%v text=%.160s\n",
				m.Subtype, m.NumTurns, m.IsError, m.Result)
		}
	}
}
