package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// bobTerminateGraceNanos optionally overrides, in nanoseconds, how long a
// cancelled bob process group is given to exit after SIGTERM before it is
// SIGKILLed. Set via atomic store in tests; zero keeps the default.
const bobDefaultTerminateGrace = 5 * time.Second

// bobBackend implements Backend by spawning the Bob CLI with
// `bob run --format stream-json`.
//
// Bob's stream-json protocol is a newline-delimited JSON stream where each
// line is one of the following event objects:
//
//	{"type":"message",    "role":"user"|"assistant", "content":"…"}
//	{"type":"tool_use",   "tool_name":"…", "tool_id":"…", "parameters":{…}}
//	{"type":"tool_result","tool_id":"…", "status":"success"|"error", "output":"…"}
//	{"type":"result",     "status":"success"|"error", "stats":{"task_id":"…", …}}
//
// Bob loads the per-task AGENTS.md the daemon writes into the workdir, so the
// runtime brief is not inlined (providerNeedsInlineSystemPrompt is false for
// "bob") and opts.SystemPrompt is normally empty.
//
// When `--max-cost` (passed through custom_args) stops a task, Bob emits an
// "error" event, then a "result" with status success, and exits 0. The backend
// reports that run as failed with an error starting with BobCostLimitError, so
// the daemon can tag it BobCostLimitReason instead of a finished turn.
type bobBackend struct {
	cfg Config
}

