//go:build linux

package sandbox

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// cgroupManager applies CPU, memory and PID limits to a sandbox.
//
// Both cgroup hierarchies are supported because the world is genuinely split:
// cgroup v2 is the default on modern distros and required by Kubernetes'
// MemoryQoS, but plenty of CI runners, older nodes and container-in-container
// environments still expose v1. Detecting at runtime beats failing on the
// operator's laptop.
//
// Why cgroups and not just rlimits: RLIMIT_AS bounds one process's address
// space, which a fork bomb trivially sidesteps and which breaks anything that
// reserves large virtual mappings (the Go runtime, the JVM, many allocators).
// cgroups bound the whole process tree's *resident* usage, which is the thing
// we actually care about.
type cgroupManager struct {
	v2     bool
	path   string // v2: full path to the leaf cgroup dir
	v1dirs map[string]string
}

const cgroupSlice = "agentorch"

func newCgroup(id string, lim Limits) (*cgroupManager, error) {
	if isCgroupV2() {
		return newCgroupV2(id, lim)
	}
	return newCgroupV1(id, lim)
}

func isCgroupV2() bool {
	// A unified hierarchy exposes cgroup.controllers at the root.
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err == nil {
		return true
	}
	return false
}

func writeFile(p, v string) error {
	if err := os.WriteFile(p, []byte(v), 0o644); err != nil {
		return fmt.Errorf("cgroup write %s=%q: %w", p, v, err)
	}
	return nil
}

func newCgroupV2(id string, lim Limits) (*cgroupManager, error) {
	root := "/sys/fs/cgroup"
	// Delegate the controllers we need to our slice. Best-effort: on a
	// delegated (containerised) cgroup namespace the root may not be writable,
	// but the leaf usually is.
	sliceDir := filepath.Join(root, cgroupSlice)
	if err := os.MkdirAll(sliceDir, 0o755); err != nil {
		return nil, fmt.Errorf("cgroup v2: mkdir slice: %w", err)
	}
	_ = writeFile(filepath.Join(root, "cgroup.subtree_control"), "+cpu +memory +pids")
	_ = writeFile(filepath.Join(sliceDir, "cgroup.subtree_control"), "+cpu +memory +pids")

	leaf := filepath.Join(sliceDir, id)
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		return nil, fmt.Errorf("cgroup v2: mkdir leaf: %w", err)
	}
	c := &cgroupManager{v2: true, path: leaf}

	if err := writeFile(filepath.Join(leaf, "memory.max"), strconv.FormatInt(lim.MemoryBytes, 10)); err != nil {
		c.Destroy()
		return nil, err
	}
	// Disable swap for the sandbox: swapping is an easy way to turn a memory
	// limit into a latency attack on every other tenant on the node.
	_ = writeFile(filepath.Join(leaf, "memory.swap.max"), "0")
	quota := lim.CPUMillis * 100 // period is 100000us, so millis*100 == quota us
	if err := writeFile(filepath.Join(leaf, "cpu.max"), fmt.Sprintf("%d 100000", quota)); err != nil {
		c.Destroy()
		return nil, err
	}
	if err := writeFile(filepath.Join(leaf, "pids.max"), strconv.Itoa(lim.PIDs)); err != nil {
		c.Destroy()
		return nil, err
	}
	return c, nil
}

func newCgroupV1(id string, lim Limits) (*cgroupManager, error) {
	c := &cgroupManager{v1dirs: map[string]string{}}
	mk := func(controller string) (string, error) {
		base := filepath.Join("/sys/fs/cgroup", controller)
		if _, err := os.Stat(base); err != nil {
			return "", fmt.Errorf("cgroup v1: controller %q unavailable: %w", controller, err)
		}
		dir := filepath.Join(base, cgroupSlice, id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("cgroup v1: mkdir %s: %w", dir, err)
		}
		c.v1dirs[controller] = dir
		return dir, nil
	}

	memDir, err := mk("memory")
	if err != nil {
		return nil, err
	}
	if err := writeFile(filepath.Join(memDir, "memory.limit_in_bytes"), strconv.FormatInt(lim.MemoryBytes, 10)); err != nil {
		c.Destroy()
		return nil, err
	}
	// memsw may be compiled out; if present, pinning it to the same value stops
	// the payload from escaping the memory limit into swap.
	_ = writeFile(filepath.Join(memDir, "memory.memsw.limit_in_bytes"), strconv.FormatInt(lim.MemoryBytes, 10))

	cpuDir, err := mk("cpu")
	if err != nil {
		return nil, err
	}
	if err := writeFile(filepath.Join(cpuDir, "cpu.cfs_period_us"), "100000"); err != nil {
		c.Destroy()
		return nil, err
	}
	if err := writeFile(filepath.Join(cpuDir, "cpu.cfs_quota_us"), strconv.Itoa(lim.CPUMillis*100)); err != nil {
		c.Destroy()
		return nil, err
	}

	pidDir, err := mk("pids")
	if err != nil {
		// pids is the fork-bomb control. Refuse to run without it rather than
		// silently offering weaker containment than the caller was promised.
		c.Destroy()
		return nil, fmt.Errorf("cgroup v1: pids controller required for fork-bomb containment: %w", err)
	}
	if err := writeFile(filepath.Join(pidDir, "pids.max"), strconv.Itoa(lim.PIDs)); err != nil {
		c.Destroy()
		return nil, err
	}
	return c, nil
}

