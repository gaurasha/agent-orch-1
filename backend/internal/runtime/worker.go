// Package runtime executes agents.
//
// A worker here is completely stateless and interchangeable. It:
//
//	1. leases a runnable Run,
//	2. rebuilds the model context by REPLAYING that run's event log,
//	3. advances the run by one step,
//	4. commits the new events atomically (fenced by the lease),
//	5. releases the lease.
//
// Nothing about the agent lives in the worker's memory between steps. That is
// what makes "an agent mid-task survives a pod restart, a node drain and a
// deploy" true by construction rather than by careful shutdown handling: if a
// worker vanishes at any instant, its lease lapses, another worker replays the
// same log and continues. The only thing that must not be lost is the log, and
// that is in Postgres.
//
// The cost of this design is that side effects have to be made idempotent
// explicitly, because replay re-reaches them. That is handled by the tool
// gateway's idempotency journal - see internal/gateway.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gaurasha/agent-orch/backend/internal/id"
	"github.com/gaurasha/agent-orch/backend/internal/llm"
	"github.com/gaurasha/agent-orch/backend/internal/obs"
	"github.com/gaurasha/agent-orch/backend/internal/store"
	"github.com/gaurasha/agent-orch/backend/internal/tools"
	"github.com/gaurasha/agent-orch/backend/internal/types"
)

// ToolCaller is the worker's view of the tool gateway. It is an interface so
// the worker can be pointed at an in-process gateway (the all-in-one demo) or
// an HTTP client (the deployed topology) without changing a line of the agent
// loop.
type ToolCaller interface {
	Call(ctx context.Context, runID string, req ToolRequest) (ToolResponse, error)
}

type ToolRequest struct {
	Tool     string
	Args     map[string]any
	IdemKey  string
	Approved bool
}

type ToolResponse struct {
	Result    string
	IsError   bool
	Decision  string
	Reason    string
	Truncated bool
	Replayed  bool
	// NeedsApproval tells the worker to park the run rather than continue.
	NeedsApproval bool
}

type Config struct {
	WorkerID string
	// LeaseTTL bounds how long a dead worker's run stays stuck. Shorter means
	// faster recovery but more risk of fencing out a merely slow worker; the
	// heartbeat interval must stay comfortably below it.
	LeaseTTL          time.Duration
	HeartbeatInterval time.Duration
	// MaxStepsPerLease caps how many steps one worker takes before releasing.
	// Without it, a long-running agent would hold its lease indefinitely and
	// the queue would lose its ability to rebalance across workers.
	MaxStepsPerLease int
	PollInterval     time.Duration
	// TypicalTokens is the estimate used to ask the limiter which tenants are
	// currently schedulable.
	TypicalTokens int64
}

func (c Config) withDefaults() Config {
	if c.WorkerID == "" {
		c.WorkerID = id.New("worker")
	}
	if c.LeaseTTL <= 0 {
		c.LeaseTTL = 30 * time.Second
	}
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = c.LeaseTTL / 3
	}
	if c.MaxStepsPerLease <= 0 {
		c.MaxStepsPerLease = 4
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 100 * time.Millisecond
	}
	if c.TypicalTokens <= 0 {
		c.TypicalTokens = 2000
	}
	return c
}

// EligibleTenantsFunc lets the worker ask the fairness layer which tenants may
// be scheduled right now.
type EligibleTenantsFunc func(typicalTokens int64) []string

type Worker struct {
	cfg      Config
	store    store.Store
	model    *llm.Gateway
	tools    ToolCaller
	registry *tools.Registry
	eligible EligibleTenantsFunc
	metrics  *obs.Metrics
	log      *obs.Logger
}

func NewWorker(cfg Config, st store.Store, model *llm.Gateway, tc ToolCaller,
	reg *tools.Registry, eligible EligibleTenantsFunc, m *obs.Metrics, log *obs.Logger) *Worker {
	cfg = cfg.withDefaults()
	return &Worker{
		cfg: cfg, store: st, model: model, tools: tc, registry: reg,
		eligible: eligible, metrics: m, log: log.With("worker", cfg.WorkerID),
	}
}

