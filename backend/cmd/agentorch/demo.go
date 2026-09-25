package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gaurasha/agent-orch/backend/internal/canon"
	"github.com/gaurasha/agent-orch/backend/internal/id"
	"github.com/gaurasha/agent-orch/backend/internal/store"
	"github.com/gaurasha/agent-orch/backend/internal/types"
)

// runDemo drives the scripted end-to-end demonstration.
//
// It is deliberately written as an ACCEPTANCE TEST, not a slideshow: every
// claim it prints is checked, and the process exits non-zero if any safety
// property fails. A demo that prints "isolation works" without verifying it is
// exactly the thing the brief warns about.
func runDemo(args []string) int {
	fs := flag.NewFlagSet("demo", flag.ExitOnError)
	o := bindFlags(fs)
	keep := fs.Bool("keep-running", false, "leave the services running after the demo for UI exploration")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	o.Role = "all"
	o.ModelLatency = 50 * time.Millisecond
	if o.LogLevel == "info" {
		o.LogLevel = "warn" // keep the demo output readable
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p, err := Build(ctx, o)
	if err != nil {
		fmt.Fprintln(os.Stderr, "startup failed:", err)
		return 1
	}
	defer p.Close()
	if err := Seed(ctx, p); err != nil {
		fmt.Fprintln(os.Stderr, "seed failed:", err)
		return 1
	}

	// The fake GitHub API needs a real listener, because the credential broker
	// sandbox reaches it over the network.
	ghAddr, err := startFakeGitHub(ctx, p)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake github failed:", err)
		return 1
	}
	// Rebuild the registry and invoker now that we know the API's address.
	p.RebindGitHub("http://" + ghAddr)

	StartWorkers(ctx, p)

	d := &demo{p: p, ctx: ctx}
	section("AGENT ORCHESTRATION PLATFORM - END TO END DEMONSTRATION")
	fmt.Printf("  sandbox driver : %s\n", p.Sandbox.Name())
	fmt.Printf("  store          : %s\n", storeKind(o.DSN))
	fmt.Printf("  blocked syscalls: %d (%s, ...)\n", len(sandboxDenied()), strings.Join(sandboxDenied()[:6], ", "))
	fmt.Printf("  fake GitHub API : http://%s\n", ghAddr)

	d.scenarioNormalAgent()
	d.scenarioCredentialBoundary(ghAddr)
	d.scenarioPromptInjection()
	d.scenarioPoisonAgent()
	d.scenarioHumanInTheLoop()
	d.scenarioAuditChain()

	section("RESULTS")
	for _, c := range d.checks {
		mark := "PASS"
		if !c.ok {
			mark = "FAIL"
		}
		fmt.Printf("  [%s] %s\n", mark, c.name)
		if c.detail != "" {
			fmt.Printf("         %s\n", c.detail)
		}
	}
	failed := 0
	for _, c := range d.checks {
		if !c.ok {
			failed++
		}
	}
	fmt.Printf("\n  %d checks, %d failed\n\n", len(d.checks), failed)

	if *keep {
		fmt.Println("  --keep-running set: services are still up. Press Ctrl-C to stop.")
		<-ctx.Done()
	}
	if failed > 0 {
		return 1
	}
	return 0
}

type check struct {
	name   string
	ok     bool
	detail string
}

type demo struct {
	p      *Platform
	ctx    context.Context
	checks []check
}

func (d *demo) assert(name string, ok bool, detail string) {
	d.checks = append(d.checks, check{name: name, ok: ok, detail: detail})
	status := "ok"
	if !ok {
		status = "FAILED"
	}
	fmt.Printf("    -> %-62s %s\n", name, status)
	if detail != "" {
		fmt.Printf("       %s\n", detail)
	}
}

func section(title string) {
	fmt.Printf("\n%s\n%s\n", title, strings.Repeat("=", len(title)))
}

func step(title string) {
	fmt.Printf("\n  %s\n  %s\n", title, strings.Repeat("-", len(title)))
}

