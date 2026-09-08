package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/santillana/ai-squad/internal/core"
)

// resultLine mirrors the fields observed from
// `claude -p --output-format json` (and the terminal line of
// --output-format stream-json) on the CLI version probed for this adapter
// (2.1.236). Fields absent from a given run keep their zero value; nothing
// here was invented from documentation the CLI itself did not produce.
type resultLine struct {
	Type              string            `json:"type"`
	Subtype           string            `json:"subtype"`
	IsError           bool              `json:"is_error"`
	Result            string            `json:"result"`
	SessionID         string            `json:"session_id"`
	StopReason        string            `json:"stop_reason"`
	DurationMS        int64             `json:"duration_ms"`
	TotalCostUSD      *float64          `json:"total_cost_usd"`
	APIErrorStatus    *int              `json:"api_error_status"`
	PermissionDenials []json.RawMessage `json:"permission_denials"`
	Usage             struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

// envelope is the common shape every stream-json line shares: a type tag plus
// a type-specific payload decoded separately.
type envelope struct {
	Type    string          `json:"type"`
	Subtype string          `json:"subtype"`
	Message json.RawMessage `json:"message"`
}

// assistantMessage and contentBlock follow the standard Anthropic Messages
// API content-block shape, which is stable across the Messages API and not
// specific to (or liable to silently change with) this CLI version.
type assistantMessage struct {
	Role    string         `json:"role"`
	Content []contentBlock `json:"content"`
}

type contentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// streamReader consumes Claude Code's NDJSON stream-json output, emitting a
// core.Event per line to sink as it goes, and returns the terminal "result"
// line once the stream ends.
func streamReader(ctx context.Context, r io.Reader, sink core.EventSink) (*resultLine, error) {
	scanner := bufio.NewScanner(r)
	// A plan, a diff, or a large tool result can exceed bufio's 64KB default.
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	var final *resultLine
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var env envelope
		if err := json.Unmarshal(line, &env); err != nil {
			// Not every line is guaranteed to be a clean envelope. Surface it
			// without failing the run over a single malformed line.
			sink.Emit(ctx, core.Event{Type: core.EventTypeLog, Text: redact(string(line))})
			continue
		}

		switch env.Type {
		case "system":
			handleSystemEvent(ctx, env, line, sink)
		case "assistant":
			handleAssistantEvent(ctx, env, line, sink)
		case "result":
			var res resultLine
			if err := json.Unmarshal(line, &res); err != nil {
				return final, fmt.Errorf("parse result line: %w", err)
			}
			final = &res
			sink.Emit(ctx, core.Event{Type: core.EventTypeCompleted, Text: res.StopReason, Raw: append([]byte(nil), line...)})
		default:
			sink.Emit(ctx, core.Event{Type: core.EventTypeLog, Text: redact(string(line)), Raw: append([]byte(nil), line...)})
		}
	}
	if err := scanner.Err(); err != nil {
		return final, fmt.Errorf("read output stream: %w", err)
	}
	return final, nil
}

func handleSystemEvent(ctx context.Context, env envelope, line []byte, sink core.EventSink) {
	raw := append([]byte(nil), line...)
	if env.Subtype == "thinking_tokens" {
		sink.Emit(ctx, core.Event{Type: core.EventTypeThinking, Raw: raw})
		return
	}
	sink.Emit(ctx, core.Event{Type: core.EventTypeLog, Text: "system: " + env.Subtype, Raw: raw})
}

func handleAssistantEvent(ctx context.Context, env envelope, line []byte, sink core.EventSink) {
	raw := append([]byte(nil), line...)

	var msg assistantMessage
	if err := json.Unmarshal(env.Message, &msg); err != nil {
		sink.Emit(ctx, core.Event{Type: core.EventTypeLog, Text: redact(string(line)), Raw: raw})
		return
	}
	for _, block := range msg.Content {
		switch block.Type {
		case "text":
			sink.Emit(ctx, core.Event{Type: core.EventTypeAssistantText, Text: block.Text, Raw: raw})
		case "tool_use":
			sink.Emit(ctx, core.Event{
				Type: core.EventTypeToolUse,
				Text: fmt.Sprintf("%s(%s)", block.Name, truncateForLog(string(block.Input))),
				Raw:  raw,
			})
		}
	}
}

func truncateForLog(s string) string {
	const max = 200
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
