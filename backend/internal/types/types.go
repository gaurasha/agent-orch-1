// Package types holds the core domain model.
//
// The single most important design statement in this system is here: a running
// agent is NOT a process and NOT a pod. It is a Run row plus an append-only
// Event log. A worker leases a Run, advances it one step, and lets go. That is
// what makes an agent survive a pod restart, a node drain and a deploy, and
// what makes an agent that waits two days for a human cost essentially nothing.
package types

import (
	"errors"
	"fmt"
	"time"
)

// ---------------------------------------------------------------------------
// Tenancy
// ---------------------------------------------------------------------------

// Tenant is the isolation unit. Every row, credential, sandbox and audit record
// in the system is owned by exactly one tenant, and every store query is
// tenant-scoped. Cross-tenant access is not a permission we can grant; there is
// no code path that takes two tenant ids.
type Tenant struct {
	ID string `json:"id"`
	// Name is a human label only; never used for authorization.
	Name string `json:"name"`
	// Weight drives the fair share of scarce LLM quota. A tenant with weight 2
	// gets twice the throughput of a tenant with weight 1 *when both are
	// contending*; an idle tenant's share is redistributed.
	Weight int `json:"weight"`
	// TokensPerMinute is this tenant's slice of provider quota.
	TokensPerMinute int64 `json:"tokens_per_minute"`
	// MaxConcurrentRuns bounds blast radius from a runaway tenant.
	MaxConcurrentRuns int       `json:"max_concurrent_runs"`
	CreatedAt         time.Time `json:"created_at"`
}

