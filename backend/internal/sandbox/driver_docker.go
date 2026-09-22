package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// dockerDriver runs the payload in a container on a local Docker/Podman daemon.
//
// This is the driver an engineer on a laptop will actually use, so the flags
// below are the whole security story and each one is deliberate. The mapping to
// the Kubernetes PodSpec in deploy/k8s is one-to-one; see the table in
// DEEP_DIVE.md "D2" so nothing silently gets weaker between environments.
type dockerDriver struct {
	cfg Config
	bin string
}

func NewDockerDriver(cfg Config) (Driver, error) {
	cfg = cfg.withDefaults()
	bin, err := exec.LookPath("docker")
	if err != nil {
		if bin, err = exec.LookPath("podman"); err != nil {
			return nil, fmt.Errorf("%w: neither docker nor podman found on PATH", ErrUnsupported)
		}
	}
	return &dockerDriver{cfg: cfg, bin: bin}, nil
}

func (d *dockerDriver) Name() string { return "docker" }
func (d *dockerDriver) Close() error { return nil }

func (d *dockerDriver) Run(ctx context.Context, spec Spec) (Result, error) {
	started := time.Now()
	lim := spec.Limits.WithDefaults()
	if len(spec.Argv) == 0 {
		return Result{}, fmt.Errorf("%w: empty argv", ErrSetup)
	}

	args := []string{
		"run", "--rm", "-i",
		// No network namespace attachment at all. Equivalent to the empty netns
		// in the namespace driver and to a deny-all NetworkPolicy in Kubernetes.
		"--network", "none",
		// The container's root filesystem cannot be modified. Anything that
		// needs to write must use an explicitly mounted, size-capped tmpfs.
		"--read-only",
		"--tmpfs", fmt.Sprintf("/tmp:rw,noexec,nosuid,nodev,size=%d", lim.TmpfsBytes),
		// Never run as root, even inside the container: a uid 0 process is one
		// misconfigured mount away from being interesting.
		"--user", fmt.Sprintf("%d:%d", containerUID, containerUID),
		// Drop every capability rather than trimming the default set, then add
		// back nothing. A payload that needs CAP_NET_ADMIN is a payload we do
		// not want to run.
		"--cap-drop", "ALL",
		// Stops setuid binaries and file capabilities from granting privilege.
		"--security-opt", "no-new-privileges",
		"--pids-limit", strconv.Itoa(lim.PIDs),
		"--memory", strconv.FormatInt(lim.MemoryBytes, 10),
		// Pin swap to the memory limit so the payload cannot escape the bound
		// into swap and degrade every other tenant on the node.
		"--memory-swap", strconv.FormatInt(lim.MemoryBytes, 10),
		"--cpus", fmt.Sprintf("%.2f", float64(lim.CPUMillis)/1000.0),
		"--workdir", "/work",
	}
	// Where the daemon supports it, ask for gVisor. This is the single biggest
	// difference between "isolated from other tenants" and "isolated from the
	// kernel"; if the runtime is not installed Docker errors out loudly rather
	// than quietly giving us a weaker boundary.
	if d.cfg.RuntimeClass != "" {
		args = append(args, "--runtime", d.cfg.RuntimeClass)
	}
	if spec.WorkspaceDir != "" {
		args = append(args, "-v", spec.WorkspaceDir+":/work:rw")
	} else {
		args = append(args, "--tmpfs", fmt.Sprintf("/work:rw,nosuid,nodev,size=%d", lim.WorkspaceBytes))
	}
	for guest, host := range spec.ExtraReadOnly {
		args = append(args, "-v", host+":"+guest+":ro")
	}
	env := spec.Env
	if len(env) == 0 {
		env = SafeEnv()
	}
	for _, kv := range env {
		args = append(args, "-e", kv)
	}
	args = append(args, d.cfg.Image)
	args = append(args, spec.Argv...)

	runCtx, cancel := context.WithTimeout(ctx, lim.Wall)
	defer cancel()

	cmd := exec.CommandContext(runCtx, d.bin, args...)
	stdout := newCapWriter(lim.MaxOutputBytes / 2)
	stderr := newCapWriter(lim.MaxOutputBytes / 2)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if len(spec.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(spec.Stdin)
	}

	err := cmd.Run()
	res := Result{
		Stdout: stdout.String(), Stderr: stderr.String(),
		Duration:  time.Since(started),
		Truncated: stdout.Truncated() || stderr.Truncated(),
		TimedOut:  errors.Is(runCtx.Err(), context.DeadlineExceeded),
		Driver:    d.Name(),
	}
	res.ExitCode = exitCodeOfPortable(err)
	// Docker reports an OOM kill as exit 137 (128+SIGKILL). A wall-clock kill
	// looks identical, so we only claim OOM when we did not time out.
	res.OOMKilled = res.ExitCode == 137 && !res.TimedOut
	// Cold start for a container run is dominated by daemon round trips; we
	// cannot separate it from execution without extra instrumentation, so we
	// report it as unknown rather than guessing.
	res.ColdStartMS = 0
	if res.ExitCode == 125 && strings.Contains(res.Stderr, "docker:") {
		// 125 is the daemon's own "could not run" code, not the payload's.
		return res, fmt.Errorf("%w: %s", ErrSetup, firstLine(res.Stderr))
	}
	return res, nil
}
