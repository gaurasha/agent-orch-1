from svgkit import Diagram, LINE, INK

PURPLE, GREEN, ORANGE, RED, BLUE, AMBER = "#7a3fd1", "#2e8b57", "#d9822b", "#c0392b", "#2f6fbf", "#b8860b"

def build():
    d = Diagram(1560, 1290, "09 · Failure handling — every failure, the mechanism that absorbs it, and what is deliberately NOT handled",
                "Left: what can break. Right: the seven mechanisms. An arrow means 'this mechanism is what turns that failure into a delay instead of a loss or a duplicate'.")

    d.zone(40, 86, 620, 900, "FAILURES · ordered by how often they happen in production", "untrusted",
           "each was either provoked by a test or reasoned about in DESIGN.md §failure modes", subtitle_inside=True)
    d.zone(900, 86, 620, 900, "MECHANISMS · seven, all in the store or the gateway", "tcb",
           "none needs a coordinator, a broker, a heartbeat service or a cache", subtitle_inside=True)

    F = [("f1", "LLM provider 429 / 5xx / timeout", "hourly", ["the largest external dependency; rate limits are normal, not exceptional"]),
         ("f2", "tenant burst (300 agents at 09:00)", "daily", ["one tenant's demand exceeds its share and the queue"]),
         ("f3", "agentd pod killed mid-step (deploy, OOM, node loss)", "daily", ["the in-memory step is lost; the lease is orphaned"]),
         ("f4", "worker paused: GC, SIGSTOP, network partition", "weekly", ["a zombie that later tries to write stale history"]),
         ("f5", "tool call repeated (retry, replay, double-delivery)", "weekly", ["the same run:step:i arrives twice at the gateway"]),
         ("f6", "gateway dies between side effect and journal write", "monthly", ["outcome genuinely unknown; MUST NOT be retried blindly"]),
         ("f7", "poison agent: loops on tool calls / tokens", "weekly", ["a prompt-injected or buggy agent spending without limit"]),
         ("f8", "sandbox runaway: fork bomb, memory, infinite loop", "weekly", ["model-authored code doing what model-authored code does"]),
         ("f9", "Postgres primary failover", "quarterly", ["every in-flight transaction aborts at once"]),
         ("f10", "control-plane pod restart during SSE", "daily", ["consoles lose their stream mid-run"]),
         ("f11", "bad manifest / kubectl edit / drift", "weekly", ["the running cluster no longer matches the reviewed state"])]
    y = 132
    for fid, title, freq, lines in F:
        d.box(fid, 70, y, 560, 62, title, kind="external", badge=freq, lines=lines)
        y += 76

    M = [("m1", "requeue with wake_at (never FAILED)", "state",
          ["ErrQuotaUnavailable → +2 s · provider down → +10 s", "the run stays QUEUED; a delay, never a loss"]),
         ("m2", "lease TTL + reaper", "state",
          ["RUNNING AND lease_expires_at < now() → QUEUED", "the ENTIRE failure detector; ≤ TTL 30 s + 3 s"]),
         ("m3", "fencing token in Commit", "tcb",
          ["owner = me AND not expired AND next_seq = expected", "a stale writer gets ErrLeaseLost, writes nothing"]),
         ("m4", "idempotency journal (tool_calls)", "tcb",
          ["INSERT ON CONFLICT DO NOTHING on run:step:i", "IN_FLIGHT → 409 · DONE → replay · stuck → 'unknown'"]),
         ("m5", "budgets + admission control", "platform",
          ["Budget.ExceedsReason at worker AND gateway", "max_concurrent_runs → 429 before the queue"]),
         ("m6", "sandbox limits enforced by the parent", "sandbox",
          ["cgroup cpu/mem/pids + rlimits + wall-clock", "killCgroup: every pid, not just the child"]),
         ("m7", "replicas · PDB · HPA · GitOps selfHeal", "k8s",
          ["2× controlplane, 2× gateway, PDB minAvailable=1", "HPA 2–40 workers; Argo reverts drift, prunes extras"])]
    y = 132
    for mid, title, kind, lines in M:
        d.box(mid, 930, y, 560, 84, title, kind=kind, lines=lines)
        y += 118

    edges = [("f1", "m1"), ("f2", "m5"), ("f2", "m1"), ("f3", "m2"), ("f3", "m4"), ("f4", "m3"), ("f4", "m2"),
             ("f5", "m4"), ("f6", "m4"), ("f6", "m2"), ("f7", "m5"), ("f8", "m6"), ("f9", "m2"), ("f9", "m3"),
             ("f10", "m7"), ("f11", "m7")]
    color = {"m1": GREEN, "m2": GREEN, "m3": PURPLE, "m4": PURPLE, "m5": BLUE, "m6": ORANGE, "m7": "#326ce5"}
    # fan the arrow ends across each mechanism's left edge so they do not pile up on one point
    per_m = {}
    for f, m in edges:
        per_m.setdefault(m, []).append(f)
    for f, m in edges:
        n = len(per_m[m]); i = per_m[m].index(f)
        off = (i - (n - 1) / 2) * 18
        d.arrow(f, m, "", src_side="e", dst_side="w", dst_off=off, color=color[m], width=1.4,
                via=[(760 + (i * 7) % 60, d.boxes[f].cy), (760 + (i * 7) % 60, d.boxes[m].cy + off)])

    d.table(40, 1010, [330, 560, 590], [
        [["Deliberately NOT handled (and why)"], ["What would happen"], ["The production step, if it were needed"]],
        [["exactly-once for tools marked UnsafeRetry"], ["the model is told 'MAY OR MAY NOT have taken effect' and must verify before retrying"],
         ["end-to-end idempotency keys in the third-party API (Stripe-style) — not something the platform can promise on its own"]],
        [["loss of the Postgres primary AND its replicas"], ["runs since the last backup are gone; the audit chain has a hole that VerifyChain reports"],
         ["PITR + cross-region replica; the design's tiering to object storage makes cold history independent of the primary"]],
        [["a kernel LPE from inside a namespace sandbox"], ["PoC: host compromise (namespace driver shares the kernel)"],
         ["production driver: gVisor RuntimeClass + tainted pool + no SA token + default-deny — the escape lands nowhere useful"]],
        [["multi-replica fairness"], ["N replicas each admit a full share: up to N× over-admission of the provider budget"],
         ["Redis-backed buckets with a local lease (the documented gap; interface already isolates the limiter)"]],
        [["prompt-injection DETECTION"], ["we do not try: a clever injection is indistinguishable from a legitimate instruction"],
         ["containment is the product: allowlists, no network, no credential in the sandbox, human approval for Dangerous tools"]],
    ], kind="untrusted", size=10)

    d.legend = [("external", "failure"), ("state", "store mechanism"), ("tcb", "gateway / fence"), ("platform", "policy"),
                ("sandbox", "sandbox"), ("k8s", "Kubernetes")]
    return d

if __name__ == "__main__":
    build().save("../svg/09-failure-map.svg")
