package store

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/gaurasha/agent-orch/backend/internal/types"
)

// Mem is an in-memory Store.
//
// It exists so unit tests run with no infrastructure and so the load harness
// can measure scheduler behaviour without database I/O dominating the numbers.
// It implements exactly the same contract as PG - enforced by the shared
// conformance suite in conformance_test.go, which runs every test against both.
type Mem struct {
	mu     sync.Mutex
	tenant map[string]types.Tenant
	defs   map[string]types.AgentDefinition
	runs   map[string]*types.Run
	events map[string][]types.Event
	calls  map[string]*types.ToolCallRecord
	audit  map[string][]AuditRecord // per tenant chain
}

func NewMem() *Mem {
	return &Mem{
		tenant: map[string]types.Tenant{},
		defs:   map[string]types.AgentDefinition{},
		runs:   map[string]*types.Run{},
		events: map[string][]types.Event{},
		calls:  map[string]*types.ToolCallRecord{},
		audit:  map[string][]AuditRecord{},
	}
}

func (m *Mem) Ping(context.Context) error { return nil }
func (m *Mem) Close() error               { return nil }

func (m *Mem) PutTenant(_ context.Context, t types.Tenant) error {
	if err := t.Validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	m.tenant[t.ID] = t
	return nil
}

func (m *Mem) GetTenant(_ context.Context, id string) (types.Tenant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tenant[id]
	if !ok {
		return t, fmt.Errorf("tenant %s: %w", id, types.ErrNotFound)
	}
	return t, nil
}

func (m *Mem) ListTenants(context.Context) ([]types.Tenant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]types.Tenant, 0, len(m.tenant))
	for _, t := range m.tenant {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *Mem) PutDefinition(_ context.Context, d types.AgentDefinition) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.defs[d.Digest]; exists {
		return nil // content addressed: identical digest means identical content
	}
	if d.CreatedAt.IsZero() {
		d.CreatedAt = time.Now().UTC()
	}
	m.defs[d.Digest] = d
	return nil
}

func (m *Mem) GetDefinition(_ context.Context, digest string) (types.AgentDefinition, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.defs[digest]
	if !ok {
		return d, fmt.Errorf("definition %s: %w", digest, types.ErrNotFound)
	}
	return d, nil
}