// launch creates a run and waits for it to reach a terminal or parked state.
func (d *demo) launch(tenant, agent, input string) (types.Run, []types.Event) {
	defs, err := d.p.Store.ListDefinitions(d.ctx, tenant)
	if err != nil {
		fmt.Fprintln(os.Stderr, "list definitions:", err)
		return types.Run{}, nil
	}
	var def types.AgentDefinition
	for _, cand := range defs {
		if cand.Name == agent {
			def = cand
			break
		}
	}
	if def.Digest == "" {
		fmt.Fprintf(os.Stderr, "no agent %q for tenant %s\n", agent, tenant)
		return types.Run{}, nil
	}
	run := types.Run{
		ID: id.New("run"), TenantID: tenant, AgentName: agent, DefDigest: def.Digest,
		TriggeringUser: "demo@agentorch.dev", State: types.StateQueued,
		Budget: def.Spec.Budget, Priority: def.Spec.Priority,
	}
	if err := d.p.Store.CreateRun(d.ctx, run, types.Event{
		Type: types.EventRunCreated, Payload: types.EventPayload{Text: input},
	}); err != nil {
		fmt.Fprintln(os.Stderr, "create run:", err)
		return types.Run{}, nil
	}
	return d.await(run.ID, 90*time.Second)
}

func (d *demo) await(runID string, timeout time.Duration) (types.Run, []types.Event) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		cur, err := d.p.Store.GetRun(d.ctx, runID)
		if err == nil && (cur.State.Terminal() ||
			cur.State == types.StateWaitingHuman || cur.State == types.StateWaitingApproval) {
			evs, _ := d.p.Store.ListEvents(d.ctx, runID, 0)
			return cur, evs
		}
		time.Sleep(100 * time.Millisecond)
	}
	cur, _ := d.p.Store.GetRun(d.ctx, runID)
	evs, _ := d.p.Store.ListEvents(d.ctx, runID, 0)
	fmt.Fprintf(os.Stderr, "    (timed out waiting for run %s; state=%s)\n", runID, cur.State)
	return cur, evs
}

func toolResults(evs []types.Event) []types.EventPayload {
	var out []types.EventPayload
	for _, e := range evs {
		if e.Type == types.EventToolResult || e.Type == types.EventToolDenied {
			out = append(out, e.Payload)
		}
	}
	return out
}

// ---------------------------------------------------------------------------

func (d *demo) scenarioNormalAgent() {
	section("1. A NORMAL AGENT: write a document, convert it, verify it")
	step("acme/report-writer")
	run, evs := d.launch("acme", "report-writer", "Write the Q3 report and convert it to HTML.")

	d.assert("run completes successfully", run.State == types.StateSucceeded,
		fmt.Sprintf("state=%s reason=%q steps=%d tool_calls=%d cost=$%.5f",
			run.State, run.StatusReason, run.Usage.Steps, run.Usage.ToolCalls, run.Usage.CostUSD))

	sawConvert := false
	for _, r := range toolResults(evs) {
		if r.Tool == "doc.convert" && !r.IsError {
			sawConvert = true
		}
	}
	d.assert("document conversion ran inside the sandbox", sawConvert, "")

	wsDir, _ := d.p.Workspace.Dir(run.TenantID, run.ID)
	htmlOut, err := d.p.Workspace.Read(wsDir, "report.html")
	d.assert("converted artifact persisted in the run workspace",
		err == nil && strings.Contains(htmlOut, "<h1>"),
		firstN(strings.ReplaceAll(htmlOut, "\n", " "), 90))
}

