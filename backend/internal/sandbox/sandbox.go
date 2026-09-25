// Package sandbox runs untrusted code.
//
// Threat model. The code we run here is written by a language model that has
// read attacker-controlled text. Treat it as hostile, every time. Specifically
// we assume the payload will try to: read another tenant's data, reach the
// cloud instance-metadata endpoint for credentials, exfiltrate the workspace
// over the network, read a credential out of its own environment or /proc,
// consume unbounded CPU/memory/disk, and exploit a kernel bug to escape.
//
// The defence is layered, because any single layer will eventually have a CVE:
//
//  1. Kernel isolation     - gVisor in production; namespaces here.
//  2. Namespaces           - mount, pid, net, ipc, uts, user.
//  3. Identity             - unprivileged uid inside a user namespace, so
//     "root in the container" owns nothing on the host.
//  4. Filesystem           - read-only rootfs, one writable workspace, size caps.
//  5. Network              - empty network namespace. Not a firewall rule that
//     can be misconfigured: there is no route to anywhere.
//  6. Syscall surface      - seccomp-BPF denylist over the escape primitives.
//  7. Resources            - cgroup cpu/memory/pids + rlimits + wall clock.
//  8. Credentials          - never present. See internal/tools CLI shim.
//
// What this package deliberately does NOT do is decide *whether* a command may
// run. That is the tool gateway's job (internal/authz). This package assumes
// the decision was already made and concerns itself only with containment.
package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// NetworkMode selects the egress policy for a sandbox.
type NetworkMode string

const (
	// NetworkNone gives the sandbox an empty network namespace: loopback only,
	// no routes, no addresses. This is the default and covers the large
	// majority of execution tools (pandoc, jq, data munging, code formatting).
	NetworkNone NetworkMode = "none"
	// NetworkProxy attaches the sandbox to an egress proxy that enforces a
	// per-run domain allowlist and injects credentials. The sandbox still has
	// no direct route to anything else.
	NetworkProxy NetworkMode = "proxy"
)

// Limits are the resource bounds applied to one execution. Zero values are
// replaced by DefaultLimits: "unset" must never mean "unlimited".
type Limits struct {
	MemoryBytes     int64
	CPUMillis       int // 1000 = one core
	PIDs            int
	Wall            time.Duration
	MaxOutputBytes  int
	WorkspaceBytes  int64
	TmpfsBytes      int64
	MaxOpenFiles    uint64
	MaxFileSizeByte uint64
}

func DefaultLimits() Limits {
	return Limits{
		MemoryBytes:     256 << 20, // 256 MiB
		CPUMillis:       500,       // half a core
		PIDs:            64,
		Wall:            30 * time.Second,
		MaxOutputBytes:  256 << 10, // 256 KiB of combined output
		WorkspaceBytes:  64 << 20,
		TmpfsBytes:      16 << 20,
		MaxOpenFiles:    256,
		MaxFileSizeByte: 64 << 20,
	}
}

// WithDefaults fills zero fields from DefaultLimits.
func (l Limits) WithDefaults() Limits {
	d := DefaultLimits()
	if l.MemoryBytes <= 0 {
		l.MemoryBytes = d.MemoryBytes
	}
	if l.CPUMillis <= 0 {
		l.CPUMillis = d.CPUMillis
	}
	if l.PIDs <= 0 {
		l.PIDs = d.PIDs
	}
	if l.Wall <= 0 {
		l.Wall = d.Wall
	}
	if l.MaxOutputBytes <= 0 {
		l.MaxOutputBytes = d.MaxOutputBytes
	}
	if l.WorkspaceBytes <= 0 {
		l.WorkspaceBytes = d.WorkspaceBytes
	}
	if l.TmpfsBytes <= 0 {
		l.TmpfsBytes = d.TmpfsBytes
	}
	if l.MaxOpenFiles == 0 {
		l.MaxOpenFiles = d.MaxOpenFiles
	}
	if l.MaxFileSizeByte == 0 {
		l.MaxFileSizeByte = d.MaxFileSizeByte
	}
	return l
}

// Spec describes one execution.
type Spec struct {
	// RunID and TenantID are carried for labelling, cgroup naming and audit.
	RunID    string
	TenantID string

	// Argv[0] is resolved via PATH inside the sandbox.
	Argv []string
	// Stdin is fed to the process and then closed.
	Stdin []byte
	// Env is the COMPLETE environment. The caller must not pass anything
	// secret; see the package doc. A minimal safe default is used when empty.
	Env []string
	// WorkspaceDir is a host path bind-mounted read-write at /work. It is the
	// only writable location that survives the sandbox.
	WorkspaceDir string
	// ExtraReadOnly are host paths bind-mounted read-only, keyed by the path
	// they should appear at inside the sandbox.
	ExtraReadOnly map[string]string

	Network NetworkMode
	Limits  Limits
}

