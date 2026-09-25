//go:build linux

package sandbox_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gaurasha/agent-orch/backend/internal/sandbox"
)

// TestMain must give the sandbox a chance to recognise that this process was
// re-executed as a jail init. /proc/self/exe is the test binary, so without
// this hook the child would re-run the test suite inside the namespace.
func TestMain(m *testing.M) {
	if sandbox.MaybeRunInit() {
		return
	}
	os.Exit(m.Run())
}

func newDriver(t *testing.T) sandbox.Driver {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("namespace driver needs root (or CAP_SYS_ADMIN) to build the jail")
	}
	d, err := sandbox.NewNamespaceDriver(sandbox.Config{
		StateDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("new namespace driver: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func run(t *testing.T, d sandbox.Driver, spec sandbox.Spec) sandbox.Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	res, err := d.Run(ctx, spec)
	if err != nil {
		t.Fatalf("sandbox run failed to set up: %v (stderr=%q)", err, res.Stderr)
	}
	return res
}

func python(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available; needed to make real syscalls inside the jail")
	}
	return p
}

// ---------------------------------------------------------------------------
// The claim: a sandboxed payload has NO network access whatsoever.
// ---------------------------------------------------------------------------

// This is the headline safety property, so it is tested with a real connect(2)
// and paired with a negative control. An earlier version of this test used a
// bash /dev/tcp redirection and "passed" on a system where /bin/sh is dash -
// it was failing on "no such file", not on network isolation. A test that can
// pass for the wrong reason is worse than no test.
func TestSandbox_HasNoNetworkAccess(t *testing.T) {
	py := python(t)
	d := newDriver(t)

	const probe = `
import socket, sys, errno
targets = [("169.254.169.254", 80), ("1.1.1.1", 443), ("8.8.8.8", 53)]
for host, port in targets:
    s = socket.socket(); s.settimeout(3)
    try:
        s.connect((host, port))
        print("REACHED %s:%d" % (host, port)); sys.exit(1)
    except OSError as e:
        print("denied %s:%d %s" % (host, port, errno.errorcode.get(e.errno, e.errno)))
    finally:
        s.close()
print("interfaces=" + ",".join(l.split(":")[0].strip()
      for l in open("/proc/net/dev").read().splitlines()[2:]))
# In an empty netns /proc/net/route is entirely empty - not even the header
# line that a namespace with interfaces would have. Clamp so an empty file
# reports 0 routes rather than -1.
print("routes=%d" % max(0, len(open("/proc/net/route").read().splitlines()) - 1))
print("ALL_DENIED")
`
	res := run(t, d, sandbox.Spec{
		RunID: "net-test", TenantID: "t1",
		Argv: []string{py, "-c", probe},
	})

	if !strings.Contains(res.Stdout, "ALL_DENIED") {
		t.Fatalf("sandbox reached the network.\nstdout:\n%s\nstderr:\n%s", res.Stdout, res.Stderr)
	}
	// The metadata endpoint is the one that matters most: on a cloud node it
	// hands out node credentials to anything that can make an HTTP request.
	if !strings.Contains(res.Stdout, "denied 169.254.169.254:80") {
		t.Fatalf("cloud metadata endpoint was not denied:\n%s", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "routes=0") {
		t.Fatalf("expected an empty routing table, got:\n%s", res.Stdout)
	}

	// NEGATIVE CONTROL: the same probe on the host must succeed, otherwise the
	// test above proves only that this machine has no network.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(ctx, py, "-c",
		`import socket
s=socket.socket(); s.settimeout(5)
try:
    s.connect(("1.1.1.1",443)); print("HOST_OK")
except OSError as e: print("HOST_FAIL", e)`).CombinedOutput()
	if !strings.Contains(string(out), "HOST_OK") {
		t.Skipf("negative control failed: this host has no outbound network either (%s), "+
			"so the isolation result above is not meaningful", strings.TrimSpace(string(out)))
	}
}

// ---------------------------------------------------------------------------
// The claim: the payload cannot see or touch the host filesystem.
// ---------------------------------------------------------------------------

func TestSandbox_HostFilesystemIsNotVisible(t *testing.T) {
	d := newDriver(t)

	// Plant a file that stands in for another tenant's data.
	secretDir := t.TempDir()
	secretPath := filepath.Join(secretDir, "other-tenant-secret.txt")
	if err := os.WriteFile(secretPath, []byte("TENANT_B_PRIVATE_DATA"), 0o600); err != nil {
		t.Fatal(err)
	}

	res := run(t, d, sandbox.Spec{
		RunID: "fs-test", TenantID: "t1",
		Argv: []string{"/bin/sh", "-c",
			"cat " + secretPath + " 2>&1; echo '---'; ls /home 2>&1; echo '---'; ls / "},
	})
	if strings.Contains(res.Stdout, "TENANT_B_PRIVATE_DATA") {
		t.Fatalf("sandbox read a host file outside its workspace:\n%s", res.Stdout)
	}
	if strings.Contains(res.Stdout, "/home/user") {
		t.Fatalf("sandbox can see host home directories:\n%s", res.Stdout)
	}
}

func TestSandbox_RootFilesystemIsReadOnly(t *testing.T) {
	d := newDriver(t)
	res := run(t, d, sandbox.Spec{
		RunID: "ro-test", TenantID: "t1",
		Argv: []string{"/bin/sh", "-c", `
touch /pwned            2>&1 || echo "DENIED /"
touch /usr/bin/pwned    2>&1 || echo "DENIED /usr/bin"
touch /etc/pwned        2>&1 || echo "DENIED /etc"
echo ok > /work/allowed && echo "ALLOWED /work"
echo ok > /tmp/allowed  && echo "ALLOWED /tmp"`},
	})
	for _, want := range []string{"DENIED /", "DENIED /usr/bin", "DENIED /etc", "ALLOWED /work", "ALLOWED /tmp"} {
		if !strings.Contains(res.Stdout, want) {
			t.Fatalf("missing %q in output:\n%s\n%s", want, res.Stdout, res.Stderr)
		}
	}
}

func TestSandbox_WorkspaceIsTheOnlyThingThatPersists(t *testing.T) {
	d := newDriver(t)
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "input.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := run(t, d, sandbox.Spec{
		RunID: "ws-test", TenantID: "t1", WorkspaceDir: ws,
		Argv: []string{"/bin/sh", "-c", "cat /work/input.txt && tr a-z A-Z < /work/input.txt > /work/output.txt"},
	})
	if res.ExitCode != 0 {
		t.Fatalf("exit %d: %s", res.ExitCode, res.Stderr)
	}
	got, err := os.ReadFile(filepath.Join(ws, "output.txt"))
	if err != nil {
		t.Fatalf("workspace output not visible on the host: %v", err)
	}
	if strings.TrimSpace(string(got)) != "HELLO" {
		t.Fatalf("workspace roundtrip wrong: %q", got)
	}
}

