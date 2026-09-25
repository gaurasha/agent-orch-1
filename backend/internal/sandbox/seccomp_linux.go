//go:build linux && amd64

package sandbox

import (
	"fmt"
	"syscall"
	"unsafe"
)

// This file installs a seccomp-BPF filter by hand.
//
// Why hand-rolled rather than libseccomp: libseccomp needs cgo and a system
// library, which would make the sandbox - the one component whose correctness
// matters most - depend on a build toolchain we cannot audit at a glance. The
// filter below is ~40 instructions of classic BPF and can be read in full.
//
// Why a denylist rather than an allowlist: an allowlist is strictly stronger
// and is what Docker ships (~350 permitted syscalls, see
// https://github.com/moby/moby/blob/master/profiles/seccomp/default.json).
// But an incomplete allowlist breaks real workloads in confusing ways -
// Python's importlib, glibc's NSS, and CPU-feature probing all reach for
// syscalls people forget. For this system the syscall filter is layer 6 of 8,
// and in production it is *superseded* by gVisor, which implements the kernel
// ABI in user space and makes the host syscall surface irrelevant. So we spend
// the complexity budget on removing known escape primitives rather than on
// enumerating everything benign. See DEEP_DIVE.md "D2 - sandbox technology".
//
// Reference for the structures: seccomp(2), and
// https://www.kernel.org/doc/html/latest/userspace-api/seccomp_filter.html

// classic BPF instruction (struct sock_filter)
type sockFilter struct {
	Code uint16
	JT   uint8
	JF   uint8
	K    uint32
}

// struct sock_fprog
type sockFprog struct {
	Len    uint16
	_      [6]byte // padding so Filter lands on an 8-byte boundary on amd64
	Filter *sockFilter
}

const (
	// BPF opcodes
	bpfLD  = 0x00
	bpfW   = 0x00
	bpfABS = 0x20
	bpfJMP = 0x05
	bpfJEQ = 0x10
	bpfJGE = 0x30
	bpfK   = 0x00
	bpfRET = 0x06

	// seccomp_data field offsets
	offNR   = 0
	offArch = 4

	auditArchX8664 = 0xC000003E
	x32SyscallBit  = 0x40000000

	retKillProcess = 0x80000000
	retErrno       = 0x00050000
	retAllow       = 0x7FFF0000

	prSetNoNewPrivs = 38
	sysSeccomp      = 317 // __NR_seccomp on linux/amd64
	setModeFilter   = 1
	flagTSync       = 1 // apply to every thread, which matters: Go is multi-threaded
)

// deniedSyscalls are the primitives an escape actually needs. Each entry has a
// reason, because a security control nobody can explain gets deleted later.
var deniedSyscalls = []struct {
	nr     uint32
	name   string
	reason string
}{
	{165, "mount", "re-mounting the rootfs read-write, or mounting host paths"},
	{166, "umount2", "unmounting our read-only protections"},
	{155, "pivot_root", "escaping the mount namespace"},
	{161, "chroot", "confusing path resolution to break out"},
	{101, "ptrace", "reading memory of, or injecting into, a sibling process"},
	{310, "process_vm_readv", "reading another process's memory directly"},
	{311, "process_vm_writev", "writing another process's memory directly"},
	{175, "init_module", "loading kernel code"},
	{313, "finit_module", "loading kernel code from an fd"},
	{176, "delete_module", "unloading kernel code"},
	{246, "kexec_load", "replacing the running kernel"},
	{320, "kexec_file_load", "replacing the running kernel"},
	{321, "bpf", "loading eBPF programs; a recurring source of LPEs"},
	{298, "perf_event_open", "a recurring source of LPEs and a side channel"},
	{248, "add_key", "kernel keyring access"},
	{249, "request_key", "kernel keyring access"},
	{250, "keyctl", "kernel keyring access"},
	{272, "unshare", "creating fresh namespaces to re-gain capabilities"},
	{308, "setns", "entering another process's namespaces"},
	{169, "reboot", "denial of service against the node"},
	{167, "swapon", "host-wide resource manipulation"},
	{168, "swapoff", "host-wide resource manipulation"},
	{164, "settimeofday", "host-wide clock manipulation"},
	{227, "clock_settime", "host-wide clock manipulation"},
	{159, "adjtimex", "host-wide clock manipulation"},
	{305, "clock_adjtime", "host-wide clock manipulation"},
	{163, "acct", "host-wide accounting manipulation"},
	{179, "quotactl", "host-wide quota manipulation"},
	{135, "personality", "disabling ASLR to make exploitation easier"},
	{323, "userfaultfd", "widening kernel race windows during exploitation"},
	{303, "name_to_handle_at", "filesystem handle escape primitive"},
	{304, "open_by_handle_at", "the classic bind-mount escape primitive"},
	{425, "io_uring_setup", "large kernel attack surface that also bypasses seccomp"},
	{426, "io_uring_enter", "large kernel attack surface that also bypasses seccomp"},
	{427, "io_uring_register", "large kernel attack surface that also bypasses seccomp"},
}

