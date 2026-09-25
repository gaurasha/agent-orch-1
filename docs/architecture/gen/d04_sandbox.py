from svgkit import Diagram, LINE, INK

PURPLE, GREEN, ORANGE, RED, BLUE = "#7a3fd1", "#2e8b57", "#d9822b", "#c0392b", "#2f6fbf"

def build():
    d = Diagram(1560, 1350, "04 · The agent sandbox — built from raw Linux primitives, layer by layer",
                "What each layer removes, which syscall does it, the order that is load-bearing, and the three drivers that provide the same contract.")

    # ── left: the layers, outermost first ─────────────────────────────────
    d.zone(40, 86, 760, 900, "LAYERS · outermost first · each removes one capability an attacker would need", "sandbox",
           "internal/sandbox/{jail,init,cgroup,seccomp}_linux.go — no libcontainer, no runc, stdlib syscall only", subtitle_inside=True)
    L, LW = 70, 700
    rows = [
        ("cg", "0 · Resource limits — enforced by the PARENT, outside the jail", "platform",
         ["cgroup v1/v2: cpu.max · memory.max (+ OOM detection via memory.events / oom_control) · pids.max",
          "rlimits: NOFILE · NPROC · FSIZE · CORE=0   ·   wall-clock timer in the parent → SIGKILL the whole cgroup",
          "stops: fork bombs (S9), memory bombs (S8), infinite loops (S7), disk filling — attacker cannot raise them"]),
        ("userns", "1 · User namespace — CLONE_NEWUSER", "sandbox",
         ["uid_map 0→0 (1) then 1→100000 (65535); setgroups=deny; inside-root is host uid 0 only for the one setup step",
          "the payload runs as uid 1000 → host uid 100999: an unprivileged nobody even if it escapes the mount ns",
          "why uid 0 first: pivot_root, mount and capset need capabilities that only exist inside the userns"]),
        ("mnt", "2 · Mount namespace — CLONE_NEWNS + pivot_root onto a tmpfs", "sandbox",
         ["MS_PRIVATE recursive (mounts never propagate back to the host) → tmpfs root → bind /usr,/bin,/lib read-only (2nd remount)",
          "synthesised /etc (passwd, group, empty resolv.conf, files-only nsswitch) · fresh /proc · /dev nodes bound (no mknod in userns)",
          "/work rw (nosuid,nodev re-applied) · /tmp rw · pivot_root then umount old root: NO mount references the host (S2, S3, S4)"]),
        ("pid", "3 · PID namespace — CLONE_NEWPID", "sandbox",
         ["the payload is pid 1 of its own tree; it cannot see, signal or ptrace host processes or the broker sandbox",
          "fresh /proc shows only this tree → /proc/<pid>/environ of the credentialed process is unreachable (S12)"]),
        ("net", "4 · Network namespace — CLONE_NEWNET, left EMPTY", "sandbox",
         ["no interface brought up, no address, no route: connect() fails with ENETUNREACH before any packet exists (S1)",
          "not a firewall rule that could be misconfigured — there is nothing to route to; broker sandboxes get NetworkProxy instead"]),
        ("uts", "5 · UTS + IPC namespaces", "sandbox",
         ["hostname 'sandbox' · own SysV IPC / POSIX mqueue — no shared-memory channel to other tenants' processes"]),
        ("nnp", "6 · no_new_privs — prctl(PR_SET_NO_NEW_PRIVS)", "tcb",
         ["first, because it is the precondition for an unprivileged seccomp install and it neuters setuid/fscaps forever"]),
        ("caps", "7 · Capability bounding set → ∅, then setuid(1000), then clear all sets", "tcb",
         ["CAP_SETPCAP to drop the bounding set, CAP_SETUID/SETGID to switch — both still held as inside-uid 0",
          "order matters: caps before uid → setgroups EPERM → glibc abort() → SIGABRT (bug B1). After: no execve can regain a cap (S5)"]),
        ("seccomp", "8 · seccomp-BPF denylist — hand-assembled filter, SECCOMP_MODE_FILTER", "tcb",
         ["arch check (kill on foreign ABI) · x32 bit → KILL · clone3 → ENOSYS (forces the filterable clone path)",
          "35 → EPERM: mount · pivot_root · chroot · unshare · setns · ptrace · *_module · kexec_* · bpf · keyctl · io_uring_* … + clone3 = 36 (S6)",
          "installed LAST so the filter need not allow the privilege-dropping syscalls above"]),
        ("exec", "9 · execve(payload)", "platform",
         ["lookPathIn(argv[0]) using the sandbox PATH (/opt/agentorch/bin first) · env = SafeEnv(): PATH, HOME=/work, LANG, plus tool extras"]),
    ]
    y = 132
    for bid, title, kind, lines in rows:
        h = 37 + 14.5 * len(lines) + 4
        d.box(bid, L, y, LW, h, title, kind=kind, lines=lines)
        y += h + 10
    ids = [r[0] for r in rows]
    for a, b in zip(ids, ids[1:]):
        d.arrow(a, b, "", src_side="s", dst_side="n", src_off=-320, dst_off=-320, color=ORANGE, width=1.6)

    # ── right top: the parent/child handshake ─────────────────────────────
    d.zone(830, 86, 690, 560, "LIFECYCLE · parent (gateway) ↔ child (re-exec'd self as init)", "platform",
           "the jail is built INSIDE the new namespaces, which only exist after clone(2)", subtitle_inside=True)
    lanes = [("parent", 150), ("child", 590)]
    d.box("p1", 860, 130, 300, 74, "parent · prepare", kind="platform",
          lines=["Spec{argv, env, workspace, limits, network}", "jailConfig JSON → fd 3 · sync pipe → fd 4", "cgroup created, limits written"])
    d.box("p2", 860, 226, 300, 60, "parent · clone", kind="platform",
          lines=["/proc/self/exe as init with CLONE_NEWUSER|NS|PID|NET|UTS|IPC", "write uid_map, gid_map, setgroups=deny"])
    d.box("c1", 1200, 226, 290, 60, "child · blocks on fd 4", kind="sandbox",
          lines=["MaybeRunInit() at the top of main()", "unbounded until released → does nothing"])
    d.box("p3", 860, 308, 300, 60, "parent · confine, then release", kind="platform",
          lines=["add child pid to cgroup.procs", "write 1 byte to the sync pipe"])
    d.box("c2", 1200, 308, 290, 130, "child · builds the jail (layers 2–9)", kind="sandbox",
          lines=["sethostname · buildRootfs · applyRlimits", "no_new_privs → bounding set → setgroups/gid/uid",
                 "→ clear caps → seccomp → execve", "setup failure: exit 125 + marker so the parent",
                 "reports 'our bug', not 'your script failed'", "payload writes to /work; stdout/stderr → pipes"])
    d.box("p4", 860, 390, 300, 118, "parent · supervise", kind="platform",
          lines=["capWriter on stdout/stderr (MaxOutputBytes/2 each)", "wall-clock context → on timeout killCgroup()",
                 "(every pid in the cgroup, not just the child)", "OOMKilled from memory.events / oom_control",
                 "exit code classified · Result{…, Truncated, Driver}"])
    d.box("p5", 860, 530, 630, 90, "Result → gateway", kind="state", mono=True,
          lines=["ExitCode · Stdout · Stderr · Truncated · OOMKilled · TimedOut · Duration · Driver",
                 "formatExecResult() renders it for the model; Scrub() removes any credential (cli path)",
                 "measured cold start: 8.4 ms median (namespace driver) — S11"])
    d.arrow("p1", "p2", "", src_side="s", dst_side="n", color=BLUE)
    d.arrow("p2", "c1", "clone", src_side="e", dst_side="w", color=BLUE)
    d.arrow("p2", "p3", "", src_side="s", dst_side="n", color=BLUE)
    d.arrow("p3", "c2", "", src_side="e", dst_side="w", dst_off=-35, color=BLUE)
    d.arrow("c1", "c2", "", src_side="s", dst_side="n", color=ORANGE)
    d.arrow("p3", "p4", "", src_side="s", dst_side="n", color=BLUE)
    d.arrow("c2", "p4", "exit / output", src_side="s", dst_side="e", src_off=0, dst_off=20,
            via=[(1345, 469)], color=ORANGE, label_at=(1260, 460))
    d.arrow("p4", "p5", "", src_side="s", dst_side="n", src_off=-100, dst_off=-265, color=BLUE)

    # ── right bottom: drivers ─────────────────────────────────────────────
    d.zone(830, 672, 690, 314, "DRIVERS · one Spec, one Result, three isolation strengths", "k8s",
           "sandbox.New(name, cfg) picks one; the gateway never knows which", subtitle_inside=True)
    d.table(850, 716, [150, 170, 170, 160], [
        [["Driver"], ["namespace (PoC default)"], ["docker / podman"], ["kubernetes (production)"]],
        [["Isolation"], ["all 6 namespaces + seccomp,", "shared host kernel"], ["same flags via CLI,", "shared host kernel"],
         ["pod + RuntimeClass gvisor:", "user-space kernel (runsc)"]],
        [["Kernel LPE →"], ["host compromise"], ["host compromise"], ["gVisor sentry, not the host"]],
        [["Cold start"], ["8.4 ms measured"], ["≈300–800 ms"], ["1–3 s (schedule + image)"]],
        [["Network"], ["empty netns / host (broker)"], ["--network none"], ["NetworkPolicy default-deny"]],
        [["Where"], ["laptop, CI, demo"], ["laptop convenience"], ["tainted node pool, no SA token"]],
        [["Why it exists"], ["measure the floor; prove the", "layers with no framework"],
         ["dev parity with the PodSpec", "(one-to-one flag table)"], ["the security property the", "design actually promises"]],
    ], kind="k8s", size=10)

    d.table(40, 1010, [300, 400, 400, 380], [
        [["Failure inside the sandbox"], ["Detection"], ["Reaction"], ["Proof"]],
        [["fork bomb"], ["pids.max reached → fork() EAGAIN"], ["payload's own forks fail; parent kills the cgroup at exit"],
         ["TestSandbox_ForkBombIsContained (os.fork loop, asserts refusal at ≈ pids.max)"]],
        [["memory bomb"], ["memory.max → OOM killer; oom_kill counter read"], ["Result.OOMKilled=true; the model is told"],
         ["TestSandbox_MemoryLimitIsEnforced (reads oom_kill, not failcnt — bug fix)"]],
        [["infinite loop / sleep"], ["parent wall-clock context"], ["killCgroup(): every pid, TimedOut=true"],
         ["TestSandbox_WallClockTimeoutIsEnforced"]],
        [["escape attempt (mount, ptrace, unshare…)"], ["seccomp EPERM / clone3 ENOSYS / CapBnd=∅"], ["syscall fails; nothing to log from inside"],
         ["TestSandbox_EscapeSyscallsAreBlocked + RunsUnprivilegedWithNoCapabilities"]],
        [["network reach (exfiltration)"], ["ENETUNREACH from connect()"], ["no packet is ever emitted"],
         ["TestSandbox_HasNoNetworkAccess (Python socket + host negative control — bug B2)"]],
        [["output flood"], ["capWriter hits MaxOutputBytes/2"], ["Truncated=true; the rest is dropped, not buffered"],
         ["TestSandbox_OutputIsCapped"]],
        [["jail fails to build (our bug)"], ["exit 125 + setup marker on stderr"], ["ErrSetup to the gateway → 502, not 'your script failed'"],
         ["exitcode.go"]],
    ], kind="sandbox")

    d.legend = [("sandbox", "namespace layer"), ("tcb", "privilege-drop step"), ("platform", "parent / platform code"),
                ("state", "result"), ("k8s", "Kubernetes")]
    return d

if __name__ == "__main__":
    build().save("../svg/04-sandbox.svg")
