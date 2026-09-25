from svgkit import Diagram, LINE, INK

PURPLE, GREEN, ORANGE, RED, BLUE = "#7a3fd1", "#2e8b57", "#d9822b", "#c0392b", "#2f6fbf"

def build():
    d = Diagram(1560, 1440, "03 · Tool gateway — the single choke point, stage by stage",
                "gateway.process(): every stage can refuse; a credential is minted only after the decision; every call leaves two audit records.")

    d.zone(40, 86, 660, 1150, "gateway.process() · :8081 · 2 replicas · PDB", "tcb",
           "inside the TCB: the only code that can reach a credential or a sandbox", subtitle_inside=True)
    d.zone(720, 86, 360, 1150, "REFUSALS · fail closed, audited", "untrusted",
           "HTTP status → what the agent is told", subtitle_inside=True)
    d.zone(1100, 86, 420, 1150, "DESIGN NOTES", "neutral",
           "the invariants each stage protects", subtitle_inside=True)

    X, W = 70, 600
    d.box("req", X, 130, W, 86, "0 · Request arrives", kind="platform", mono=True,
          lines=["POST /v1/toolcalls   Authorization: Bearer <run JWT>",
                 "{tool, args, idem_key, approved}",
                 "JWT: HS256 · sub=run_id · tenant · aud=tool-gateway · exp ≤ 5 min"])
    d.box("verify", X, 246, W, 86, "1 · Verify the token, then RELOAD the truth from the store", kind="tcb",
          lines=["jwtmini.Verify(secret, token, aud) → claims  (signature · expiry · audience)",
                 "run = GetRun(claims.sub) · run.TenantID must equal claims.tenant",
                 "def = GetDefinition(run.DefDigest) — the PINNED definition, not the latest"])
    d.box("decide", X, 362, W, 208, "2 · Decide — authz.Evaluate, in this exact order", kind="tcb", mono=True,
          lines=["run.terminal        cancelled/finished runs cannot act through a straggler",
                 "budget.exhausted    tokens · cost · wall-clock · steps · tool calls",
                 "tool.unknown        hallucinated or probing for an internal tool",
                 "tool.not_granted    def.Spec.Tools must list it  (capability check)",
                 "args.missing        schema-required arguments present",
                 "params.*            host allowlist · argv allowlist · denied patterns",
                 "                    bad_url · bad_scheme · ssrf (link-local, private, .internal)",
                 "approval.required   Dangerous tool or RequiresApproval, and not yet approved",
                 "args.invalid        tool.ValidateArgs — types, only for granted tools",
                 "                                          → default.granted"])
    d.box("idem", X, 600, W, 96, "3 · Reserve the idempotency key BEFORE anything observable", kind="state", mono=True,
          lines=["idem = req.idem_key  (worker sends run:step:i)  else run:step:sha256(args)[:16]",
                 "INSERT tool_calls(idem_key, …, state='IN_FLIGHT') ON CONFLICT DO NOTHING",
                 "not fresh → replayResponse: IN_FLIGHT → 409 · DONE/FAILED → 200 Replayed=true",
                 "journal write failed → refuse to execute (503)"])
    d.box("pre", X, 726, W, 62, "4 · audit(pre) — 'we are about to do this'", kind="state",
          lines=["record {phase:pre, idem_key, args_sha256, rule} in the tenant's hash chain",
                 "if the process dies during the call, the intent is already on record"])
    d.box("invoke", X, 818, W, 178, "5 · Execute by backend kind (tools.Invoker)", kind="tcb",
          lines=["exec  → agent sandbox · argv + workspace · NO network · NO credential (exec.bash, exec.python, doc.convert)",
                 "fs    → fs.read / fs.write / fs.list on the run's workspace, path-validated, no sandbox",
                 "http  → mint(CredRef, 60 s) → the GATEWAY makes the request with the header → revoke",
                 "cli   → mint(github/token, 60 s) → BROKER sandbox with GH_TOKEN env → Scrub(stdout) → revoke",
                 "",
                 "cap output (MaxOutputBytes, split stdout/stderr) · Truncated flag · wall-clock in the parent",
                 "the credential exists for one call: ≤ 60 s, revoked in a defer that runs on every path"])
    d.box("post", X, 1026, W, 96, "6 · Record the outcome, then audit(post)", kind="state",
          lines=["infrastructure error → tool_calls.state=FAILED, is_error; if UnsafeRetry the agent is told",
                 "  'MAY OR MAY NOT have taken effect — verify before retrying' (never a blind retry)",
                 "else → DONE or FAILED(is_error) with the capped content",
                 "audit(post): {phase:post, idem_key, is_error, result_bytes, truncated, driver, exit_code…}"])
    d.box("resp", X, 1152, W, 62, "7 · Respond", kind="platform", mono=True,
          lines=["200 {result, is_error, decision:ALLOW, truncated}",
                 "the worker appends TOOL_RESULT (or TOOL_DENIED) to the run's event log"])

    for a, b in [("req", "verify"), ("verify", "decide"), ("decide", "idem"), ("idem", "pre"),
                 ("pre", "invoke"), ("invoke", "post"), ("post", "resp")]:
        d.arrow(a, b, "", src_side="s", dst_side="n", color=PURPLE, width=2)

    # right-hand notes inside the TCB zone: the invariants
    d.note(1120, 130, 380, kind="tcb", title="Why this order",
           lines=["decide → reserve → audit → mint → execute:",
                  "a DENIED call never mints a credential;",
                  "a REPEATED call never repeats a side effect;",
                  "an ABANDONED call is never silent.",
                  "",
                  "The registry's MarshalJSON emits the schema",
                  "only, so the model never sees Backend{}",
                  "(binary, CredRef, timeouts, Dangerous)."])
    d.note(1120, 290, 380, kind="tcb", title="Where the request's claims are NOT trusted",
           lines=["tenant: taken from the stored run, not the token",
                  "tool grants: from the pinned definition digest",
                  "args: validated against the tool's schema",
                  "idem key: accepted, but scoped by run in the store",
                  "approved flag: only honoured after the human's",
                  "APPROVAL_GIVEN event re-queued the run"])
    d.note(1120, 430, 380, kind="platform", title="Parameter policy examples (from the demo agent)",
           lines=["http.get   allowed_hosts: [api.github.com]",
                  "github.cli argv[0] allowlist: [pr, issue, repo]",
                  "           denied_arg_patterns: ['auth token']",
                  "http.post  Dangerous → WAITING_APPROVAL",
                  "exec.*     no params policy — the sandbox IS",
                  "           the policy (no network, ro root)"])
    d.note(1120, 570, 380, kind="state", title="Two records per allowed call",
           lines=["pre  : intent, before the side effect",
                  "post : outcome, after it",
                  "A denial writes exactly one record.",
                  "Chain: hash = sha256(prev_hash ‖ canonical(rec))",
                  "ts truncated to µs (Postgres resolution);",
                  "VerifyChain recomputes the whole tenant chain."])
    d.note(1120, 700, 380, kind="sandbox", title="Exec path details",
           lines=["namespace driver: 8.4 ms cold start",
                  "workspace /work bind-mounted rw",
                  "stdout/stderr capped at MaxOutputBytes/2 each",
                  "exit 126/127 vs jail-setup failure told apart",
                  "cgroup killed on timeout — not just the child"])
    d.note(1120, 830, 380, kind="broker", title="CLI path details",
           lines=["our own binary bound at /opt/agentorch/bin/gh",
                  "dispatch on argv[0] (busybox pattern)",
                  "separate pid + user ns from the agent sandbox",
                  "GH_TOKEN visible to that process only",
                  "PoC gap: broker sandbox uses the host netns"])
    d.note(1120, 960, 380, kind="neutral", title="What the gateway does NOT do",
           lines=["no retries of tool calls (the agent decides)",
                  "no streaming of tool output (capped, returned whole)",
                  "no per-tenant network egress today (designed: proxy)",
                  "no cross-replica idempotency cache — Postgres is it"])

    # refusals column
    R, RW = 740, 320
    d.box("r401", R, 130, RW, 50, "401 · invalid or expired run token", kind="external",
          lines=["the worker's JWT is ≤ 5 min; it re-signs per call"])
    d.box("r404", R, 246, RW, 86, "404 unknown run · 403 mismatch · 424 no def", kind="external",
          lines=["404 not 403: a foreign run id must look like nothing",
                 "403 tenant mismatch is logged as a possible attack",
                 "424: the pinned digest must exist — never fall back to 'latest'"])
    d.box("r403", R, 362, RW, 208, "403 DENY · 202 NEEDS_APPROVAL", kind="external",
          lines=["every deny is audited with {rule, args_sha256}",
                 "the agent is told: 'Tool call refused: <reason>'",
                 "  → the model sees the reason and can re-plan",
                 "  → the worker records TOOL_DENIED in the log",
                 "NEEDS_APPROVAL: 202; the worker parks the run as",
                 "  WAITING_APPROVAL; the human approves; the run",
                 "  re-queues and the SAME idem key is retried",
                 "  with approved=true (the journal has no row yet:",
                 "  approval happens before stage 3)",
                 "budget.exhausted also fails the run at the worker",
                 "the demo's prompt-injection scenario exercises 4",
                 "of these rules in one run (safety proof S14)"])
    d.box("r409", R, 600, RW, 96, "409 IN_FLIGHT · 200 REPLAYED · 503", kind="external",
          lines=["409: another worker (or our former self) is mid-call;",
                 "  the worker reports 'in progress' — never duplicates",
                 "200 Replayed: recorded result, side effect NOT repeated",
                 "503: cannot journal → refuse; nothing observable happened"])
    d.box("r500", R, 726, RW, 62, "500 · workspace unavailable", kind="external",
          lines=["tool_calls row finished as FAILED before returning",
                 "audit(pre) already written → the attempt is on record"])
    d.box("r502", R, 818, RW, 178, "502 · platform could not complete it", kind="external",
          lines=["sandbox setup failure (exit 125 + marker) vs payload failure",
                 "  are told apart so the agent is not blamed for our bug",
                 "UnsafeRetry tools (http.post): 'MAY OR MAY NOT have",
                 "  taken effect' — the model must verify state first",
                 "the credential is still revoked (defer) and the post",
                 "  audit record says ERROR with the cause",
                 "gateway crash mid-call: no post record, row stays",
                 "  IN_FLIGHT → reaper marks FAILED 'outcome unknown'",
                 "  after stuck_after; the run is told the truth"])
    d.box("rcap", R, 1026, RW, 96, "200 with is_error / truncated", kind="external",
          lines=["a tool's own failure (exit≠0, 4xx) is a normal result",
                 "truncated=true tells the model the output was capped",
                 "denied CLI arguments never reach the broker sandbox:",
                 "  policy ran at stage 2, before any mint"])

    for a, b in [("req", "r401"), ("verify", "r404"), ("decide", "r403"), ("idem", "r409"),
                 ("pre", "r500"), ("invoke", "r502"), ("post", "rcap")]:
        d.arrow(a, b, "", src_side="e", dst_side="w", color=RED, style="dashed")

    d.table(40, 1256, [330, 380, 380, 390], [
        [["Trade-off taken"], ["Because"], ["Cost"], ["Where it is argued"]],
        [["One gateway process type, on the hot path"], ["authorization must be un-bypassable; a library can be skipped"],
         ["+1 network hop per tool call (≈1 ms in-cluster); PDB + 2 replicas"], ["DEEP_DIVE D5 · reasoning/05-authorization"]],
        [["Deny-by-default + allowlists over a detector"], ["prompt injection cannot be reliably detected; it can be contained"],
         ["agents need explicit grants; onboarding a tool is a policy change"], ["docs/01-concepts/08 · reasoning/10"]],
        [["Idempotency journal in Postgres, not in memory"], ["a replacement worker on another node must see the same journal"],
         ["one INSERT per call; ~5 % of tool-call latency"], ["benchmarks §2 exactly-once check"]],
    ], kind="tcb")

    d.legend = [("tcb", "TCB code"), ("state", "durable write"), ("platform", "trusted code"),
                ("external", "refusal / failure path"), ("sandbox", "agent sandbox"), ("broker", "broker sandbox")]
    return d

if __name__ == "__main__":
    build().save("../svg/03-tool-gateway.svg")
