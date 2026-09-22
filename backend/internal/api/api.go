// Package api is the control plane: the operator- and caller-facing HTTP
// surface. It answers the two questions the brief asks an operator to be able
// to answer - "what is agent X doing right now" and "what has it cost" - and
// it is the only way runs are created, paused, resumed, approved or cancelled.
//
// Tenancy is enforced on every route by resolving the caller to a tenant and
// then scoping the query. There is no route that takes a tenant id from the
// request body.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gaurasha/agent-orch/backend/internal/canon"
	"github.com/gaurasha/agent-orch/backend/internal/fairness"
	"github.com/gaurasha/agent-orch/backend/internal/id"
	"github.com/gaurasha/agent-orch/backend/internal/obs"
	"github.com/gaurasha/agent-orch/backend/internal/store"
	"github.com/gaurasha/agent-orch/backend/internal/tools"
	"github.com/gaurasha/agent-orch/backend/internal/types"
)

type Server struct {
	store    store.Store
	registry *tools.Registry
	limiter  *fairness.Limiter
	metrics  *obs.Metrics
	log      *obs.Logger
	// apiKeys maps a bearer token to a tenant. A real deployment resolves the
	// caller through OIDC and an IdP group mapping; this keeps the demo
	// runnable while still making every request tenant-scoped rather than
	// trusting a tenant id in the body.
	apiKeys map[string]Principal
}

// Principal is the authenticated caller.
type Principal struct {
	TenantID string
	User     string
	// Operator can read across tenants. Used by the platform team's own view,
	// and audited exactly like everything else.
	Operator bool
}

func New(st store.Store, reg *tools.Registry, lim *fairness.Limiter, m *obs.Metrics, log *obs.Logger) *Server {
	return &Server{store: st, registry: reg, limiter: lim, metrics: m, log: log,
		apiKeys: map[string]Principal{}}
}

func (s *Server) AddAPIKey(key string, p Principal) { s.apiKeys[key] = p }

func (s *Server) Routes(mux *http.ServeMux) {
	h := func(fn func(http.ResponseWriter, *http.Request, Principal)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			p, ok := s.authenticate(r)
			if !ok {
				writeErr(w, http.StatusUnauthorized, "missing or invalid API key")
				return
			}
			fn(w, r, p)
		}
	}
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.Handle("GET /metrics", s.metrics.Handler())

	mux.HandleFunc("GET /v1/tenants", h(s.listTenants))
	mux.HandleFunc("GET /v1/agents", h(s.listAgents))
	mux.HandleFunc("POST /v1/agents", h(s.createAgent))

	mux.HandleFunc("GET /v1/runs", h(s.listRuns))
	mux.HandleFunc("POST /v1/runs", h(s.createRun))
	mux.HandleFunc("GET /v1/runs/{id}", h(s.getRun))
	mux.HandleFunc("GET /v1/runs/{id}/events", h(s.getEvents))
	mux.HandleFunc("GET /v1/runs/{id}/stream", h(s.streamRun))
	mux.HandleFunc("POST /v1/runs/{id}/resume", h(s.resumeRun))
	mux.HandleFunc("POST /v1/runs/{id}/approve", h(s.approveRun))
	mux.HandleFunc("POST /v1/runs/{id}/cancel", h(s.cancelRun))

	mux.HandleFunc("GET /v1/audit", h(s.listAudit))
	mux.HandleFunc("GET /v1/quota", h(s.getQuota))
	mux.HandleFunc("GET /v1/tools", h(s.listTools))
	mux.HandleFunc("GET /v1/overview", h(s.overview))
}

func (s *Server) authenticate(r *http.Request) (Principal, bool) {
	key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if key == "" {
		key = r.URL.Query().Get("api_key") // EventSource cannot set headers
	}
	p, ok := s.apiKeys[key]
	return p, ok
}

