// Package durability_test proves the claim that makes the whole architecture
// work: "an agent mid-task survives a pod restart, a node drain and a deploy".
//
// Killing a worker in these tests is not graceful. We cancel its context with
// no drain, exactly as a SIGKILL from a node eviction would, in the middle of
// a step. The run must then be picked up by a different worker, replayed from
// the event log, and completed - with no duplicated side effect.
package durability_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gaurasha/agent-orch/backend/internal/canon"
	"github.com/gaurasha/agent-orch/backend/internal/fairness"
	"github.com/gaurasha/agent-orch/backend/internal/id"
	"github.com/gaurasha/agent-orch/backend/internal/llm"
	"github.com/gaurasha/agent-orch/backend/internal/obs"
	"github.com/gaurasha/agent-orch/backend/internal/runtime"
	"github.com/gaurasha/agent-orch/backend/internal/store"
	"github.com/gaurasha/agent-orch/backend/internal/testsupport"
	"github.com/gaurasha/agent-orch/backend/internal/tools"
	"github.com/gaurasha/agent-orch/backend/internal/types"
)

// harness wires a minimal platform with a controllable tool caller.
type harness struct {
	store    store.Store
	model    *llm.Gateway
	registry *tools.Registry
	limiter  *fairness.Limiter
	metrics  *obs.Metrics
	log      *obs.Logger
	tools    *countingToolCaller
}

// countingToolCaller records every tool invocation and, crucially, implements
// the same idempotency contract the real gateway does: a repeated key returns
// the stored result WITHOUT performing the side effect again.
type countingToolCaller struct {
	mu sync.Mutex
	// sideEffects counts actual executions, not requests. This is the number
	// that must not grow when a worker is killed and the step replays.
	sideEffects map[string]int
	journal     map[string]string
	// blockOn causes the caller to hang on a given tool, so a test can kill a
	// worker at a precise moment.
	blockOn  string
	blocked  chan struct{}
	released chan struct{}
	calls    atomic.Int64
}

func newCountingToolCaller() *countingToolCaller {
	return &countingToolCaller{
		sideEffects: map[string]int{},
		journal:     map[string]string{},
		blocked:     make(chan struct{}, 8),
		released:    make(chan struct{}),
	}
}

func (c *countingToolCaller) Call(ctx context.Context, runID string, req runtime.ToolRequest) (runtime.ToolResponse, error) {
	c.calls.Add(1)

	c.mu.Lock()
	if prior, ok := c.journal[req.IdemKey]; ok {
		c.mu.Unlock()
		// The replay path: return the recorded result, do NOT re-execute.
		return runtime.ToolResponse{Result: prior, Decision: "ALLOW", Replayed: true}, nil
	}
	shouldBlock := c.blockOn != "" && req.Tool == c.blockOn
	c.mu.Unlock()

	if shouldBlock {
		select {
		case c.blocked <- struct{}{}:
		default:
		}
		select {
		case <-c.released:
		case <-ctx.Done():
			// The worker was killed while we were mid-call. From the
			// platform's point of view the outcome is UNKNOWN, which is
			// exactly the ambiguous case the design has to handle.
			return runtime.ToolResponse{}, ctx.Err()
		case <-time.After(30 * time.Second):
			return runtime.ToolResponse{}, fmt.Errorf("test tool caller timed out")
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.sideEffects[req.IdemKey]++
	result := fmt.Sprintf("executed %s (idem=%s)", req.Tool, req.IdemKey)
	c.journal[req.IdemKey] = result
	return runtime.ToolResponse{Result: result, Decision: "ALLOW"}, nil
}

func (c *countingToolCaller) totalSideEffects() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, v := range c.sideEffects {
		n += v
	}
	return n
}

func (c *countingToolCaller) duplicated() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for k, v := range c.sideEffects {
		if v > 1 {
			out = append(out, fmt.Sprintf("%s executed %d times", k, v))
		}
	}
	return out
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	var st store.Store
	dsn, err := testsupport.SchemaDSN("test_durability")
	if err != nil {
		t.Fatalf("prepare test schema: %v", err)
	}
	if dsn != "" {
		pg, err := store.OpenPostgres(context.Background(), dsn, 12)
		if err != nil {
			t.Fatalf("open postgres: %v", err)
		}
		if err := testsupport.TruncateAll(pg.DB()); err != nil {
			t.Fatalf("%v", err)
		}
		st = pg
	} else {
		st = store.NewMem()
	}
	t.Cleanup(func() { st.Close() })

	log := obs.NewLogger("durability-test", "error")
	metrics := obs.NewMetrics()
	limiter := fairness.New(fairness.Config{ProviderTokensPerMinute: 100_000_000, BurstSeconds: 60})
	tenant := types.Tenant{ID: "t1", Name: "t1", Weight: 1, TokensPerMinute: 10_000_000, MaxConcurrentRuns: 1000}
	if err := st.PutTenant(context.Background(), tenant); err != nil {
		t.Fatalf("put tenant: %v", err)
	}
	limiter.SetTenants([]types.Tenant{tenant})

	return &harness{
		store: st, registry: tools.DefaultRegistry(""),
		limiter: limiter, metrics: metrics, log: log,
		model: llm.NewGateway(llm.NewFakeProvider(10*time.Millisecond, 0), limiter, metrics, log),
		tools: newCountingToolCaller(),
	}
}

