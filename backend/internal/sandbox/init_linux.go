//go:build linux

package sandbox

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"unsafe"
)

// MaybeRunInit is called at the very top of main(). When the process was
// re-executed as a sandbox init it builds the jail and execs the payload,
// never returning. Otherwise it returns false and the binary proceeds normally.
//
// Re-executing our own binary is how every container runtime does this: the
// setup has to happen *inside* the new namespaces, which only exist after
// clone(2), and Go cannot safely run arbitrary code between fork and exec in
// the child (the runtime's threads and locks do not survive fork). Re-exec
// gives us a clean single-threaded starting point inside the namespaces.
func MaybeRunInit() bool {
	if os.Getenv(initEnvVar) != "1" {
		return false
	}
	if err := runInit(); err != nil {
		// Marked so the parent can tell "the jail failed to build" apart from
		// "the payload exited non-zero", which are very different for a user.
		fmt.Fprintf(os.Stderr, "%s %v\n", setupFailureMarker, err)
		os.Exit(exitSetupFailure)
	}
	os.Exit(0) // unreachable: runInit ends in execve
	return true
}

func runInit() error {
	cfgFile := os.NewFile(3, "jailcfg")
	syncFile := os.NewFile(4, "jailsync")
	if cfgFile == nil || syncFile == nil {
		return fmt.Errorf("missing config/sync file descriptors")
	}
	var c jailConfig
	if err := json.NewDecoder(cfgFile).Decode(&c); err != nil {
		return fmt.Errorf("decode jail config: %w", err)
	}
	cfgFile.Close()

	// Block until the parent has placed us in the cgroup. Until this returns we
	// are unbounded, so we do nothing but wait.
	var b [1]byte
	if _, err := syncFile.Read(b[:]); err != nil {
		return fmt.Errorf("wait for parent release: %w", err)
	}
	syncFile.Close()

	if err := syscall.Sethostname([]byte("sandbox")); err != nil {
		return fmt.Errorf("sethostname: %w", err)
	}
	if err := buildRootfs(c); err != nil {
		return err
	}
	if err := applyRlimits(c); err != nil {
		return err
	}

	// The remaining order is load-bearing and each step depends on privilege
	// that the NEXT step removes. Getting it wrong does not fail open, but it
	// does fail confusingly: dropping capabilities before the uid switch makes
	// setgroups(2) return EPERM, and glibc responds to a failed setxid
	// broadcast by calling abort(), which surfaces as a SIGABRT with no
	// obvious cause.
	//
	//   1. no_new_privs   - must come first; it is a precondition for
	//                       installing a seccomp filter unprivileged, and it
	//                       permanently neuters setuid binaries and file
	//                       capabilities for everything below.
	//   2. bounding set   - needs CAP_SETPCAP, which we still hold as uid 0 in
	//                       the user namespace. Emptying it means no execve in
	//                       this tree can ever ACQUIRE a capability.
	//   3. uid/gid drop   - needs CAP_SETGID/CAP_SETUID, which we also still
	//                       hold. Changing away from uid 0 makes the kernel
	//                       clear our permitted and effective sets for us.
	//   4. clear caps     - belt and braces; a no-op after (3) but explicit.
	//   5. seccomp        - last, so the filter does not have to permit any of
	//                       the privilege-dropping syscalls above.
	//   6. execve
	if err := setNoNewPrivs(); err != nil {
		return err
	}
	if err := dropBoundingSet(); err != nil {
		return err
	}
	if err := syscall.Setgroups([]int{}); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	if err := syscall.Setgid(c.GID); err != nil {
		return fmt.Errorf("setgid(%d): %w", c.GID, err)
	}
	if err := syscall.Setuid(c.UID); err != nil {
		return fmt.Errorf("setuid(%d): %w", c.UID, err)
	}
	if err := clearCapabilities(); err != nil {
		return err
	}
	if c.Seccomp {
		if err := applySeccomp(); err != nil {
			return fmt.Errorf("install seccomp filter: %w", err)
		}
	}

	bin, err := lookPathIn(c.Argv[0], c.Env)
	if err != nil {
		return fmt.Errorf("resolve %q: %w", c.Argv[0], err)
	}
	return syscall.Exec(bin, c.Argv, c.Env)
}