// ---------------------------------------------------------------------------
// The claim: the payload runs unprivileged and cannot regain privilege.
// ---------------------------------------------------------------------------

func TestSandbox_RunsUnprivilegedWithNoCapabilities(t *testing.T) {
	d := newDriver(t)
	res := run(t, d, sandbox.Spec{
		RunID: "priv-test", TenantID: "t1",
		Argv: []string{"/bin/sh", "-c",
			`echo "uid=$(id -u)"; grep -E '^(CapEff|CapBnd|NoNewPrivs|Seccomp):' /proc/self/status`},
	})
	if !strings.Contains(res.Stdout, "uid=1000") {
		t.Fatalf("payload should run as the unprivileged sandbox uid, got:\n%s", res.Stdout)
	}
	// An empty effective set means no capability is usable right now; an empty
	// BOUNDING set is the stronger claim: nothing in this process tree can ever
	// acquire one, however it is exec'd.
	if !strings.Contains(res.Stdout, "CapEff:\t0000000000000000") {
		t.Fatalf("payload holds effective capabilities:\n%s", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "CapBnd:\t0000000000000000") {
		t.Fatalf("capability bounding set is not empty:\n%s", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "NoNewPrivs:\t1") {
		t.Fatalf("no_new_privs is not set:\n%s", res.Stdout)
	}
	// Seccomp: 2 == SECCOMP_MODE_FILTER.
	if !strings.Contains(res.Stdout, "Seccomp:\t2") {
		t.Fatalf("seccomp filter is not active:\n%s", res.Stdout)
	}
}

func TestSandbox_EscapeSyscallsAreBlocked(t *testing.T) {
	py := python(t)
	d := newDriver(t)
	// Exercise the denylist through ctypes so we hit the raw syscall rather
	// than a libc wrapper that might refuse first for its own reasons.
	const probe = `
import ctypes, ctypes.util, os, errno
libc = ctypes.CDLL(ctypes.util.find_library("c"), use_errno=True)
def call(nr, *args):
    ctypes.set_errno(0)
    r = libc.syscall(ctypes.c_long(nr), *[ctypes.c_long(a) for a in args])
    return r, ctypes.get_errno()
checks = {"unshare(CLONE_NEWNS)": (272, 0x00020000), "setns": (308, 0, 0),
          "ptrace": (101, 0, 1), "bpf": (321, 0, 0, 0),
          "perf_event_open": (298, 0, 0, 0, 0, 0), "keyctl": (250, 0),
          "open_by_handle_at": (304, 0, 0, 0), "init_module": (175, 0, 0, 0)}
bad = []
for name, args in checks.items():
    r, e = call(*args)
    if r != -1 or e != errno.EPERM:
        bad.append("%s -> ret=%d errno=%s" % (name, r, errno.errorcode.get(e, e)))
    else:
        print("blocked  %s" % name)
# clone3 must report ENOSYS so glibc falls back to the filterable clone(2)
r, e = call(435, 0, 0)
print("clone3 -> %s" % errno.errorcode.get(e, e))
if e != errno.ENOSYS:
    bad.append("clone3 should be ENOSYS, got %s" % errno.errorcode.get(e, e))
print("FAILURES:" + ("none" if not bad else "; ".join(bad)))
`
	res := run(t, d, sandbox.Spec{
		RunID: "seccomp-test", TenantID: "t1",
		Argv: []string{py, "-c", probe},
	})
	if !strings.Contains(res.Stdout, "FAILURES:none") {
		t.Fatalf("seccomp denylist did not hold:\n%s\n%s", res.Stdout, res.Stderr)
	}
}

// ---------------------------------------------------------------------------
// The claim: resource limits are enforced, not advisory.
// ---------------------------------------------------------------------------

func TestSandbox_WallClockTimeoutIsEnforced(t *testing.T) {
	d := newDriver(t)
	start := time.Now()
	res := run(t, d, sandbox.Spec{
		RunID: "timeout-test", TenantID: "t1",
		// Ignore SIGTERM on purpose: a cooperative shutdown is not a control.
		Argv:   []string{"/bin/sh", "-c", "trap '' TERM; while :; do :; done"},
		Limits: sandbox.Limits{Wall: 2 * time.Second, CPUMillis: 200},
	})
	elapsed := time.Since(start)
	if !res.TimedOut {
		t.Fatalf("runaway loop was not reported as timed out (exit=%d)", res.ExitCode)
	}
	if elapsed > 15*time.Second {
		t.Fatalf("timeout took %s to take effect; the bound is not real", elapsed)
	}
}

func TestSandbox_MemoryLimitIsEnforced(t *testing.T) {
	py := python(t)
	d := newDriver(t)
	res := run(t, d, sandbox.Spec{
		RunID: "mem-test", TenantID: "t1",
		Argv: []string{py, "-c",
			`buf=[]
for _ in range(400):
    buf.append(bytearray(4*1024*1024))   # touch the pages so they are resident
print("ALLOCATED_1_6GB")`},
		Limits: sandbox.Limits{MemoryBytes: 64 << 20, Wall: 30 * time.Second},
	})
	if strings.Contains(res.Stdout, "ALLOCATED_1_6GB") {
		t.Fatalf("payload allocated far past its 64 MiB limit:\n%s", res.Stdout)
	}
	if !res.OOMKilled && res.ExitCode == 0 {
		t.Fatalf("expected an OOM kill or a non-zero exit, got exit=0 oom=%v:\n%s\n%s",
			res.OOMKilled, res.Stdout, res.Stderr)
	}
	t.Logf("memory limit held: oom_killed=%v exit=%d", res.OOMKilled, res.ExitCode)
}

func TestSandbox_ForkBombIsContained(t *testing.T) {
	py := python(t)
	d := newDriver(t)

	// An earlier version of this test used a shell fork bomb. It "passed" in
	// 27ms with exit 0 - because the bomb was a dash syntax error and never
	// forked at all. The test proved nothing. This version forks explicitly,
	// counts successes, and asserts the kernel refused at the configured limit.
	const bomb = `
import os, sys
children = 0
try:
    while children < 5000:
        pid = os.fork()
        if pid == 0:
            # Child: block forever so it keeps occupying a pid slot.
            try: os.pause()
            finally: os._exit(0)
        children += 1
except OSError as e:
    print("FORK_REFUSED after %d children: %s" % (children, e.strerror))
    sys.exit(0)
print("FORK_UNBOUNDED reached %d children" % children)
sys.exit(1)
`
	const pidLimit = 24
	res := run(t, d, sandbox.Spec{
		RunID: "fork-test", TenantID: "t1",
		Argv:   []string{py, "-c", bomb},
		Limits: sandbox.Limits{PIDs: pidLimit, Wall: 20 * time.Second, MemoryBytes: 128 << 20},
	})

	if strings.Contains(res.Stdout, "FORK_UNBOUNDED") {
		t.Fatalf("fork bomb was NOT contained:\n%s", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "FORK_REFUSED") {
		t.Fatalf("expected the kernel to refuse a fork; got exit=%d\nstdout:%s\nstderr:%s",
			res.ExitCode, res.Stdout, res.Stderr)
	}
	// The refusal must happen at roughly the configured limit, not at some
	// unrelated system-wide ceiling - otherwise the cgroup is not what stopped it.
	var got int
	if _, err := fmt.Sscanf(res.Stdout, "FORK_REFUSED after %d children", &got); err != nil {
		t.Fatalf("could not parse child count from %q: %v", res.Stdout, err)
	}
	if got > pidLimit+8 {
		t.Fatalf("forked %d children against a pids limit of %d; the cgroup is not binding",
			got, pidLimit)
	}
	t.Logf("fork bomb contained: kernel refused fork after %d children (pids.max=%d)", got, pidLimit)
}

func TestSandbox_OutputIsCapped(t *testing.T) {
	d := newDriver(t)
	res := run(t, d, sandbox.Spec{
		RunID: "output-test", TenantID: "t1",
		Argv:   []string{"/bin/sh", "-c", "yes AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
		Limits: sandbox.Limits{MaxOutputBytes: 32 << 10, Wall: 5 * time.Second},
	})
	if !res.Truncated {
		t.Fatal("unbounded output was not reported as truncated")
	}
	// The cap must be honoured with only modest slack, or an agent could push
	// hundreds of megabytes into the event log and the model context.
	if len(res.Stdout) > 64<<10 {
		t.Fatalf("stdout is %d bytes despite a 32 KiB cap", len(res.Stdout))
	}
}

// ---------------------------------------------------------------------------
// Cold start: the number that decides sandbox-per-call vs sandbox-per-run.
// ---------------------------------------------------------------------------

func TestSandbox_ColdStartIsMeasured(t *testing.T) {
	d := newDriver(t)
	const n = 10
	var total time.Duration
	var worst time.Duration
	for i := 0; i < n; i++ {
		start := time.Now()
		res := run(t, d, sandbox.Spec{
			RunID: "cold-start", TenantID: "t1",
			Argv: []string{"/bin/sh", "-c", "exit 0"},
		})
		el := time.Since(start)
		total += el
		if el > worst {
			worst = el
		}
		if res.ExitCode != 0 {
			t.Fatalf("iteration %d exited %d: %s", i, res.ExitCode, res.Stderr)
		}
	}
	avg := total / n
	t.Logf("COLD START over %d runs: mean=%s worst=%s", n, avg.Truncate(time.Microsecond), worst.Truncate(time.Microsecond))
	// Not a tight assertion - CI machines vary - but a regression from
	// milliseconds to seconds would invalidate the per-call sandbox model and
	// must fail the build rather than quietly change the architecture.
	if avg > 2*time.Second {
		t.Fatalf("cold start averaged %s; per-call sandboxes are no longer viable", avg)
	}
}