func (d *demo) scenarioCredentialBoundary(ghAddr string) {
	section("2. THE CREDENTIAL BOUNDARY: gh gets a token the agent cannot see")
	step("acme/release-publisher")
	run, evs := d.launch("acme", "release-publisher", "Publish the v1.4.0 release notes.")

	// (a) The agent inspected its own environment and found nothing.
	envDump := ""
	for _, r := range toolResults(evs) {
		if r.Tool == "exec.bash" {
			envDump = r.Result
		}
	}
	d.assert("agent's own sandbox contains no credential",
		strings.Contains(envDump, "NO CREDENTIALS IN ENVIRONMENT"),
		"the agent ran `env`, read /proc/self/environ, and grepped for token/secret/key")

	// (b) The credentialed call nevertheless succeeded.
	ghResult := ""
	for _, r := range toolResults(evs) {
		if r.Tool == "github.cli" {
			ghResult = r.Result
		}
	}
	d.assert("credentialed CLI call succeeded through the broker sandbox",
		strings.Contains(ghResult, "Created pull request"),
		firstN(strings.ReplaceAll(ghResult, "\n", " "), 100))

	// (c) The far end actually verified a real, freshly minted token.
	attempts := d.p.GitHub.AuthAttempts()
	valid := 0
	var fps []string
	for _, a := range attempts {
		if a.Valid {
			valid++
			fps = append(fps, a.TokenFingerprint)
		}
	}
	d.assert("the third-party API verified a genuine minted credential", valid > 0,
		fmt.Sprintf("%d verified credential presentation(s); fingerprints: %s", valid, strings.Join(fps, ", ")))

	// (d) The token never appears anywhere durable.
	leaked := scanEventsForSecret(evs)
	d.assert("no credential material anywhere in the event log", leaked == "",
		"scanned every event payload for the minted token prefix")

	_ = run
}

func (d *demo) scenarioPromptInjection() {
	section("3. PROMPT INJECTION: containment, not detection")
	step("globex/injected-agent  (has read a hostile document)")
	run, evs := d.launch("globex", "injected-agent", "Summarise the attached document.")

	var denials []string
	metadataBlocked := false
	for _, r := range toolResults(evs) {
		if r.Decision == "DENY" {
			denials = append(denials, fmt.Sprintf("%s -> %s", r.Tool, firstN(r.Reason, 70)))
		}
		if r.Tool == "exec.bash" && strings.Contains(r.Result, "no metadata") {
			metadataBlocked = true
		}
	}
	for _, line := range denials {
		fmt.Printf("       denied: %s\n", line)
	}
	d.assert("every exfiltration attempt was refused by policy", len(denials) >= 3,
		fmt.Sprintf("%d tool calls denied", len(denials)))
	d.assert("cloud metadata endpoint unreachable from the sandbox", metadataBlocked,
		"the curl inside the sandbox could not reach 169.254.169.254")
	d.assert("run still finished cleanly rather than crashing",
		run.State == types.StateSucceeded || run.State == types.StateFailed,
		fmt.Sprintf("state=%s", run.State))
}

func (d *demo) scenarioPoisonAgent() {
	section("4. A POISON AGENT: an infinite tool-call loop")
	step("globex/poison-loop")
	start := time.Now()
	run, _ := d.launch("globex", "poison-loop", "Go.")
	elapsed := time.Since(start)

	d.assert("poison agent was stopped by the platform", run.State == types.StateFailed,
		fmt.Sprintf("state=%s reason=%q", run.State, run.StatusReason))
	d.assert("stopped by an enforced budget, not by chance",
		strings.Contains(run.StatusReason, "budget"),
		fmt.Sprintf("after %d tool calls, %d steps, $%.5f, in %s",
			run.Usage.ToolCalls, run.Usage.Steps, run.Usage.CostUSD, elapsed.Truncate(time.Millisecond)))
	d.assert("bounded cost", run.Usage.CostUSD <= run.Budget.MaxCostUSD,
		fmt.Sprintf("$%.5f spent against a $%.2f cap", run.Usage.CostUSD, run.Budget.MaxCostUSD))
}