func (t Tenant) Validate() error {
	if t.ID == "" {
		return errors.New("tenant: id required")
	}
	if t.Weight <= 0 {
		return errors.New("tenant: weight must be > 0")
	}
	if t.TokensPerMinute <= 0 {
		return errors.New("tenant: tokens_per_minute must be > 0")
	}
	if t.MaxConcurrentRuns <= 0 {
		return errors.New("tenant: max_concurrent_runs must be > 0")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Agent definitions
// ---------------------------------------------------------------------------

// AgentDefinition is the versioned, immutable description of an agent. It is
// content-addressed: Digest is the sha256 of the canonical spec. A Run pins a
// digest, so redefining an agent never changes the behaviour of runs already in
// flight, and an auditor can always reconstruct exactly what an agent was
// allowed to do at the time it ran. This is the container-image model applied
// to agents.
type AgentDefinition struct {
	Digest   string `json:"digest"`
	TenantID string `json:"tenant_id"`
	Name     string `json:"name"`
	Spec     Spec   `json:"spec"`

	CreatedAt time.Time `json:"created_at"`
}

// Spec is the hashed part of an agent definition.
type Spec struct {
	SystemPrompt string `json:"system_prompt"`
	Model        string `json:"model"`
	// Tools is the set of tool names this agent may call. This is a grant, not
	// a hint: the tool gateway rejects anything outside it, so an agent that is
	// prompt-injected into calling `http.post` simply cannot.
	Tools []string `json:"tools"`
	// ToolParams carries per-tool parameter policy, e.g. a domain allowlist for
	// http.get. Keyed by tool name.
	ToolParams map[string]ParamPolicy `json:"tool_params,omitempty"`
	Budget     Budget                 `json:"budget"`
	// Priority selects the scheduling lane. Human-blocking work jumps ahead of
	// batch work because a person is literally waiting for it.
	Priority Priority `json:"priority"`
}

// ParamPolicy constrains the arguments an agent may pass to a tool. Tool-level
// grants alone are too coarse: "may call http.get" must not mean "may GET
// anything", or the first prompt injection exfiltrates the workspace.
type ParamPolicy struct {
	// AllowedHosts is an exact-or-suffix host allowlist ("api.github.com",
	// ".internal.example.com"). Empty means no host restriction is applied by
	// this layer (the egress proxy still applies its own).
	AllowedHosts []string `json:"allowed_hosts,omitempty"`
	// AllowedCommands restricts exec-style tools to specific argv[0] values.
	AllowedCommands []string `json:"allowed_commands,omitempty"`
	// DeniedArgPatterns are substrings that must not appear in any argument.
	// Used for things like blocking `gh auth token`.
	DeniedArgPatterns []string `json:"denied_arg_patterns,omitempty"`
	// RequiresApproval forces the run to pause for a human before the call
	// executes. This is the containment mechanism for high-blast-radius tools.
	RequiresApproval bool `json:"requires_approval,omitempty"`
}

// Budget bounds a single run. Every field is enforced, not merely reported:
// the gateway refuses calls once a budget is exhausted. This is the primary
// defence against a poison agent that loops on tool calls.
type Budget struct {
	MaxSteps       int     `json:"max_steps"`
	MaxToolCalls   int     `json:"max_tool_calls"`
	MaxTokens      int64   `json:"max_tokens"`
	MaxCostUSD     float64 `json:"max_cost_usd"`
	MaxWallSeconds int     `json:"max_wall_seconds"`
}

// Usage is the consumed counterpart of Budget.
type Usage struct {
	Steps        int     `json:"steps"`
	ToolCalls    int     `json:"tool_calls"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
	SandboxMS    int64   `json:"sandbox_ms"`
}

func (u Usage) TotalTokens() int64 { return u.InputTokens + u.OutputTokens }

// ExceedsReason returns a non-empty explanation if usage has hit the budget.
func (b Budget) ExceedsReason(u Usage, elapsed time.Duration) string {
	switch {
	case b.MaxSteps > 0 && u.Steps >= b.MaxSteps:
		return fmt.Sprintf("step budget exhausted (%d/%d)", u.Steps, b.MaxSteps)
	case b.MaxToolCalls > 0 && u.ToolCalls >= b.MaxToolCalls:
		return fmt.Sprintf("tool-call budget exhausted (%d/%d)", u.ToolCalls, b.MaxToolCalls)
	case b.MaxTokens > 0 && u.TotalTokens() >= b.MaxTokens:
		return fmt.Sprintf("token budget exhausted (%d/%d)", u.TotalTokens(), b.MaxTokens)
	case b.MaxCostUSD > 0 && u.CostUSD >= b.MaxCostUSD:
		return fmt.Sprintf("cost budget exhausted ($%.4f/$%.4f)", u.CostUSD, b.MaxCostUSD)
	case b.MaxWallSeconds > 0 && elapsed > time.Duration(b.MaxWallSeconds)*time.Second:
		return fmt.Sprintf("wall-clock budget exhausted (%s/%ds)", elapsed.Truncate(time.Second), b.MaxWallSeconds)
	}
	return ""
}

// DefaultBudget is applied when an agent definition omits limits. Defaulting to
// "unlimited" would make an omitted field a production incident, so the safe
// default is a small, finite budget.
func DefaultBudget() Budget {
	return Budget{MaxSteps: 25, MaxToolCalls: 50, MaxTokens: 200_000, MaxCostUSD: 5.0, MaxWallSeconds: 3600}
}

type Priority string

const (
	// PriorityInteractive is for agents a human is actively waiting on.
	PriorityInteractive Priority = "interactive"
	// PriorityNormal is the default lane.
	PriorityNormal Priority = "normal"
	// PriorityBatch is for background work that may be starved under load.
	PriorityBatch Priority = "batch"
)

func (p Priority) Rank() int {
	switch p {
	case PriorityInteractive:
		return 2
	case PriorityBatch:
		return 0
	default:
		return 1
	}
}

func (s Spec) Validate() error {
	if s.Model == "" {
		return errors.New("spec: model required")
	}
	if s.SystemPrompt == "" {
		return errors.New("spec: system_prompt required")
	}
	for _, t := range s.Tools {
		if t == "" {
			return errors.New("spec: empty tool name")
		}
	}
	switch s.Priority {
	case "", PriorityInteractive, PriorityNormal, PriorityBatch:
	default:
		return fmt.Errorf("spec: unknown priority %q", s.Priority)
	}
	return nil
}

// Grants reports whether the definition permits calling the named tool.
func (s Spec) Grants(tool string) bool {
	for _, t := range s.Tools {
		if t == tool {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Runs
// ---------------------------------------------------------------------------

type RunState string

const (
	// StateQueued: runnable, waiting for a worker or for quota.
	StateQueued RunState = "QUEUED"
	// StateRunning: a worker currently holds a lease.
	StateRunning RunState = "RUNNING"
	// StateWaitingHuman: parked on human input. Holds no worker, no sandbox and
	// no quota. This is the state that makes a two-day wait cost ~nothing.
	StateWaitingHuman RunState = "WAITING_HUMAN"
	// StateWaitingApproval: a tool call needs human sign-off before it runs.
	StateWaitingApproval RunState = "WAITING_APPROVAL"
	StateSucceeded       RunState = "SUCCEEDED"
	StateFailed          RunState = "FAILED"
	StateCancelled       RunState = "CANCELLED"
)

func (s RunState) Terminal() bool {
	return s == StateSucceeded || s == StateFailed || s == StateCancelled
}

// Schedulable reports whether the dispatcher may hand this state to a worker.
func (s RunState) Schedulable() bool { return s == StateQueued }

// Run is the durable identity of an agent at runtime.
//
// Note what is NOT here: no pod name, no node, no process id, no connection.
// A Run is not bound to any compute. Whichever worker happens to win the lease
// reconstructs everything it needs from the event log.
type Run struct {
	ID        string `json:"id"`
	TenantID  string `json:"tenant_id"`
	AgentName string `json:"agent_name"`
	DefDigest string `json:"def_digest"`
	// TriggeringUser is the human on whose behalf this run acts. It rides every
	// tool call into the audit log so "show me every command agent X ran last
	// Tuesday" can also answer "and who asked for it".
	TriggeringUser string `json:"triggering_user"`

	State        RunState `json:"state"`
	StatusReason string   `json:"status_reason,omitempty"`
	// NextSeq is the sequence number the next appended event will take. It
	// doubles as an optimistic-concurrency version for the run.
	NextSeq  int      `json:"next_seq"`
	Step     int      `json:"step"`
	Budget   Budget   `json:"budget"`
	Usage    Usage    `json:"usage"`
	Priority Priority `json:"priority"`

	// LeaseOwner/LeaseExpiresAt implement at-most-one-active-worker. A worker
	// that dies simply stops renewing; the lease lapses and the run is picked
	// up again. There is no failure detector to get wrong.
	LeaseOwner     string     `json:"lease_owner,omitempty"`
	LeaseExpiresAt *time.Time `json:"lease_expires_at,omitempty"`

	// WakeAt parks a run until a deadline (retry backoff, human timeout).
	WakeAt *time.Time `json:"wake_at,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (r Run) Elapsed() time.Duration { return time.Since(r.CreatedAt) }

// ---------------------------------------------------------------------------
// Events: the append-only log that IS the agent
// ---------------------------------------------------------------------------

type EventType string

const (
	EventRunCreated     EventType = "RUN_CREATED"
	EventUserMessage    EventType = "USER_MESSAGE"
	EventModelRequest   EventType = "MODEL_REQUEST"
	EventModelResponse  EventType = "MODEL_RESPONSE"
	EventToolCall       EventType = "TOOL_CALL"
	EventToolResult     EventType = "TOOL_RESULT"
	EventToolDenied     EventType = "TOOL_DENIED"
	EventApprovalNeeded EventType = "APPROVAL_NEEDED"
	EventApprovalGiven  EventType = "APPROVAL_GIVEN"
	EventHumanPause     EventType = "HUMAN_PAUSE"
	EventHumanResume    EventType = "HUMAN_RESUME"
	EventLeaseLost      EventType = "LEASE_LOST"
	EventRunFinished    EventType = "RUN_FINISHED"
	EventNote           EventType = "NOTE"
)

// Event is one immutable fact about a run. Events are never updated or deleted.
// Rebuilding model context, rendering the UI timeline and satisfying an audit
// request are all the same operation: read the log.
type Event struct {
	RunID     string       `json:"run_id"`
	Seq       int          `json:"seq"`
	Type      EventType    `json:"type"`
	Payload   EventPayload `json:"payload"`
	CreatedAt time.Time    `json:"created_at"`
}

// EventPayload is a union. Only the fields relevant to Type are set; this keeps
// the log self-describing without an interface{} soup that is painful to query
// from SQL or render in a UI.
type EventPayload struct {
	Text string `json:"text,omitempty"`

	// Model turn
	Model        string  `json:"model,omitempty"`
	InputTokens  int64   `json:"input_tokens,omitempty"`
	OutputTokens int64   `json:"output_tokens,omitempty"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
	StopReason   string  `json:"stop_reason,omitempty"`

	// Tool turn
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Tool       string         `json:"tool,omitempty"`
	Args       map[string]any `json:"args,omitempty"`
	IdemKey    string         `json:"idem_key,omitempty"`
	Result     string         `json:"result,omitempty"`
	IsError    bool           `json:"is_error,omitempty"`
	DurationMS int64          `json:"duration_ms,omitempty"`
	Decision   string         `json:"decision,omitempty"`
	Reason     string         `json:"reason,omitempty"`

	// Bookkeeping
	Worker string `json:"worker,omitempty"`
}

// ---------------------------------------------------------------------------
// Tool calls (the idempotency journal)
// ---------------------------------------------------------------------------

type ToolCallState string

const (
	// ToolCallInFlight means we told the outside world to do something and do
	// not yet know whether it happened. Replay must NOT re-issue it.
	ToolCallInFlight ToolCallState = "IN_FLIGHT"
	ToolCallDone     ToolCallState = "DONE"
	ToolCallFailed   ToolCallState = "FAILED"
	ToolCallDenied   ToolCallState = "DENIED"
)

// ToolCallRecord is the journal entry that makes replay safe. The key is
// deterministic (run/step/call-index), so a worker replaying after a crash
// produces the same key and gets the recorded result instead of a second
// side effect.
type ToolCallRecord struct {
	IdemKey    string        `json:"idem_key"`
	RunID      string        `json:"run_id"`
	TenantID   string        `json:"tenant_id"`
	Tool       string        `json:"tool"`
	ArgsHash   string        `json:"args_hash"`
	State      ToolCallState `json:"state"`
	Result     string        `json:"result,omitempty"`
	IsError    bool          `json:"is_error,omitempty"`
	StartedAt  time.Time     `json:"started_at"`
	FinishedAt *time.Time    `json:"finished_at,omitempty"`
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

var (
	ErrNotFound      = errors.New("not found")
	ErrConflict      = errors.New("conflict")
	ErrLeaseLost     = errors.New("lease lost")
	ErrInFlight      = errors.New("tool call in flight")
	ErrQuotaExceeded = errors.New("quota exceeded")
	ErrDenied        = errors.New("denied by policy")
)