// buildRootfs assembles a minimal filesystem view and pivots into it.
func buildRootfs(c jailConfig) error {
	// Detach our mount propagation from the host. Without MS_PRIVATE our mounts
	// would propagate back out and be visible on the host - and, worse, our
	// later unmounts could tear down host mounts.
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make mounts private: %w", err)
	}
	// A tmpfs root means nothing the payload writes outside /work and /tmp
	// touches a real disk, and everything vanishes when the namespace dies.
	if err := syscall.Mount("tmpfs", c.Root, "tmpfs", 0, "size=8m,mode=0755"); err != nil {
		return fmt.Errorf("mount rootfs tmpfs: %w", err)
	}

	mkdir := func(p string) (string, error) {
		full := filepath.Join(c.Root, p)
		if err := os.MkdirAll(full, 0o755); err != nil {
			return "", fmt.Errorf("mkdir %s: %w", p, err)
		}
		return full, nil
	}

	// System directories, read-only. On merged-/usr distributions /bin, /sbin
	// and /lib are symlinks into /usr; replicate the symlink rather than
	// bind-mounting through it, so the layout inside matches outside and
	// hard-coded interpreter paths such as /bin/sh keep working.
	for _, dir := range []string{"/usr", "/bin", "/sbin", "/lib", "/lib64", "/lib32"} {
		fi, err := os.Lstat(dir)
		if err != nil {
			continue // not present on this distro
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(dir)
			if err != nil {
				return fmt.Errorf("readlink %s: %w", dir, err)
			}
			if err := os.Symlink(target, filepath.Join(c.Root, dir)); err != nil && !os.IsExist(err) {
				return fmt.Errorf("symlink %s -> %s: %w", dir, target, err)
			}
			continue
		}
		if err := bindReadOnly(dir, filepath.Join(c.Root, dir)); err != nil {
			return err
		}
	}

	// A synthesised /etc rather than the host's. Binding host /etc would expose
	// machine configuration, and any secret an operator ever drops in there,
	// to every tenant's agents.
	if err := buildEtc(c.Root, c.WithNetwork); err != nil {
		return err
	}

	if _, err := mkdir("/proc"); err != nil {
		return err
	}
	// A fresh procfs, so /proc shows only our PID namespace. Without this the
	// payload could read the host's process list and command lines - which is
	// one of the places credentials leak.
	if err := syscall.Mount("proc", filepath.Join(c.Root, "proc"), "proc",
		syscall.MS_NOSUID|syscall.MS_NOEXEC|syscall.MS_NODEV, ""); err != nil {
		return fmt.Errorf("mount /proc: %w", err)
	}

	tmpDir, err := mkdir("/tmp")
	if err != nil {
		return err
	}
	if err := syscall.Mount("tmpfs", tmpDir, "tmpfs",
		syscall.MS_NOSUID|syscall.MS_NODEV,
		fmt.Sprintf("size=%d,mode=1777", c.TmpfsBytes)); err != nil {
		return fmt.Errorf("mount /tmp: %w", err)
	}

	if err := buildDev(c.Root); err != nil {
		return err
	}

	// The writable workspace: the only thing that survives the sandbox.
	workDir, err := mkdir("/work")
	if err != nil {
		return err
	}
	if err := syscall.Mount(c.Workspace, workDir, "",
		syscall.MS_BIND|syscall.MS_NOSUID|syscall.MS_NODEV, ""); err != nil {
		return fmt.Errorf("bind workspace: %w", err)
	}
	// Re-apply nosuid/nodev: the flags on the original mount win unless we
	// explicitly remount, so a workspace on a permissive filesystem could
	// otherwise carry setuid binaries in.
	if err := syscall.Mount("", workDir, "",
		syscall.MS_REMOUNT|syscall.MS_BIND|syscall.MS_NOSUID|syscall.MS_NODEV, ""); err != nil {
		return fmt.Errorf("harden workspace mount: %w", err)
	}

	for guest, host := range c.ExtraReadOnly {
		dst := filepath.Join(c.Root, guest)
		fi, err := os.Stat(host)
		if err != nil {
			return fmt.Errorf("extra read-only %s: %w", host, err)
		}
		if fi.IsDir() {
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
		} else {
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(dst, nil, 0o644); err != nil {
				return err
			}
		}
		if err := bindReadOnly(host, dst); err != nil {
			return err
		}
	}

	// pivot_root rather than chroot: chroot only changes the path resolution
	// root and is escapable by a process that holds a directory fd outside it,
	// which is a decades-old trick. pivot_root replaces the mount namespace's
	// root outright, and after unmounting the old root there is no longer any
	// mount referring to the host filesystem at all.
	oldRoot := filepath.Join(c.Root, ".oldroot")
	if err := os.MkdirAll(oldRoot, 0o700); err != nil {
		return fmt.Errorf("mkdir put_old: %w", err)
	}
	if err := syscall.PivotRoot(c.Root, oldRoot); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := syscall.Chdir("/"); err != nil {
		return fmt.Errorf("chdir after pivot: %w", err)
	}
	if err := syscall.Unmount("/.oldroot", syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("detach old root: %w", err)
	}
	if err := os.Remove("/.oldroot"); err != nil {
		return fmt.Errorf("remove put_old: %w", err)
	}
	// Seal the root. /work and /tmp keep their own writable mounts.
	if err := syscall.Mount("", "/", "", syscall.MS_REMOUNT|syscall.MS_RDONLY, ""); err != nil {
		return fmt.Errorf("remount root read-only: %w", err)
	}
	if err := syscall.Chdir("/work"); err != nil {
		return fmt.Errorf("chdir /work: %w", err)
	}
	return nil
}