// Add places a pid (and therefore all of its future children) under the limits.
func (c *cgroupManager) Add(pid int) error {
	v := strconv.Itoa(pid)
	if c.v2 {
		return writeFile(filepath.Join(c.path, "cgroup.procs"), v)
	}
	for controller, dir := range c.v1dirs {
		if err := writeFile(filepath.Join(dir, "cgroup.procs"), v); err != nil {
			return fmt.Errorf("cgroup v1 %s: %w", controller, err)
		}
	}
	return nil
}

// OOMKilled reports whether the kernel killed something in this cgroup for
// exceeding the memory limit. Without this, an OOM is indistinguishable from
// "the script exited 137 for its own reasons", and the operator cannot tell a
// misbehaving agent from an under-provisioned one.
func (c *cgroupManager) OOMKilled() bool {
	if c.v2 {
		b, err := os.ReadFile(filepath.Join(c.path, "memory.events"))
		if err != nil {
			return false
		}
		for _, line := range strings.Split(string(b), "\n") {
			// "oom_kill N" - any non-zero means the kernel killed a task here.
			if strings.HasPrefix(line, "oom_kill ") {
				n, _ := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "oom_kill ")))
				return n > 0
			}
		}
		return false
	}
	dir, ok := c.v1dirs["memory"]
	if !ok {
		return false
	}
	// Prefer memory.oom_control's oom_kill counter: it counts actual kills.
	// memory.failcnt only counts times the limit was reached, which also
	// happens when reclaim succeeds and nothing is killed - so using failcnt
	// alone reports an OOM for workloads that merely ran close to their limit.
	if b, err := os.ReadFile(filepath.Join(dir, "memory.oom_control")); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "oom_kill ") {
				n, _ := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "oom_kill ")))
				return n > 0
			}
		}
	}
	// Older kernels have no oom_kill field; failcnt is the best available
	// signal there, and over-reporting is better than telling an operator a
	// memory kill was something else.
	b, err := os.ReadFile(filepath.Join(dir, "memory.failcnt"))
	if err != nil {
		return false
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n > 0
}

// PeakMemoryBytes reports high-water usage, for cost attribution and for
// telling an operator that an agent is close to its limit before it is killed.
func (c *cgroupManager) PeakMemoryBytes() int64 {
	var p string
	if c.v2 {
		p = filepath.Join(c.path, "memory.peak")
	} else if dir, ok := c.v1dirs["memory"]; ok {
		p = filepath.Join(dir, "memory.max_usage_in_bytes")
	} else {
		return 0
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return n
}

// procs lists pids still alive in the cgroup.
func (c *cgroupManager) procs() []int {
	var files []string
	if c.v2 {
		files = []string{filepath.Join(c.path, "cgroup.procs")}
	} else {
		for _, d := range c.v1dirs {
			files = append(files, filepath.Join(d, "cgroup.procs"))
		}
	}
	seen := map[int]bool{}
	var out []int
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Fields(string(b)) {
			if pid, err := strconv.Atoi(line); err == nil && !seen[pid] {
				seen[pid] = true
				out = append(out, pid)
			}
		}
	}
	return out
}

// Destroy removes the cgroup. Callers must have killed the members first;
// KillAll does that.
func (c *cgroupManager) Destroy() {
	if c == nil {
		return
	}
	dirs := []string{c.path}
	if !c.v2 {
		dirs = dirs[:0]
		for _, d := range c.v1dirs {
			dirs = append(dirs, d)
		}
	}
	// rmdir fails with EBUSY while the cgroup still has members, and the
	// SIGKILLs we sent are delivered asynchronously. Retry briefly rather than
	// leaking a directory per sandbox - at hundreds of sandboxes a minute that
	// leak becomes a real problem on the node.
	for _, d := range dirs {
		if d == "" {
			continue
		}
		for attempt := 0; attempt < 20; attempt++ {
			if err := os.Remove(d); err == nil || os.IsNotExist(err) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

var errCgroupUnavailable = errors.New("sandbox: no usable cgroup hierarchy")
