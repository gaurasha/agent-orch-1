// Package gateway is the tool-call choke point.
//
// Every tool call in the system passes through Handle(). That is the whole
// security argument: there is exactly one place where authorization is decided,
// exactly one place where credentials are minted, and exactly one place where
// the audit record is written - so those three things cannot drift apart, and
// reviewing them means reading one file rather than auditing every tool.
//
// Network policy backs this up: an agent worker's egress allowlist contains the
// gateway and nothing else, so "call the API directly and skip the gateway" is
// not an option available to a compromised worker, let alone to sandboxed code.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gaurasha/agent-orch/backend/internal/authz"
	"github.com/gaurasha/agent-orch/backend/internal/canon"
	"github.com/gaurasha/agent-orch/backend/internal/jwtmini"
	"github.com/gaurasha/agent-orch/backend/internal/obs"
	"github.com/gaurasha/agent-orch/backend/internal/store"
	"github.com/gaurasha/agent-orch/backend/internal/tools"
	"github.com/gaurasha/agent-orch/backend/internal/types"
)

// Audience binds a run token to this service. A token minted for the model
// gateway must not be replayable here.
const Audience = "tool-gateway"

type Gateway struct {
	store    store.Store
	registry *tools.Registry
	policy   *authz.Engine
	invoker  *tools.Invoker
	ws       *tools.WorkspaceManager
	secret   []byte
	metrics  *obs.Metrics
	log      *obs.Logger
}

func New(st store.Store, reg *tools.Registry, inv *tools.Invoker, ws *tools.WorkspaceManager,
	secret []byte, m *obs.Metrics, log *obs.Logger) *Gateway {
	return &Gateway{
		store: st, registry: reg, policy: authz.New(reg), invoker: inv,
		ws: ws, secret: secret, metrics: m, log: log,
	}
}

// Request is the wire format an agent worker sends.
type Request struct {
	Tool string         `json:"tool"`
	Args map[string]any `json:"args"`
	// IdemKey is deterministic - run:step:index - so a worker replaying after a
	// crash produces the identical key and gets the journalled result back
	// instead of causing the side effect a second time.
	IdemKey string `json:"idem_key"`
	// Approved is set by the control plane when a human has signed off a call
	// that policy marked as requiring approval.
	Approved bool `json:"approved"`
}