// scope returns the tenant a request must be limited to. An operator may pass
// ?tenant= to look at one tenant; everyone else is pinned to their own.
func (s *Server) scope(r *http.Request, p Principal) string {
	if p.Operator {
		return r.URL.Query().Get("tenant")
	}
	return p.TenantID
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		writeErr(w, http.StatusServiceUnavailable, "store unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ---------------------------------------------------------------------------
// Tenants, agents
// ---------------------------------------------------------------------------

func (s *Server) listTenants(w http.ResponseWriter, r *http.Request, p Principal) {
	ts, err := s.store.ListTenants(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !p.Operator {
		filtered := ts[:0]
		for _, t := range ts {
			if t.ID == p.TenantID {
				filtered = append(filtered, t)
			}
		}
		ts = filtered
	}
	writeJSON(w, http.StatusOK, map[string]any{"tenants": ts})
}

func (s *Server) listAgents(w http.ResponseWriter, r *http.Request, p Principal) {
	defs, err := s.store.ListDefinitions(r.Context(), s.scope(r, p))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": defs})
}

type createAgentRequest struct {
	Name string     `json:"name"`
	Spec types.Spec `json:"spec"`
}

// createAgent registers an immutable, content-addressed agent definition.
//
// Registering the same spec twice returns the same digest and is a no-op. That
// is what lets a run pin a digest and be certain its permissions cannot be
// widened underneath it.
func (s *Server) createAgent(w http.ResponseWriter, r *http.Request, p Principal) {
	var req createAgentRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Name == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	if err := req.Spec.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Reject grants for tools that do not exist, at registration time rather
	// than at the first call. A typo in a tool name should fail when a human is
	// looking at it, not at 3am inside an agent run.
	for _, t := range req.Spec.Tools {
		if _, ok := s.registry.Get(t); !ok {
			writeErr(w, http.StatusBadRequest,
				fmt.Sprintf("unknown tool %q; available: %s", t, strings.Join(s.registry.Names(), ", ")))
			return
		}
	}
	if req.Spec.Priority == "" {
		req.Spec.Priority = types.PriorityNormal
	}
	if req.Spec.Budget == (types.Budget{}) {
		req.Spec.Budget = types.DefaultBudget()
	}
	tenantID := p.TenantID
	if p.Operator && r.URL.Query().Get("tenant") != "" {
		tenantID = r.URL.Query().Get("tenant")
	}
	digest, err := canon.Digest(req.Spec)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	def := types.AgentDefinition{
		Digest: digest, TenantID: tenantID, Name: req.Name,
		Spec: req.Spec, CreatedAt: time.Now().UTC(),
	}
	if err := s.store.PutDefinition(r.Context(), def); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, def)
}

// ---------------------------------------------------------------------------
// Runs
// ---------------------------------------------------------------------------

type createRunRequest struct {
	AgentDigest string `json:"agent_digest"`
	AgentName   string `json:"agent_name"`
	Input       string `json:"input"`
}

func (s *Server) createRun(w http.ResponseWriter, r *http.Request, p Principal) {
	var req createRunRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Context()

	def, err := s.resolveAgent(ctx, req, p)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// Tenancy check: an agent definition belongs to exactly one tenant and can
	// only be run by that tenant.
	if !p.Operator && def.TenantID != p.TenantID {
		writeErr(w, http.StatusForbidden, "that agent belongs to a different tenant")
		return
	}

	// Admission control: a per-tenant concurrency cap is what stops a tenant
	// that spins 300 agents at 09:00 from consuming the whole worker pool and
	// the whole queue depth. Rejecting at admission, with a clear message, is
	// far kinder than accepting work the platform cannot get to.
	tenant, err := s.store.GetTenant(ctx, def.TenantID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "unknown tenant")
		return
	}
	active, err := s.store.CountActiveRuns(ctx, def.TenantID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if active >= tenant.MaxConcurrentRuns {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{
			"error": fmt.Sprintf("tenant %s already has %d active runs (limit %d)",
				def.TenantID, active, tenant.MaxConcurrentRuns),
			"active_runs": active, "limit": tenant.MaxConcurrentRuns,
			"retry_after_seconds": 30,
		})
		return
	}

	input := req.Input
	if input == "" {
		input = "Begin."
	}
	run := types.Run{
		ID: id.New("run"), TenantID: def.TenantID, AgentName: def.Name,
		DefDigest: def.Digest, TriggeringUser: p.User,
		State: types.StateQueued, Budget: def.Spec.Budget, Priority: def.Spec.Priority,
	}
	first := types.Event{
		Type:    types.EventRunCreated,
		Payload: types.EventPayload{Text: input},
	}
	if err := s.store.CreateRun(ctx, run, first); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.metrics.RunState(run.TenantID, string(types.StateQueued), 1)
	s.log.Info("run created", "run_id", run.ID, "tenant_id", run.TenantID,
		"agent", run.AgentName, "user", p.User)
	writeJSON(w, http.StatusCreated, run)
}

func (s *Server) resolveAgent(ctx context.Context, req createRunRequest, p Principal) (types.AgentDefinition, error) {
	if req.AgentDigest != "" {
		return s.store.GetDefinition(ctx, req.AgentDigest)
	}
	if req.AgentName == "" {
		return types.AgentDefinition{}, errors.New("one of agent_digest or agent_name is required")
	}
	tenantID := p.TenantID
	defs, err := s.store.ListDefinitions(ctx, tenantID)
	if err != nil {
		return types.AgentDefinition{}, err
	}
	// Newest definition with that name. Resolving by name is a convenience for
	// humans; the run still pins the resulting digest, so the resolution
	// happens exactly once, here.
	for _, d := range defs {
		if d.Name == req.AgentName {
			return d, nil
		}
	}
	return types.AgentDefinition{}, fmt.Errorf("no agent named %q for tenant %s", req.AgentName, tenantID)
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request, p Principal) {
	f := store.RunFilter{TenantID: s.scope(r, p), Limit: intParam(r, "limit", 100)}
	if st := r.URL.Query().Get("state"); st != "" {
		for _, one := range strings.Split(st, ",") {
			f.States = append(f.States, types.RunState(strings.TrimSpace(one)))
		}
	}
	runs, err := s.store.ListRuns(r.Context(), f)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

// mustRun loads a run and enforces tenancy. Every per-run route goes through it.
func (s *Server) mustRun(w http.ResponseWriter, r *http.Request, p Principal) (types.Run, bool) {
	run, err := s.store.GetRun(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "run not found")
		return types.Run{}, false
	}
	if !p.Operator && run.TenantID != p.TenantID {
		// 404, not 403: a caller should not be able to probe which run ids
		// exist in another tenant.
		writeErr(w, http.StatusNotFound, "run not found")
		return types.Run{}, false
	}
	return run, true
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request, p Principal) {
	run, ok := s.mustRun(w, r, p)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) getEvents(w http.ResponseWriter, r *http.Request, p Principal) {
	run, ok := s.mustRun(w, r, p)
	if !ok {
		return
	}
	evs, err := s.store.ListEvents(r.Context(), run.ID, intParam(r, "since", 0))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": evs, "run": run})
}

// streamRun pushes run state and new events over Server-Sent Events.
//
// SSE rather than WebSocket: the data flows one way, it is text, it reconnects
// automatically in every browser, and it needs no handshake, no framing library
// and no extra dependency. WebSocket would buy bidirectionality we do not need.
//
// This is a poll-and-push bridge rather than a true change feed. At this scale
// that is fine; at 10x it should be replaced by Postgres LISTEN/NOTIFY or a
// broker, because one polling goroutine per open browser tab does not scale.
func (s *Server) streamRun(w http.ResponseWriter, r *http.Request, p Principal) {
	run, ok := s.mustRun(w, r, p)
	if !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // stop proxies buffering the stream
	w.WriteHeader(http.StatusOK)

	ctx := r.Context()
	since := 0
	ticker := time.NewTicker(400 * time.Millisecond)
	defer ticker.Stop()
	// Bound the stream so a forgotten tab does not hold a connection forever.
	deadline := time.After(30 * time.Minute)

	send := func(event string, v any) bool {
		b, err := json.Marshal(v)
		if err != nil {
			return true
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	for {
		cur, err := s.store.GetRun(ctx, run.ID)
		if err == nil {
			if !send("run", cur) {
				return
			}
			evs, err := s.store.ListEvents(ctx, run.ID, since)
			if err == nil {
				for _, e := range evs {
					if !send("event", e) {
						return
					}
					since = e.Seq + 1
				}
			}
			if cur.State.Terminal() {
				send("done", map[string]string{"state": string(cur.State)})
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			send("done", map[string]string{"state": "stream_timeout"})
			return
		case <-ticker.C:
		}
	}
}

type resumeRequest struct {
	Input string `json:"input"`
}

// resumeRun delivers human input to a parked agent.
//
// This is the other half of "an agent that waits two days for a human costs
// near zero": while parked it held no worker, no sandbox and no quota, and
// resuming is just an append to the log plus a state change.
func (s *Server) resumeRun(w http.ResponseWriter, r *http.Request, p Principal) {
	run, ok := s.mustRun(w, r, p)
	if !ok {
		return
	}
	if run.State != types.StateWaitingHuman {
		writeErr(w, http.StatusConflict,
			fmt.Sprintf("run is %s; only a run WAITING_HUMAN can be resumed", run.State))
		return
	}
	var req resumeRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Input == "" {
		writeErr(w, http.StatusBadRequest, "input is required")
		return
	}
	err := s.store.AppendSystemEvents(r.Context(), run.ID, []types.Event{{
		Type:    types.EventHumanResume,
		Payload: types.EventPayload{Text: req.Input, Worker: p.User},
	}}, store.RunUpdate{
		State:        ptr(types.StateQueued),
		StatusReason: ptr("resumed by " + p.User),
		WakeAt:       ptrNilTime(),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.log.Info("run resumed by human", "run_id", run.ID, "user", p.User)
	writeJSON(w, http.StatusOK, map[string]string{"status": "resumed"})
}

func (s *Server) approveRun(w http.ResponseWriter, r *http.Request, p Principal) {
	run, ok := s.mustRun(w, r, p)
	if !ok {
		return
	}
	if run.State != types.StateWaitingApproval {
		writeErr(w, http.StatusConflict,
			fmt.Sprintf("run is %s; only a run WAITING_APPROVAL can be approved", run.State))
		return
	}
	// The approval is itself an audited event: who approved what, and when.
	err := s.store.AppendSystemEvents(r.Context(), run.ID, []types.Event{{
		Type:    types.EventApprovalGiven,
		Payload: types.EventPayload{Text: "approved by " + p.User, Worker: p.User},
	}}, store.RunUpdate{
		State:        ptr(types.StateQueued),
		StatusReason: ptr("approved by " + p.User),
		WakeAt:       ptrNilTime(),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.log.Info("tool call approved by human", "run_id", run.ID, "user", p.User)
	writeJSON(w, http.StatusOK, map[string]string{"status": "approved"})
}

func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request, p Principal) {
	run, ok := s.mustRun(w, r, p)
	if !ok {
		return
	}
	if run.State.Terminal() {
		writeErr(w, http.StatusConflict, "run has already finished")
		return
	}
	// Cancellation is cooperative-but-enforced: the state change makes the
	// gateway refuse any further tool call from this run immediately, even if a
	// worker is mid-step and has not noticed yet.
	err := s.store.AppendSystemEvents(r.Context(), run.ID, []types.Event{{
		Type:    types.EventRunFinished,
		Payload: types.EventPayload{Text: "cancelled by " + p.User, Reason: "cancelled"},
	}}, store.RunUpdate{
		State:        ptr(types.StateCancelled),
		StatusReason: ptr("cancelled by " + p.User),
		ReleaseLease: true,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.log.Warn("run cancelled", "run_id", run.ID, "user", p.User)
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

// ---------------------------------------------------------------------------
// Audit, quota, overview
// ---------------------------------------------------------------------------

// listAudit answers "show me every command agent X ran last Tuesday", and
// verifies the hash chain as it goes so the answer comes with evidence that it
// has not been edited.
func (s *Server) listAudit(w http.ResponseWriter, r *http.Request, p Principal) {
	tenant := s.scope(r, p)
	runID := r.URL.Query().Get("run_id")
	recs, err := s.store.ListAudit(r.Context(), tenant, runID, intParam(r, "limit", 500))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp := map[string]any{"records": recs, "count": len(recs)}
	// Only a complete, tenant-scoped chain can be verified; a filtered slice
	// legitimately has gaps, and reporting those as tampering would train
	// operators to ignore the warning.
	if tenant != "" && runID == "" {
		if err := store.VerifyChain(recs); err != nil {
			resp["chain_verified"] = false
			resp["chain_error"] = err.Error()
			s.log.Error("AUDIT CHAIN VERIFICATION FAILED", "tenant", tenant, "err", err)
		} else {
			resp["chain_verified"] = true
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) getQuota(w http.ResponseWriter, r *http.Request, p Principal) {
	snap := s.limiter.Snapshot()
	if !p.Operator {
		filtered := snap[:0]
		for _, q := range snap {
			if q.TenantID == p.TenantID {
				filtered = append(filtered, q)
			}
		}
		snap = filtered
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tenants": snap, "spare_tokens": s.limiter.SpareTokens(),
	})
}

func (s *Server) listTools(w http.ResponseWriter, r *http.Request, _ Principal) {
	// Model-facing schemas only. tools.Tool marshals to its schema, so no
	// backend configuration can be exposed here.
	writeJSON(w, http.StatusOK, map[string]any{"tools": s.registry.SchemasFor(s.registry.Names())})
}

// overview is the dashboard's single call: counts by state and spend by tenant.
func (s *Server) overview(w http.ResponseWriter, r *http.Request, p Principal) {
	ctx := r.Context()
	runs, err := s.store.ListRuns(ctx, store.RunFilter{TenantID: s.scope(r, p), Limit: 1000})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	byState := map[string]int{}
	byTenant := map[string]map[string]any{}
	var totalCost float64
	var totalTokens int64
	for _, run := range runs {
		byState[string(run.State)]++
		t, ok := byTenant[run.TenantID]
		if !ok {
			t = map[string]any{"runs": 0, "cost_usd": 0.0, "tokens": int64(0), "tool_calls": 0}
			byTenant[run.TenantID] = t
		}
		t["runs"] = t["runs"].(int) + 1
		t["cost_usd"] = t["cost_usd"].(float64) + run.Usage.CostUSD
		t["tokens"] = t["tokens"].(int64) + run.Usage.TotalTokens()
		t["tool_calls"] = t["tool_calls"].(int) + run.Usage.ToolCalls
		totalCost += run.Usage.CostUSD
		totalTokens += run.Usage.TotalTokens()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"runs_by_state":  byState,
		"by_tenant":      byTenant,
		"total_cost_usd": totalCost,
		"total_tokens":   totalTokens,
		"quota":          s.limiter.Snapshot(),
		"spare_tokens":   s.limiter.SpareTokens(),
		"generated_at":   time.Now().UTC(),
	})
}

// ---------------------------------------------------------------------------

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func intParam(r *http.Request, name string, def int) int {
	if v := r.URL.Query().Get(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func ptr[T any](v T) *T { return &v }

// ptrNilTime produces the "clear this field" value for RunUpdate.WakeAt.
func ptrNilTime() **time.Time {
	var t *time.Time
	return &t
}