func (h *harness) startWorker(ctx context.Context, name string) {
	w := runtime.NewWorker(runtime.Config{
		WorkerID: name,
		// Short lease so a killed worker's run becomes available quickly and
		// the test does not take a minute.
		LeaseTTL:          2 * time.Second,
		HeartbeatInterval: 500 * time.Millisecond,
		MaxStepsPerLease:  8,
		PollInterval:      50 * time.Millisecond,
	}, h.store, h.model, h.tools, h.registry, h.limiter.Eligible, h.metrics, h.log)
	go w.Run(ctx)
}

func (h *harness) startReaper(ctx context.Context) {
	go runtime.NewReaper(h.store, 300*time.Millisecond, 3*time.Second, h.metrics, h.log).Run(ctx)
}

func (h *harness) seedAgent(t *testing.T, model string, tools []string) string {
	t.Helper()
	spec := types.Spec{
		SystemPrompt: "durability test agent", Model: model, Tools: tools,
		Budget:   types.Budget{MaxSteps: 30, MaxToolCalls: 40, MaxTokens: 10_000_000, MaxCostUSD: 1000, MaxWallSeconds: 600},
		Priority: types.PriorityNormal,
	}
	digest, err := canon.Digest(spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.store.PutDefinition(context.Background(), types.AgentDefinition{
		Digest: digest, TenantID: "t1", Name: "dt", Spec: spec,
	}); err != nil {
		t.Fatal(err)
	}
	return digest
}

func (h *harness) createRun(t *testing.T, digest string) string {
	t.Helper()
	run := types.Run{
		ID: id.New("run"), TenantID: "t1", AgentName: "dt", DefDigest: digest,
		TriggeringUser: "test@example.com", State: types.StateQueued,
		Budget:   types.Budget{MaxSteps: 30, MaxToolCalls: 40, MaxTokens: 10_000_000, MaxCostUSD: 1000, MaxWallSeconds: 600},
		Priority: types.PriorityNormal,
	}
	if err := h.store.CreateRun(context.Background(), run, types.Event{
		Type: types.EventRunCreated, Payload: types.EventPayload{Text: "go"},
	}); err != nil {
		t.Fatal(err)
	}
	return run.ID
}

func (h *harness) awaitState(t *testing.T, runID string, timeout time.Duration, want ...types.RunState) types.Run {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		r, err := h.store.GetRun(context.Background(), runID)
		if err == nil {
			for _, w := range want {
				if r.State == w {
					return r
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	r, _ := h.store.GetRun(context.Background(), runID)
	evs, _ := h.store.ListEvents(context.Background(), runID, 0)
	t.Fatalf("run %s did not reach %v within %s (state=%s reason=%q, %d events)",
		runID, want, timeout, r.State, r.StatusReason, len(evs))
	return r
}

// ---------------------------------------------------------------------------

// The headline durability test: kill the worker mid-tool-call, with no drain.
func TestDurability_WorkerKilledMidToolCall_RunCompletesExactlyOnce(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.startReaper(ctx)

	// Block the first fs.write so we can kill the worker while it is inside a
	// tool call - the worst possible moment.
	h.tools.blockOn = "fs.write"

	digest := h.seedAgent(t, "fake:report-writer", []string{"fs.write", "doc.convert", "exec.bash", "fs.read", "fs.list"})
	runID := h.createRun(t, digest)

	victimCtx, killVictim := context.WithCancel(ctx)
	h.startWorker(victimCtx, "victim")

	// Wait until the victim is genuinely inside the tool call.
	select {
	case <-h.tools.blocked:
	case <-time.After(15 * time.Second):
		t.Fatal("worker never reached the blocking tool call")
	}

	run, err := h.store.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.LeaseOwner != "victim" {
		t.Fatalf("expected the victim to hold the lease, got %q", run.LeaseOwner)
	}
	t.Logf("victim holds the lease on %s at step %d; killing it now with no drain", runID, run.Step)

	// SIGKILL equivalent: context cancelled, nothing flushed, lease not released.
	killVictim()
	// Stop blocking so the replacement can make progress.
	h.tools.mu.Lock()
	h.tools.blockOn = ""
	h.tools.mu.Unlock()
	close(h.tools.released)

	// A replacement worker joins, as a rescheduled pod would.
	h.startWorker(ctx, "replacement")

	final := h.awaitState(t, runID, 45*time.Second, types.StateSucceeded, types.StateFailed)
	if final.State != types.StateSucceeded {
		t.Fatalf("run did not recover: state=%s reason=%q", final.State, final.StatusReason)
	}

	// THE PROPERTY: recovery must not duplicate side effects.
	if dups := h.tools.duplicated(); len(dups) > 0 {
		t.Fatalf("a side effect was executed more than once after recovery: %v", dups)
	}
	evs, _ := h.store.ListEvents(ctx, runID, 0)
	t.Logf("recovered: %d events, %d tool requests, %d actual side effects, final state %s",
		len(evs), h.tools.calls.Load(), h.tools.totalSideEffects(), final.State)

	// The log must show the handover: two different workers contributed.
	workers := map[string]bool{}
	for _, e := range evs {
		if e.Payload.Worker != "" {
			workers[e.Payload.Worker] = true
		}
	}
	if !workers["replacement"] {
		t.Fatal("the replacement worker did not contribute; the run did not actually fail over")
	}
	t.Logf("workers that contributed to this run: %v", keys(workers))
}

// A rolling deploy replaces every worker while runs are in flight. Nothing
// should be lost and nothing should be duplicated.
func TestDurability_RollingDeployLosesNoWork(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.startReaper(ctx)

	digest := h.seedAgent(t, "fake:report-writer", []string{"fs.write", "doc.convert", "exec.bash", "fs.read", "fs.list"})
	const runs = 12
	ids := make([]string, 0, runs)
	for i := 0; i < runs; i++ {
		ids = append(ids, h.createRun(t, digest))
	}

	// Generation 1.
	gen1, killGen1 := context.WithCancel(ctx)
	for i := 0; i < 3; i++ {
		h.startWorker(gen1, fmt.Sprintf("gen1-%d", i))
	}
	time.Sleep(700 * time.Millisecond) // let them get in the middle of things

	// Generation 2 comes up, generation 1 is killed abruptly - the pessimistic
	// version of a rolling update, with no graceful drain at all.
	for i := 0; i < 3; i++ {
		h.startWorker(ctx, fmt.Sprintf("gen2-%d", i))
	}
	killGen1()

	for _, rid := range ids {
		r := h.awaitState(t, rid, 60*time.Second, types.StateSucceeded, types.StateFailed)
		if r.State != types.StateSucceeded {
			t.Fatalf("run %s did not survive the deploy: state=%s reason=%q", rid, r.State, r.StatusReason)
		}
	}
	if dups := h.tools.duplicated(); len(dups) > 0 {
		t.Fatalf("side effects duplicated across the deploy: %v", dups)
	}
	t.Logf("all %d runs survived a hard worker replacement; %d tool requests, %d side effects (no duplicates)",
		runs, h.tools.calls.Load(), h.tools.totalSideEffects())
}

// A worker that is merely slow (a long GC pause, a network partition) must not
// be able to write stale history over the replacement's progress.
func TestDurability_PartitionedWorkerIsFencedOut(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	digest := h.seedAgent(t, "fake:report-writer", []string{"fs.write", "doc.convert", "exec.bash", "fs.read", "fs.list"})
	runID := h.createRun(t, digest)

	// "slow" acquires a short lease and then stalls, as a partitioned pod would.
	slow, err := h.store.AcquireLease(ctx, "slow", 500*time.Millisecond, nil)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	time.Sleep(900 * time.Millisecond)
	if _, err := h.store.ReapExpiredLeases(ctx); err != nil {
		t.Fatal(err)
	}
	// A healthy worker takes over and makes progress.
	h.startWorker(ctx, "healthy")
	h.awaitState(t, runID, 30*time.Second, types.StateSucceeded, types.StateFailed)

	// The partitioned worker finally wakes up and tries to commit what it was
	// doing. It must be rejected.
	err = h.store.Commit(ctx, runID, "slow", slow.NextSeq, []types.Event{{
		Type: types.EventNote, Payload: types.EventPayload{Text: "stale write from a partitioned worker"},
	}}, store.RunUpdate{})
	if err == nil {
		t.Fatal("a partitioned worker was allowed to write after losing its lease")
	}
	evs, _ := h.store.ListEvents(ctx, runID, 0)
	for _, e := range evs {
		if strings.Contains(e.Payload.Text, "stale write") {
			t.Fatal("stale history from a fenced worker reached the log")
		}
	}
	t.Logf("partitioned worker correctly fenced out: %v", err)
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