// Response is what comes back. Note what is absent: no credential, no URL
// template, no internal hostname, no policy internals beyond the human-readable
// reason.
type Response struct {
	Result     string `json:"result"`
	IsError    bool   `json:"is_error"`
	Decision   string `json:"decision"`
	Reason     string `json:"reason,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	Truncated  bool   `json:"truncated,omitempty"`
	Replayed   bool   `json:"replayed,omitempty"`
}

func (g *Gateway) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/toolcalls", g.handleToolCall)
	mux.HandleFunc("GET /v1/tools", g.handleListTools)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := g.store.Ping(r.Context()); err != nil {
			http.Error(w, "store unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok"))
	})
}

// handleListTools exposes the model-facing schemas only. tools.Tool marshals to
// its schema, so a backend field cannot be exposed here even by accident.
func (g *Gateway) handleListTools(w http.ResponseWriter, r *http.Request) {
	names := g.registry.Names()
	writeJSON(w, http.StatusOK, map[string]any{"tools": g.registry.SchemasFor(names)})
}

func (g *Gateway) handleToolCall(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	ctx := r.Context()

	claims, err := g.authenticate(r)
	if err != nil {
		// Deliberately terse: an unauthenticated caller learns nothing about
		// why. The detail goes to our logs.
		g.log.Warn("tool call rejected at authentication", "err", err, "remote", r.RemoteAddr)
		writeJSON(w, http.StatusUnauthorized, Response{
			Decision: string(authz.Deny), Reason: "invalid or expired run token", IsError: true})
		return
	}

	var req Request
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, Response{
			Decision: string(authz.Deny), Reason: "malformed request body", IsError: true})
		return
	}

	resp, status := g.process(ctx, claims, req)
	resp.DurationMS = time.Since(start).Milliseconds()
	g.metrics.ToolCall(claims.Tenant, req.Tool, resp.Decision, time.Since(start))
	writeJSON(w, status, resp)
}

// authenticate verifies the run-scoped token.
//
// In production this is one of two checks: the caller's workload identity
// (SPIFFE mTLS, proving it is a real agent worker) AND this token (proving
// which run it is acting for). The PoC has only the token; see DEEP_DIVE.md
// "D7 - workload identity" for why that is not sufficient on its own.
func (g *Gateway) authenticate(r *http.Request) (jwtmini.Claims, error) {
	h := r.Header.Get("Authorization")
	tok, ok := strings.CutPrefix(h, "Bearer ")
	if !ok || tok == "" {
		return jwtmini.Claims{}, errors.New("missing bearer token")
	}
	return jwtmini.Verify(g.secret, tok, Audience)
}

func (g *Gateway) process(ctx context.Context, claims jwtmini.Claims, req Request) (Response, int) {
	// 1. Reload the run and its PINNED definition from the store rather than
	//    trusting anything in the request. The token says which run; the store
	//    says what that run is currently allowed to do.
	run, err := g.store.GetRun(ctx, claims.Subject)
	if err != nil {
		return Response{Decision: string(authz.Deny), Reason: "unknown run", IsError: true},
			http.StatusNotFound
	}
	// Cross-check the token against the stored row. A token whose tenant does
	// not match the run's tenant means either a bug or an attack; either way
	// it must not proceed.
	if run.TenantID != claims.Tenant {
		g.log.Error("run token tenant does not match the run's tenant",
			"run", run.ID, "token_tenant", claims.Tenant, "run_tenant", run.TenantID)
		return Response{Decision: string(authz.Deny), Reason: "token does not match run", IsError: true},
			http.StatusForbidden
	}
	def, err := g.store.GetDefinition(ctx, run.DefDigest)
	if err != nil {
		return Response{Decision: string(authz.Deny),
			Reason: "the agent definition for this run is unavailable", IsError: true},
			http.StatusFailedDependency
	}

	// 2. Decide. No credential has been touched yet: a denied call never
	//    causes a secret to be minted at all.
	decision := g.policy.Evaluate(authz.Request{
		Tool: req.Tool, Args: req.Args, Run: run, Spec: def.Spec, Approved: req.Approved,
	})

	// Argument validation is part of the decision, but only for tools that
	// exist and are granted - otherwise an unknown-tool denial would be
	// reported as a schema error and confuse the operator.
	if decision.Effect == authz.Allow {
		tool, _ := g.registry.Get(req.Tool)
		if err := tool.ValidateArgs(req.Args); err != nil {
			decision = authz.Decision{Effect: authz.Deny, Rule: "args.invalid", Reason: err.Error()}
		}
	}

	argsHash := hashArgs(req.Args)

	if decision.Effect != authz.Allow {
		// Denials are audited exactly as thoroughly as successes. A security
		// log that only records what worked answers the wrong question.
		g.audit(ctx, run, def, req.Tool, string(decision.Effect), decision.Reason, req.Args,
			map[string]any{"rule": decision.Rule, "args_sha256": argsHash})
		status := http.StatusForbidden
		if decision.Effect == authz.NeedsApproval {
			status = http.StatusAccepted
		}
		return Response{
			Decision: string(decision.Effect),
			Reason:   decision.Reason,
			Result:   "Tool call refused: " + decision.Reason,
			IsError:  true,
		}, status
	}

	// 3. Idempotency. Reserve the key BEFORE doing anything observable.
	idem := req.IdemKey
	if idem == "" {
		idem = fmt.Sprintf("%s:%d:%s", run.ID, run.Step, argsHash[:16])
	}
	existing, fresh, err := g.store.BeginToolCall(ctx, types.ToolCallRecord{
		IdemKey: idem, RunID: run.ID, TenantID: run.TenantID,
		Tool: req.Tool, ArgsHash: argsHash,
	})
	if err != nil {
		return Response{Decision: string(authz.Deny),
			Reason: "could not journal the call; refusing to execute", IsError: true},
			http.StatusServiceUnavailable
	}
	if !fresh {
		return g.replayResponse(existing, idem)
	}

	// 4. Audit BEFORE the side effect. If the process dies mid-call we still
	//    have a record that we were about to do it, which is the difference
	//    between "we do not know what happened" and "we have no idea it was
	//    even attempted".
	g.audit(ctx, run, def, req.Tool, string(authz.Allow), decision.Rule, req.Args,
		map[string]any{"phase": "pre", "idem_key": idem, "args_sha256": argsHash})

	wsDir, err := g.ws.Dir(run.TenantID, run.ID)
	if err != nil {
		_ = g.store.FinishToolCall(ctx, idem, types.ToolCallFailed, "workspace unavailable", true)
		return Response{Decision: string(authz.Deny), Reason: "workspace unavailable", IsError: true},
			http.StatusInternalServerError
	}

	tool, _ := g.registry.Get(req.Tool)
	out, invokeErr := g.invoker.Invoke(ctx, tools.Invocation{
		Tool: tool, Args: req.Args, RunID: run.ID, TenantID: run.TenantID, Workspace: wsDir,
	})

	// 5. Record the outcome.
	if invokeErr != nil {
		// Infrastructure failure. We genuinely do not know whether the side
		// effect happened, so say so rather than inviting a blind retry.
		msg := "the platform failed to complete this tool call"
		if tool.Backend.UnsafeRetry {
			msg += ". This tool cannot be safely retried automatically: it MAY OR MAY NOT have taken effect. " +
				"Verify the current state before trying again."
		}
		_ = g.store.FinishToolCall(ctx, idem, types.ToolCallFailed, msg, true)
		g.audit(ctx, run, def, req.Tool, "ERROR", invokeErr.Error(), req.Args,
			map[string]any{"phase": "post", "idem_key": idem})
		g.log.Error("tool invocation failed", "run", run.ID, "tool", req.Tool, "err", invokeErr)
		return Response{Decision: string(authz.Allow), Result: msg, IsError: true},
			http.StatusBadGateway
	}

	state := types.ToolCallDone
	if out.IsError {
		state = types.ToolCallFailed
	}
	_ = g.store.FinishToolCall(ctx, idem, state, out.Content, out.IsError)
	g.audit(ctx, run, def, req.Tool, string(authz.Allow), decision.Rule, req.Args,
		mergeMeta(out.Meta, map[string]any{
			"phase": "post", "idem_key": idem, "is_error": out.IsError,
			"result_bytes": len(out.Content), "truncated": out.Truncated,
		}))

	return Response{
		Result: out.Content, IsError: out.IsError,
		Decision: string(authz.Allow), Truncated: out.Truncated,
	}, http.StatusOK
}

// replayResponse handles a repeated idempotency key.
func (g *Gateway) replayResponse(rec types.ToolCallRecord, idem string) (Response, int) {
	switch rec.State {
	case types.ToolCallInFlight:
		// Another worker (or our former self) is mid-call. Returning 409 lets
		// the caller wait and retry rather than duplicating the effect. The
		// reaper eventually resolves a genuinely abandoned call.
		return Response{
			Decision: string(authz.Allow), Replayed: true, IsError: true,
			Reason: "a call with this idempotency key is still in flight",
			Result: "This tool call is already in progress and has not reported an outcome yet.",
		}, http.StatusConflict
	default:
		return Response{
			Result: rec.Result, IsError: rec.IsError,
			Decision: string(authz.Allow), Replayed: true,
			Reason: "replayed from the idempotency journal; the side effect was not repeated",
		}, http.StatusOK
	}
}

func (g *Gateway) audit(ctx context.Context, run types.Run, def types.AgentDefinition,
	tool, decision, reason string, args map[string]any, meta map[string]any) {
	rec := store.AuditRecord{
		TenantID: run.TenantID, RunID: run.ID,
		AgentName: run.AgentName, TriggeringUser: run.TriggeringUser,
		Tool: tool, Decision: decision, Reason: reason,
		ArgsRedacted: redactArgs(args),
		ResultMeta:   mergeMeta(meta, map[string]any{"def_digest": def.Digest}),
	}
	if _, err := g.store.AppendAudit(ctx, rec); err != nil {
		// An audit write failure is serious. We log loudly but do not fail the
		// call that already happened - hiding a completed side effect from the
		// operator would be worse than a gap we have shouted about.
		g.log.Error("AUDIT WRITE FAILED", "run", run.ID, "tool", tool, "err", err)
		g.metrics.AuditFailure()
	}
}

// redactArgs bounds what goes into the audit log.
//
// Arguments are attacker-influenced and can be large (a whole script, a whole
// document). We keep them readable for the auditor but capped, and we never
// store a value under a key that looks like a secret - an agent that was
// (wrongly) handed a token should not have it durably recorded here.
func redactArgs(args map[string]any) map[string]any {
	const maxLen = 2000
	out := make(map[string]any, len(args))
	for k, v := range args {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "token") || strings.Contains(lk, "secret") ||
			strings.Contains(lk, "password") || strings.Contains(lk, "authorization") ||
			strings.Contains(lk, "api_key") || strings.Contains(lk, "apikey") {
			out[k] = "[REDACTED]"
			continue
		}
		if s, ok := v.(string); ok && len(s) > maxLen {
			out[k] = s[:maxLen] + fmt.Sprintf("...[truncated, %d bytes total]", len(s))
			continue
		}
		out[k] = v
	}
	return out
}

func mergeMeta(base, extra map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func hashArgs(args map[string]any) string {
	b, err := canon.Bytes(args)
	if err != nil {
		return "unhashable"
	}
	return canon.HashHex(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