// Result is the outcome of one execution. Note that a non-zero ExitCode is NOT
// an error from this package's point of view: "the script failed" is a normal
// result the agent must be told about. Errors returned alongside a Result mean
// the sandbox itself malfunctioned.
type Result struct {
	ExitCode int
	Stdout   string
	Stderr   string
	Duration time.Duration
	// Truncated is set when output hit MaxOutputBytes. The model is told, so it
	// does not silently reason about a partial result.
	Truncated bool
	// TimedOut and OOMKilled distinguish the two most common containment hits,
	// because the remediation a human needs differs.
	TimedOut  bool
	OOMKilled bool
	// Driver records which isolation backend ran this, so the audit log and the
	// UI never imply a stronger boundary than was actually used.
	Driver string
	// ColdStartMS is the setup cost before the payload's first instruction.
	ColdStartMS int64
}

var (
	ErrUnsupported = errors.New("sandbox: driver unsupported on this platform")
	ErrSetup       = errors.New("sandbox: setup failed")
)

// Driver is an isolation backend.
//
// Three exist so the same tool contract holds from a laptop to a hardened
// cluster: "namespace" (this host, no daemon needed), "docker" (a local
// container runtime) and "kubernetes" (a Pod with RuntimeClass=gvisor plus a
// deny-all NetworkPolicy). The security properties are NOT equal - Name() and
// Result.Driver exist precisely so nothing in the system can claim otherwise.
type Driver interface {
	Name() string
	Run(ctx context.Context, spec Spec) (Result, error)
	// Close releases any pooled resources.
	Close() error
}

// New selects a driver by name.
func New(name string, cfg Config) (Driver, error) {
	switch name {
	case "", "namespace", "nsjail":
		return NewNamespaceDriver(cfg)
	case "docker":
		return NewDockerDriver(cfg)
	case "kubernetes", "k8s":
		return NewKubernetesDriver(cfg)
	default:
		return nil, fmt.Errorf("sandbox: unknown driver %q (want namespace|docker|kubernetes)", name)
	}
}

// Config is driver construction configuration.
type Config struct {
	// StateDir holds per-sandbox rootfs scaffolding and workspaces.
	StateDir string
	// Image is used by the docker and kubernetes drivers.
	Image string
	// Namespace is used by the kubernetes driver.
	Namespace string
	// RuntimeClass is used by the kubernetes driver (e.g. "gvisor").
	RuntimeClass string
	// UIDBase is the host uid that container uid 1 maps to. Sandboxes for
	// different tenants can be given different bases so that even a mount-level
	// mistake cannot make one tenant's files readable by another.
	UIDBase int
}

func (c Config) withDefaults() Config {
	if c.StateDir == "" {
		c.StateDir = "/var/lib/agentorch/sandboxes"
	}
	if c.Image == "" {
		c.Image = "agentorch/sandbox:dev"
	}
	if c.Namespace == "" {
		c.Namespace = "agentorch-sandboxes"
	}
	if c.UIDBase == 0 {
		c.UIDBase = 100000
	}
	return c
}

// HelperBinDir is where platform-provided helper binaries are mounted inside
// a sandbox.
//
// It deliberately is NOT /usr/local/bin: /usr is a read-only bind mount of the
// host's, so nothing can be created under it inside the jail. HelperBinDir
// lives on the sandbox's own tmpfs root, which is writable during setup and
// sealed read-only before the payload runs.
const HelperBinDir = "/opt/agentorch/bin"

// SafeEnv is the default environment for a sandboxed process.
//
// It is an allowlist, not the host environment minus some keys. Inheriting the
// host environment and subtracting is how credentials leak: someone adds
// DATABASE_URL to a Deployment and it silently becomes readable by every agent.
func SafeEnv(extra ...string) []string {
	base := []string{
		// HelperBinDir comes first so platform-provided helpers (the CLI
		// personalities) win over anything of the same name in the image.
		"PATH=" + HelperBinDir + ":/usr/local/bin:/usr/bin:/bin",
		"HOME=/work",
		"TMPDIR=/tmp",
		"LANG=C.UTF-8",
		"LC_ALL=C.UTF-8",
		"PYTHONDONTWRITEBYTECODE=1",
		// Tell well-behaved SDKs not to go looking for instance credentials.
		// This is a courtesy, not a control: the control is the empty netns.
		"AWS_EC2_METADATA_DISABLED=true",
	}
	return append(base, extra...)
}
