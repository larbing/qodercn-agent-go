// Command quickstart mirrors examples/quickstart/main.py from the Python
// SDK: a one-shot query that streams assistant messages and prints the
// final result.
//
// Requires qoderclicn on PATH (or QODERCLI_PATH) and authentication via
// QoderCLIAuth (local `qoderclicn login` state).
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	qoder "github.com/larbing/qodercn-agent-go"
)

func main() {
	ctx := context.Background()

	opts := &qoder.Options{
		Auth: qoder.QoderCLIAuth(),
		// Model:            "glm-4.7", // optional
		// PermissionMode:   qoder.PermissionAcceptEdits,
	}

	stream, err := qoder.Query(ctx, "使用react+vite+ts 写一个hello world页面", opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "query failed: %v\n", err)
		os.Exit(1)
	}
	defer stream.Close()

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
		case *qoder.AssistantMessage:
			for _, block := range m.Content {
				if t, ok := block.(*qoder.TextBlock); ok {
					fmt.Println(t.Text)
				}
			}
		case *qoder.ResultMessage:
			fmt.Printf("\n--- result (subtype=%s, turns=%d) ---\n", m.Subtype, m.NumTurns)
			if m.IsError {
				fmt.Fprintln(os.Stderr, "result was an error")
			} else {
				fmt.Println(m.Result)
			}
		}
	}
}