// Run is the worker loop. It returns when ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	w.log.Info("agent worker started",
		"lease_ttl", w.cfg.LeaseTTL.String(), "max_steps_per_lease", w.cfg.MaxStepsPerLease)
	for {
		select {
		case <-ctx.Done():
			w.log.Info("agent worker stopping")
			return
		default:
		}
		worked, err := w.tick(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			w.log.Error("worker tick failed", "err", err)
		}
		if !worked {
			// Nothing runnable. Sleep briefly rather than spinning the database.
			select {
			case <-ctx.Done():
				return
			case <-time.After(w.cfg.PollInterval):
			}
		}
	}
}

func (w *Worker) tick(ctx context.Context) (bool, error) {
	var eligible []string
	if w.eligible != nil {
		eligible = w.eligible(w.cfg.TypicalTokens)
		if len(eligible) == 0 {
			// Every tenant is out of quota. Not an error - just nothing to do.
			return false, nil
		}
	}
	run, err := w.store.AcquireLease(ctx, w.cfg.WorkerID, w.cfg.LeaseTTL, eligible)
	if errors.Is(err, types.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("acquire lease: %w", err)
	}
	w.executeLease(ctx, run)
	return true, nil
}

// executeLease advances one run while holding its lease.
func (w *Worker) executeLease(ctx context.Context, run types.Run) {
	log := w.log.WithRun(run.ID, run.TenantID)

	// Heartbeat in the background. If renewal fails the run has been taken from
	// us, so we cancel our own work immediately rather than continuing to make
	// side effects we can no longer journal.
	leaseCtx, cancelLease := context.WithCancel(ctx)
	defer cancelLease()
	go w.heartbeat(leaseCtx, run.ID, cancelLease, log)

	// Whatever happens below - normal finish, lease lost, panic, shutdown - the
	// run must not be left RUNNING with no owner. YieldRun is fenced and is a
	// no-op once the run has reached a terminal or parked state, so it is safe
	// to call unconditionally.
	//
	// context.WithoutCancel matters: on shutdown the parent context is already
	// cancelled, and we still need this write to land. If the process is SIGKILLed
	// instead, none of this runs and the lease simply expires - the reaper then
	// reclaims the run within one lease TTL. Both paths converge.
	defer func() {
		relCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := w.store.YieldRun(relCtx, run.ID, w.cfg.WorkerID, "worker yielded mid-step"); err != nil {
			log.Error("failed to yield run back to the queue", "err", err)
		}
		_ = w.store.ReleaseLease(relCtx, run.ID, w.cfg.WorkerID)
	}()

	for step := 0; step < w.cfg.MaxStepsPerLease; step++ {
		select {
		case <-leaseCtx.Done():
			log.Warn("abandoning run: lease lost or context cancelled")
			return
		default:
		}
		start := time.Now()
		cont, err := w.step(leaseCtx, &run, log)
		w.metrics.StepLatency(time.Since(start))
		if err != nil {
			if errors.Is(err, types.ErrLeaseLost) {
				// Expected under node loss and rolling deploys. Another worker
				// already owns this run and has replayed from the log.
				log.Warn("lease lost mid-step; another worker has taken over")
				return
			}
			log.Error("step failed", "err", err)
			return
		}
		if !cont {
			return
		}
	}
	// The per-lease step budget is up. The run is NOT finished, so it must go
	// back on the queue explicitly. Simply dropping the lease would leave the
	// row in RUNNING with no owner, where neither the dispatcher (which selects
	// QUEUED) nor the reaper (which looks for an EXPIRED lease) would ever find
	// it again - a permanent stall.
	if err := w.commit(ctx, &run, []types.Event{{
		Type: types.EventNote,
		Payload: types.EventPayload{
			Text:   "yielding to the queue after the per-lease step budget",
			Worker: w.cfg.WorkerID,
		},
	}}, store.RunUpdate{
		State:        ptr(types.StateQueued),
		StatusReason: ptr("yielded after " + strconv.Itoa(w.cfg.MaxStepsPerLease) + " steps"),
		ReleaseLease: true,
	}); err != nil && !errors.Is(err, types.ErrLeaseLost) {
		log.Error("failed to requeue run after the step budget", "err", err)
	}
	log.Debug("yielded lease after the per-lease step budget", "steps", w.cfg.MaxStepsPerLease)
}