// clone3 gets ENOSYS rather than EPERM on purpose: seccomp cannot inspect its
// arguments (they live behind a struct pointer), so it could otherwise be used
// to create namespaces without us seeing the flags. Returning ENOSYS makes
// modern glibc fall back to clone(2), which we *can* filter. This is exactly
// what Docker does, for the same reason.
const nrClone3 = 435

// DeniedSyscallNames exposes the denylist for documentation and tests.
func DeniedSyscallNames() []string {
	out := make([]string, 0, len(deniedSyscalls)+1)
	for _, d := range deniedSyscalls {
		out = append(out, d.name)
	}
	return append(out, "clone3")
}

// buildFilter assembles the BPF program.
//
// Layout:
//
//	[0] load arch;  if != x86_64 -> KILL          (blocks 32-bit ABI confusion)
//	[.] load nr;    if >= x32 bit -> KILL         (blocks the x32 ABI entirely)
//	[.] clone3      -> ENOSYS                     (force the filterable fallback)
//	[.] per-denied  -> EPERM
//	[.] default     -> ALLOW
func buildFilter() []sockFilter {
	var f []sockFilter
	jmp := func(code uint16, k uint32, jt, jf uint8) {
		f = append(f, sockFilter{Code: code, K: k, JT: jt, JF: jf})
	}

	// Verify the architecture first. Without this check a 32-bit process could
	// use a different syscall numbering and walk straight past the denylist.
	f = append(f, sockFilter{Code: bpfLD | bpfW | bpfABS, K: offArch})
	jmp(bpfJMP|bpfJEQ|bpfK, auditArchX8664, 1, 0) // match -> skip the kill
	f = append(f, sockFilter{Code: bpfRET | bpfK, K: retKillProcess})

	// Load the syscall number for everything below.
	f = append(f, sockFilter{Code: bpfLD | bpfW | bpfABS, K: offNR})

	// Reject the x32 ABI (syscall numbers OR'd with 0x40000000).
	jmp(bpfJMP|bpfJGE|bpfK, x32SyscallBit, 0, 1) // >= bit -> fall through to kill
	f = append(f, sockFilter{Code: bpfRET | bpfK, K: retKillProcess})

	// clone3 -> ENOSYS
	jmp(bpfJMP|bpfJEQ|bpfK, nrClone3, 0, 1)
	f = append(f, sockFilter{Code: bpfRET | bpfK, K: retErrno | uint32(syscall.ENOSYS)})

	// Everything on the denylist -> EPERM. EPERM rather than KILL so the payload
	// gets a legible error it can report, and so the agent's transcript shows
	// *what* it tried - which is valuable signal for the operator.
	for _, d := range deniedSyscalls {
		jmp(bpfJMP|bpfJEQ|bpfK, d.nr, 0, 1)
		f = append(f, sockFilter{Code: bpfRET | bpfK, K: retErrno | uint32(syscall.EPERM)})
	}

	f = append(f, sockFilter{Code: bpfRET | bpfK, K: retAllow})
	return f
}

// applySeccomp installs the filter on the calling thread and, via TSYNC, on
// every other thread in the process. It must be called after PR_SET_NO_NEW_PRIVS
// (unprivileged callers are refused otherwise) and before execve.
func applySeccomp() error {
	filter := buildFilter()
	if len(filter) > 4096 {
		return fmt.Errorf("seccomp: filter too long (%d instructions)", len(filter))
	}
	prog := sockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	_, _, errno := syscall.RawSyscall(sysSeccomp, setModeFilter, flagTSync,
		uintptr(unsafe.Pointer(&prog)))
	if errno != 0 {
		return fmt.Errorf("seccomp: SECCOMP_SET_MODE_FILTER: %w", errno)
	}
	return nil
}

// setNoNewPrivs stops any execve from ever gaining privilege - it neuters
// setuid binaries and file capabilities for this process and all descendants,
// permanently. It is also a precondition for an unprivileged seccomp filter.
func setNoNewPrivs() error {
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0); errno != 0 {
		return fmt.Errorf("prctl(PR_SET_NO_NEW_PRIVS): %w", errno)
	}
	return nil
}
