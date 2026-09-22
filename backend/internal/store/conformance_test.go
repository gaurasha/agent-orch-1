package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/gaurasha/agent-orch/backend/internal/store"
	"github.com/gaurasha/agent-orch/backend/internal/types"
)

// Every test in this file runs against BOTH the in-memory store and Postgres.
// Two implementations of a contract that is this subtle (lease fencing,
// idempotency, hash chaining) will drift apart the moment they are tested
// separately, and a test that passes on the fake while the real store is broken
// is worse than no test at all.
//
// Postgres is exercised when AGENTORCH_TEST_DSN is set; otherwise that half
// skips loudly rather than silently passing.
func eachStore(t *testing.T, fn func(t *testing.T, s store.Store)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) {
		s := store.NewMem()
		defer s.Close()
		fn(t, s)
	})
	t.Run("postgres", func(t *testing.T) {
		dsn := os.Getenv("AGENTORCH_TEST_DSN")
		if dsn == "" {
			t.Skip("AGENTORCH_TEST_DSN not set; skipping Postgres conformance")
		}
		ctx := context.Background()
		pg, err := store.OpenPostgres(ctx, dsn, 8)
		if err != nil {
			t.Fatalf("open postgres: %v", err)
		}
		defer pg.Close()
		// Each subtest gets a clean slate; CASCADE handles the dependent rows.
		if _, err := pg.DB().ExecContext(ctx,
			`TRUNCATE audit_log, tool_calls, events, runs, agent_definitions, tenants CASCADE`); err != nil {
			t.Fatalf("truncate: %v", err)
		}
		fn(t, pg)
	})
}

func seedTenant(t *testing.T, s store.Store, id string) types.Tenant {
	t.Helper()
	tn := types.Tenant{ID: id, Name: id, Weight: 1, TokensPerMinute: 100000, MaxConcurrentRuns: 100}
	if err := s.PutTenant(context.Background(), tn); err != nil {
		t.Fatalf("put tenant: %v", err)
	}
	return tn
}

func seedRun(t *testing.T, s store.Store, tenantID, runID string, pri types.Priority) types.Run {
	t.Helper()
	r := types.Run{
		ID: runID, TenantID: tenantID, AgentName: "a", DefDigest: "sha256:x",
		TriggeringUser: "u@example.com", State: types.StateQueued,
		Budget: types.DefaultBudget(), Priority: pri,
	}
	ev := types.Event{Type: types.EventRunCreated, Payload: types.EventPayload{Text: "created"}}
	if err := s.CreateRun(context.Background(), r, ev); err != nil {
		t.Fatalf("create run: %v", err)
	}
	return r
}

// ---------------------------------------------------------------------------

func TestLease_OnlyOneWorkerWins(t *testing.T) {
	eachStore(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		seedTenant(t, s, "t1")
		seedRun(t, s, "t1", "run1", types.PriorityNormal)

		const workers = 16
		var wg sync.WaitGroup
		var mu sync.Mutex
		winners := []string{}
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				r, err := s.AcquireLease(ctx, fmt.Sprintf("w%d", i), 10*time.Second, nil)
				if err == nil {
					mu.Lock()
					winners = append(winners, r.LeaseOwner)
					mu.Unlock()
				} else if !errors.Is(err, types.ErrNotFound) {
					t.Errorf("unexpected acquire error: %v", err)
				}
			}(i)
		}
		wg.Wait()
		// The safety property: a run is never executed by two workers at once.
		if len(winners) != 1 {
			t.Fatalf("expected exactly 1 lease winner, got %d: %v", len(winners), winners)
		}
	})
}

func TestCommit_RejectedAfterLeaseLost(t *testing.T) {
	eachStore(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		seedTenant(t, s, "t1")
		seedRun(t, s, "t1", "run1", types.PriorityNormal)

		// w1 acquires a very short lease and then stalls (simulating a paused
		// process, a GC hang or a network partition).
		r, err := s.AcquireLease(ctx, "w1", 200*time.Millisecond, nil)
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		time.Sleep(400 * time.Millisecond)
		if _, err := s.ReapExpiredLeases(ctx); err != nil {
			t.Fatalf("reap: %v", err)
		}
		// w2 takes over.
		if _, err := s.AcquireLease(ctx, "w2", 10*time.Second, nil); err != nil {
			t.Fatalf("w2 acquire: %v", err)
		}
		// w1 finally wakes up and tries to write. It must be fenced out.
		err = s.Commit(ctx, r.ID, "w1", r.NextSeq,
			[]types.Event{{Type: types.EventNote, Payload: types.EventPayload{Text: "stale write"}}},
			store.RunUpdate{})
		if !errors.Is(err, types.ErrLeaseLost) {
			t.Fatalf("expected ErrLeaseLost for fenced worker, got %v", err)
		}
		evs, _ := s.ListEvents(ctx, r.ID, 0)
		for _, e := range evs {
			if e.Payload.Text == "stale write" {
				t.Fatal("stale write from fenced worker landed in the event log")
			}
		}
	})
}

