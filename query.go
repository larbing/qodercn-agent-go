package qoder

import (
	"context"
	"io"
)

// maxQueryQueue bounds the pending message buffer for one-shot Query, so a
// consumer that stops reading fails the stream instead of buffering
// without limit (multi-turn Client streams stay unbounded).
const maxQueryQueue = 512

// Query runs a one-shot conversation: spawn the CLI, perform the
// initialize handshake, send prompt and return a stream of messages. The
// final message is a *ResultMessage; Next returns io.EOF afterwards.
//
// Basic usage:
//
//	stream, err := qoder.Query(ctx, "Explain this repo", opts)
//	...
//	for {
//	    msg, err := stream.Next(ctx)
//	    if err == io.EOF { break }
//	    ...
//	}
func Query(ctx context.Context, prompt string, opts *Options) (*MessageStream, error) {
	sess, err := newSession(ctx, opts)
	if err != nil {
		return nil, err
	}
	sess.queue.limit = maxQueryQueue

	if _, err := sess.initialize(ctx); err != nil {
		sess.finish(err)
		return nil, err
	}

	out := map[string]any{
		"type":               "user",
		"message":            map[string]any{"role": "user", "content": prompt},
		"parent_tool_use_id": nil,
		"session_id":         defaultSessionID,
		"priority":           "next",
	}
	if opts != nil && opts.CustomContext != nil {
		out["custom_context"] = opts.CustomContext
	}
	if err := sess.writeUserMessage(out); err != nil {
		sess.finish(err)
		return nil, err
	}
	return &MessageStream{sess: sess, ownsSession: true}, nil
}

// MessageStream iterates messages from Query or Client.ReceiveMessages.
type MessageStream struct {
	sess *session
	// ownsSession marks one-shot Query streams: they end the session at the
	// terminal ResultMessage (the CLI then only waits for further prompts).
	ownsSession bool
	done        bool
}

// Next blocks for the next message. It returns io.EOF after the terminal
// ResultMessage, a typed *qoder.Error on stream failure, or the ctx error
// on cancellation.
func (ms *MessageStream) Next(ctx context.Context) (Message, error) {
	if ms == nil || ms.sess == nil {
		return nil, cliConnectionError("stream is closed")
	}
	msg, err := ms.sess.queue.pop(ctx)
	if err == io.EOF {
		ms.done = true
		return nil, io.EOF
	}
	if err != nil {
		ms.done = true
		return nil, err
	}
	if _, ok := msg.(*ResultMessage); ok && ms.ownsSession {
		// Terminal message of a one-shot query: end the session. The queue
		// drains buffered messages first, so Next keeps returning them and
		// only then reports io.EOF; the CLI process is released (the old
		// worker protocol keeps it alive waiting for further prompts).
		ms.sess.finish(nil)
	}
	return msg, nil
}

// Text is a convenience for the concatenated text of an assistant message.
func (ms *MessageStream) Text(msg Message) string {
	if a, ok := msg.(*AssistantMessage); ok {
		return textOf(a)
	}
	return ""
}

// Close terminates the session early (kills the CLI process). Safe to call
// multiple times and after io.EOF.
func (ms *MessageStream) Close() error {
	if ms == nil || ms.sess == nil {
		return nil
	}
	ms.sess.finish(nil)
	return nil
}
