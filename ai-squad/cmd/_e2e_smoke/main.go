package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/santillana/ai-squad/internal/core"
	"github.com/santillana/ai-squad/internal/runtimes/claudecode"
)

func main() {
	rt, err := claudecode.New(claudecode.Config{
		Name:    "claude",
		Command: "claude",
		Model:   "sonnet",
	})
	if err != nil {
		fmt.Println("NEW ERROR:", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var events int
	sink := core.EventSink(func(_ context.Context, ev core.Event) {
		events++
		fmt.Printf("EVENT %-16s %s\n", ev.Type, truncate(ev.Text, 80))
	})

	res, err := rt.Execute(ctx, core.RunRequest{
		Agent: &core.AgentDefinition{
			Name:  "developer",
			Model: "haiku",
			Permissions: core.Permissions{
				Filesystem: core.FSWorkspace,
				Shell:      core.ShellPolicy{Enabled: true, Commands: []string{"echo"}},
			},
		},
		SystemPrompt: "You are a terse assistant that uses the shell tool when asked.",
		Prompt:       "Run `echo adapter-shell-ok` using your shell tool and report only its output.",
	}, sink)
	if err != nil {
		fmt.Println("EXECUTE ERROR:", err)
		os.Exit(1)
	}

	fmt.Println("---")
	fmt.Println("TEXT:       ", res.Text)
	fmt.Println("SESSION:    ", res.SessionID)
	fmt.Println("STOP REASON:", res.StopReason)
	fmt.Println("MODEL:      ", res.Usage.Model)
	fmt.Println("IN TOKENS:  ", res.Usage.InputTokens)
	fmt.Println("OUT TOKENS: ", res.Usage.OutputTokens)
	fmt.Println("COST USD:   ", costStr(res.Usage.CostUSD))
	fmt.Println("DURATION:   ", res.Duration)
	fmt.Println("EVENTS SEEN:", events)
}

func costStr(v *float64) string {
	if v == nil {
		return "<unknown>"
	}
	return fmt.Sprintf("%.6f", *v)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