func TestCommit_SeqMismatchIsConflict(t *testing.T) {
	eachStore(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		seedTenant(t, s, "t1")
		seedRun(t, s, "t1", "run1", types.PriorityNormal)
		r, err := s.AcquireLease(ctx, "w1", 10*time.Second, nil)
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		err = s.Commit(ctx, r.ID, "w1", r.NextSeq+99,
			[]types.Event{{Type: types.EventNote}}, store.RunUpdate{})
		if !errors.Is(err, types.ErrConflict) {
			t.Fatalf("expected ErrConflict on seq mismatch, got %v", err)
		}
	})
}

func TestLease_PriorityBeatsFIFO(t *testing.T) {
	eachStore(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		seedTenant(t, s, "t1")
		// Batch run is created FIRST, so FIFO alone would pick it.
		seedRun(t, s, "t1", "batch", types.PriorityBatch)
		time.Sleep(10 * time.Millisecond)
		seedRun(t, s, "t1", "interactive", types.PriorityInteractive)

		r, err := s.AcquireLease(ctx, "w1", 5*time.Second, nil)
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		if r.ID != "interactive" {
			t.Fatalf("expected the interactive (human-blocking) run first, got %q", r.ID)
		}
	})
}

func TestLease_RespectsEligibleTenants(t *testing.T) {
	eachStore(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		seedTenant(t, s, "noisy")
		seedTenant(t, s, "quiet")
		seedRun(t, s, "noisy", "r-noisy", types.PriorityNormal)
		time.Sleep(5 * time.Millisecond)
		seedRun(t, s, "quiet", "r-quiet", types.PriorityNormal)

		// The fairness layer has decided "noisy" is out of quota this instant.
		// Backpressure must be expressed as not-scheduling, so the quiet tenant
		// still makes progress even though the noisy run is older.
		r, err := s.AcquireLease(ctx, "w1", 5*time.Second, []string{"quiet"})
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		if r.TenantID != "quiet" {
			t.Fatalf("expected quiet tenant to be scheduled, got %s", r.TenantID)
		}
	})
}

func TestToolCall_IdempotencyPreventsSecondSideEffect(t *testing.T) {
	eachStore(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		seedTenant(t, s, "t1")
		seedRun(t, s, "t1", "run1", types.PriorityNormal)
		rec := types.ToolCallRecord{
			IdemKey: "run1:0:call0", RunID: "run1", TenantID: "t1",
			Tool: "http.post", ArgsHash: "abc",
		}
		// Many concurrent racers, exactly one may be told "you own the side effect".
		const n = 12
		var wg sync.WaitGroup
		var mu sync.Mutex
		fresh := 0
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, isFresh, err := s.BeginToolCall(ctx, rec)
				if err != nil {
					t.Errorf("begin: %v", err)
					return
				}
				if isFresh {
					mu.Lock()
					fresh++
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if fresh != 1 {
			t.Fatalf("expected exactly 1 fresh reservation, got %d", fresh)
		}
		if err := s.FinishToolCall(ctx, rec.IdemKey, types.ToolCallDone, "ok", false); err != nil {
			t.Fatalf("finish: %v", err)
		}
		// A replaying worker must now be handed the recorded result.
		got, isFresh, err := s.BeginToolCall(ctx, rec)
		if err != nil {
			t.Fatalf("replay begin: %v", err)
		}
		if isFresh {
			t.Fatal("replay was told to execute the side effect again")
		}
		if got.State != types.ToolCallDone || got.Result != "ok" {
			t.Fatalf("replay got %+v, want the recorded DONE result", got)
		}
	})
}

