//go:build linux

package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// namespaceDriver runs a payload in a fresh set of Linux namespaces on this
// host, with no container runtime and no daemon.
//
// Why implement this rather than shell out to `docker run`:
//
//   - It runs anywhere a Linux kernel does, including CI and a dev box with no
//     Docker daemon, which is exactly where the safety tests need to run.
//   - "A container" is not a kernel object. It is a process with namespaces,
//     cgroups, a pivoted root, dropped capabilities and a seccomp filter.
//     Writing those explicitly means the security properties are visible in the
//     diff and testable one at a time, instead of being implied by a runtime's
//     defaults that may change under us.
//
// It is NOT the production answer. The production answer is the Kubernetes
// driver with RuntimeClass=gvisor, because this driver shares the host kernel:
// one kernel LPE and the containment is gone. See DEEP_DIVE.md "D2".
type namespaceDriver struct {
	cfg Config
}

func NewNamespaceDriver(cfg Config) (Driver, error) {
	cfg = cfg.withDefaults()
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("%w: state dir: %v", ErrSetup, err)
	}
	return &namespaceDriver{cfg: cfg}, nil
}

func (d *namespaceDriver) Name() string { return "namespace" }
func (d *namespaceDriver) Close() error { return nil }

// initEnvVar marks the re-executed child. The binary inspects this before doing
// anything else; see MaybeRunInit.
const initEnvVar = "AGENTORCH_SANDBOX_INIT"

// jailConfig is what the parent hands the child over a pipe. It is passed by
// pipe rather than argv or env so it never shows up in `ps` on the host.
type jailConfig struct {
	Root          string            `json:"root"`
	Workspace     string            `json:"workspace"`
	ExtraReadOnly map[string]string `json:"extra_read_only"`
	Argv          []string          `json:"argv"`
	Env           []string          `json:"env"`
	UID           int               `json:"uid"`
	GID           int               `json:"gid"`
	TmpfsBytes    int64             `json:"tmpfs_bytes"`
	MaxOpenFiles  uint64            `json:"max_open_files"`
	MaxFileSize   uint64            `json:"max_file_size"`
	MaxProcs      uint64            `json:"max_procs"`
	Seccomp       bool              `json:"seccomp"`
	// WithNetwork tells the child to provide a working resolver configuration.
	// Without it a sandbox that has egress still cannot resolve a hostname.
	WithNetwork bool `json:"with_network"`
}

// containerUID is the uid the payload runs as *inside* the user namespace. It
// maps to Config.UIDBase + containerUID - 1 on the host, which is an identity
// that owns no file on the system.
const containerUID = 1000