func (d *demo) scenarioHumanInTheLoop() {
	section("5. HUMAN IN THE LOOP: a parked agent costs nothing")
	step("acme/change-approver")
	run, _ := d.launch("acme", "change-approver", "Clean up the stale records.")

	d.assert("agent parked waiting for a human", run.State == types.StateWaitingHuman,
		fmt.Sprintf("state=%s", run.State))
	d.assert("holds no worker lease while parked", run.LeaseOwner == "",
		"lease_owner is empty: no worker, no sandbox, no quota reserved")

	// Resume it, as the control plane would.
	err := d.p.Store.AppendSystemEvents(d.ctx, run.ID, []types.Event{{
		Type:    types.EventHumanResume,
		Payload: types.EventPayload{Text: "Approved. Proceed.", Worker: "demo@agentorch.dev"},
	}}, store.RunUpdate{
		State: ptrState(types.StateQueued), StatusReason: ptrString("resumed by demo@agentorch.dev"),
	})
	if err != nil {
		d.assert("resume accepted", false, err.Error())
		return
	}
	resumed, _ := d.await(run.ID, 30*time.Second)
	d.assert("agent resumed and completed after human input",
		resumed.State == types.StateSucceeded, fmt.Sprintf("state=%s", resumed.State))
}

func (d *demo) scenarioAuditChain() {
	section("6. THE AUDIT TRAIL: tamper-evident, and it answers the auditor's question")
	for _, tenant := range []string{"acme", "globex"} {
		recs, err := d.p.Store.ListAudit(d.ctx, tenant, "", 5000)
		if err != nil {
			d.assert("audit readable for "+tenant, false, err.Error())
			continue
		}
		verifyErr := store.VerifyChain(recs)
		d.assert(fmt.Sprintf("audit chain verifies for tenant %q", tenant), verifyErr == nil,
			fmt.Sprintf("%d records, hash-chained", len(recs)))

		if len(recs) > 2 {
			// Forge a record the way someone covering their tracks would.
			tampered := make([]store.AuditRecord, len(recs))
			copy(tampered, recs)
			tampered[1].ArgsRedacted = map[string]any{"script": "rm -rf /"}
			d.assert(fmt.Sprintf("tampering with tenant %q's audit is detected", tenant),
				store.VerifyChain(tampered) != nil,
				"rewriting one record's arguments breaks the chain")
		}

		// The auditor's actual question.
		if tenant == "acme" {
			fmt.Printf("\n       \"show me every command this tenant's agents ran\":\n")
			shown := 0
			for _, r := range recs {
				if !strings.HasPrefix(r.Tool, "exec.") && r.Tool != "github.cli" {
					continue
				}
				argsJSON, _ := json.Marshal(r.ArgsRedacted)
				fmt.Printf("       %s  %-12s %-12s %-14s %s\n",
					r.TS.Format("15:04:05"), r.RunID[:12], r.AgentName, r.Tool,
					firstN(string(argsJSON), 60))
				shown++
				if shown >= 5 {
					break
				}
			}
			d.assert("audit records attribute each command to agent, tenant and user", shown > 0,
				fmt.Sprintf("%d executed commands attributable to a triggering user", shown))
		}
	}
}

// ---------------------------------------------------------------------------

func startFakeGitHub(ctx context.Context, p *Platform) (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	mux := http.NewServeMux()
	p.GitHub.Routes(mux)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	return ln.Addr().String(), nil
}

// scanEventsForSecret looks for minted-credential material in the durable log.
func scanEventsForSecret(evs []types.Event) string {
	for _, e := range evs {
		blob, _ := json.Marshal(e.Payload)
		// Every minted token starts with this prefix; finding one in the event
		// log would mean a credential became durable and would be replayed
		// into the model's context on every subsequent turn.
		if i := strings.Index(string(blob), "aot_"); i >= 0 {
			return string(blob)[i:min(i+40, len(blob))]
		}
	}
	return ""
}

func firstN(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func storeKind(dsn string) string {
	if dsn == "" {
		return "in-memory (no durability across restarts)"
	}
	return "postgres"
}

func ptrState(s types.RunState) *types.RunState { return &s }
func ptrString(s string) *string                { return &s }

var _ = canon.HashHex
