from svgkit import Diagram, LINE, INK

PURPLE, GREEN, ORANGE, RED, BLUE = "#7a3fd1", "#2e8b57", "#d9822b", "#c0392b", "#2f6fbf"

def build():
    d = Diagram(1560, 1260, "02 · Scheduling and the agent runtime — leases, replay, fenced commits",
                "A run is a row plus an append-only log. Workers are stateless and interchangeable; the only failure detector is a lease that expires.")

    # ── left: run state machine ───────────────────────────────────────────
    d.zone(40, 86, 560, 560, "RUN STATE MACHINE · runs.state", "state", "one txn per transition")
    d.box("queued", 200, 130, 240, 64, "QUEUED", kind="state",
          lines=["wake_at NULL or ≤ now() · lease NULL", "visible to AcquireLease"])
    d.box("running", 200, 262, 240, 76, "RUNNING", kind="platform",
          lines=["lease_owner = worker · lease_expires_at", "invisible to the dispatcher"])
    d.box("done", 70, 390, 130, 58, "SUCCEEDED", kind="state", lines=["model returned", "no tool calls"])
    d.box("failed", 220, 390, 130, 58, "FAILED", kind="external", lines=["budget exceeded", "(only reason today)"])
    d.box("cancelled", 370, 390, 160, 58, "CANCELLED", kind="external", lines=["POST /cancel", "straggler denied at gw"])
    d.box("wh", 70, 490, 220, 62, "WAITING_HUMAN", kind="human",
          lines=["stop_reason human_input_required", "POST /resume → QUEUED"])
    d.box("wa", 320, 490, 220, 62, "WAITING_APPROVAL", kind="human",
          lines=["a Dangerous tool asked", "POST /approve → QUEUED"])

    d.arrow("queued", "running", "AcquireLease · SKIP LOCKED", src_side="s", dst_side="n", color=GREEN, width=2, label_dx=110)
    d.arrow("running", "queued", "yield (4 steps) · quota wait\noutage · lease expired", src_side="w", dst_side="w",
            via=[(120, 300), (120, 162)], color=ORANGE, label_at=(120, 231))
    d.arrow("running", "done", "", src_side="s", dst_side="n", src_off=-80, via=[(240, 364), (135, 364)])
    d.arrow("running", "failed", "", src_side="s", dst_side="n", src_off=-35)
    d.arrow("running", "cancelled", "", src_side="s", dst_side="n", src_off=80, via=[(400, 364), (450, 364)])
    d.arrow("running", "wh", "", src_side="s", dst_side="n", src_off=-105, via=[(215, 470), (180, 470)])
    d.arrow("running", "wa", "", src_side="s", dst_side="n", src_off=40, via=[(360, 470), (430, 470)])
    d.arrow("wh", "queued", "resume", src_side="w", dst_side="n", dst_off=-70,
            via=[(56, 521), (56, 112), (250, 112)], style="dashed", label_at=(150, 112))
    d.arrow("wa", "queued", "approve", src_side="e", dst_side="n", dst_off=70,
            via=[(580, 521), (580, 112), (390, 112)], style="dashed", label_at=(490, 112))
    d.arrow("wa", "cancelled", "cancel from any parked state", src_side="n", dst_side="s", src_off=50, dst_off=30,
            style="dashed", label_at=(455, 476))

    # ── right: the worker loop ────────────────────────────────────────────
    d.zone(640, 86, 880, 560, "ONE WORKER · agentd · stateless · HPA 2–40", "platform",
           "tick every 100 ms; nothing in memory survives a step", subtitle_inside=True)
    d.box("tick", 670, 130, 260, 92, "tick()", kind="platform",
          lines=["eligible = limiter.Eligible(2000 tok)", "none eligible → do nothing (not an error)",
                 "AcquireLease(worker, TTL 30 s, eligible)", "ErrNotFound → sleep 100 ms"])
    d.box("lease", 960, 130, 530, 92, "AcquireLease — the whole scheduler is one query", kind="state", mono=True,
          lines=["SELECT id FROM runs WHERE state='QUEUED' AND wake_at<=now()",
                 "  AND tenant_id = ANY($eligible)          -- fairness, pushed into SQL",
                 "  ORDER BY priority DESC, created_at ASC   -- interactive > normal > batch",
                 "  FOR UPDATE SKIP LOCKED LIMIT 1;  UPDATE runs SET RUNNING, lease_owner, expires"])
    d.box("hb", 670, 252, 260, 64, "heartbeat goroutine", kind="platform",
          lines=["RenewLease every TTL/3 (10 s)", "0 rows updated → cancel our own ctx"])
    d.box("step", 960, 252, 530, 176, "step() — replay, think, act, commit", kind="platform",
          lines=["1 replay   events = ListEvents(run, 0) · def = GetDefinition(run.def_digest) · Rebuild(events)",
                 "2 budget   Budget.ExceedsReason(usage, elapsed) → FAILED before paying for another call",
                 "3 model    Complete(tenant, priority, {system, messages, SchemasFor(def.Tools)})",
                 "           ErrQuotaUnavailable → requeue wake_at+2 s · provider down → requeue +10 s",
                 "4 outcome  human_input_required → WAITING_HUMAN · no tool calls → SUCCEEDED",
                 "5 tools    for i, call: idem = run:step:i → gateway → TOOL_RESULT | TOOL_DENIED",
                 "           NeedsApproval → APPROVAL_NEEDED, park as WAITING_APPROVAL, stop",
                 "6 commit   Commit(run, worker, expectedNextSeq, events, {usage, step+1})"])
    d.box("commit", 960, 458, 530, 96, "Commit — the fenced write (one transaction)", kind="state", mono=True,
          lines=["SELECT next_seq, lease_owner, lease_expires_at FROM runs WHERE id=$1 FOR UPDATE",
                 "owner != me OR expired          → ErrLeaseLost  (abandon; someone else owns it)",
                 "next_seq != expected            → ErrConflict   (our replay is stale)",
                 "INSERT events(seq..) ; UPDATE runs SET next_seq, step, usage, state?, lease?"])
    d.box("yield", 670, 346, 260, 92, "after 4 steps: yield", kind="platform",
          lines=["commit NOTE + state=QUEUED + release", "so a long run cannot pin one worker",
                 "and the queue can rebalance"])
    d.box("defer", 670, 468, 260, 86, "deferred, always", kind="platform",
          lines=["YieldRun(run, me): fenced UPDATE", "WHERE lease_owner=me AND RUNNING",
                 "no-op if we no longer own it", "SIGKILL → lease simply expires"])
    d.box("reaper", 670, 578, 820, 52, "Reaper (any replica, every 3 s) — the entire failure detector", kind="platform",
          lines=["RUNNING AND lease_expires_at < now() → QUEUED  ·  IN_FLIGHT tool_calls older than stuck_after → FAILED 'outcome unknown'"])

    d.arrow("tick", "lease", "", src_side="e", dst_side="w", color=GREEN)
    d.arrow("tick", "hb", "", src_side="s", dst_side="n")
    d.arrow("tick", "step", "run", src_side="e", dst_side="w", src_off=30, dst_off=-40, via=[(945, 207), (945, 300)], label_dx=-14)
    d.arrow("step", "commit", "", src_side="s", dst_side="n", color=GREEN, width=2)
    d.arrow("commit", "step", "ok → step++ (≤4)", src_side="w", dst_side="w", src_off=-20, dst_off=75,
            via=[(945, 486), (945, 415)], color=GREEN, label_at=(890, 452))
    d.arrow("step", "yield", "", src_side="w", dst_side="e", src_off=52, dst_off=0)
    d.arrow("yield", "defer", "", src_side="s", dst_side="n", style="dotted")
    d.arrow("hb", "defer", "lease lost → cancel", src_side="w", dst_side="w", via=[(655, 284), (655, 511)], style="dotted", label_at=(655, 330))

    # ── bottom: fencing timeline ──────────────────────────────────────────
    d.zone(40, 672, 1480, 372, "WHY THE FENCE MATTERS · a paused worker must not overwrite its replacement", "tcb",
           "TestDurability_PartitionedWorkerIsFencedOut asserts this exact sequence against Postgres", subtitle_inside=True)
    lanes = [("worker A", 748), ("Postgres", 846), ("reaper", 916), ("worker B", 986)]
    for name, y in lanes:
        d.text(70, y + 4, [name], size=11.5, weight="700", color=INK)
        d.line(160, y, 1490, y, color=LINE, style="dotted", width=1)
    def ev(x, y, txt, color=INK, w=150, up=False):
        d.line(x, y - 6, x, y + 6, color=color, width=2)
        lines = txt.split("\n")
        y0 = (y + 20) if up else (y - 12 - 14 * (len(lines) - 1))
        d.text(x, y0, lines, size=10, color=color, anchor="middle")
    ev(220, 748, "acquires lease\nseq=5", GREEN)
    ev(360, 748, "replays 0..4\ncalls the model", INK)
    ev(500, 748, "STOP-THE-WORLD\n(GC pause, partition, SIGSTOP)", RED)
    d.line(500, 748, 1150, 748, color=RED, style="dashed", width=2.2)
    ev(560, 846, "lease_expires_at\npasses (30 s)", ORANGE)
    ev(620, 916, "RUNNING + expired\n→ QUEUED", ORANGE)
    ev(720, 986, "acquires lease\nowner=B, seq=5", GREEN, up=True)
    ev(860, 986, "replays 0..4 → same\nidem key run:step:0", INK, up=True)
    ev(1000, 986, "Commit seq 5,6\nok → next_seq=7", GREEN, up=True)
    ev(1150, 748, "wakes up · Commit(seq 5)\nowner=B ≠ A → ErrLeaseLost", RED)
    ev(1300, 748, "abandons the step\nno write happened", INK)
    ev(1300, 986, "continues from 7\nexactly-once tool effects", GREEN, up=True)
    d.note(1000, 842, 500, kind="tcb", title="Why not a heartbeat service or leader election",
           lines=["the lease and the fence are the same row, checked in the same transaction as the write",
                  "there is no window in which A believes it owns the run and B has already written",
                  "A cannot even repeat B's side effect: the gateway replays run:step:i from the journal"])

    d.table(40, 1062, [300, 380, 400, 400], [
        [["Optimisation"], ["Mechanism"], ["Trade-off"], ["Evidence"]],
        [["Backpressure without blocked workers"], ["Eligible() → tenant_id = ANY($1) in the lease query"],
         ["per-process view of quota; stale by ≤ one refill"], ["TestFairness_NoisyTenantCannotStarveQuietTenant"]],
        [["No poll storm on an idle queue"], ["100 ms poll + partial index WHERE state='QUEUED'"],
         ["≤100 ms added latency to first step"], ["500 agents, 16 workers, 1000 tool calls exactly once; p99 1–1.6 s (benchmarks §2)"]],
        [["Replay cost bounded per step"], ["4 steps per lease; context rebuilt from the log each step"],
         ["O(events) reads per step — checkpoints are designed, not built"], ["2→24 workers: 102 → 5 394 runs/s, nothing serialises (benchmarks §3)"]],
    ], kind="platform")

    d.legend = [("state", "durable state / SQL"), ("platform", "worker code"), ("human", "waits on a person"),
                ("external", "terminal failure")]
    return d

if __name__ == "__main__":
    build().save("../svg/02-scheduling-runtime.svg")
