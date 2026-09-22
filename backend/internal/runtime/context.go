package runtime

import (
	"fmt"
	"strings"

	"github.com/gaurasha/agent-orch/backend/internal/llm"
	"github.com/gaurasha/agent-orch/backend/internal/types"
)

// Rebuild reconstructs the model conversation from the event log.
//
// This function is the reason a worker can die at any point without losing
// work: it is a pure function from the durable log to the model's view. Two
// different workers replaying the same log produce the same messages, so
// whichever one wins the lease continues from the identical position.
//
// It is also where the system decides what the model is allowed to know. Note
// what is deliberately NOT carried across:
//
//   - internal notes (quota waits, provider retries, lease churn). Operational
//     noise in the context costs money on every subsequent turn and gives a
//     prompt-injected agent material to reason about the platform with.
//   - the identity of the worker, the idempotency keys, the audit metadata.
//   - anything a denial revealed beyond the human-readable reason.
func Rebuild(events []types.Event) []llm.Message {
	msgs := make([]llm.Message, 0, len(events))
	for _, e := range events {
		switch e.Type {
		case types.EventUserMessage, types.EventRunCreated:
			if e.Payload.Text != "" {
				msgs = append(msgs, llm.Message{Role: llm.RoleUser, Content: e.Payload.Text})
			}

		case types.EventModelResponse:
			m := llm.Message{Role: llm.RoleAssistant, Content: e.Payload.Text}
			msgs = append(msgs, m)

		case types.EventToolCall:
			// Attach the tool call to the assistant turn it belongs to, so the
			// provider sees a well-formed alternation rather than orphan
			// results.
			if n := len(msgs); n > 0 && msgs[n-1].Role == llm.RoleAssistant {
				msgs[n-1].ToolCalls = append(msgs[n-1].ToolCalls, llm.ToolCall{
					ID: e.Payload.ToolCallID, Name: e.Payload.Tool, Args: e.Payload.Args,
				})
			}

		case types.EventToolResult, types.EventToolDenied:
			msgs = append(msgs, llm.Message{
				Role:       llm.RoleTool,
				ToolCallID: e.Payload.ToolCallID,
				Content:    e.Payload.Result,
				IsError:    e.Payload.IsError,
			})

		case types.EventHumanResume:
			msgs = append(msgs, llm.Message{
				Role:    llm.RoleUser,
				Content: e.Payload.Text,
			})

		case types.EventApprovalGiven:
			msgs = append(msgs, llm.Message{
				Role:    llm.RoleUser,
				Content: fmt.Sprintf("A human approved the %s call. Proceeding.", e.Payload.Tool),
			})

		case types.EventApprovalNeeded:
			msgs = append(msgs, llm.Message{
				Role:       llm.RoleTool,
				ToolCallID: e.Payload.ToolCallID,
				Content:    "This call is held pending human approval: " + e.Payload.Reason,
			})

		case types.EventNote, types.EventLeaseLost, types.EventHumanPause, types.EventRunFinished:
			// Operator-facing only. Not shown to the model.
		}
	}
	return msgs
}

// buildSystemPrompt assembles the system prompt.
//
// The budget footer is not decoration. Telling a model how much of its budget
// remains measurably reduces the "keep trying the same thing" failure mode,
// and it costs a handful of tokens. It does NOT replace enforcement: the
// gateway refuses calls past the budget regardless of whether the model
// cooperates. Guidance for the well-behaved case, enforcement for the rest.
func buildSystemPrompt(spec types.Spec, run types.Run) string {
	var b strings.Builder
	b.WriteString(spec.SystemPrompt)
	b.WriteString("\n\n---\nPlatform context:\n")
	fmt.Fprintf(&b, "- You are running as agent %q for tenant %q, on behalf of %s.\n",
		run.AgentName, run.TenantID, run.TriggeringUser)
	b.WriteString("- Code you run executes in an isolated sandbox with no network access. " +
		"Its working directory /work is the only writable location that persists.\n")
	b.WriteString("- Credentials are injected by the platform at the point of use. " +
		"You do not have them, cannot read them, and must never ask for them.\n")

	remaining := func(used, limit int) string {
		if limit <= 0 {
			return "unlimited"
		}
		return fmt.Sprintf("%d of %d remaining", limit-used, limit)
	}
	fmt.Fprintf(&b, "- Budget: steps %s; tool calls %s; spent $%.4f of $%.2f.\n",
		remaining(run.Usage.Steps, run.Budget.MaxSteps),
		remaining(run.Usage.ToolCalls, run.Budget.MaxToolCalls),
		run.Usage.CostUSD, run.Budget.MaxCostUSD)
	b.WriteString("- If a tool call is refused, the refusal is final. " +
		"Do not retry it; explain the limitation and continue with what you can do.\n")
	return b.String()
}
