package ollama

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/LuisRaLo/ai-squad/internal/core"
)

func TestCompleteSuccess(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		var body wireRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if body.Stream {
			t.Error("expected non-streaming request")
		}
		if body.Model != "qwen2.5-coder" {
			t.Errorf("unexpected model %q", body.Model)
		}
		w.Write([]byte(`{"message":{"role":"assistant","content":"hi"},"done":true,"done_reason":"stop","prompt_eval_count":7,"eval_count":4}`))
	}))
	defer srv.Close()

	p, err := New(Config{Name: "local", BaseURL: srv.URL, Model: "qwen2.5-coder"})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	res, err := p.Complete(context.Background(), core.CompletionRequest{
		Messages: []core.Message{{Role: core.RoleUser, Text: "hello"}},
	}, nil)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if res.Text != "hi" || res.StopReason != "stop" {
		t.Errorf("unexpected result: %+v", res)
	}
	if res.Usage.InputTokens != 7 || res.Usage.OutputTokens != 4 {
		t.Errorf("unexpected usage: %+v", res.Usage)
	}
}

func TestCompleteConnectionRefused(t *testing.T) {
	t.Parallel()

	p, err := New(Config{Name: "local", BaseURL: "http://127.0.0.1:1", Model: "m", Timeout: 500 * time.Millisecond})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	_, err = p.Complete(context.Background(), core.CompletionRequest{}, nil)
	if err == nil {
		t.Fatal("expected a connection error when nothing is listening")
	}
}

func TestCompleteModelNotFound(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":"model not found"}`))
	}))
	defer srv.Close()

	p, err := New(Config{Name: "local", BaseURL: srv.URL, Model: "ghost"})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	_, err = p.Complete(context.Background(), core.CompletionRequest{}, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if core.IsRetryable(err) {
		t.Error("a missing model must not be retryable")
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