func bindReadOnly(host, dst string) error {
	fi, err := os.Stat(host)
	if err != nil {
		return fmt.Errorf("stat %s: %w", host, err)
	}
	if fi.IsDir() {
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dst, err)
		}
	} else if _, err := os.Stat(dst); os.IsNotExist(err) {
		if err := os.WriteFile(dst, nil, 0o644); err != nil {
			return fmt.Errorf("touch %s: %w", dst, err)
		}
	}
	if err := syscall.Mount(host, dst, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("bind %s: %w", host, err)
	}
	// A bind mount ignores flags on the initial call; read-only requires a
	// second remount. Getting this wrong is a classic container bug that
	// silently leaves the mount writable.
	if err := syscall.Mount("", dst, "", syscall.MS_REMOUNT|syscall.MS_BIND|
		syscall.MS_RDONLY|syscall.MS_REC|syscall.MS_NOSUID|syscall.MS_NODEV, ""); err != nil {
		return fmt.Errorf("remount %s read-only: %w", dst, err)
	}
	return nil
}

// buildEtc writes the smallest /etc that lets a normal toolchain work.
func buildEtc(root string, withNetwork bool) error {
	etc := filepath.Join(root, "etc")
	if err := os.MkdirAll(etc, 0o755); err != nil {
		return fmt.Errorf("mkdir /etc: %w", err)
	}
	files := map[string]string{
		"passwd": "root:x:0:0:root:/root:/bin/sh\n" +
			fmt.Sprintf("sandbox:x:%d:%d:sandbox:/work:/bin/sh\n", containerUID, containerUID) +
			"nobody:x:65534:65534:nobody:/nonexistent:/bin/false\n",
		"group": "root:x:0:\n" +
			fmt.Sprintf("sandbox:x:%d:\n", containerUID) +
			"nogroup:x:65534:\n",
		"hostname": "sandbox\n",
		"hosts":    "127.0.0.1 localhost sandbox\n::1 localhost\n",
		// Empty by default: with no network there is no resolver, and a stale
		// host resolv.conf would leak internal DNS server addresses - free
		// reconnaissance for an attacker who lands here. Populated only for a
		// sandbox that is meant to have egress.
		"resolv.conf": resolvConf(withNetwork),
		// files-only NSS: no nis, no ldap, no sss. Those modules would try to
		// dlopen and talk to sockets that must not exist here.
		"nsswitch.conf": "passwd: files\ngroup: files\nhosts: files\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(etc, name), []byte(content), 0o644); err != nil {
			return fmt.Errorf("write /etc/%s: %w", name, err)
		}
	}
	// Bind a short allowlist of host /etc entries that the dynamic linker and
	// TLS verification genuinely need. Everything else stays out.
	for _, name := range []string{"ld.so.cache", "ld.so.conf", "ld.so.conf.d", "ssl", "alternatives", "localtime"} {
		host := filepath.Join("/etc", name)
		if _, err := os.Lstat(host); err != nil {
			continue
		}
		if err := bindReadOnly(host, filepath.Join(etc, name)); err != nil {
			return err
		}
	}
	return nil
}

