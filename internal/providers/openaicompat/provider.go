// Package openaicompat implements core.LLMProvider against the OpenAI
// chat-completions HTTP contract, which is what DeepSeek, most hosted
// inference platforms, and many self-hosted servers speak. This provider is
// what "openai-compatible" and "deepseek" (a compatible superset) resolve
// to in configuration.
package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/LuisRaLo/ai-squad/internal/core"
)

// Config configures a provider instance.
type Config struct {
	Name    string
	BaseURL string
	Model   string
	// APIKeyEnv names the environment variable holding the bearer token.
	// The credential is read at call time only, never stored.
	APIKeyEnv string
	Timeout   time.Duration
}

// Provider is an OpenAI-chat-completions-compatible core.LLMProvider.
type Provider struct {
	name      string
	baseURL   string
	model     string
	apiKeyEnv string
	client    *http.Client
}

// New builds a provider. It does not make a network call.
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
		apiKeyEnv: cfg.APIKeyEnv, client: &http.Client{Timeout: timeout},
	}, nil
}

var _ core.LLMProvider = (*Provider)(nil)

func (p *Provider) Name() string  { return p.name }
func (p *Provider) Model() string { return p.model }

// wireMessage and wireRequest/wireResponse mirror the OpenAI chat-completions
// JSON contract: https://platform.openai.com/docs/api-reference/chat — a
// stable, publicly documented shape, unlike a CLI whose flags can change
// across versions.
type wireMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type wireToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
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
	Model       string        `json:"model"`
	Messages    []wireMessage `json:"messages"`
	Tools       []wireTool    `json:"tools,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Temperature *float64      `json:"temperature,omitempty"`
	Stop        []string      `json:"stop,omitempty"`
}

type wireResponse struct {
	Choices []struct {
		Message      wireMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Complete implements core.LLMProvider. Streaming is not implemented: the
// chat-completions SSE format differs enough from a single JSON response
// that it is left for when a caller actually needs token-by-token delivery
// (RunRequest's sink still receives a synthetic "completed" event so
// callers do not need to special-case a non-streaming provider).
func (p *Provider) Complete(ctx context.Context, req core.CompletionRequest, sink core.EventSink) (*core.CompletionResult, error) {
	messages := make([]wireMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		messages = append(messages, wireMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		wm := wireMessage{Role: string(m.Role), Content: m.Text, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			wtc := wireToolCall{ID: tc.ID, Type: "function"}
			wtc.Function.Name = tc.Name
			wtc.Function.Arguments = string(tc.Input)
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

	body, err := json.Marshal(wireRequest{
		Model: p.model, Messages: messages, Tools: tools,
		MaxTokens: req.MaxTokens, Temperature: req.Temperature, Stop: req.Stop,
	})
	if err != nil {
		return nil, &core.RuntimeError{Runtime: p.name, Kind: core.ErrKindPermanent, Message: "encode request", Err: err}
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, &core.RuntimeError{Runtime: p.name, Kind: core.ErrKindPermanent, Message: "build request", Err: err}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if p.apiKeyEnv != "" {
		if key := os.Getenv(p.apiKeyEnv); key != "" {
			httpReq.Header.Set("Authorization", "Bearer "+key)
		}
	}

	sink.Emit(ctx, core.Event{Type: core.EventTypeStarted})

	resp, err := p.client.Do(httpReq)
	if err != nil {
		kind := core.ErrKindTransient
		if ctx.Err() != nil {
			kind = core.ErrKindTimeout
			if ctxErrIsCancel(ctx) {
				kind = core.ErrKindCancelled
			}
		}
		return nil, &core.RuntimeError{Runtime: p.name, Kind: kind, Message: "request failed", Err: err}
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
	if wr.Error != nil {
		return nil, &core.RuntimeError{Runtime: p.name, Kind: core.ErrKindPermanent, Message: wr.Error.Message}
	}
	if len(wr.Choices) == 0 {
		return nil, &core.RuntimeError{Runtime: p.name, Kind: core.ErrKindPermanent, Message: "response had no choices"}
	}

	choice := wr.Choices[0]
	result := &core.CompletionResult{
		Text:       choice.Message.Content,
		StopReason: choice.FinishReason,
		Usage: core.Usage{
			Model: p.model, InputTokens: wr.Usage.PromptTokens, OutputTokens: wr.Usage.CompletionTokens,
		},
	}
	for _, tc := range choice.Message.ToolCalls {
		result.ToolCalls = append(result.ToolCalls, core.ToolCall{
			ID: tc.ID, Name: tc.Function.Name, Input: json.RawMessage(tc.Function.Arguments),
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
	default:
		return core.ErrKindPermanent
	}
}

func ctxErrIsCancel(ctx context.Context) bool {
	return ctx.Err() != nil && ctx.Err().Error() == "context canceled"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