func (w *Worker) heartbeat(ctx context.Context, runID string, onLost func(), log *obs.Logger) {
	t := time.NewTicker(w.cfg.HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			hbCtx, cancel := context.WithTimeout(ctx, w.cfg.HeartbeatInterval)
			err := w.store.RenewLease(hbCtx, runID, w.cfg.WorkerID, w.cfg.LeaseTTL)
			cancel()
			if err != nil {
				log.Warn("lease renewal failed; stopping work on this run", "err", err)
				onLost()
				return
			}
		}
	}
}

// step advances the run by exactly one model turn plus any tool calls it
// requested. It returns cont=true when the run is ready for another step.
func (w *Worker) step(ctx context.Context, run *types.Run, log *obs.Logger) (bool, error) {
	// --- 1. Replay -------------------------------------------------------
	// The event log is the only source of truth. Nothing carried over from a
	// previous step in this worker's memory is trusted.
	events, err := w.store.ListEvents(ctx, run.ID, 0)
	if err != nil {
		return false, fmt.Errorf("replay events: %w", err)
	}
	def, err := w.store.GetDefinition(ctx, run.DefDigest)
	if err != nil {
		return false, fmt.Errorf("load pinned definition %s: %w", run.DefDigest, err)
	}
	messages := Rebuild(events)

	// --- 2. Budget -------------------------------------------------------
	// Checked here as well as at the gateway: stopping a poison agent before we
	// pay for another model call is much cheaper than stopping it after.
	if reason := run.Budget.ExceedsReason(run.Usage, run.Elapsed()); reason != "" {
		log.Warn("run exceeded its budget", "reason", reason)
		return false, w.finish(ctx, run, types.StateFailed, reason,
			types.Event{Type: types.EventRunFinished, Payload: types.EventPayload{
				Text: "Run stopped: " + reason, Reason: reason, Worker: w.cfg.WorkerID}})
	}

	// --- 3. Model --------------------------------------------------------
	req := llm.Request{
		Model:     def.Spec.Model,
		System:    buildSystemPrompt(def.Spec, *run),
		Messages:  messages,
		Tools:     w.registry.SchemasFor(def.Spec.Tools),
		MaxTokens: 2048,
	}
	resp, err := w.model.Complete(ctx, run.TenantID, def.Spec.Priority, req)
	if err != nil {
		if errors.Is(err, llm.ErrQuotaUnavailable) {
			// Not a failure. Park the run with a short wake time and let the
			// worker pick up someone else's work.
			return false, w.requeue(ctx, run, "waiting for LLM quota", 2*time.Second)
		}
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		// The provider is unavailable after retries. Durability means we can
		// simply wait: the run stays QUEUED rather than failing.
		log.Warn("model unavailable; requeueing the run", "err", err)
		return false, w.requeue(ctx, run, "model provider unavailable: "+firstLine(err.Error()), 10*time.Second)
	}

	newEvents := []types.Event{{
		Type: types.EventModelResponse,
		Payload: types.EventPayload{
			Text: resp.Content, Model: resp.Model,
			InputTokens: resp.InputTokens, OutputTokens: resp.OutputTokens,
			CostUSD: resp.CostUSD, StopReason: resp.StopReason, Worker: w.cfg.WorkerID,
		},
	}}
	usage := run.Usage
	usage.Steps++
	usage.InputTokens += resp.InputTokens
	usage.OutputTokens += resp.OutputTokens
	usage.CostUSD += resp.CostUSD

	// --- 4. Terminal model outcomes --------------------------------------
	if resp.StopReason == "human_input_required" {
		newEvents = append(newEvents, types.Event{
			Type:    types.EventHumanPause,
			Payload: types.EventPayload{Text: resp.Content, Worker: w.cfg.WorkerID},
		})
		return false, w.commit(ctx, run, newEvents, store.RunUpdate{
			State:        ptr(types.StateWaitingHuman),
			StatusReason: ptr("waiting for human input"),
			Usage:        &usage,
			Step:         ptr(run.Step + 1),
			ReleaseLease: true,
		})
	}
	if len(resp.ToolCalls) == 0 {
		newEvents = append(newEvents, types.Event{
			Type:    types.EventRunFinished,
			Payload: types.EventPayload{Text: resp.Content, Worker: w.cfg.WorkerID},
		})
		return false, w.commit(ctx, run, newEvents, store.RunUpdate{
			State:        ptr(types.StateSucceeded),
			StatusReason: ptr("completed"),
			Usage:        &usage,
			Step:         ptr(run.Step + 1),
			ReleaseLease: true,
		})
	}

	// --- 5. Tool calls ---------------------------------------------------
	needsApproval := false
	for i, tc := range resp.ToolCalls {
		// The idempotency key is DETERMINISTIC in (run, step, index). A worker
		// that dies here and is replaced will replay to this exact point and
		// produce the same key, so the gateway can recognise the repeat and
		// return the recorded result instead of repeating the side effect.
		idemKey := fmt.Sprintf("%s:%d:%d", run.ID, run.Step, i)

		newEvents = append(newEvents, types.Event{
			Type: types.EventToolCall,
			Payload: types.EventPayload{
				ToolCallID: tc.ID, Tool: tc.Name, Args: tc.Args,
				IdemKey: idemKey, Worker: w.cfg.WorkerID,
			},
		})

		callStart := time.Now()
		out, err := w.tools.Call(ctx, run.ID, ToolRequest{
			Tool: tc.Name, Args: tc.Args, IdemKey: idemKey,
		})
		dur := time.Since(callStart)

		if err != nil {
			// Transport-level failure talking to the gateway. Tell the model
			// the truth - that the outcome is unknown - rather than presenting
			// it as a clean failure it might blithely retry.
			out = ToolResponse{
				Result: "The platform could not complete this tool call and does not know whether it took effect: " +
					firstLine(err.Error()),
				IsError: true, Decision: "ERROR",
			}
			log.Error("tool call transport failure", "tool", tc.Name, "err", err)
		}

		if out.NeedsApproval {
			needsApproval = true
			newEvents = append(newEvents, types.Event{
				Type: types.EventApprovalNeeded,
				Payload: types.EventPayload{
					ToolCallID: tc.ID, Tool: tc.Name, Args: tc.Args,
					Reason: out.Reason, IdemKey: idemKey,
				},
			})
			break
		}

		evType := types.EventToolResult
		if out.Decision == string("DENY") {
			evType = types.EventToolDenied
		}
		newEvents = append(newEvents, types.Event{
			Type: evType,
			Payload: types.EventPayload{
				ToolCallID: tc.ID, Tool: tc.Name, Result: out.Result,
				IsError: out.IsError, DurationMS: dur.Milliseconds(),
				Decision: out.Decision, Reason: out.Reason, IdemKey: idemKey,
			},
		})
		usage.ToolCalls++
	}

	if needsApproval {
		return false, w.commit(ctx, run, newEvents, store.RunUpdate{
			State:        ptr(types.StateWaitingApproval),
			StatusReason: ptr("waiting for human approval of a tool call"),
			Usage:        &usage, Step: ptr(run.Step + 1), ReleaseLease: true,
		})
	}

	if err := w.commit(ctx, run, newEvents, store.RunUpdate{
		Usage: &usage, Step: ptr(run.Step + 1),
	}); err != nil {
		return false, err
	}
	run.Step++
	run.Usage = usage
	run.NextSeq += len(newEvents)
	return true, nil
}

func (w *Worker) commit(ctx context.Context, run *types.Run, evs []types.Event, up store.RunUpdate) error {
	err := w.store.Commit(ctx, run.ID, w.cfg.WorkerID, run.NextSeq, evs, up)
	if err != nil {
		return err
	}
	return nil
}

func (w *Worker) finish(ctx context.Context, run *types.Run, state types.RunState, reason string, ev types.Event) error {
	return w.commit(ctx, run, []types.Event{ev}, store.RunUpdate{
		State: &state, StatusReason: &reason, ReleaseLease: true,
	})
}

// requeue parks a run without consuming it, for quota and provider waits.
func (w *Worker) requeue(ctx context.Context, run *types.Run, reason string, after time.Duration) error {
	wake := time.Now().UTC().Add(after)
	wakePtr := &wake
	return w.commit(ctx, run, []types.Event{{
		Type:    types.EventNote,
		Payload: types.EventPayload{Text: reason, Worker: w.cfg.WorkerID},
	}}, store.RunUpdate{
		State:        ptr(types.StateQueued),
		StatusReason: &reason,
		WakeAt:       &wakePtr,
		ReleaseLease: true,
	})
}

func ptr[T any](v T) *T { return &v }

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