// buildDev provides the handful of character devices normal programs expect.
// mknod is not permitted inside a user namespace, so each node is bind-mounted
// from the host instead. Note what is absent: no /dev/kmsg, no /dev/mem, no
// block devices, no /dev/kvm.
// resolvConf returns resolver configuration only for sandboxes with egress.
func resolvConf(withNetwork bool) string {
	if !withNetwork {
		return ""
	}
	// Public resolvers rather than the host's: the host's file may name
	// internal DNS servers, and a sandbox has no business learning them.
	return "nameserver 1.1.1.1\nnameserver 8.8.8.8\noptions timeout:2 attempts:2\n"
}

func buildDev(root string) error {
	dev := filepath.Join(root, "dev")
	if err := os.MkdirAll(dev, 0o755); err != nil {
		return fmt.Errorf("mkdir /dev: %w", err)
	}
	if err := syscall.Mount("tmpfs", dev, "tmpfs",
		syscall.MS_NOSUID|syscall.MS_NOEXEC, "size=1m,mode=0755"); err != nil {
		return fmt.Errorf("mount /dev: %w", err)
	}
	for _, node := range []string{"null", "zero", "full", "random", "urandom", "tty"} {
		src := filepath.Join("/dev", node)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		dst := filepath.Join(dev, node)
		if err := os.WriteFile(dst, nil, 0o666); err != nil {
			return fmt.Errorf("touch /dev/%s: %w", node, err)
		}
		if err := syscall.Mount(src, dst, "", syscall.MS_BIND, ""); err != nil {
			return fmt.Errorf("bind /dev/%s: %w", node, err)
		}
	}
	// /dev/shm: POSIX shared memory. Size-capped, because it is tmpfs and
	// therefore counts against node memory.
	shm := filepath.Join(dev, "shm")
	if err := os.MkdirAll(shm, 0o1777); err != nil {
		return fmt.Errorf("mkdir /dev/shm: %w", err)
	}
	if err := syscall.Mount("tmpfs", shm, "tmpfs",
		syscall.MS_NOSUID|syscall.MS_NODEV|syscall.MS_NOEXEC, "size=8m,mode=1777"); err != nil {
		return fmt.Errorf("mount /dev/shm: %w", err)
	}
	for i, name := range []string{"stdin", "stdout", "stderr"} {
		_ = os.Symlink(fmt.Sprintf("/proc/self/fd/%d", i), filepath.Join(dev, name))
	}
	_ = os.Symlink("/proc/self/fd", filepath.Join(dev, "fd"))
	return nil
}

