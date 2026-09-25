// Package llm abstracts the model provider.
//
// The provider is external, rate limited and the single largest cost line, so
// this package exists mainly to make that resource governable: one interface,
// one place where tokens are counted and priced, one place where retries and
// backoff live. Every model call in the system goes through Gateway.Complete.
package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gaurasha/agent-orch/backend/internal/tools"
)

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

type ToolCall struct {
	ID   string         `json:"id"`
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`
	// ToolCalls is set on assistant turns that request tools.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID links a tool result back to its request.
	ToolCallID string `json:"tool_call_id,omitempty"`
	IsError    bool   `json:"is_error,omitempty"`
}

type Request struct {
	Model    string
	System   string
	Messages []Message
	// Tools carries ONLY the model-facing schemas. tools.Schema has no
	// backend fields, so a credential reference or an internal URL template
	// cannot reach the provider even by accident.
	Tools     []tools.Schema
	MaxTokens int
}

type Response struct {
	Content      string
	ToolCalls    []ToolCall
	StopReason   string // end_turn | tool_use | max_tokens
	InputTokens  int64
	OutputTokens int64
	CostUSD      float64
	Model        string
	// Latency is recorded so an operator can distinguish "the agent is slow"
	// from "the provider is slow", which lead to completely different actions.
	Latency time.Duration
}

type Provider interface {
	Name() string
	Complete(ctx context.Context, req Request) (Response, error)
}

var (
	// ErrRateLimited means the provider pushed back. It is retryable.
	ErrRateLimited = errors.New("llm: provider rate limited")
	// ErrOverloaded means the provider is degraded. Retryable with backoff.
	ErrOverloaded = errors.New("llm: provider overloaded")
	// ErrBadRequest is permanent; retrying will not help.
	ErrBadRequest = errors.New("llm: bad request")
)

// Pricing is USD per million tokens. Keeping this in code rather than a config
// file is deliberate for the PoC: an operator asking "what has this agent cost"
// must get a number that came from somewhere auditable.
type Pricing struct{ InputPerMTok, OutputPerMTok float64 }

var priceTable = map[string]Pricing{
	"claude-opus-5":    {15.00, 75.00},
	"claude-sonnet-5":  {3.00, 15.00},
	"claude-haiku-4-5": {0.80, 4.00},
	"fake-small":       {0.25, 1.25},
	"fake-large":       {3.00, 15.00},
}

// Price returns the cost of a call. An unknown model is priced at the most
// expensive known rate rather than zero: under-reporting spend is the failure
// mode that actually hurts, because nobody investigates a cheap-looking agent.
func Price(model string, in, out int64) float64 {
	p, ok := priceTable[strings.TrimPrefix(model, "fake:")]
	if !ok {
		p = Pricing{15.00, 75.00}
	}
	return (float64(in)*p.InputPerMTok + float64(out)*p.OutputPerMTok) / 1_000_000
}

// EstimateTokens is a cheap pre-call estimate used for quota reservation.
//
// It is intentionally crude - roughly four characters per token - because it
// only has to be approximately right: the fairness layer reserves this much,
// then settles against the provider's reported usage afterwards. Getting the
// estimate wrong costs a little scheduling accuracy for one call, not
// correctness. Being able to reserve BEFORE the call is what prevents a tenant
// from blowing past its quota with a single enormous request.
func EstimateTokens(req Request) int64 {
	n := len(req.System)
	for _, m := range req.Messages {
		n += len(m.Content) + 16
		for _, tc := range m.ToolCalls {
			n += len(tc.Name) + 32
			for k, v := range tc.Args {
				n += len(k) + len(fmt.Sprint(v))
			}
		}
	}
	for _, t := range req.Tools {
		n += len(t.Name) + len(t.Description)
		for pn, p := range t.Parameters {
			n += len(pn) + len(p.Description) + 16
		}
	}
	est := int64(n/4) + int64(req.MaxTokens)
	if est < 64 {
		est = 64
	}
	return est
}
