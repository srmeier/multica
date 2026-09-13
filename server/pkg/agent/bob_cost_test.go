package agent

import "testing"

func TestBobCostLimitText(t *testing.T) {
	cases := []struct {
		name  string
		event bobStreamEvent
		want  string
	}{
		{"content", bobStreamEvent{Type: "error", Content: "Maximum cost limit reached ($0.0146 of $0.01)"}, "Maximum cost limit reached ($0.0146 of $0.01)"},
		{"message field", bobStreamEvent{Type: "error", Message: "maximum cost limit reached"}, "maximum cost limit reached"},
		{"error field", bobStreamEvent{Type: "error", Error: "Maximum cost limit reached"}, "Maximum cost limit reached"},
		{"other error", bobStreamEvent{Type: "error", Content: "rate limited"}, ""},
		{"not an error event", bobStreamEvent{Type: "message", Role: "assistant", Content: "Maximum cost limit reached"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := bobCostLimitText(tc.event); got != tc.want {
				t.Errorf("bobCostLimitText = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBobCostLimitResult(t *testing.T) {
	status, errMsg := bobCostLimitResult("completed", "", "Maximum cost limit reached")
	if status != "failed" || errMsg != BobCostLimitError+": Maximum cost limit reached" {
		t.Errorf("capped run = (%q, %q), want failed with the cost-limit error", status, errMsg)
	}
	status, errMsg = bobCostLimitResult("timeout", "bob timed out after 4h0m0s", "Maximum cost limit reached")
	if status != "timeout" || errMsg != "bob timed out after 4h0m0s" {
		t.Errorf("a timeout keeps its own status, got (%q, %q)", status, errMsg)
	}
	status, errMsg = bobCostLimitResult("completed", "", "")
	if status != "completed" || errMsg != "" {
		t.Errorf("an uncapped run is unchanged, got (%q, %q)", status, errMsg)
	}
}