func (b *bobBackend) Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error) {
	execPath := b.cfg.ExecutablePath
	if execPath == "" {
		execPath = "bob"
	}
	if _, err := exec.LookPath(execPath); err != nil {
		return nil, fmt.Errorf("bob executable not found at %q: %w", execPath, err)
	}

	timeout := opts.Timeout
	runCtx, cancel := runContext(ctx, timeout)

	args := buildBobArgs(opts)

	// The prompt is the positional argument. A SystemPrompt, if a caller still
	// sets one, is prepended; the daemon's runtime brief reaches Bob through the
	// workdir AGENTS.md instead.
	briefAndPrompt := opts.SystemPrompt
	if prompt != "" {
		if briefAndPrompt != "" {
			briefAndPrompt = briefAndPrompt + "\n\n" + prompt
		} else {
			briefAndPrompt = prompt
		}
	}
	if briefAndPrompt != "" {
		args = append(args, briefAndPrompt)
	}

	cmd := b.cfg.commandAt(execPath).exec(runCtx, args...)
	hideAgentWindow(cmd)
	// Take over context cancellation to drive a graceful SIGTERM → SIGKILL
	// rather than the default immediate kill, matching the claude backend.
	cmd.Cancel = func() error { return nil }
	b.cfg.logAgentCommandWithPrompt(cmd, newAgentCommandLogArgs(args,
		trustAgentCommandPositional(0, "run"),
		trustAgentCommandPositional(1, "--format"),
		trustAgentCommandPositional(2, "stream-json"),
	), len(briefAndPrompt))
	cmd.WaitDelay = 10 * time.Second
	if opts.Cwd != "" {
		cmd.Dir = opts.Cwd
	}
	cmd.Env = buildEnv(b.cfg.Env)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("bob stdout pipe: %w", err)
	}
	stderrBuf := newStderrTail(newLogWriter(b.cfg.Logger, "[bob:stderr] "), agentStderrTailBytes)
	cmd.Stderr = stderrBuf

	if err := startOwnedProcessTree(cmd, b.cfg.Logger); err != nil {
		cancel()
		return nil, fmt.Errorf("start bob: %w", err)
	}

	b.cfg.Logger.Info("bob started", "pid", cmd.Process.Pid, "cwd", opts.Cwd)

	msgCh := make(chan Message, 256)
	resCh := make(chan Result, 1)

	procDone := make(chan struct{})

	go func() {
		defer cancel()
		defer close(msgCh)
		defer close(resCh)

		startTime := time.Now()
		var lastAssistantText strings.Builder
		var finalResultText string
		var sessionID string
		var sessionCostsUSD float64
		var costLimitText string
		sawResult := false
		resultIsError := false
		eventCount := 0
		invalidEventCount := 0
		toolUseCount := 0

		// Graceful shutdown: close stdout after SIGKILL so the scanner
		// unblocks even if a descendant still holds the pipe.
		var closeStdoutOnce sync.Once
		closeStdout := func() { closeStdoutOnce.Do(func() { _ = stdout.Close() }) }

		go func() {
			select {
			case <-procDone:
				return
			case <-runCtx.Done():
			}
			if cmd.Process != nil {
				signalProcessGroup(cmd, syscall.SIGTERM)
				if !waitProcessGroupGone(cmd, bobDefaultTerminateGrace) {
					signalProcessGroup(cmd, syscall.SIGKILL)
				}
			}
			closeStdout()
		}()

		scanner := newAgentStreamScanner(stdout)

		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}

			var event bobStreamEvent
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				invalidEventCount++
				continue
			}
			eventCount++

			switch event.Type {
			case "message":
				if event.Role == "assistant" && event.Content != "" && event.IsReasoning {
					// A model's thinking, which Bob streams as assistant messages marked isReasoning (a
					// backend such as vLLM that returns reasoning apart from content, Farmhouse V35). It is
					// shown as thinking and never becomes part of the output.
					trySend(msgCh, Message{Type: MessageThinking, Content: event.Content})
					break
				}
				if event.Role == "assistant" && event.Content != "" {
					lastAssistantText.WriteString(event.Content)
					trySend(msgCh, Message{Type: MessageText, Content: event.Content})
				}
			case "tool_use":
				toolUseCount++
				var input map[string]any
				if event.Parameters != nil {
					_ = json.Unmarshal(event.Parameters, &input)
				}
				trySend(msgCh, Message{
					Type:   MessageToolUse,
					Tool:   event.ToolName,
					CallID: event.ToolID,
					Input:  input,
				})
				// A tool_use turn is intermediate; clear any prior assistant
				// text so pre-tool narration never becomes the final output.
				lastAssistantText.Reset()
			case "tool_result":
				trySend(msgCh, Message{
					Type:   MessageToolResult,
					CallID: event.ToolID,
					Output: event.Output,
				})
			case "error":
				if text := bobCostLimitText(event); text != "" {
					costLimitText = text
				}
			case "result":
				sawResult = true
				resultIsError = event.Status == "error"
				if event.Stats != nil {
					// task_id is Bob's session resume handle.
					sessionID = event.Stats.TaskID
					sessionCostsUSD = event.Stats.SessionCosts
				}
				finalResultText = "" // result event carries no prose; output comes from message events
			}
		}
		scanErr := scanner.Err()
		if scanErr != nil {
			closeStdout()
		}

		exitErr := cmd.Wait()
		close(procDone)
		releaseProcessGroup(cmd)
		duration := time.Since(startTime)

		stderrTail := stderrBuf.Tail()

		finalStatus, finalOutput, finalError := finalizeStreamResult(
			"bob",
			timeout,
			runCtx.Err(),
			nil, // bob takes prompt as a positional arg, no stdin write
			exitErr,
			sessionID,
			streamTerminalState{
				lastAssistantText: lastAssistantText.String(),
				finalResultText:   finalResultText,
				sawResult:         sawResult,
				resultIsError:     resultIsError,
				scanErr:           scanErr,
			},
			"",
		)
		finalStatus, finalError = bobCostLimitResult(finalStatus, finalError, costLimitText)

		if finalError != "" {
			finalError = withAgentStderr(finalError, "bob", stderrTail)
		}

		logStreamProtocolObservation(b.cfg.Logger, streamProtocolObservation{
			provider:           "bob",
			cliVersion:         b.cfg.CLIVersion,
			exitCode:           streamProcessExitCode(exitErr),
			eventCount:         eventCount,
			invalidEventCount:  invalidEventCount,
			toolUseCount:       toolUseCount,
			sawResult:          sawResult,
			resultIsError:      resultIsError,
			resultBytes:        0,
			lastAssistantBytes: lastAssistantText.Len(),
			scannerError:       scanErr != nil,
		})

		b.cfg.Logger.Info("bob finished", "pid", cmd.Process.Pid, "status", finalStatus, "duration", duration.Round(time.Millisecond).String())

		res := Result{
			Status:     finalStatus,
			Output:     finalOutput,
			Error:      finalError,
			DurationMs: duration.Milliseconds(),
			SessionID:  sessionID,
		}
		// Bob reports a single session cost in USD rather than per-model token
		// counts. Map it onto CostUSDTicks so the analytics pipeline sees it.
		if sessionCostsUSD > 0 {
			ticks := int64(sessionCostsUSD * float64(CostUSDTicksPerUSD))
			if ticks > 0 {
				res.Usage = map[string]TokenUsage{
					"bob": {CostUSDTicks: ticks},
				}
			}
		}
		resCh <- res
	}()

	return &Session{Messages: msgCh, Result: resCh}, nil
}

