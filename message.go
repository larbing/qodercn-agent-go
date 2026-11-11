package qoder

import (
	"encoding/json"
	"fmt"
)

// Message is implemented by every message yielded from Query or
// Client.ReceiveMessages. Concrete types: *UserMessage, *AssistantMessage,
// *SystemMessage, *ResultMessage, *StreamEvent.
type Message interface {
	messageType() string
}

// ContentBlock is an element of UserMessage / AssistantMessage content.
// Concrete types: *TextBlock, *ToolUseBlock, *ThinkingBlock,
// *ToolResultBlock, *ImageBlock.
type ContentBlock interface {
	contentBlockType() string
}

// TextBlock is a plain text segment.
type TextBlock struct{ Text string }

func (*TextBlock) contentBlockType() string { return "text" }

// ToolUseBlock represents a tool invocation requested by the model.
type ToolUseBlock struct {
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

func (*ToolUseBlock) contentBlockType() string { return "tool_use" }

// ThinkingBlock carries extended thinking output.
type ThinkingBlock struct {
	Thinking  string `json:"thinking"`
	Signature string `json:"signature,omitempty"`
}

func (*ThinkingBlock) contentBlockType() string { return "thinking" }

// ToolResultBlock is a tool result echoed inside user messages.
type ToolResultBlock struct {
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

func (*ToolResultBlock) contentBlockType() string { return "tool_result" }

// ImageBlock is a raw image content block (base64 data URL style).
type ImageBlock struct {
	Source map[string]any `json:"source"`
}

func (*ImageBlock) contentBlockType() string { return "image" }

// baseMessage carries fields present on most stream messages.
type baseMessage struct {
	UUID            string
	SessionID       string
	ParentToolUseID string
}

// UserMessage is a user message observed on the CLI output stream. Priority
// and ShouldQuery preserve scheduling metadata captured at enqueue time.
type UserMessage struct {
	baseMessage
	Content        string // plain text when available
	ContentBlocks  []ContentBlock
	ToolUseResult  map[string]any
	Priority       string
	ShouldQuery    *bool
	hasContentList bool
}

func (*UserMessage) messageType() string { return "user" }

// IsContentList reports whether content arrived as structured blocks.
func (m *UserMessage) IsContentList() bool { return m.hasContentList }

// AssistantMessage is one full model reply (per turn). Content is a list of
// TextBlock / ToolUseBlock / ThinkingBlock.
type AssistantMessage struct {
	baseMessage
	ID           string
	Model        string
	Content      []ContentBlock
	StopReason   string
	Usage        *Usage
	RequestID    string
	RawMessageOf json.RawMessage `json:"-"`
}

func (*AssistantMessage) messageType() string { return "assistant" }

// SystemMessage carries session-level events; Subtype "init" holds the
// initialization snapshot in Data.
type SystemMessage struct {
	Subtype string
	Data    map[string]any
	baseMessage
}

func (*SystemMessage) messageType() string { return "system" }

// ResultMessage is the final message of a session. Result is populated only
// when Subtype == "success".
type ResultMessage struct {
	Subtype           string
	DurationMS        int64
	DurationAPIMS     int64
	NumTurns          int
	Result            string
	SessionID         string
	TotalCostUSD      *float64
	Usage             map[string]any
	ModelUsage        map[string]any
	PermissionDenials []map[string]any
	IsError           bool
	UUID              string
}

func (*ResultMessage) messageType() string { return "result" }

// StreamEvent is a token-level incremental event, only delivered when
// Options.IncludePartialMessages is true. Event mirrors the upstream raw
// event format (message_start / content_block_delta / ...).
type StreamEvent struct {
	baseMessage
	Event map[string]any
}

func (*StreamEvent) messageType() string { return "stream_event" }

// Usage reports token consumption for a model reply.
type Usage struct {
	InputTokens              int            `json:"input_tokens"`
	CacheCreationInputTokens int            `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int            `json:"cache_read_input_tokens"`
	OutputTokens             int            `json:"output_tokens"`
	ServiceTier              string         `json:"service_tier,omitempty"`
	ServerToolUse            map[string]any `json:"server_tool_use,omitempty"`
}

// wireMessage mirrors the raw NDJSON envelope emitted by qoderclicn.
type wireMessage struct {
	Type    string  `json:"type"`
	Subtype string  `json:"subtype,omitempty"`
	UUID    string  `json:"uuid,omitempty"`
	Session string  `json:"session_id,omitempty"`
	Parent  *string `json:"parent_tool_use_id,omitempty"`

	// Nested payloads.
	Message json.RawMessage `json:"message,omitempty"`
	Event   json.RawMessage `json:"event,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`

	// result-level fields.
	DurationMS        *int64           `json:"duration_ms,omitempty"`
	DurationAPIMS     *int64           `json:"duration_api_ms,omitempty"`
	NumTurns          *int             `json:"num_turns,omitempty"`
	Result            *string          `json:"result,omitempty"`
	TotalCostUSD      *float64         `json:"total_cost_usd,omitempty"`
	IsError           *bool            `json:"is_error,omitempty"`
	Usage             map[string]any   `json:"usage,omitempty"`
	ModelUsage        map[string]any   `json:"modelUsage,omitempty"`
	PermissionDenials []map[string]any `json:"permission_denials,omitempty"`

	// user-level fields.
	ToolUseResult map[string]any `json:"tool_use_result,omitempty"`
	Priority      *string        `json:"priority,omitempty"`
	ShouldQuery   *bool          `json:"should_query,omitempty"`
}

// assistantEnvelope mirrors the Anthropic-style nested message object.
type assistantEnvelope struct {
	ID         string          `json:"id"`
	Type       string          `json:"type"`
	Role       string          `json:"role"`
	Model      string          `json:"model"`
	Content    json.RawMessage `json:"content"`
	StopReason *string         `json:"stop_reason"`
	Usage      *Usage          `json:"usage"`
	RequestID  string          `json:"request_id,omitempty"`
}

// parseMessage converts one raw CLI NDJSON line into a typed Message.
// Unrecognized but well-formed types yield nil (callers skip silently),
// matching the Python SDK's permissive behavior for forward compatibility.
func parseMessage(line []byte) (Message, error) {
	var w wireMessage
	if err := json.Unmarshal(line, &w); err != nil {
		return nil, jsonDecodeError(err, string(line))
	}
	if w.Type == "" {
		return nil, jsonDecodeError(fmt.Errorf("message missing 'type' field"), string(line))
	}

	base := baseMessage{
		UUID:      w.UUID,
		SessionID: w.Session,
	}
	if w.Parent != nil {
		base.ParentToolUseID = *w.Parent
	}

	switch w.Type {
	case "user":
		m := &UserMessage{baseMessage: base, ToolUseResult: w.ToolUseResult}
		if w.Priority != nil {
			m.Priority = *w.Priority
		}
		m.ShouldQuery = w.ShouldQuery
		blocks, plain, isList, err := decodeContent(w.Message, "user")
		if err != nil {
			return nil, err
		}
		m.Content, m.ContentBlocks, m.hasContentList = plain, blocks, isList
		return m, nil

	case "assistant":
		var env assistantEnvelope
		if len(w.Message) > 0 {
			if err := json.Unmarshal(w.Message, &env); err != nil {
				return nil, jsonDecodeError(err, string(line))
			}
		}
		blocks, _, _, err := decodeContent(w.Message, "assistant")
		if err != nil {
			return nil, err
		}
		m := &AssistantMessage{
			baseMessage: base,
			ID:          env.ID,
			Model:       env.Model,
			Content:     blocks,
			Usage:       env.Usage,
			RequestID:   env.RequestID,
		}
		if env.StopReason != nil {
			m.StopReason = *env.StopReason
		}
		return m, nil

	case "system":
		m := &SystemMessage{Subtype: w.Subtype, baseMessage: base}
		if len(w.Data) > 0 {
			if err := json.Unmarshal(w.Data, &m.Data); err != nil {
				return nil, jsonDecodeError(err, string(line))
			}
		}
		return m, nil

	case "result":
		m := &ResultMessage{
			Subtype:           w.Subtype,
			SessionID:         w.Session,
			Usage:             w.Usage,
			ModelUsage:        w.ModelUsage,
			PermissionDenials: w.PermissionDenials,
			IsError:           w.IsError != nil && *w.IsError,
			UUID:              w.UUID,
		}
		if w.DurationMS != nil {
			m.DurationMS = *w.DurationMS
		}
		if w.DurationAPIMS != nil {
			m.DurationAPIMS = *w.DurationAPIMS
		}
		if w.NumTurns != nil {
			m.NumTurns = *w.NumTurns
		}
		if w.Result != nil {
			m.Result = *w.Result
		}
		m.TotalCostUSD = w.TotalCostUSD
		return m, nil

	case "stream_event":
		m := &StreamEvent{baseMessage: base}
		if len(w.Event) > 0 {
			if err := json.Unmarshal(w.Event, &m.Event); err != nil {
				return nil, jsonDecodeError(err, string(line))
			}
		}
		return m, nil

	default:
		// Unknown/forward-compatible message type: skip.
		return nil, nil
	}
}

// decodeContent extracts content blocks and plain text from the nested
// "message" object of user/assistant messages.
func decodeContent(message json.RawMessage, role string) ([]ContentBlock, string, bool, error) {
	if len(message) == 0 {
		return nil, "", false, nil
	}
	var env struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(message, &env); err != nil {
		return nil, "", false, jsonDecodeError(err, string(message))
	}
	if len(env.Content) == 0 {
		return nil, "", false, nil
	}

	// Plain string content.
	var s string
	if err := json.Unmarshal(env.Content, &s); err == nil {
		return nil, s, false, nil
	}

	var rawBlocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`

		// tool_use
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`

		// thinking
		Thinking  string `json:"thinking"`
		Signature string `json:"signature"`

		// tool_result
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
		IsError   bool            `json:"is_error"`

		// image
		Source map[string]any `json:"source"`
	}
	if err := json.Unmarshal(env.Content, &rawBlocks); err != nil {
		return nil, "", false, jsonDecodeError(err, string(env.Content))
	}

	var blocks []ContentBlock
	for i := range rawBlocks {
		b := rawBlocks[i]
		switch b.Type {
		case "text":
			blocks = append(blocks, &TextBlock{Text: b.Text})
		case "tool_use":
			var input map[string]any
			if len(b.Input) > 0 {
				if err := json.Unmarshal(b.Input, &input); err != nil {
					input = map[string]any{"_raw": string(b.Input)}
				}
			}
			blocks = append(blocks, &ToolUseBlock{ID: b.ID, Name: b.Name, Input: input})
		case "thinking":
			blocks = append(blocks, &ThinkingBlock{Thinking: b.Thinking, Signature: b.Signature})
		case "tool_result":
			blocks = append(blocks, &ToolResultBlock{
				ToolUseID: b.ToolUseID,
				Content:   b.Content,
				IsError:   b.IsError,
			})
		case "image":
			blocks = append(blocks, &ImageBlock{Source: b.Source})
		default:
			// Unknown block: skip, like the Python SDK's forward-compatible parser.
		}
	}
	return blocks, "", true, nil
}

// textOf flattens TextBlocks out of an assistant message (convenience used
// by examples).
func textOf(m *AssistantMessage) string {
	out := ""
	for _, b := range m.Content {
		if t, ok := b.(*TextBlock); ok {
			if out != "" {
				out += "\n"
			}
			out += t.Text
		}
	}
	return out
}
