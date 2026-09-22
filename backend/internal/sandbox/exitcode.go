package sandbox

import (
	"errors"
	"os/exec"
)

// exitCodeOfPortable extracts an exit code without depending on Linux-only
// wait status types, so the docker and kubernetes drivers build everywhere.
func exitCodeOfPortable(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}