func (m *Mem) ListDefinitions(_ context.Context, tenantID string) ([]types.AgentDefinition, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []types.AgentDefinition{}
	for _, d := range m.defs {
		if tenantID == "" || d.TenantID == tenantID {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (m *Mem) CreateRun(_ context.Context, r types.Run, first types.Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.runs[r.ID]; exists {
		return fmt.Errorf("run %s exists: %w", r.ID, types.ErrConflict)
	}
	now := time.Now().UTC()
	r.CreatedAt, r.UpdatedAt, r.NextSeq = now, now, 1
	cp := r
	m.runs[r.ID] = &cp
	first.RunID, first.Seq, first.CreatedAt = r.ID, 0, now
	m.events[r.ID] = []types.Event{first}
	return nil
}

func (m *Mem) GetRun(_ context.Context, id string) (types.Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	if !ok {
		return types.Run{}, fmt.Errorf("run %s: %w", id, types.ErrNotFound)
	}
	return *r, nil
}

func (m *Mem) ListRuns(_ context.Context, f RunFilter) ([]types.Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	want := map[types.RunState]bool{}
	for _, s := range f.States {
		want[s] = true
	}
	out := []types.Run{}
	for _, r := range m.runs {
		if f.TenantID != "" && r.TenantID != f.TenantID {
			continue
		}
		if len(want) > 0 && !want[r.State] {
			continue
		}
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

func (m *Mem) CountActiveRuns(_ context.Context, tenantID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, r := range m.runs {
		if r.TenantID == tenantID && (r.State == types.StateQueued || r.State == types.StateRunning) {
			n++
		}
	}
	return n, nil
}

func (m *Mem) AcquireLease(_ context.Context, worker string, ttl time.Duration, eligible []string) (types.Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	allowed := map[string]bool{}
	for _, t := range eligible {
		allowed[t] = true
	}
	var best *types.Run
	for _, r := range m.runs {
		if r.State != types.StateQueued {
			continue
		}
		if r.WakeAt != nil && r.WakeAt.After(now) {
			continue
		}
		if r.LeaseExpiresAt != nil && r.LeaseExpiresAt.After(now) {
			continue
		}
		if len(allowed) > 0 && !allowed[r.TenantID] {
			continue
		}
		if best == nil ||
			r.Priority.Rank() > best.Priority.Rank() ||
			(r.Priority.Rank() == best.Priority.Rank() && r.CreatedAt.Before(best.CreatedAt)) {
			best = r
		}
	}
	if best == nil {
		return types.Run{}, types.ErrNotFound
	}
	exp := now.Add(ttl)
	best.State, best.LeaseOwner, best.LeaseExpiresAt, best.UpdatedAt = types.StateRunning, worker, &exp, now
	return *best, nil
}

func (m *Mem) RenewLease(_ context.Context, runID, worker string, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[runID]
	if !ok || r.LeaseOwner != worker || r.LeaseExpiresAt == nil || r.LeaseExpiresAt.Before(time.Now()) {
		return types.ErrLeaseLost
	}
	exp := time.Now().UTC().Add(ttl)
	r.LeaseExpiresAt, r.UpdatedAt = &exp, time.Now().UTC()
	return nil
}

func (m *Mem) ReleaseLease(_ context.Context, runID, worker string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.runs[runID]; ok && r.LeaseOwner == worker {
		r.LeaseOwner, r.LeaseExpiresAt, r.UpdatedAt = "", nil, time.Now().UTC()
	}
	return nil
}

func (m *Mem) YieldRun(_ context.Context, runID, worker, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[runID]
	if !ok || r.LeaseOwner != worker || r.State != types.StateRunning {
		return nil // fenced out, or already finished: not ours to touch
	}
	r.State, r.LeaseOwner, r.LeaseExpiresAt = types.StateQueued, "", nil
	r.StatusReason, r.UpdatedAt = reason, time.Now().UTC()
	return nil
}

func (m *Mem) ReapExpiredLeases(context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	n := 0
	for _, r := range m.runs {
		expired := r.LeaseExpiresAt != nil && r.LeaseExpiresAt.Before(now)
		// See the Postgres implementation: an ownerless RUNNING row would
		// otherwise be invisible to both the dispatcher and the reaper.
		orphaned := r.LeaseOwner == "" && r.UpdatedAt.Before(now.Add(-60*time.Second))
		if r.State == types.StateRunning && (expired || orphaned) {
			r.State, r.LeaseOwner, r.LeaseExpiresAt = types.StateQueued, "", nil
			r.StatusReason, r.UpdatedAt = "lease expired; reclaimed", now
			n++
		}
	}
	return n, nil
}

func (m *Mem) applyLocked(r *types.Run, up RunUpdate, newSeq int) {
	r.NextSeq, r.UpdatedAt = newSeq, time.Now().UTC()
	if up.State != nil {
		r.State = *up.State
	}
	if up.StatusReason != nil {
		r.StatusReason = *up.StatusReason
	}
	if up.Step != nil {
		r.Step = *up.Step
	}
	if up.Usage != nil {
		r.Usage = *up.Usage
	}
	if up.WakeAt != nil {
		r.WakeAt = *up.WakeAt
	}
	if up.ReleaseLease {
		r.LeaseOwner, r.LeaseExpiresAt = "", nil
	}
}

func (m *Mem) Commit(_ context.Context, runID, worker string, expectedNextSeq int, evs []types.Event, up RunUpdate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[runID]
	if !ok {
		return fmt.Errorf("run %s: %w", runID, types.ErrNotFound)
	}
	if r.LeaseOwner != worker || r.LeaseExpiresAt == nil || r.LeaseExpiresAt.Before(time.Now()) {
		return fmt.Errorf("commit rejected for worker %s (owner=%q): %w", worker, r.LeaseOwner, types.ErrLeaseLost)
	}
	if r.NextSeq != expectedNextSeq {
		return fmt.Errorf("seq mismatch have=%d want=%d: %w", r.NextSeq, expectedNextSeq, types.ErrConflict)
	}
	m.appendLocked(r, evs)
	m.applyLocked(r, up, r.NextSeq+len(evs))
	return nil
}

func (m *Mem) AppendSystemEvents(_ context.Context, runID string, evs []types.Event, up RunUpdate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[runID]
	if !ok {
		return fmt.Errorf("run %s: %w", runID, types.ErrNotFound)
	}
	m.appendLocked(r, evs)
	m.applyLocked(r, up, r.NextSeq+len(evs))
	return nil
}

func (m *Mem) appendLocked(r *types.Run, evs []types.Event) {
	now := time.Now().UTC()
	for i, e := range evs {
		e.RunID, e.Seq, e.CreatedAt = r.ID, r.NextSeq+i, now
		m.events[r.ID] = append(m.events[r.ID], e)
	}
}

func (m *Mem) ListEvents(_ context.Context, runID string, sinceSeq int) ([]types.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []types.Event{}
	for _, e := range m.events[runID] {
		if e.Seq >= sinceSeq {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

func (m *Mem) BeginToolCall(_ context.Context, rec types.ToolCallRecord) (types.ToolCallRecord, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if got, ok := m.calls[rec.IdemKey]; ok {
		return *got, false, nil
	}
	rec.State, rec.StartedAt = types.ToolCallInFlight, time.Now().UTC()
	cp := rec
	m.calls[rec.IdemKey] = &cp
	return rec, true, nil
}

func (m *Mem) FinishToolCall(_ context.Context, idemKey string, state types.ToolCallState, result string, isErr bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.calls[idemKey]
	if !ok {
		return fmt.Errorf("tool call %s: %w", idemKey, types.ErrNotFound)
	}
	now := time.Now().UTC()
	c.State, c.Result, c.IsError, c.FinishedAt = state, result, isErr, &now
	return nil
}

func (m *Mem) ReapStuckToolCalls(_ context.Context, olderThan time.Duration) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cutoff := time.Now().UTC().Add(-olderThan)
	n := 0
	for _, c := range m.calls {
		if c.State == types.ToolCallInFlight && c.StartedAt.Before(cutoff) {
			now := time.Now().UTC()
			c.State, c.IsError, c.FinishedAt = types.ToolCallFailed, true, &now
			c.Result = "tool call abandoned: the gateway did not report an outcome. " +
				"This call MAY OR MAY NOT have taken effect."
			n++
		}
	}
	return n, nil
}

func (m *Mem) AppendAudit(_ context.Context, rec AuditRecord) (AuditRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	chain := m.audit[rec.TenantID]
	rec.Seq, rec.PrevHash = 1, GenesisHash
	if n := len(chain); n > 0 {
		rec.Seq, rec.PrevHash = chain[n-1].Seq+1, chain[n-1].Hash
	}
	// Truncated to microseconds to match Postgres timestamptz resolution, so
	// that a chain written here and one written there hash identically.
	rec.TS = time.Now().UTC().Truncate(time.Microsecond)
	h, err := HashAudit(rec)
	if err != nil {
		return rec, err
	}
	rec.Hash = h
	m.audit[rec.TenantID] = append(chain, rec)
	return rec, nil
}

func (m *Mem) ListAudit(_ context.Context, tenantID, runID string, limit int) ([]AuditRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 {
		limit = 500
	}
	if limit > 5000 {
		limit = 5000
	}
	tenants := []string{tenantID}
	if tenantID == "" {
		tenants = tenants[:0]
		for t := range m.audit {
			tenants = append(tenants, t)
		}
		sort.Strings(tenants)
	}
	out := []AuditRecord{}
	for _, t := range tenants {
		for _, r := range m.audit[t] {
			if runID != "" && r.RunID != runID {
				continue
			}
			out = append(out, r)
			if len(out) >= limit {
				return out, nil
			}
		}
	}
	return out, nil
}

var _ Store = (*Mem)(nil)
