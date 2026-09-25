package main

import (
	"context"
	"crypto/rand"
	"fmt"

	"github.com/gaurasha/agent-orch/backend/internal/canon"
	"github.com/gaurasha/agent-orch/backend/internal/types"
)

// Seed installs the demo tenants, credentials and agent definitions.
//
// Two tenants with different weights, because the fairness story only means
// something with more than one. Agent definitions are deliberately varied in
// their GRANTS, not just their prompts: the "injected" agent is granted almost
// nothing, which is what makes its exfiltration attempts fail at the policy
// layer rather than at the model's discretion.
func Seed(ctx context.Context, p *Platform) error {
	tenants := []types.Tenant{
		{ID: "acme", Name: "Acme Corp", Weight: 2, TokensPerMinute: 300_000, MaxConcurrentRuns: 500},
		{ID: "globex", Name: "Globex Inc", Weight: 1, TokensPerMinute: 150_000, MaxConcurrentRuns: 300},
	}
	for _, t := range tenants {
		if err := p.Store.PutTenant(ctx, t); err != nil {
			return fmt.Errorf("seed tenant %s: %w", t.ID, err)
		}
		// Each tenant gets its OWN root secret. A credential minted for acme
		// cannot be verified as globex's, so a cross-tenant replay fails at the
		// far end as well as in our own policy.
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return fmt.Errorf("generate tenant root secret: %w", err)
		}
		p.Broker.AddRoot(t.ID, "github/token", "github_token", key)
		httpKey := make([]byte, 32)
		if _, err := rand.Read(httpKey); err != nil {
			return err
		}
		p.Broker.AddRoot(t.ID, "http/default", "bearer", httpKey)
	}
	p.Limiter.SetTenants(tenants)

	defs := []struct {
		tenant string
		name   string
		spec   types.Spec
	}{
		{
			tenant: "acme", name: "report-writer",
			spec: types.Spec{
				SystemPrompt: "You write and convert business reports. Work in /work.",
				Model:        "fake:report-writer",
				Tools:        []string{"fs.write", "fs.read", "fs.list", "doc.convert", "exec.bash"},
				Budget:       types.Budget{MaxSteps: 10, MaxToolCalls: 20, MaxTokens: 100_000, MaxCostUSD: 1.0, MaxWallSeconds: 600},
				Priority:     types.PriorityNormal,
			},
		},
		{
			tenant: "acme", name: "release-publisher",
			spec: types.Spec{
				SystemPrompt: "You prepare and publish release notes to GitHub.",
				Model:        "fake:github-publisher",
				Tools:        []string{"fs.write", "fs.read", "exec.bash", "github.cli"},
				ToolParams: map[string]types.ParamPolicy{
					"github.cli": {
						// Allowed to open a pull request; NOT allowed to ask the
						// CLI for its own credential, nor to touch secrets.
						// This is the parameter-level policy that makes a broad
						// CLI grant safe.
						DeniedArgPatterns: []string{"auth token", "auth status", "secret", "variable"},
					},
				},
				Budget:   types.Budget{MaxSteps: 10, MaxToolCalls: 20, MaxTokens: 100_000, MaxCostUSD: 1.0, MaxWallSeconds: 600},
				Priority: types.PriorityInteractive,
			},
		},
		{
			tenant: "acme", name: "change-approver",
			spec: types.Spec{
				SystemPrompt: "You prepare changes and ask a human before anything destructive.",
				Model:        "fake:needs-human",
				Tools:        []string{"fs.write", "fs.read"},
				Budget:       types.DefaultBudget(),
				Priority:     types.PriorityInteractive,
			},
		},
		{
			tenant: "globex", name: "poison-loop",
			spec: types.Spec{
				SystemPrompt: "A deliberately misbehaving agent that never stops calling tools.",
				Model:        "fake:poison-loop",
				Tools:        []string{"exec.bash"},
				// A tight budget is the containment. Without it this agent runs
				// until someone notices the bill.
				Budget:   types.Budget{MaxSteps: 6, MaxToolCalls: 5, MaxTokens: 50_000, MaxCostUSD: 0.5, MaxWallSeconds: 120},
				Priority: types.PriorityBatch,
			},
		},
		{
			tenant: "globex", name: "injected-agent",
			spec: types.Spec{
				SystemPrompt: "You summarise documents. You have read a document containing hostile instructions.",
				Model:        "fake:injected",
				// Granted ONLY what it needs for its stated job. Every
				// exfiltration route the injected prompt tries is outside this
				// set, so the attempts fail at the gateway rather than relying
				// on the model to refuse.
				Tools: []string{"fs.write", "fs.read", "exec.bash"},
				ToolParams: map[string]types.ParamPolicy{
					"http.get": {AllowedHosts: []string{"api.github.com", ".acme.example"}},
				},
				Budget:   types.Budget{MaxSteps: 10, MaxToolCalls: 12, MaxTokens: 80_000, MaxCostUSD: 1.0, MaxWallSeconds: 300},
				Priority: types.PriorityBatch,
			},
		},
		{
			tenant: "globex", name: "load-agent",
			spec: types.Spec{
				SystemPrompt: "Minimal agent used for load testing the scheduler.",
				Model:        "fake:load",
				Tools:        []string{"fs.write"},
				Budget:       types.Budget{MaxSteps: 5, MaxToolCalls: 5, MaxTokens: 20_000, MaxCostUSD: 0.2, MaxWallSeconds: 120},
				Priority:     types.PriorityBatch,
			},
		},
	}

	for _, d := range defs {
		digest, err := canon.Digest(d.spec)
		if err != nil {
			return fmt.Errorf("digest %s: %w", d.name, err)
		}
		if err := p.Store.PutDefinition(ctx, types.AgentDefinition{
			Digest: digest, TenantID: d.tenant, Name: d.name, Spec: d.spec,
		}); err != nil {
			return fmt.Errorf("seed agent %s: %w", d.name, err)
		}
	}
	p.Log.Info("seeded demo data", "tenants", len(tenants), "agents", len(defs))
	return nil
}
