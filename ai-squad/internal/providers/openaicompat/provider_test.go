package openaicompat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LuisRaLo/ai-squad/internal/core"
)

func TestCompleteSuccess(t *testing.T) {
	// No t.Parallel: t.Setenv forbids it.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sekret" {
			t.Errorf("expected bearer auth, got %q", got)
		}
		var body wireRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if body.Model != "deepseek-chat" {
			t.Errorf("unexpected model %q", body.Model)
		}
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hi there"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`))
	}))
	defer srv.Close()

	t.Setenv("TEST_DEEPSEEK_KEY", "sekret")
	p, err := New(Config{Name: "deepseek", BaseURL: srv.URL, Model: "deepseek-chat", APIKeyEnv: "TEST_DEEPSEEK_KEY"})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	res, err := p.Complete(context.Background(), core.CompletionRequest{
		Messages: []core.Message{{Role: core.RoleUser, Text: "hello"}},
	}, nil)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if res.Text != "hi there" || res.StopReason != "stop" {
		t.Errorf("unexpected result: %+v", res)
	}
	if res.Usage.InputTokens != 5 || res.Usage.OutputTokens != 3 {
		t.Errorf("unexpected usage: %+v", res.Usage)
	}
}

func TestCompleteToolCallRoundTrip(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a.go\"}"}}]},"finish_reason":"tool_calls"}]}`))
	}))
	defer srv.Close()

	p, err := New(Config{Name: "x", BaseURL: srv.URL, Model: "m"})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	res, err := p.Complete(context.Background(), core.CompletionRequest{
		Messages: []core.Message{{Role: core.RoleUser, Text: "read a.go"}},
		Tools:    []core.ToolSpec{{Name: "read_file"}},
	}, nil)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Name != "read_file" {
		t.Fatalf("expected a read_file tool call, got %+v", res.ToolCalls)
	}
	if !strings.Contains(string(res.ToolCalls[0].Input), "a.go") {
		t.Errorf("tool call input lost: %s", res.ToolCalls[0].Input)
	}
}

func TestCompleteClassifiesHTTPErrors(t *testing.T) {
	t.Parallel()

	tests := map[int]core.RuntimeErrorKind{
		401: core.ErrKindAuth,
		429: core.ErrKindRateLimit,
		500: core.ErrKindTransient,
		400: core.ErrKindPermanent,
	}
	for status, want := range tests {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			w.Write([]byte(`{"error":"boom"}`))
		}))
		p, err := New(Config{Name: "x", BaseURL: srv.URL, Model: "m"})
		if err != nil {
			t.Fatalf("new: %v", err)
		}
		_, err = p.Complete(context.Background(), core.CompletionRequest{}, nil)
		srv.Close()
		if err == nil {
			t.Fatalf("status %d: expected an error", status)
		}
		var re *core.RuntimeError
		if !asRuntimeError(err, &re) || re.Kind != want {
			t.Errorf("status %d: expected kind %s, got %v", status, want, err)
		}
	}
}

func TestCompleteRespectsTimeout(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()

	p, err := New(Config{Name: "x", BaseURL: srv.URL, Model: "m", Timeout: 20 * time.Millisecond})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	_, err = p.Complete(context.Background(), core.CompletionRequest{}, nil)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
}

func TestNewRejectsMissingFields(t *testing.T) {
	t.Parallel()
	if _, err := New(Config{BaseURL: "http://x", Model: "m"}); err == nil {
		t.Error("expected an error for a missing name")
	}
	if _, err := New(Config{Name: "x", Model: "m"}); err == nil {
		t.Error("expected an error for a missing base_url")
	}
	if _, err := New(Config{Name: "x", BaseURL: "http://x"}); err == nil {
		t.Error("expected an error for a missing model")
	}
}

func asRuntimeError(err error, target **core.RuntimeError) bool {
	re, ok := err.(*core.RuntimeError)
	if !ok {
		return false
	}
	*target = re
	return true
}