func (d *namespaceDriver) Run(ctx context.Context, spec Spec) (Result, error) {
	started := time.Now()
	lim := spec.Limits.WithDefaults()

	if len(spec.Argv) == 0 {
		return Result{}, fmt.Errorf("%w: empty argv", ErrSetup)
	}
	if spec.Network != NetworkNone && spec.Network != NetworkProxy && spec.Network != "" {
		return Result{}, fmt.Errorf("%w: namespace driver supports network=none|proxy, got %q",
			ErrUnsupported, spec.Network)
	}

	sbxID := sanitizeID(spec.RunID) + "-" + shortRand()
	base := filepath.Join(d.cfg.StateDir, sbxID)
	root := filepath.Join(base, "rootfs")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return Result{}, fmt.Errorf("%w: mkdir rootfs: %v", ErrSetup, err)
	}
	// The rootfs scaffold is disposable; the workspace is the caller's.
	defer os.RemoveAll(base)

	ws := spec.WorkspaceDir
	if ws == "" {
		ws = filepath.Join(base, "work")
		if err := os.MkdirAll(ws, 0o700); err != nil {
			return Result{}, fmt.Errorf("%w: mkdir workspace: %v", ErrSetup, err)
		}
	}
	hostUID := d.cfg.UIDBase + containerUID - 1
	// The workspace must be owned by the identity the payload will run as, or
	// it cannot write. chown is recursive because tools leave files behind
	// between calls in a warm sandbox.
	if err := chownTree(ws, hostUID, hostUID); err != nil {
		return Result{}, fmt.Errorf("%w: chown workspace: %v", ErrSetup, err)
	}

	cg, err := newCgroup(sbxID, lim)
	if err != nil {
		return Result{}, fmt.Errorf("%w: cgroup: %v", ErrSetup, err)
	}
	defer cg.Destroy()

	cfgR, cfgW, err := os.Pipe()
	if err != nil {
		return Result{}, fmt.Errorf("%w: cfg pipe: %v", ErrSetup, err)
	}
	defer cfgR.Close()
	defer cfgW.Close()
	syncR, syncW, err := os.Pipe()
	if err != nil {
		return Result{}, fmt.Errorf("%w: sync pipe: %v", ErrSetup, err)
	}
	defer syncR.Close()
	defer syncW.Close()

	env := spec.Env
	if len(env) == 0 {
		env = SafeEnv()
	}
	jc := jailConfig{
		Root: root, Workspace: ws, ExtraReadOnly: spec.ExtraReadOnly,
		Argv: spec.Argv, Env: env,
		UID: containerUID, GID: containerUID,
		TmpfsBytes:   lim.TmpfsBytes,
		MaxOpenFiles: lim.MaxOpenFiles,
		MaxFileSize:  lim.MaxFileSizeByte,
		MaxProcs:     uint64(lim.PIDs),
		Seccomp:      true,
		WithNetwork:  spec.Network == NetworkProxy,
	}

	// Wall-clock deadline. This is a hard bound the payload cannot influence:
	// it lives in the parent, which the payload cannot reach.
	runCtx, cancel := context.WithTimeout(ctx, lim.Wall)
	defer cancel()

	cmd := exec.Command("/proc/self/exe")
	cmd.Args = []string{"agentorch-sandbox-init"}
	cmd.Env = []string{initEnvVar + "=1"}
	cmd.ExtraFiles = []*os.File{cfgR, syncR} // -> fd 3, fd 4 in the child
	// Every sandbox gets mount, pid, uts, ipc and user isolation. Only the
	// NETWORK namespace is conditional.
	//
	// NetworkNone (the default, and what every agent-authored payload gets)
	// adds CLONE_NEWNET, producing an empty network stack: loopback down, no
	// addresses, no routes. There is nothing to misconfigure.
	//
	// NetworkProxy is used ONLY for the credential broker sandbox, which runs
	// a trusted binary from our own image - never model-authored code - and
	// needs to reach an API. In production that sandbox joins a prepared
	// network namespace whose only route is an egress proxy enforcing a
	// per-run domain allowlist. Here it shares the host's network namespace,
	// which is weaker; the property being demonstrated (the agent cannot
	// observe the credential) does not depend on it. See DEEP_DIVE.md "D6".
	netFlag := uintptr(syscall.CLONE_NEWNET)
	if spec.Network == NetworkProxy {
		netFlag = 0
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWNS | // own mount table
			syscall.CLONE_NEWPID | // cannot see or signal host processes
			syscall.CLONE_NEWUTS | // own hostname
			syscall.CLONE_NEWIPC | // own SysV IPC and POSIX message queues
			netFlag |
			syscall.CLONE_NEWUSER, // uid 0 inside is unprivileged outside
		// Map container uid 0 to host root only for the duration of setup
		// (mounting requires CAP_SYS_ADMIN in this userns), then map a large
		// unprivileged range for the payload itself. The child drops to
		// containerUID before exec, so the payload never runs as host root.
		UidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: 0, Size: 1},
			{ContainerID: 1, HostID: d.cfg.UIDBase, Size: 65535},
		},
		GidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: 0, Size: 1},
			{ContainerID: 1, HostID: d.cfg.UIDBase, Size: 65535},
		},
		GidMappingsEnableSetgroups: true,
		// If the executor dies, the kernel kills the sandbox rather than
		// leaving an orphan burning CPU with nobody watching it.
		Pdeathsig: syscall.SIGKILL,
		Setpgid:   true,
	}

	stdout := newCapWriter(lim.MaxOutputBytes / 2)
	stderr := newCapWriter(lim.MaxOutputBytes / 2)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if len(spec.Stdin) > 0 {
		cmd.Stdin = strings.NewReader(string(spec.Stdin))
	}

	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("%w: start init: %v", ErrSetup, err)
	}
	pid := cmd.Process.Pid

	// Hand over the config, then place the child under its resource limits
	// BEFORE releasing it. Doing this in the other order would leave a window
	// in which the payload runs unbounded.
	if err := json.NewEncoder(cfgW).Encode(jc); err != nil {
		_ = cmd.Process.Kill()
		return Result{}, fmt.Errorf("%w: write jail config: %v", ErrSetup, err)
	}
	cfgW.Close()

	if err := cg.Add(pid); err != nil {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		return Result{}, fmt.Errorf("%w: cgroup add: %v", ErrSetup, err)
	}
	// Release the child: limits are in place.
	if _, err := syncW.Write([]byte{1}); err != nil {
		_ = cmd.Process.Kill()
		return Result{}, fmt.Errorf("%w: release child: %v", ErrSetup, err)
	}
	syncW.Close()

	coldStart := time.Since(started)

	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	var timedOut bool
	select {
	case err = <-waitErr:
	case <-runCtx.Done():
		timedOut = errors.Is(runCtx.Err(), context.DeadlineExceeded)
		// Kill the whole cgroup, not just the direct child: the payload may
		// have forked, and killing only pid 1 of the namespace would leave
		// children reparented and still running.
		killCgroup(cg, cmd.Process)
		err = <-waitErr
	}

	// Anything still alive (a grandchild that outlived the init) dies here.
	killCgroup(cg, nil)

	res := Result{
		Stdout:      stdout.String(),
		Stderr:      stderr.String(),
		Duration:    time.Since(started),
		Truncated:   stdout.Truncated() || stderr.Truncated(),
		TimedOut:    timedOut,
		OOMKilled:   cg.OOMKilled(),
		Driver:      d.Name(),
		ColdStartMS: coldStart.Milliseconds(),
	}
	res.ExitCode = exitCodeOf(err)

	// A setup failure inside the child is not the payload's fault and must not
	// be reported to the agent as "your script exited 127".
	if res.ExitCode == exitSetupFailure && strings.Contains(res.Stderr, setupFailureMarker) {
		return res, fmt.Errorf("%w: %s", ErrSetup, firstLine(res.Stderr))
	}
	return res, nil
}

