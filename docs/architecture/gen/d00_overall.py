from svgkit import Diagram, LINE, INK

PURPLE, GREEN, ORANGE, RED = "#7a3fd1", "#2e8b57", "#d9822b", "#c0392b"

def build():
    d = Diagram(1600, 1370, "Agent orchestration platform — overall architecture",
                "Every component, all four trust zones, every data flow. “(designed)” = in the design, not built in the PoC.")

    # ── zones ──────────────────────────────────────────────────────────────
    d.zone(40, 86, 1520, 125, "ZONE 0 · UNTRUSTED", "untrusted",
           "outside our control: people, the model provider, third parties, the documents agents read")
    d.zone(40, 262, 1020, 585, "ZONE 1 · PLATFORM", "platform",
           "namespace agentorch · Pod Security restricted · default-deny NetworkPolicy", subtitle_align="right")
    d.zone(700, 318, 315, 498, "ZONE 1a · TCB", "tcb",
           "the only code that ever holds a plaintext credential", subtitle_inside=True)
    d.zone(1100, 262, 460, 585, "ZONE 2 · STATE", "state", "one txn commits log + state + journal + audit")
    d.zone(40, 900, 1520, 300, "ZONE 3 · SANDBOXES", "sandbox",
           "namespace agentorch-sandboxes · tainted node pool · no SA token", subtitle_x=900)

    # ── zone 0 ─────────────────────────────────────────────────────────────
    d.box("user", 70, 114, 240, 82, "Human / CI caller", kind="human",
          lines=["OIDC identity (API key in the PoC)", "creates runs · resumes · approves · cancels"])
    d.box("browser", 340, 114, 240, 82, "Operator console (browser)", kind="human",
          lines=["React SPA served by the control plane", "SSE stream · runs · audit · quota"])
    d.box("llm", 720, 114, 300, 82, "LLM provider", kind="external",
          lines=["external · rate-limited · largest cost line", "sees ONLY tool schemas, never backend config"])
    d.box("third", 1240, 114, 290, 82, "Third-party APIs", kind="external",
          lines=["GitHub, internal services", "receive a ≤60 s tenant-scoped credential"])

    # ── zone 1 · platform ──────────────────────────────────────────────────
    d.box("api", 70, 312, 250, 118, "Control plane", badge="×2",
          lines=["REST + SSE · :8080", "resolves caller → (tenant, user, operator?)",
                 "admission control: MaxConcurrentRuns", "pins the agent definition DIGEST", "holds no credentials"])
    d.box("agentd", 70, 470, 250, 135, "agentd workers", badge="HPA 2–40",
          lines=["STATELESS · interchangeable", "lease → replay log → one step → fenced commit",
                 "holds a run-scoped JWT (≤5 min) only", "egress: Postgres + gateway, nothing else",
                 "MaxStepsPerLease=4 then yield"])
    d.box("reaper", 70, 650, 250, 82, "Reaper",
          lines=["the entire failure detector", "expired leases → QUEUED", "IN_FLIGHT calls → 'outcome unknown'"])
    d.box("modelgw", 345, 312, 260, 118, "Model gateway",
          lines=["single choke point for the LLM", "reserve quota BEFORE the call",
                 "3 attempts, full-jitter backoff", "settle against real usage", "requeue on outage, never fail"])
    d.box("fair", 345, 470, 260, 120, "Fairness limiter",
          lines=["weighted max-min: w_i/Σw × provider rate", "one bucket per tenant + shared spare pool",
                 "20% interactive reserve", "Eligible() → who may be scheduled",
                 "backpressure = NOT scheduling"])
    d.box("registry", 345, 650, 240, 82, "Tool registry",
          lines=["Schema (model-facing) vs", "Backend (gateway-only)", "Tool.MarshalJSON emits schema ONLY"])

    # ── zone 1a · TCB (top→bottom: egress, broker, gateway) ────────────────
    d.box("egress", 715, 350, 290, 100, "Egress proxy  (designed)", kind="tcb",
          lines=["per-run domain allowlist", "the only route out of a broker sandbox",
                 "not built in the PoC — see gaps"])
    d.box("broker", 715, 474, 290, 104, "Credential broker", kind="tcb",
          lines=["mints aot_<tenant>.<id>.<exp>.<sig>", "per-tenant HMAC roots · TTL ≤60 s (cap 10 min)",
                 "creds.Secret redacts everywhere; .Reveal() ×3", "revoke after every call"])
    d.box("gw", 715, 620, 290, 168, "Tool gateway", badge="×2 · PDB", kind="tcb",
          lines=["verify JWT (sig · exp · aud)", "reload run + PINNED digest from store",
                 "DECIDE before any credential is touched", "reserve idem key → audit(pre) → mint",
                 "execute → cap · scrub → audit(post) → revoke", ":8081"])

    # ── zone 2 · state ─────────────────────────────────────────────────────
    d.box("pg", 1140, 312, 390, 262, "PostgreSQL", kind="state", mono=True,
          lines=["runs        row + lease_owner/expires + next_seq",
                 "events      (run_id, seq) append-only — the agent IS this",
                 "tool_calls  idem_key PK · IN_FLIGHT | DONE | FAILED",
                 "audit_log   (tenant_id, seq) hash chain",
                 "agent_definitions  digest PK, immutable",
                 "tenants     weight · tpm · max_concurrent_runs",
                 "",
                 "AcquireLease:  FOR UPDATE SKIP LOCKED",
                 "Commit:  lease fence + next_seq OCC, one txn"])
    d.box("objstore", 1140, 606, 390, 88, "Object storage  (designed)", kind="state",
          lines=["workspace checkpoints at tool-call boundaries", "tiered cold event-log partitions (Parquet)"])
    d.box("redis", 1140, 726, 390, 88, "Redis  (needed for multi-replica)", kind="state",
          lines=["shared fairness buckets — limiter is per-process today", "with local lease/batch to avoid a round trip per call"])

    # ── zone 3 · sandboxes ─────────────────────────────────────────────────
    d.box("asbx", 70, 950, 460, 210, "AGENT SANDBOX", kind="sandbox",
          lines=["runs MODEL-AUTHORED code (exec.bash, exec.python, doc.convert)",
                 "user/mount/pid/net/uts/ipc namespaces · uid 1000 → host 100999",
                 "EMPTY network namespace: no iface up, no addr, no route → ENETUNREACH",
                 "pivot_root onto read-only tmpfs · synthesised /etc · /work + /tmp only writable",
                 "CapBnd = 0 · no_new_privs · 36-syscall seccomp-BPF denylist",
                 "cgroup cpu/mem/pids + rlimits + wall-clock in the PARENT",
                 "prod: RuntimeClass gvisor · cold start 8.4 ms (ns) / 1–3 s (gVisor pod)"])
    d.box("work", 600, 1000, 200, 100, "/work", kind="neutral", mono=True,
          lines=["bind mount, rw", "<root>/<tenant>/<run>", "THE ONLY CHANNEL", "between the two sandboxes"])
    d.box("bsbx", 870, 950, 460, 210, "BROKER SANDBOX", kind="broker",
          lines=["runs a TRUSTED binary from our image (gh, aoconvert) — never model code",
                 "separate pid + user namespace from the agent sandbox",
                 "GH_TOKEN in ITS environment only · same /work mounted",
                 "network: via egress proxy (PoC: host netns, labelled as weaker)",
                 "agent cannot: read its /proc/<pid>/environ · ptrace it · see it in ps",
                 "credential scrubbed from stdout as belt-and-braces",
                 "self-binary bind-mounted at /opt/agentorch/bin (busybox pattern)"])
    d.note(1350, 950, 190, kind="sandbox", title="Node pool",
           lines=["label agentorch.io/workload=sandbox", "taint NoSchedule", "ResourceQuota ceiling",
                  "LimitRange defaults", "Guaranteed QoS", "no control-plane creds", "eBPF runtime detection (prod)"])

    # ── flows ──────────────────────────────────────────────────────────────
    # people
    d.arrow("user", "api", "HTTPS · create / resume / approve / cancel", src_side="s", dst_side="n",
            src_off=60, dst_off=55, label_dy=-30)
    d.arrow("api", "browser", "SSE · JSON", src_side="n", dst_side="s", src_off=105,
            via=[(300, 284), (460, 284)], label_dx=-30)
    # control plane → state, along the top of the platform zone, entering pg from above
    d.arrow("api", "pg", "create run · read log · verify audit chain", src_side="e", dst_side="n",
            src_off=-31, dst_off=-135, via=[(336, 340), (336, 298), (1200, 298)], label_dx=124)
    # workers ↔ state, through the gap between broker and gateway
    d.arrow("agentd", "pg", "lease · replay · fenced commit", src_side="e", dst_side="w",
            src_off=47.5, dst_off=-13, via=[(338, 585), (338, 604), (1108, 604), (1108, 430)],
            color=GREEN, width=2, label_dx=-107, label_dy=2)
    d.arrow("reaper", "pg", "reap expired leases · stuck calls", src_side="s", dst_side="w", dst_off=97,
            via=[(195, 828), (1128, 828), (1128, 540)], style="dotted", label_dx=-263)
    # workers → model plane
    d.arrow("agentd", "modelgw", "Complete()", src_side="n", dst_side="w", src_off=80, dst_off=29,
            via=[(275, 450), (330, 450), (330, 400)], label_dx=-23)
    d.arrow("modelgw", "fair", "reserve / settle", src_side="s", dst_side="n", bidir=True)
    d.arrow("modelgw", "llm", "tokens · quota-governed · tool SCHEMAS only", src_side="n", dst_side="s",
            src_off=60, via=[(535, 232), (870, 232)], color=RED, label_dy=-8)
    # workers → gateway, through the channel under the fairness limiter and up the corridor
    d.arrow("agentd", "gw", "POST /v1/toolcalls · run-scoped JWT · idem key run:step:i", src_side="e", dst_side="w",
            src_off=22.5, dst_off=-4, via=[(328, 560), (328, 628), (660, 628), (660, 700)],
            color=PURPLE, width=2)
    d.arrow("gw", "registry", "lookup · validate args", src_side="w", dst_side="e", src_off=46, dst_off=-1,
            via=[(640, 750), (640, 690)], style="dotted", label_dy=30)
    # gateway → state / broker / outside
    d.arrow("gw", "pg", "journal\n+ audit", src_side="e", dst_side="w",
            src_off=-24, dst_off=57, via=[(1118, 680), (1118, 500)], color=PURPLE, label_dx=-36, label_dy=44.5)
    d.arrow("gw", "broker", "mint ≤60 s · revoke", src_side="n", dst_side="s", color=PURPLE, label_dx=40, label_dy=-9)
    d.arrow("gw", "third", "API tool: the GATEWAY makes the call\nAuthorization header injected",
            src_side="e", dst_side="s", src_off=-64, via=[(1026, 640), (1026, 232), (1385, 232)],
            color=PURPLE, label_dx=179, label_dy=-19)
    # gateway → sandboxes
    d.arrow("gw", "asbx", "exec tool: argv in, stdout out\nNO credential · NO network", src_side="s", dst_side="n",
            src_off=-55, via=[(805, 858), (300, 858)], color=ORANGE, width=2, label_dy=10)
    d.arrow("gw", "bsbx", "CLI tool: argv + GH_TOKEN env\n(policy already blocked `auth token`)", src_side="s", dst_side="w",
            src_off=-10, dst_off=-55, via=[(850, 1000)], color=ORANGE, width=2, label_dy=24)
    d.arrow("bsbx", "egress", "allowlisted\nhosts only", src_side="n", dst_side="e", src_off=190, dst_off=20,
            via=[(1290, 886), (1040, 886), (1040, 420)], style="dashed", label_dx=20, label_dy=-10)
    d.arrow("egress", "third", "", src_side="e", dst_side="s", dst_off=60,
            via=[(1072, 400), (1072, 252), (1445, 252)], style="dashed")
    d.arrow("asbx", "work", "", src_side="e", dst_side="w", style="dashed", color=ORANGE, bidir=True)
    d.arrow("work", "bsbx", "", src_side="e", dst_side="w", style="dashed", color=ORANGE, bidir=True)

    # ── the absences that matter ───────────────────────────────────────────
    d.note(40, 1214, 1000, kind="untrusted", title="What is deliberately ABSENT from this picture",
           lines=["• no arrow from agentd to any third party — NetworkPolicy agentd-egress permits Postgres + gateway only",
                  "• no arrow out of the AGENT SANDBOX — an empty network namespace has nowhere to route to",
                  "• no credential anywhere left of the TCB — workers and the control plane have never held one"])

    d.legend = [("human", "people & browsers"), ("external", "outside our control"),
                ("platform", "trusted platform code"), ("tcb", "trusted computing base"),
                ("state", "durable state"), ("sandbox", "untrusted execution"),
                ("broker", "trusted binary holding a credential"), ("neutral", "shared / neutral")]
    return d

if __name__ == "__main__":
    build().save("../svg/00-overall.svg")
