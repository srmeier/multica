//go:build unix

package agent

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBobReasoningIsThinkingNotOutput replays what Bob 2.0.4 streamed on a model backend that returns reasoning
// apart from content (vLLM with a reasoning parser, Farmhouse V35): the thinking arrives as assistant messages marked
// isReasoning, and must reach the task as thinking, never as its output.
func TestBobReasoningIsThinkingNotOutput(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	events := []string{
		`{"type":"message","role":"user","content":"What is 17*23?"}`,
		`{"type":"message","role":"assistant","content":"17*23 = 39","isReasoning":true}`,
		`{"type":"message","role":"assistant","content":"1. Just the number.\n","isReasoning":true}`,
		`{"type":"message","role":"assistant","content":"\n\n39"}`,
		`{"type":"message","role":"assistant","content":"1"}`,
		`{"type":"result","status":"success","stats":{"task_id":"t1","duration_ms":10,"session_costs":0.001,"tool_calls":0}}`,
	}
	var script strings.Builder
	script.WriteString("#!/bin/sh\ncat > /dev/null\n")
	for _, e := range events {
		fmt.Fprintf(&script, "printf '%%s\\n' '%s'\n", e)
	}
	fake := filepath.Join(dir, "bob")
	writeTestExecutable(t, fake, []byte(script.String()))

	backend, err := New("bob", Config{ExecutablePath: fake, Logger: slog.Default()})
	if err != nil {
		t.Fatalf("New(bob): %v", err)
	}
	session, err := backend.Execute(t.Context(), "What is 17*23?", ExecOptions{Timeout: 30 * time.Second, Cwd: dir})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var thinking, text strings.Builder
	done := make(chan struct{})
	go func() {
		defer close(done)
		for m := range session.Messages {
			switch m.Type {
			case MessageThinking:
				thinking.WriteString(m.Content)
			case MessageText:
				text.WriteString(m.Content)
			}
		}
	}()
	result := <-session.Result
	<-done

	if result.Status != "completed" {
		t.Fatalf("status %q (%s)", result.Status, result.Error)
	}
	if got := strings.TrimSpace(result.Output); got != "391" {
		t.Fatalf("output %q, want only the answer", result.Output)
	}
	if strings.Contains(text.String(), "Just the number") {
		t.Fatalf("thinking reached the text stream: %q", text.String())
	}
	if !strings.Contains(thinking.String(), "Just the number") {
		t.Fatalf("thinking wasn't sent as thinking: %q", thinking.String())
	}
}