const (
	// exitSetupFailure is returned by the child when the jail could not be
	// built. 126/127 are taken by the shell for "not executable"/"not found",
	// so we use a value outside the range a normal payload would produce.
	exitSetupFailure   = 125
	setupFailureMarker = "agentorch-sandbox-setup-error:"
)

func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if st, ok := ee.Sys().(syscall.WaitStatus); ok && st.Signaled() {
			// Conventional shell encoding, so 137 reads as "SIGKILL" to anyone
			// who has ever looked at a container exit code.
			return 128 + int(st.Signal())
		}
		return ee.ExitCode()
	}
	return -1
}

// killCgroup SIGKILLs every process in the sandbox.
func killCgroup(cg *cgroupManager, direct *os.Process) {
	if direct != nil {
		// Negative pid targets the process group, catching anything the payload
		// spawned that has not yet been reparented.
		_ = syscall.Kill(-direct.Pid, syscall.SIGKILL)
		_ = direct.Kill()
	}
	for _, pid := range cg.procs() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// capWriter accumulates output up to a byte cap and then discards the rest.
//
// The cap exists because an agent that runs `yes` or dumps a large binary would
// otherwise push hundreds of megabytes through the event log and, worse, into
// the model context where it costs real money. Truncation is reported.
type capWriter struct {
	mu        sync.Mutex
	buf       []byte
	cap       int
	truncated bool
}

func newCapWriter(capBytes int) *capWriter {
	if capBytes < 1024 {
		capBytes = 1024
	}
	return &capWriter{cap: capBytes, buf: make([]byte, 0, min(capBytes, 32<<10))}
}

func (w *capWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	room := w.cap - len(w.buf)
	if room <= 0 {
		w.truncated = true
		return len(p), nil // absorb and drop: never block or error the payload
	}
	if len(p) > room {
		w.buf = append(w.buf, p[:room]...)
		w.truncated = true
		return len(p), nil
	}
	w.buf = append(w.buf, p...)
	return len(p), nil
}

func (w *capWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.truncated {
		return string(w.buf) + "\n...[output truncated by the sandbox output cap]"
	}
	return string(w.buf)
}

func (w *capWriter) Truncated() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.truncated
}

var _ io.Writer = (*capWriter)(nil)

func sanitizeID(s string) string {
	if s == "" {
		return "anon"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := b.String()
	if len(out) > 48 {
		out = out[:48]
	}
	return out
}

func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			return t
		}
	}
	return s
}

func chownTree(root string, uid, gid int) error {
	return filepath.Walk(root, func(p string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(p, uid, gid)
	})
}
