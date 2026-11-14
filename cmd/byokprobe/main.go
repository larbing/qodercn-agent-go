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
	setMode := false
	if len(args) > 0 && args[0] == "-set" {
		setMode = true
		args = args[1:]
	}
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: byokprobe [-set] <model-id> [prompt]")
		os.Exit(2)
	}
	model := args[0]
	prompt := "用一个词回答：你是什么模型"
	if len(args) > 1 {
		prompt = args[1]
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	opts := &qoder.Options{Auth: qoder.AccessTokenFromEnv()}
	if opts.Auth == nil {
		opts.Auth = qoder.QoderCLIAuth()
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
