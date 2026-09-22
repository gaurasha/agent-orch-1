// Command agentorch is the single binary for the whole platform.
//
// One binary, several personalities, selected by argv[0] or a subcommand. This
// is the busybox / hyperkube pattern, and it is here for three reasons:
//
//   - One image to build, scan, sign and deploy. In a system whose entire
//     point is containing untrusted code, a smaller set of artefacts to trust
//     is worth real complexity elsewhere.
//   - The sandbox helpers (`gh`, `aoconvert`) can be bind-mounted into a
//     sandbox as a single static file with no image, no package manager and no
//     shared library surface.
//   - `make demo` starts everything in one process, so the reviewer runs one
//     command; the same code runs as separate Deployments in Kubernetes.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/gaurasha/agent-orch/backend/internal/sandbox"
)

func main() {
	// FIRST, before anything else touches the process: if we were re-executed
	// as a sandbox init, build the jail and exec the payload. This must happen
	// before flag parsing, logging setup or any goroutine starts.
	if sandbox.MaybeRunInit() {
		return
	}

	// Personality dispatch by argv[0], for binaries bind-mounted into a sandbox
	// under another name.
	switch filepath.Base(os.Args[0]) {
	case "gh":
		os.Exit(runGH(os.Args[1:]))
	case "aoconvert":
		os.Exit(runConvert(os.Args[1:]))
	}

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "serve":
		os.Exit(runServe(args))
	case "gh":
		os.Exit(runGH(args))
	case "convert":
		os.Exit(runConvert(args))
	case "demo":
		os.Exit(runDemo(args))
	case "version":
		fmt.Println("agentorch dev")
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `agentorch - agentic orchestration platform

Usage:
  agentorch serve   [flags]   Run platform services (control plane, workers, gateway)
  agentorch demo    [flags]   Run the scripted end-to-end demonstration and exit
  agentorch gh      <args>    GitHub CLI personality (used inside the credential broker sandbox)
  agentorch convert <args>    Document converter personality (used inside the agent sandbox)
  agentorch version

Run 'agentorch serve -h' for the service flags.
`)
}
