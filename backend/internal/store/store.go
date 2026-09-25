// Package store is the durability boundary.
//
// Everything that must survive a pod restart, a node drain or a deploy lives
// behind this interface. The two operations that carry the most design weight
// are AcquireLease (how a stateless worker pool safely divides work) and
// Commit (how a worker's progress is made durable atomically, or not at all).
package store

import (
	"context"
	"time"

	"github.com/gaurasha/agent-orch/backend/internal/types"
)

// RunFilter narrows a run listing. TenantID is mandatory at the API layer; the
// zero value is only used by operator tooling that has already authorized a
// cross-tenant view.
type RunFilter struct {
	TenantID string
	States   []types.RunState
	Limit    int
}

// RunUpdate is the mutation applied to a Run as part of Commit. Pointer fields
// mean "leave unchanged" when nil, so a caller never has to read-modify-write
// fields it does not care about.
type RunUpdate struct {
	State        *types.RunState
	StatusReason *string
	Step         *int
	Usage        *types.Usage
	WakeAt       **time.Time // double pointer: set to (*time.Time)(nil) to clear
	ReleaseLease bool
}

// Store is the persistence port.
type Store interface {
	// --- tenants ---
	PutTenant(ctx context.Context, t types.Tenant) error
	GetTenant(ctx context.Context, id string) (types.Tenant, error)
	ListTenants(ctx context.Context) ([]types.Tenant, error)

	// --- agent definitions (content addressed, immutable) ---
	PutDefinition(ctx context.Context, d types.AgentDefinition) error
	GetDefinition(ctx context.Context, digest string) (types.AgentDefinition, error)
	ListDefinitions(ctx context.Context, tenantID string) ([]types.AgentDefinition, error)

	// --- runs ---
	CreateRun(ctx context.Context, r types.Run, first types.Event) error
	GetRun(ctx context.Context, id string) (types.Run, error)
	ListRuns(ctx context.Context, f RunFilter) ([]types.Run, error)
	CountActiveRuns(ctx context.Context, tenantID string) (int, error)

	// AcquireLease atomically claims one schedulable run for `worker`.
	//
	// Selection honours: priority lane, then FIFO. `eligibleTenants` is the set
	// of tenants the fairness layer currently permits; passing nil means "any".
	// Returns types.ErrNotFound when nothing is runnable, which is a normal,
	// non-exceptional outcome that the worker loop treats as "sleep briefly".
	AcquireLease(ctx context.Context, worker string, ttl time.Duration, eligibleTenants []string) (types.Run, error)
	// RenewLease extends the lease. Returns ErrLeaseLost if we no longer hold it,
	// which is the signal for a worker to abandon the step immediately rather
	// than keep making side effects it can no longer journal.
	RenewLease(ctx context.Context, runID, worker string, ttl time.Duration) error
	// ReleaseLease drops the lease without changing run state.
	//
	// Use this only when the run's state has already been set to something the
	// dispatcher can act on. Dropping a lease while the run is still RUNNING
	// leaves a row that neither the dispatcher (which selects QUEUED) nor the
	// reaper (which looks for an expired lease) will ever pick up.
	ReleaseLease(ctx context.Context, runID, worker string) error
	// YieldRun puts a still-unfinished run back on the queue and drops the
	// lease, in one fenced operation. This is what a worker does when it stops
	// part-way through: shutting down, losing its lease, or hitting its
	// per-lease step budget. It is a no-op if `worker` no longer owns the run,
	// so a worker that was already replaced cannot disturb its successor.
	YieldRun(ctx context.Context, runID, worker, reason string) error
	// ReapExpiredLeases returns runs whose lease lapsed back to QUEUED. This is
	// the entire failure detector: no heartbeat service, no leader election.
	ReapExpiredLeases(ctx context.Context) (int, error)

	// --- events ---

	// Commit appends events and applies a run update in ONE transaction, but
	// only if `worker` still holds the lease and the run is still at
	// `expectedNextSeq`. This is what makes "a node died mid-step" safe: the
	// dead worker's writes are rejected, and the replacement worker's replay is
	// the only version of history that lands.
	Commit(ctx context.Context, runID, worker string, expectedNextSeq int, evs []types.Event, up RunUpdate) error
	// AppendSystemEvents writes events without holding a lease. Used by the API
	// for human input (resume, cancel, approval) where no worker is active.
	AppendSystemEvents(ctx context.Context, runID string, evs []types.Event, up RunUpdate) error
	ListEvents(ctx context.Context, runID string, sinceSeq int) ([]types.Event, error)

	// --- tool call journal (idempotency) ---

	// BeginToolCall reserves an idempotency key. If the key already exists it
	// returns the existing record and `fresh=false`; the caller must then NOT
	// execute the side effect again.
	BeginToolCall(ctx context.Context, rec types.ToolCallRecord) (existing types.ToolCallRecord, fresh bool, err error)
	FinishToolCall(ctx context.Context, idemKey string, state types.ToolCallState, result string, isErr bool) error
	// ReapStuckToolCalls fails calls left IN_FLIGHT past `olderThan`, so a
	// replaying worker eventually gets a definite (if pessimistic) answer
	// instead of blocking forever.
	ReapStuckToolCalls(ctx context.Context, olderThan time.Duration) (int, error)

	// --- audit ---
	AppendAudit(ctx context.Context, rec AuditRecord) (AuditRecord, error)
	ListAudit(ctx context.Context, tenantID string, runID string, limit int) ([]AuditRecord, error)

	Ping(ctx context.Context) error
	Close() error
}

// AuditRecord is one tamper-evident line in a tenant's audit chain.
//
// Hash = sha256(canonical(record without Hash) || PrevHash). Chaining per
// tenant rather than globally keeps appends from serialising across the whole
// fleet and makes a per-tenant export self-verifying.
type AuditRecord struct {
	TenantID       string         `json:"tenant_id"`
	Seq            int64          `json:"seq"`
	TS             time.Time      `json:"ts"`
	RunID          string         `json:"run_id"`
	AgentName      string         `json:"agent_name"`
	TriggeringUser string         `json:"triggering_user"`
	Tool           string         `json:"tool"`
	Decision       string         `json:"decision"` // ALLOW | DENY | ERROR
	Reason         string         `json:"reason,omitempty"`
	ArgsRedacted   map[string]any `json:"args_redacted,omitempty"`
	ResultMeta     map[string]any `json:"result_meta,omitempty"`
	PrevHash       string         `json:"prev_hash"`
	Hash           string         `json:"hash"`
}