// applyRlimits adds per-process bounds that complement the cgroup's per-tree
// bounds. RLIMIT_NPROC in particular is checked at fork time against the real
// uid, giving a second, independent fork-bomb stop.
func applyRlimits(c jailConfig) error {
	set := func(res int, name string, cur, max uint64) error {
		if err := syscall.Setrlimit(res, &syscall.Rlimit{Cur: cur, Max: max}); err != nil {
			return fmt.Errorf("setrlimit %s: %w", name, err)
		}
		return nil
	}
	if err := set(syscall.RLIMIT_NOFILE, "NOFILE", c.MaxOpenFiles, c.MaxOpenFiles); err != nil {
		return err
	}
	if err := set(syscall.RLIMIT_FSIZE, "FSIZE", c.MaxFileSize, c.MaxFileSize); err != nil {
		return err
	}
	// No core dumps: a core of a compromised payload would land in the
	// workspace and could contain material from other parts of the process.
	if err := set(syscall.RLIMIT_CORE, "CORE", 0, 0); err != nil {
		return err
	}
	const rlimitNProc = 6 // RLIMIT_NPROC; not exported by the syscall package
	if err := set(rlimitNProc, "NPROC", c.MaxProcs, c.MaxProcs); err != nil {
		return err
	}
	return nil
}

// dropBoundingSet empties the capability bounding set so that no execve
// anywhere in this process tree can ever acquire a capability. It requires
// CAP_SETPCAP and must therefore run before the uid drop.
func dropBoundingSet() error {
	const prCapBsetDrop = 24
	// CAP_LAST_CAP moves with kernel versions; walking past the end simply
	// returns EINVAL, which is harmless, so we cover generously.
	for capID := 0; capID <= 64; capID++ {
		_, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prCapBsetDrop, uintptr(capID), 0)
		if errno != 0 && errno != syscall.EINVAL {
			return fmt.Errorf("prctl(PR_CAPBSET_DROP, %d): %w", capID, errno)
		}
	}
	return nil
}

// clearCapabilities zeroes the effective, permitted and inheritable sets.
//
// After switching away from uid 0 the kernel has already cleared these, so
// this is redundant - deliberately. It costs one syscall and it means the
// property is asserted in code rather than inferred from kernel behaviour that
// depends on securebits nobody reads.
//
// Go's syscall package keeps the capability structs unexported, so they are
// declared below to match <linux/capability.h> for _LINUX_CAPABILITY_VERSION_3.
func clearCapabilities() error {
	hdr := capHeader{version: 0x20080522, pid: 0} // pid 0 == "this thread"
	var data [2]capData                           // v3 is a two-element array (64 caps)
	if _, _, errno := syscall.RawSyscall(syscall.SYS_CAPSET,
		uintptr(unsafe.Pointer(&hdr)), uintptr(unsafe.Pointer(&data[0])), 0); errno != 0 {
		return fmt.Errorf("capset(empty): %w", errno)
	}
	return nil
}

// capHeader mirrors struct __user_cap_header_struct.
type capHeader struct {
	version uint32
	pid     int32
}

// capData mirrors struct __user_cap_data_struct.
type capData struct {
	effective   uint32
	permitted   uint32
	inheritable uint32
}

// lookPathIn resolves argv[0] using the PATH from the sandbox environment
// rather than the executor's own PATH, which does not exist in here.
func lookPathIn(name string, env []string) (string, error) {
	if filepath.IsAbs(name) || filepath.Base(name) != name {
		return name, nil
	}
	path := "/usr/local/bin:/usr/bin:/bin"
	for _, kv := range env {
		if len(kv) > 5 && kv[:5] == "PATH=" {
			path = kv[5:]
		}
	}
	old := os.Getenv("PATH")
	_ = os.Setenv("PATH", path)
	defer os.Setenv("PATH", old)
	return exec.LookPath(name)
}

func shortRand() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("sandbox: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
