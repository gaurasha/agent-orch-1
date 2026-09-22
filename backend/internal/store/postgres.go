package store

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gaurasha/agent-orch/backend/internal/canon"
	"github.com/gaurasha/agent-orch/backend/internal/types"
	"github.com/lib/pq"
)

//go:embed schema.sql
var schemaSQL string

// PG is the Postgres-backed Store.
//
// Why Postgres and not a workflow engine (Temporal) or a log (Kafka): see
// DEEP_DIVE.md "D4 - durable execution substrate". Short version: we need
// transactional co-location of the event log, the run state, the idempotency
// journal and the audit chain. One database that can commit all four together
// removes an entire class of split-brain bug, and `FOR UPDATE SKIP LOCKED`
// gives us a work queue with exactly the semantics we want for free.
type PG struct{ db *sql.DB }

func OpenPostgres(ctx context.Context, dsn string, maxConns int) (*PG, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	if maxConns <= 0 {
		maxConns = 20
	}
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
	db.SetConnMaxLifetime(30 * time.Minute)

	// Postgres may still be starting (compose, kind). Retry briefly rather than
	// crash-looping the pod and burning restart backoff.
	var pingErr error
	for i := 0; i < 30; i++ {
		if pingErr = db.PingContext(ctx); pingErr == nil {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if pingErr != nil {
		return nil, fmt.Errorf("store: ping after retries: %w", pingErr)
	}
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	return &PG{db: db}, nil
}

func (p *PG) Ping(ctx context.Context) error { return p.db.PingContext(ctx) }
func (p *PG) Close() error                   { return p.db.Close() }
func (p *PG) DB() *sql.DB                    { return p.db }

// tx runs fn inside a transaction, rolling back on error or panic.
func (p *PG) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	t, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer func() {
		if r := recover(); r != nil {
			_ = t.Rollback()
			panic(r)
		}
	}()
	if err := fn(t); err != nil {
		_ = t.Rollback()
		return err
	}
	return t.Commit()
}

// ---------------------------------------------------------------------------
// Tenants
// ---------------------------------------------------------------------------

func (p *PG) PutTenant(ctx context.Context, t types.Tenant) error {
	if err := t.Validate(); err != nil {
		return err
	}
	_, err := p.db.ExecContext(ctx, `
		INSERT INTO tenants (id, name, weight, tokens_per_minute, max_concurrent_runs)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (id) DO UPDATE SET
			name=EXCLUDED.name, weight=EXCLUDED.weight,
			tokens_per_minute=EXCLUDED.tokens_per_minute,
			max_concurrent_runs=EXCLUDED.max_concurrent_runs`,
		t.ID, t.Name, t.Weight, t.TokensPerMinute, t.MaxConcurrentRuns)
	if err != nil {
		return fmt.Errorf("store: put tenant %s: %w", t.ID, err)
	}
	return nil
}

func (p *PG) GetTenant(ctx context.Context, id string) (types.Tenant, error) {
	var t types.Tenant
	err := p.db.QueryRowContext(ctx,
		`SELECT id,name,weight,tokens_per_minute,max_concurrent_runs,created_at FROM tenants WHERE id=$1`, id).
		Scan(&t.ID, &t.Name, &t.Weight, &t.TokensPerMinute, &t.MaxConcurrentRuns, &t.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return t, fmt.Errorf("tenant %s: %w", id, types.ErrNotFound)
	}
	if err != nil {
		return t, fmt.Errorf("store: get tenant: %w", err)
	}
	return t, nil
}

func (p *PG) ListTenants(ctx context.Context) ([]types.Tenant, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT id,name,weight,tokens_per_minute,max_concurrent_runs,created_at FROM tenants ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list tenants: %w", err)
	}
	defer rows.Close()
	out := []types.Tenant{}
	for rows.Next() {
		var t types.Tenant
		if err := rows.Scan(&t.ID, &t.Name, &t.Weight, &t.TokensPerMinute, &t.MaxConcurrentRuns, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Definitions
// ---------------------------------------------------------------------------

func (p *PG) PutDefinition(ctx context.Context, d types.AgentDefinition) error {
	spec, err := json.Marshal(d.Spec)
	if err != nil {
		return err
	}
	// ON CONFLICT DO NOTHING: the digest IS the content, so a second write of
	// the same digest is by definition a no-op. Attempting to overwrite would
	// mean a hash collision, not an update.
	_, err = p.db.ExecContext(ctx, `
		INSERT INTO agent_definitions (digest, tenant_id, name, spec)
		VALUES ($1,$2,$3,$4) ON CONFLICT (digest) DO NOTHING`,
		d.Digest, d.TenantID, d.Name, spec)
	if err != nil {
		return fmt.Errorf("store: put definition: %w", err)
	}
	return nil
}

func (p *PG) GetDefinition(ctx context.Context, digest string) (types.AgentDefinition, error) {
	var d types.AgentDefinition
	var spec []byte
	err := p.db.QueryRowContext(ctx,
		`SELECT digest,tenant_id,name,spec,created_at FROM agent_definitions WHERE digest=$1`, digest).
		Scan(&d.Digest, &d.TenantID, &d.Name, &spec, &d.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return d, fmt.Errorf("definition %s: %w", digest, types.ErrNotFound)
	}
	if err != nil {
		return d, fmt.Errorf("store: get definition: %w", err)
	}
	if err := json.Unmarshal(spec, &d.Spec); err != nil {
		return d, fmt.Errorf("store: decode spec: %w", err)
	}
	return d, nil
}

func (p *PG) ListDefinitions(ctx context.Context, tenantID string) ([]types.AgentDefinition, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT digest,tenant_id,name,spec,created_at FROM agent_definitions
		 WHERE ($1='' OR tenant_id=$1) ORDER BY created_at DESC`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("store: list definitions: %w", err)
	}
	defer rows.Close()
	out := []types.AgentDefinition{}
	for rows.Next() {
		var d types.AgentDefinition
		var spec []byte
		if err := rows.Scan(&d.Digest, &d.TenantID, &d.Name, &spec, &d.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(spec, &d.Spec); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Runs
// ---------------------------------------------------------------------------

const runCols = `id,tenant_id,agent_name,def_digest,triggering_user,state,status_reason,
	next_seq,step,priority,budget,usage,lease_owner,lease_expires_at,wake_at,created_at,updated_at`

func scanRun(sc interface{ Scan(...any) error }) (types.Run, error) {
	var r types.Run
	var budget, usage []byte
	var leaseOwner sql.NullString
	var leaseExp, wakeAt sql.NullTime
	err := sc.Scan(&r.ID, &r.TenantID, &r.AgentName, &r.DefDigest, &r.TriggeringUser,
		&r.State, &r.StatusReason, &r.NextSeq, &r.Step, &r.Priority, &budget, &usage,
		&leaseOwner, &leaseExp, &wakeAt, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(budget, &r.Budget); err != nil {
		return r, fmt.Errorf("decode budget: %w", err)
	}
	if err := json.Unmarshal(usage, &r.Usage); err != nil {
		return r, fmt.Errorf("decode usage: %w", err)
	}
	r.LeaseOwner = leaseOwner.String
	if leaseExp.Valid {
		t := leaseExp.Time
		r.LeaseExpiresAt = &t
	}
	if wakeAt.Valid {
		t := wakeAt.Time
		r.WakeAt = &t
	}
	return r, nil
}

func (p *PG) CreateRun(ctx context.Context, r types.Run, first types.Event) error {
	budget, _ := json.Marshal(r.Budget)
	usage, _ := json.Marshal(r.Usage)
	payload, err := json.Marshal(first.Payload)
	if err != nil {
		return err
	}
	return p.tx(ctx, func(t *sql.Tx) error {
		if _, err := t.ExecContext(ctx, `
			INSERT INTO runs (id,tenant_id,agent_name,def_digest,triggering_user,state,
				status_reason,next_seq,step,priority,budget,usage,created_at,updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,'',1,0,$7,$8,$9,now(),now())`,
			r.ID, r.TenantID, r.AgentName, r.DefDigest, r.TriggeringUser, r.State,
			string(r.Priority), budget, usage); err != nil {
			return fmt.Errorf("store: insert run: %w", err)
		}
		if _, err := t.ExecContext(ctx,
			`INSERT INTO events (run_id,seq,type,payload) VALUES ($1,0,$2,$3)`,
			r.ID, first.Type, payload); err != nil {
			return fmt.Errorf("store: insert first event: %w", err)
		}
		return nil
	})
}

func (p *PG) GetRun(ctx context.Context, id string) (types.Run, error) {
	r, err := scanRun(p.db.QueryRowContext(ctx, `SELECT `+runCols+` FROM runs WHERE id=$1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return r, fmt.Errorf("run %s: %w", id, types.ErrNotFound)
	}
	if err != nil {
		return r, fmt.Errorf("store: get run: %w", err)
	}
	return r, nil
}

func (p *PG) ListRuns(ctx context.Context, f RunFilter) ([]types.Run, error) {
	// Clamp to the maximum, not to the default. Silently shrinking a request
	// for 2000 down to 200 makes a caller believe it has seen everything when
	// it has seen a fifth - which is exactly how a paging bug hides.
	const (
		defaultRunLimit = 200
		maxRunLimit     = 1000
	)
	if f.Limit <= 0 {
		f.Limit = defaultRunLimit
	}
	if f.Limit > maxRunLimit {
		f.Limit = maxRunLimit
	}
	states := make([]string, 0, len(f.States))
	for _, s := range f.States {
		states = append(states, string(s))
	}
	rows, err := p.db.QueryContext(ctx, `SELECT `+runCols+` FROM runs
		WHERE ($1='' OR tenant_id=$1)
		  AND ($2::text[] IS NULL OR cardinality($2::text[])=0 OR state = ANY($2))
		ORDER BY created_at DESC LIMIT $3`,
		f.TenantID, pq.Array(states), f.Limit)
	if err != nil {
		return nil, fmt.Errorf("store: list runs: %w", err)
	}
	defer rows.Close()
	out := []types.Run{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (p *PG) CountActiveRuns(ctx context.Context, tenantID string) (int, error) {
	var n int
	err := p.db.QueryRowContext(ctx,
		`SELECT count(*) FROM runs WHERE tenant_id=$1 AND state IN ('QUEUED','RUNNING')`, tenantID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count active runs: %w", err)
	}
	return n, nil
}

// AcquireLease is the heart of the scheduling model.
//
// `FOR UPDATE SKIP LOCKED` lets N workers poll the same table concurrently
// without any of them blocking each other and without a broker: each worker
// takes a different row. This is the standard Postgres work-queue pattern
// (see https://www.postgresql.org/docs/current/sql-select.html#SQL-FOR-UPDATE-SHARE).
//
// The WHERE clause also enforces the fairness decision made upstream: the
// dispatcher passes only tenants that currently have LLM quota, so a tenant
// that has blown its budget is simply not selected. Backpressure is expressed
// as *not scheduling*, never as a blocked worker - that is what stops one
// noisy tenant from consuming the worker pool.
func (p *PG) AcquireLease(ctx context.Context, worker string, ttl time.Duration, eligibleTenants []string) (types.Run, error) {
	var r types.Run
	err := p.tx(ctx, func(t *sql.Tx) error {
		var id string
		err := t.QueryRowContext(ctx, `
			SELECT id FROM runs
			WHERE state='QUEUED'
			  AND (wake_at IS NULL OR wake_at <= now())
			  AND (lease_expires_at IS NULL OR lease_expires_at < now())
			  AND ($1::text[] IS NULL OR cardinality($1::text[])=0 OR tenant_id = ANY($1))
			ORDER BY CASE priority WHEN 'interactive' THEN 2 WHEN 'normal' THEN 1 ELSE 0 END DESC,
			         created_at ASC
			FOR UPDATE SKIP LOCKED
			LIMIT 1`, pq.Array(eligibleTenants)).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return types.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: select for lease: %w", err)
		}
		r, err = scanRun(t.QueryRowContext(ctx, `
			UPDATE runs SET state='RUNNING', lease_owner=$2,
				lease_expires_at=now() + ($3 || ' milliseconds')::interval,
				updated_at=now()
			WHERE id=$1 RETURNING `+runCols, id, worker, ttl.Milliseconds()))
		if err != nil {
			return fmt.Errorf("store: claim lease: %w", err)
		}
		return nil
	})
	return r, err
}

func (p *PG) RenewLease(ctx context.Context, runID, worker string, ttl time.Duration) error {
	res, err := p.db.ExecContext(ctx, `
		UPDATE runs SET lease_expires_at = now() + ($3 || ' milliseconds')::interval, updated_at=now()
		WHERE id=$1 AND lease_owner=$2 AND lease_expires_at > now()`,
		runID, worker, ttl.Milliseconds())
	if err != nil {
		return fmt.Errorf("store: renew lease: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return types.ErrLeaseLost
	}
	return nil
}

func (p *PG) ReleaseLease(ctx context.Context, runID, worker string) error {
	_, err := p.db.ExecContext(ctx,
		`UPDATE runs SET lease_owner=NULL, lease_expires_at=NULL, updated_at=now()
		 WHERE id=$1 AND lease_owner=$2`, runID, worker)
	if err != nil {
		return fmt.Errorf("store: release lease: %w", err)
	}
	return nil
}

func (p *PG) YieldRun(ctx context.Context, runID, worker, reason string) error {
	// The lease_owner predicate is the fence: if another worker already took
	// this run over, this statement matches nothing and we leave it alone.
	_, err := p.db.ExecContext(ctx, `
		UPDATE runs
		SET state='QUEUED', lease_owner=NULL, lease_expires_at=NULL,
		    status_reason=$3, updated_at=now()
		WHERE id=$1 AND lease_owner=$2 AND state='RUNNING'`, runID, worker, reason)
	if err != nil {
		return fmt.Errorf("store: yield run: %w", err)
	}
	return nil
}

// ReapExpiredLeases moves abandoned RUNNING rows back to QUEUED.
//
// This is the whole failure detector for worker death: no heartbeat service,
// no leader election, no membership protocol. A worker that dies stops
// renewing and its work becomes available again after at most one TTL.
func (p *PG) ReapExpiredLeases(ctx context.Context) (int, error) {
	res, err := p.db.ExecContext(ctx, `
		UPDATE runs SET state='QUEUED', lease_owner=NULL, lease_expires_at=NULL,
			status_reason='lease expired; reclaimed', updated_at=now()
		WHERE state='RUNNING'
		  AND (
		        -- the normal case: a worker stopped renewing
		        (lease_expires_at IS NOT NULL AND lease_expires_at < now())
		        -- belt and braces: RUNNING with no owner at all. No correct code
		        -- path produces this, but if one ever does the run would be
		        -- invisible to both the dispatcher and this reaper forever, so
		        -- we reclaim it rather than let it stall silently.
		     OR (lease_owner IS NULL AND updated_at < now() - interval '60 seconds')
		  )`)
	if err != nil {
		return 0, fmt.Errorf("store: reap leases: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ---------------------------------------------------------------------------
// Events
// ---------------------------------------------------------------------------

func applyRunUpdate(ctx context.Context, t *sql.Tx, runID string, up RunUpdate, newSeq int) error {
	sets := []string{"next_seq=$2", "updated_at=now()"}
	args := []any{runID, newSeq}
	add := func(frag string, v any) {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf(frag, len(args)))
	}
	if up.State != nil {
		add("state=$%d", string(*up.State))
	}
	if up.StatusReason != nil {
		add("status_reason=$%d", *up.StatusReason)
	}
	if up.Step != nil {
		add("step=$%d", *up.Step)
	}
	if up.Usage != nil {
		b, err := json.Marshal(*up.Usage)
		if err != nil {
			return err
		}
		add("usage=$%d", b)
	}
	if up.WakeAt != nil {
		if *up.WakeAt == nil {
			sets = append(sets, "wake_at=NULL")
		} else {
			add("wake_at=$%d", **up.WakeAt)
		}
	}
	if up.ReleaseLease {
		sets = append(sets, "lease_owner=NULL", "lease_expires_at=NULL")
	}
	q := "UPDATE runs SET " + strings.Join(sets, ",") + " WHERE id=$1"
	if _, err := t.ExecContext(ctx, q, args...); err != nil {
		return fmt.Errorf("store: apply run update: %w", err)
	}
	return nil
}

func insertEvents(ctx context.Context, t *sql.Tx, runID string, startSeq int, evs []types.Event) error {
	for i, e := range evs {
		payload, err := json.Marshal(e.Payload)
		if err != nil {
			return fmt.Errorf("store: marshal event payload: %w", err)
		}
		if _, err := t.ExecContext(ctx,
			`INSERT INTO events (run_id,seq,type,payload) VALUES ($1,$2,$3,$4)`,
			runID, startSeq+i, e.Type, payload); err != nil {
			return fmt.Errorf("store: insert event seq=%d: %w", startSeq+i, err)
		}
	}
	return nil
}

// Commit is the fenced write. Three conditions must all hold, checked inside
// the transaction against a locked row:
//
//	1. the run still exists
//	2. `worker` still owns an unexpired lease
//	3. next_seq is exactly what the worker thinks it is
//
// If any fails we return ErrLeaseLost and the worker abandons its step. This is
// how a partitioned or paused worker is prevented from writing stale history
// over a replacement worker's progress: the classic fencing-token argument
// (Kleppmann, "How to do distributed locking").
func (p *PG) Commit(ctx context.Context, runID, worker string, expectedNextSeq int, evs []types.Event, up RunUpdate) error {
	return p.tx(ctx, func(t *sql.Tx) error {
		var seq int
		var owner sql.NullString
		var exp sql.NullTime
		err := t.QueryRowContext(ctx,
			`SELECT next_seq, lease_owner, lease_expires_at FROM runs WHERE id=$1 FOR UPDATE`, runID).
			Scan(&seq, &owner, &exp)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("run %s: %w", runID, types.ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("store: lock run: %w", err)
		}
		if owner.String != worker || !exp.Valid || exp.Time.Before(time.Now()) {
			return fmt.Errorf("commit rejected for worker %s (owner=%q): %w", worker, owner.String, types.ErrLeaseLost)
		}
		if seq != expectedNextSeq {
			return fmt.Errorf("store: seq mismatch have=%d want=%d: %w", seq, expectedNextSeq, types.ErrConflict)
		}
		if err := insertEvents(ctx, t, runID, seq, evs); err != nil {
			return err
		}
		return applyRunUpdate(ctx, t, runID, up, seq+len(evs))
	})
}

// AppendSystemEvents writes on behalf of a human action (resume, cancel,
// approve) where no worker holds a lease. It does not take a fencing token
// because the API layer is the only writer and the run is, by construction,
// parked.
func (p *PG) AppendSystemEvents(ctx context.Context, runID string, evs []types.Event, up RunUpdate) error {
	return p.tx(ctx, func(t *sql.Tx) error {
		var seq int
		err := t.QueryRowContext(ctx, `SELECT next_seq FROM runs WHERE id=$1 FOR UPDATE`, runID).Scan(&seq)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("run %s: %w", runID, types.ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("store: lock run: %w", err)
		}
		if err := insertEvents(ctx, t, runID, seq, evs); err != nil {
			return err
		}
		return applyRunUpdate(ctx, t, runID, up, seq+len(evs))
	})
}

func (p *PG) ListEvents(ctx context.Context, runID string, sinceSeq int) ([]types.Event, error) {
	rows, err := p.db.QueryContext(ctx,
		`SELECT run_id,seq,type,payload,created_at FROM events
		 WHERE run_id=$1 AND seq >= $2 ORDER BY seq ASC`, runID, sinceSeq)
	if err != nil {
		return nil, fmt.Errorf("store: list events: %w", err)
	}
	defer rows.Close()
	out := []types.Event{}
	for rows.Next() {
		var e types.Event
		var payload []byte
		if err := rows.Scan(&e.RunID, &e.Seq, &e.Type, &payload, &e.CreatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(payload, &e.Payload); err != nil {
			return nil, fmt.Errorf("store: decode payload seq=%d: %w", e.Seq, err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Tool call journal
// ---------------------------------------------------------------------------

// BeginToolCall reserves an idempotency key, or returns the existing record.
//
// The INSERT ... ON CONFLICT DO NOTHING + re-SELECT pattern makes the reserve
// atomic under concurrency: exactly one caller sees fresh=true, and everyone
// else gets the journal entry. That is what turns "at least once delivery of a
// replayed step" into "at most one side effect".
func (p *PG) BeginToolCall(ctx context.Context, rec types.ToolCallRecord) (types.ToolCallRecord, bool, error) {
	res, err := p.db.ExecContext(ctx, `
		INSERT INTO tool_calls (idem_key,run_id,tenant_id,tool,args_hash,state)
		VALUES ($1,$2,$3,$4,$5,'IN_FLIGHT') ON CONFLICT (idem_key) DO NOTHING`,
		rec.IdemKey, rec.RunID, rec.TenantID, rec.Tool, rec.ArgsHash)
	if err != nil {
		return types.ToolCallRecord{}, false, fmt.Errorf("store: begin tool call: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 1 {
		rec.State = types.ToolCallInFlight
		rec.StartedAt = time.Now().UTC()
		return rec, true, nil
	}
	var got types.ToolCallRecord
	var fin sql.NullTime
	err = p.db.QueryRowContext(ctx, `
		SELECT idem_key,run_id,tenant_id,tool,args_hash,state,result,is_error,started_at,finished_at
		FROM tool_calls WHERE idem_key=$1`, rec.IdemKey).
		Scan(&got.IdemKey, &got.RunID, &got.TenantID, &got.Tool, &got.ArgsHash,
			&got.State, &got.Result, &got.IsError, &got.StartedAt, &fin)
	if err != nil {
		return got, false, fmt.Errorf("store: read existing tool call: %w", err)
	}
	if fin.Valid {
		t := fin.Time
		got.FinishedAt = &t
	}
	return got, false, nil
}

func (p *PG) FinishToolCall(ctx context.Context, idemKey string, state types.ToolCallState, result string, isErr bool) error {
	_, err := p.db.ExecContext(ctx,
		`UPDATE tool_calls SET state=$2,result=$3,is_error=$4,finished_at=now() WHERE idem_key=$1`,
		idemKey, state, result, isErr)
	if err != nil {
		return fmt.Errorf("store: finish tool call: %w", err)
	}
	return nil
}

func (p *PG) ReapStuckToolCalls(ctx context.Context, olderThan time.Duration) (int, error) {
	res, err := p.db.ExecContext(ctx, `
		UPDATE tool_calls
		SET state='FAILED', is_error=true, finished_at=now(),
		    result='tool call abandoned: the gateway did not report an outcome. '
		         ||'This call MAY OR MAY NOT have taken effect.'
		WHERE state='IN_FLIGHT' AND started_at < now() - ($1 || ' milliseconds')::interval`,
		olderThan.Milliseconds())
	if err != nil {
		return 0, fmt.Errorf("store: reap stuck tool calls: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ---------------------------------------------------------------------------
// Audit chain
// ---------------------------------------------------------------------------

// AppendAudit adds a record to the tenant's hash chain.
//
// The whole chain for a tenant is serialised by locking the chain head inside
// the transaction. That costs us throughput per tenant (fine: at 10k tool
// calls/min spread over many tenants each chain sees a small fraction) and
// buys tamper evidence: removing or editing any record breaks every hash after
// it. See audit.VerifyChain.
func (p *PG) AppendAudit(ctx context.Context, rec AuditRecord) (AuditRecord, error) {
	err := p.tx(ctx, func(t *sql.Tx) error {
		var prevSeq sql.NullInt64
		var prevHash sql.NullString
		// Lock the tenant's tail row so concurrent appends serialise behind us.
		err := t.QueryRowContext(ctx,
			`SELECT seq, hash FROM audit_log WHERE tenant_id=$1 ORDER BY seq DESC LIMIT 1 FOR UPDATE`,
			rec.TenantID).Scan(&prevSeq, &prevHash)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("store: read audit head: %w", err)
		}
		rec.Seq = prevSeq.Int64 + 1
		rec.PrevHash = prevHash.String
		if rec.PrevHash == "" {
			rec.PrevHash = GenesisHash
		}
		// Postgres timestamptz has microsecond resolution. Hashing nanoseconds
		// that the column will truncate makes every chain fail to re-verify
		// after a round trip, so we truncate before hashing and storing.
		rec.TS = time.Now().UTC().Truncate(time.Microsecond)
		h, err := HashAudit(rec)
		if err != nil {
			return err
		}
		rec.Hash = h
		args, _ := json.Marshal(rec.ArgsRedacted)
		meta, _ := json.Marshal(rec.ResultMeta)
		_, err = t.ExecContext(ctx, `
			INSERT INTO audit_log (tenant_id,seq,ts,run_id,agent_name,triggering_user,tool,
				decision,reason,args_redacted,result_meta,prev_hash,hash)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
			rec.TenantID, rec.Seq, rec.TS, rec.RunID, rec.AgentName, rec.TriggeringUser,
			rec.Tool, rec.Decision, rec.Reason, args, meta, rec.PrevHash, rec.Hash)
		if err != nil {
			return fmt.Errorf("store: insert audit: %w", err)
		}
		return nil
	})
	return rec, err
}

func (p *PG) ListAudit(ctx context.Context, tenantID, runID string, limit int) ([]AuditRecord, error) {
	const (
		defaultAuditLimit = 500
		maxAuditLimit     = 5000
	)
	if limit <= 0 {
		limit = defaultAuditLimit
	}
	if limit > maxAuditLimit {
		limit = maxAuditLimit
	}
	rows, err := p.db.QueryContext(ctx, `
		SELECT tenant_id,seq,ts,run_id,agent_name,triggering_user,tool,decision,reason,
		       args_redacted,result_meta,prev_hash,hash
		FROM audit_log
		WHERE ($1='' OR tenant_id=$1) AND ($2='' OR run_id=$2)
		ORDER BY tenant_id, seq ASC LIMIT $3`, tenantID, runID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list audit: %w", err)
	}
	defer rows.Close()
	out := []AuditRecord{}
	for rows.Next() {
		var r AuditRecord
		var args, meta []byte
		if err := rows.Scan(&r.TenantID, &r.Seq, &r.TS, &r.RunID, &r.AgentName, &r.TriggeringUser,
			&r.Tool, &r.Decision, &r.Reason, &args, &meta, &r.PrevHash, &r.Hash); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(args, &r.ArgsRedacted)
		_ = json.Unmarshal(meta, &r.ResultMeta)
		out = append(out, r)
	}
	return out, rows.Err()
}

// GenesisHash anchors every tenant's chain so that "the first record" is still
// verifiable rather than trivially forgeable.
const GenesisHash = "genesis"

// HashAudit computes the chain hash for a record. Hash and TS-as-stored are
// excluded/included deliberately: TS is part of the hash (so backdating breaks
// the chain), Hash obviously is not.
func HashAudit(r AuditRecord) (string, error) {
	payload := map[string]any{
		"tenant_id": r.TenantID, "seq": r.Seq, "ts": r.TS.UTC().Format(time.RFC3339Nano),
		"run_id": r.RunID, "agent_name": r.AgentName, "triggering_user": r.TriggeringUser,
		"tool": r.Tool, "decision": r.Decision, "reason": r.Reason,
		"args_redacted": r.ArgsRedacted, "result_meta": r.ResultMeta, "prev_hash": r.PrevHash,
	}
	b, err := canon.Bytes(payload)
	if err != nil {
		return "", err
	}
	return canon.HashHex(b), nil
}

var _ Store = (*PG)(nil)