func TestToolCall_StuckCallsAreReapedAsAmbiguous(t *testing.T) {
	eachStore(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		seedTenant(t, s, "t1")
		seedRun(t, s, "t1", "run1", types.PriorityNormal)
		rec := types.ToolCallRecord{IdemKey: "k1", RunID: "run1", TenantID: "t1", Tool: "http.post", ArgsHash: "h"}
		if _, _, err := s.BeginToolCall(ctx, rec); err != nil {
			t.Fatalf("begin: %v", err)
		}
		time.Sleep(60 * time.Millisecond)
		n, err := s.ReapStuckToolCalls(ctx, 20*time.Millisecond)
		if err != nil {
			t.Fatalf("reap: %v", err)
		}
		if n != 1 {
			t.Fatalf("expected 1 reaped call, got %d", n)
		}
		got, _, _ := s.BeginToolCall(ctx, rec)
		if got.State != types.ToolCallFailed || !got.IsError {
			t.Fatalf("reaped call should be FAILED+error, got %+v", got)
		}
		// The agent must be told the truth: we genuinely do not know.
		if !contains(got.Result, "MAY OR MAY NOT") {
			t.Fatalf("reaped result must state the outcome is unknown, got %q", got.Result)
		}
	})
}

func TestAudit_ChainIsTamperEvident(t *testing.T) {
	eachStore(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		seedTenant(t, s, "t1")
		seedRun(t, s, "t1", "run1", types.PriorityNormal)
		for i := 0; i < 5; i++ {
			_, err := s.AppendAudit(ctx, store.AuditRecord{
				TenantID: "t1", RunID: "run1", AgentName: "a", TriggeringUser: "u",
				Tool: "exec.bash", Decision: "ALLOW",
				ArgsRedacted: map[string]any{"i": i},
			})
			if err != nil {
				t.Fatalf("append audit: %v", err)
			}
		}
		recs, err := s.ListAudit(ctx, "t1", "", 100)
		if err != nil {
			t.Fatalf("list audit: %v", err)
		}
		if len(recs) != 5 {
			t.Fatalf("want 5 audit records, got %d", len(recs))
		}
		if err := store.VerifyChain(recs); err != nil {
			t.Fatalf("freshly written chain must verify: %v", err)
		}
		// Now forge one record the way an attacker covering their tracks would:
		// rewrite the arguments of a command that was actually run.
		recs[2].ArgsRedacted = map[string]any{"i": 999}
		if err := store.VerifyChain(recs); err == nil {
			t.Fatal("tampering with an audit record went undetected")
		}
	})
}

func TestEvents_AppendOnlyOrdering(t *testing.T) {
	eachStore(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		seedTenant(t, s, "t1")
		r := seedRun(t, s, "t1", "run1", types.PriorityNormal)
		lease, err := s.AcquireLease(ctx, "w1", 10*time.Second, nil)
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		next := lease.NextSeq
		for i := 0; i < 4; i++ {
			err := s.Commit(ctx, r.ID, "w1", next, []types.Event{
				{Type: types.EventNote, Payload: types.EventPayload{Text: fmt.Sprintf("n%d", i)}},
			}, store.RunUpdate{})
			if err != nil {
				t.Fatalf("commit %d: %v", i, err)
			}
			next++
		}
		evs, err := s.ListEvents(ctx, r.ID, 0)
		if err != nil {
			t.Fatalf("list events: %v", err)
		}
		if len(evs) != 5 { // RUN_CREATED + 4 notes
			t.Fatalf("want 5 events, got %d", len(evs))
		}
		for i, e := range evs {
			if e.Seq != i {
				t.Fatalf("event %d has seq %d; the log must be densely ordered", i, e.Seq)
			}
		}
	})
}

func contains(h, n string) bool {
	return len(h) >= len(n) && (func() bool {
		for i := 0; i+len(n) <= len(h); i++ {
			if h[i:i+len(n)] == n {
				return true
			}
		}
		return false
	})()
}

// A caller that asks for more than the maximum must get the MAXIMUM, not the
// default. Returning 200 rows to a caller that asked for 2000 makes it believe
// it has seen everything, which is how a paging bug becomes silent data loss.
func TestListRuns_OverMaxLimitReturnsMaximumNotDefault(t *testing.T) {
	eachStore(t, func(t *testing.T, s store.Store) {
		ctx := context.Background()
		seedTenant(t, s, "t1")
		const n = 260 // more than the 200 default, fewer than the 1000 maximum
		for i := 0; i < n; i++ {
			seedRun(t, s, "t1", fmt.Sprintf("run%03d", i), types.PriorityNormal)
		}
		got, err := s.ListRuns(ctx, store.RunFilter{TenantID: "t1", Limit: 9999})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != n {
			t.Fatalf("asked for 9999 of %d runs and got %d; an over-max limit must clamp "+
				"to the maximum, not fall back to the default page size", n, len(got))
		}
	})
}
