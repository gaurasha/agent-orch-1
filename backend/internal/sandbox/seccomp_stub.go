//go:build !(linux && amd64)

package sandbox

import "errors"

// On platforms where we have not written and verified a filter, refuse to
// pretend. Returning nil here would let the rest of the system report
// "seccomp: applied" on, say, arm64 while no filter existed at all.
func applySeccomp() error {
	return errors.New("sandbox: seccomp filter is only implemented for linux/amd64; " +
		"run the docker or kubernetes driver on other platforms")
}

func setNoNewPrivs() error {
	return errors.New("sandbox: no_new_privs is only implemented for linux")
}

// DeniedSyscallNames returns nothing where no filter exists.
func DeniedSyscallNames() []string { return nil }
