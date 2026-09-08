// Package ollama implements core.LLMProvider against Ollama's native HTTP
// API (POST /api/chat), which differs from the OpenAI chat-completions shape
// enough to need its own provider rather than reusing openaicompat.
//
// Ollama is not installed on the machine this adapter was built on (checked
// directly: `which ollama` found nothing — see docs/architecture.md's Phase 1
// environment inspection). This implementation follows Ollama's public,
// versioned API documentation rather than a version actually exercised live,
// unlike the Claude Code adapter, which was verified against a running
// process. Treat the wire format here as correct-per-spec, not
// independently confirmed on this machine.
package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/LuisRaLo/ai-squad/internal/core"
)

// Config configures a provider instance.
type Config struct {
	Name    string
	BaseURL string
	Model   string
	Timeout time.Duration
}

// Provider is an Ollama core.LLMProvider.
type Provider struct {
	name    string
	baseURL string
	model   string
	client  *http.Client
}

// New builds a provider. It does not make a network call, so a
// not-yet-running Ollama server is only discovered on first use.
func New(cfg Config) (*Provider, error) {
	if cfg.Name == "" {
		return nil, core.Invalid("name", "must be set")
	}
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, core.Invalid("base_url", "must be set")
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, core.Invalid("model", "must be set")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	return &Provider{
		name: cfg.Name, baseURL: strings.TrimRight(cfg.BaseURL, "/"), model: cfg.Model,
		client: &http.Client{Timeout: timeout},
	}, nil
}

var _ core.LLMProvider = (*Provider)(nil)

func (p *Provider) Name() string  { return p.name }
func (p *Provider) Model() string { return p.model }

type wireMessage struct {
	Role      string         `json:"role"`
	Content   string         `json:"content"`
	ToolCalls []wireToolCall `json:"tool_calls,omitempty"`
}

type wireToolCall struct {
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type wireTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}

type wireRequest struct {
	Model    string         `json:"model"`
	Messages []wireMessage  `json:"messages"`
	Tools    []wireTool     `json:"tools,omitempty"`
	Stream   bool           `json:"stream"`
	Options  map[string]any `json:"options,omitempty"`
}

type wireResponse struct {
	Message         wireMessage `json:"message"`
	Done            bool        `json:"done"`
	DoneReason      string      `json:"done_reason"`
	PromptEvalCount int64       `json:"prompt_eval_count"`
	EvalCount       int64       `json:"eval_count"`
	Error           string      `json:"error"`
}

// Complete implements core.LLMProvider. stream is always false: Ollama's
// streaming mode returns newline-delimited partial-message JSON, which adds
// real complexity for marginal benefit until a caller actually needs
// token-by-token delivery.
func (p *Provider) Complete(ctx context.Context, req core.CompletionRequest, sink core.EventSink) (*core.CompletionResult, error) {
	messages := make([]wireMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		messages = append(messages, wireMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		wm := wireMessage{Role: string(m.Role), Content: m.Text}
		for _, tc := range m.ToolCalls {
			wtc := wireToolCall{}
			wtc.Function.Name = tc.Name
			wtc.Function.Arguments = tc.Input
			wm.ToolCalls = append(wm.ToolCalls, wtc)
		}
		messages = append(messages, wm)
	}

	var tools []wireTool
	for _, t := range req.Tools {
		wt := wireTool{Type: "function"}
		wt.Function.Name = t.Name
		wt.Function.Description = t.Description
		wt.Function.Parameters = t.InputSchema
		tools = append(tools, wt)
	}

	options := map[string]any{}
	if req.MaxTokens > 0 {
		options["num_predict"] = req.MaxTokens
	}
	if req.Temperature != nil {
		options["temperature"] = *req.Temperature
	}
	if len(req.Stop) > 0 {
		options["stop"] = req.Stop
	}

	body, err := json.Marshal(wireRequest{Model: p.model, Messages: messages, Tools: tools, Stream: false, Options: options})
	if err != nil {
		return nil, &core.RuntimeError{Runtime: p.name, Kind: core.ErrKindPermanent, Message: "encode request", Err: err}
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, &core.RuntimeError{Runtime: p.name, Kind: core.ErrKindPermanent, Message: "build request", Err: err}
	}
	httpReq.Header.Set("Content-Type", "application/json")

	sink.Emit(ctx, core.Event{Type: core.EventTypeStarted})

	resp, err := p.client.Do(httpReq)
	if err != nil {
		kind := core.ErrKindTransient
		if ctx.Err() != nil {
			kind = core.ErrKindTimeout
		}
		return nil, &core.RuntimeError{
			Runtime: p.name, Kind: kind,
			Message: "request failed (is Ollama running at " + p.baseURL + "?)", Err: err,
		}
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &core.RuntimeError{Runtime: p.name, Kind: core.ErrKindTransient, Message: "read response", Err: err}
	}

	if resp.StatusCode != http.StatusOK {
		return nil, &core.RuntimeError{
			Runtime: p.name, Kind: classifyStatus(resp.StatusCode),
			Message: fmt.Sprintf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 500)),
		}
	}

	var wr wireResponse
	if err := json.Unmarshal(raw, &wr); err != nil {
		return nil, &core.RuntimeError{Runtime: p.name, Kind: core.ErrKindPermanent, Message: "decode response", Err: err}
	}
	if wr.Error != "" {
		return nil, &core.RuntimeError{Runtime: p.name, Kind: core.ErrKindPermanent, Message: wr.Error}
	}

	result := &core.CompletionResult{
		Text:       wr.Message.Content,
		StopReason: wr.DoneReason,
		Usage: core.Usage{
			Model: p.model, InputTokens: wr.PromptEvalCount, OutputTokens: wr.EvalCount,
		},
	}
	for _, tc := range wr.Message.ToolCalls {
		result.ToolCalls = append(result.ToolCalls, core.ToolCall{
			Name: tc.Function.Name, Input: tc.Function.Arguments,
		})
	}
	sink.Emit(ctx, core.Event{Type: core.EventTypeCompleted, Text: result.StopReason})
	return result, nil
}

func classifyStatus(code int) core.RuntimeErrorKind {
	switch {
	case code == 401 || code == 403:
		return core.ErrKindAuth
	case code == 429:
		return core.ErrKindRateLimit
	case code >= 500:
		return core.ErrKindTransient
	case code == 404:
		// Ollama returns 404 for "model not found" — not retryable by
		// simply trying again.
		return core.ErrKindPermanent
	default:
		return core.ErrKindPermanent
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