// buildBobArgs assembles the argv for `bob run` up to (but not including) the
// positional prompt argument.
func buildBobArgs(opts ExecOptions) []string {
	args := []string{
		"run",
		"--format", "stream-json",
		"--accept-license",
	}
	if opts.Cwd != "" {
		args = append(args, "--workspace", opts.Cwd)
	}
	if opts.MaxTurns > 0 {
		args = append(args, "--max-turns", fmt.Sprintf("%d", opts.MaxTurns))
	}
	if opts.ResumeSessionID != "" {
		args = append(args, "--resume", opts.ResumeSessionID)
	}
	args = append(args, filterCustomArgs(opts.ExtraArgs, bobBlockedArgs, nil)...)
	args = append(args, filterCustomArgs(opts.CustomArgs, bobBlockedArgs, nil)...)
	return args
}

// bobBlockedArgs are flags the daemon hardcodes that must not be overridden by
// user-configured custom_args. Overriding these would break the daemon↔Bob
// communication protocol.
var bobBlockedArgs = map[string]blockedArgMode{
	"--format":         blockedWithValue,  // stream-json protocol
	"--accept-license": blockedStandalone, // required for headless operation
	"--workspace":      blockedWithValue,  // set by the daemon from opts.Cwd
	"--resume":         blockedWithValue,  // set by the daemon from opts.ResumeSessionID
}

// bobStreamEvent is the shared envelope for all Bob CLI stream-json events.
//
//	{"type":"message",    "timestamp":"…", "role":"user"|"assistant", "content":"…"}
//	{"type":"tool_use",   "timestamp":"…", "tool_name":"…", "tool_id":"…", "parameters":{…}}
//	{"type":"tool_result","timestamp":"…", "tool_id":"…", "status":"success"|"error", "output":"…"}
//	{"type":"result",     "timestamp":"…", "status":"success"|"error", "stats":{…}}
type bobStreamEvent struct {
	Type    string `json:"type"`
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
	// IsReasoning marks an assistant message that is the model's thinking, not its answer.
	IsReasoning bool            `json:"isReasoning,omitempty"`
	ToolName    string          `json:"tool_name,omitempty"`
	ToolID      string          `json:"tool_id,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Output      string          `json:"output,omitempty"`
	Status      string          `json:"status,omitempty"`
	Stats       *bobResultStats `json:"stats,omitempty"`
	// "error" events carry their text in one of these (content is also used).
	Message string `json:"message,omitempty"`
	Error   string `json:"error,omitempty"`
}

const (
	// BobCostLimitError starts the error of a run Bob stopped at --max-cost.
	BobCostLimitError = "bob stopped at its cost limit"
	// BobCostLimitReason is the task failure_reason for such a run.
	BobCostLimitReason = "bob_cost_limit"
)

// bobCostLimitPhrases are how Bob words its --max-cost stop: 2.0.0 says
// "Maximum cost limit reached …", 2.0.4 "The task reached the cost limit of
// 0.0010 (spent: 0.021)."
var bobCostLimitPhrases = []string{"maximum cost limit", "reached the cost limit"}

// bobCostLimitText returns the text of an "error" event that reports the
// --max-cost cap, or "".
func bobCostLimitText(event bobStreamEvent) string {
	if event.Type != "error" {
		return ""
	}
	for _, text := range []string{event.Content, event.Message, event.Error} {
		lower := strings.ToLower(text)
		for _, phrase := range bobCostLimitPhrases {
			if strings.Contains(lower, phrase) {
				return text
			}
		}
	}
	return ""
}

// bobCostLimitResult turns a run that otherwise completed into a failure when
// Bob reported its cost cap; other terminal states keep their own status.
func bobCostLimitResult(status, errMsg, costLimitText string) (string, string) {
	if costLimitText == "" || status != "completed" {
		return status, errMsg
	}
	return "failed", BobCostLimitError + ": " + costLimitText
}

// bobResultStats holds the payload of a Bob "result" event's stats field.
// task_id is Bob's session handle; pass it back as --resume on subsequent runs.
type bobResultStats struct {
	TaskID       string  `json:"task_id"`
	DurationMs   float64 `json:"duration_ms"`
	SessionCosts float64 `json:"session_costs"`
	ToolCalls    int     `json:"tool_calls"`
}
