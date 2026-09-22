package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/gaurasha/agent-orch/backend/internal/gateway"
	"github.com/gaurasha/agent-orch/backend/internal/jwtmini"
	"github.com/gaurasha/agent-orch/backend/internal/store"
	"github.com/gaurasha/agent-orch/backend/internal/types"
)

// HTTPToolCaller talks to the tool gateway over the network.
//
// This is the deployed topology: the worker and the gateway are separate
// Deployments with separate ServiceAccounts, separate NetworkPolicies and
// separate blast radii. The worker holds a run-scoped token and nothing else;
// the credentials live only on the gateway side.
type HTTPToolCaller struct {
	baseURL string
	client  *http.Client
	secret  []byte
	// tokenTTL is short on purpose. A run token stolen from a worker is useful
	// for about as long as it takes to notice.
	tokenTTL time.Duration
	store    store.Store
}

func NewHTTPToolCaller(baseURL string, secret []byte, st store.Store) *HTTPToolCaller {
	return &HTTPToolCaller{
		baseURL: baseURL,
		client: &http.Client{
			Timeout: 120 * time.Second, // must exceed the longest tool timeout
		},
		secret: secret, tokenTTL: 5 * time.Minute, store: st,
	}
}

func (c *HTTPToolCaller) Call(ctx context.Context, runID string, req ToolRequest) (ToolResponse, error) {
	run, err := c.store.GetRun(ctx, runID)
	if err != nil {
		return ToolResponse{}, fmt.Errorf("load run for token minting: %w", err)
	}
	now := time.Now()
	token, err := jwtmini.Sign(c.secret, jwtmini.Claims{
		Subject: run.ID, Tenant: run.TenantID, AgentName: run.AgentName,
		DefDigest: run.DefDigest, User: run.TriggeringUser,
		Audience: gateway.Audience,
		IssuedAt: now.Unix(), ExpiresAt: now.Add(c.tokenTTL).Unix(),
	})
	if err != nil {
		return ToolResponse{}, fmt.Errorf("sign run token: %w", err)
	}

	body, err := json.Marshal(gateway.Request{
		Tool: req.Tool, Args: req.Args, IdemKey: req.IdemKey, Approved: req.Approved,
	})
	if err != nil {
		return ToolResponse{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/toolcalls", bytes.NewReader(body))
	if err != nil {
		return ToolResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+token)

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return ToolResponse{}, fmt.Errorf("tool gateway unreachable: %w", err)
	}
	defer resp.Body.Close()

	var out gateway.Response
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ToolResponse{}, fmt.Errorf("decode gateway response (status %d): %w", resp.StatusCode, err)
	}
	return ToolResponse{
		Result: out.Result, IsError: out.IsError, Decision: out.Decision,
		Reason: out.Reason, Truncated: out.Truncated, Replayed: out.Replayed,
		NeedsApproval: resp.StatusCode == http.StatusAccepted,
	}, nil
}

// LocalToolCaller calls the gateway in-process.
//
// Used by the single-binary demo so `make demo` needs no service mesh. The
// policy, credential and audit path is byte-for-byte the same code - only the
// transport differs - so a property demonstrated locally is the same property
// in the deployed topology.
type LocalToolCaller struct {
	gw     *gateway.Gateway
	secret []byte
	store  store.Store
}

func NewLocalToolCaller(gw *gateway.Gateway, secret []byte, st store.Store) *LocalToolCaller {
	return &LocalToolCaller{gw: gw, secret: secret, store: st}
}

func (c *LocalToolCaller) Call(ctx context.Context, runID string, req ToolRequest) (ToolResponse, error) {
	run, err := c.store.GetRun(ctx, runID)
	if err != nil {
		return ToolResponse{}, fmt.Errorf("load run: %w", err)
	}
	// Go through the real HTTP handler rather than calling process() directly,
	// so authentication and request decoding are exercised locally too.
	now := time.Now()
	token, err := jwtmini.Sign(c.secret, jwtmini.Claims{
		Subject: run.ID, Tenant: run.TenantID, AgentName: run.AgentName,
		DefDigest: run.DefDigest, User: run.TriggeringUser,
		Audience: gateway.Audience,
		IssuedAt: now.Unix(), ExpiresAt: now.Add(5 * time.Minute).Unix(),
	})
	if err != nil {
		return ToolResponse{}, err
	}
	body, _ := json.Marshal(gateway.Request{
		Tool: req.Tool, Args: req.Args, IdemKey: req.IdemKey, Approved: req.Approved,
	})
	httpReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, "/v1/toolcalls", bytes.NewReader(body))
	httpReq.Header.Set("Authorization", "Bearer "+token)
	rec := newRecorder()
	mux := http.NewServeMux()
	c.gw.Routes(mux)
	mux.ServeHTTP(rec, httpReq)

	var out gateway.Response
	if err := json.Unmarshal(rec.body.Bytes(), &out); err != nil {
		return ToolResponse{}, fmt.Errorf("decode gateway response: %w", err)
	}
	return ToolResponse{
		Result: out.Result, IsError: out.IsError, Decision: out.Decision,
		Reason: out.Reason, Truncated: out.Truncated, Replayed: out.Replayed,
		NeedsApproval: rec.status == http.StatusAccepted,
	}, nil
}

// recorder is a minimal http.ResponseWriter (httptest is a test-only package).
type recorder struct {
	header http.Header
	body   *bytes.Buffer
	status int
}

func newRecorder() *recorder {
	return &recorder{header: http.Header{}, body: &bytes.Buffer{}, status: http.StatusOK}
}

func (r *recorder) Header() http.Header         { return r.header }
func (r *recorder) Write(b []byte) (int, error) { return r.body.Write(b) }
func (r *recorder) WriteHeader(code int)        { r.status = code }

var (
	_ ToolCaller = (*HTTPToolCaller)(nil)
	_ ToolCaller = (*LocalToolCaller)(nil)
	_            = types.StateQueued
)
