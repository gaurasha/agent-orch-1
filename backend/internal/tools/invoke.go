package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gaurasha/agent-orch/backend/internal/creds"
	"github.com/gaurasha/agent-orch/backend/internal/sandbox"
)

// Invocation is an authorized tool call, ready to execute. Authorization has
// already happened; by the time an Invocation exists the decision is made.
type Invocation struct {
	Tool      Tool
	Args      map[string]any
	RunID     string
	TenantID  string
	Workspace string
}

// Outcome is the result handed back to the agent.
type Outcome struct {
	Content   string
	IsError   bool
	Duration  time.Duration
	Truncated bool
	// Meta is recorded in the audit log but not returned to the model. It is
	// where operational detail lives: which sandbox driver ran, how long the
	// cold start was, whether a credential was minted and which one.
	Meta map[string]any
}

// Invoker executes tool calls.
type Invoker struct {
	sandbox   sandbox.Driver
	broker    creds.Broker
	workspace *WorkspaceManager
	http      *http.Client
	// selfBin is the path to this binary, bind-mounted into sandboxes so that
	// helper personalities (`gh`, `aoconvert`) are available inside without
	// shipping a second image. This is the busybox pattern.
	selfBin      string
	githubAPI    string
	maxResultLen int
}

func NewInvoker(drv sandbox.Driver, broker creds.Broker, ws *WorkspaceManager, githubAPI string) *Invoker {
	self, err := os.Executable()
	if err != nil {
		self = "/proc/self/exe"
	}
	return &Invoker{
		sandbox: drv, broker: broker, workspace: ws,
		// A bounded client: no redirects to unexpected hosts, a hard timeout,
		// and no connection reuse across tenants would be better still.
		http: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				// Following redirects would let an allowed host bounce a call
				// to a denied one, defeating the host allowlist entirely.
				return http.ErrUseLastResponse
			},
		},
		selfBin: self, githubAPI: githubAPI, maxResultLen: 64 << 10,
	}
}

// Invoke dispatches on tool kind.
func (in *Invoker) Invoke(ctx context.Context, iv Invocation) (Outcome, error) {
	start := time.Now()
	var out Outcome
	var err error
	switch iv.Tool.Backend.Kind {
	case KindExec:
		out, err = in.invokeExec(ctx, iv)
	case KindFS:
		out, err = in.invokeFS(ctx, iv)
	case KindHTTP:
		out, err = in.invokeHTTP(ctx, iv)
	case KindCLI:
		out, err = in.invokeCLI(ctx, iv)
	default:
		return Outcome{}, fmt.Errorf("tools: tool %q has no backend kind", iv.Tool.Schema.Name)
	}
	out.Duration = time.Since(start)
	if out.Meta == nil {
		out.Meta = map[string]any{}
	}
	out.Meta["duration_ms"] = out.Duration.Milliseconds()
	out.Content = in.clamp(out.Content, &out)
	return out, err
}

func (in *Invoker) clamp(s string, out *Outcome) string {
	if len(s) <= in.maxResultLen {
		return s
	}
	out.Truncated = true
	return s[:in.maxResultLen] + "\n...[result truncated: exceeded the tool result size limit]"
}

// ---------------------------------------------------------------------------
// exec: model-authored code, maximum containment
// ---------------------------------------------------------------------------

func (in *Invoker) invokeExec(ctx context.Context, iv Invocation) (Outcome, error) {
	argv, extraRO, err := in.execArgv(iv)
	if err != nil {
		return Outcome{Content: err.Error(), IsError: true}, nil
	}
	res, err := in.sandbox.Run(ctx, sandbox.Spec{
		RunID: iv.RunID, TenantID: iv.TenantID,
		Argv:         argv,
		WorkspaceDir: iv.Workspace,
		// The whole point: code the model wrote gets no network, ever.
		Network:       sandbox.NetworkNone,
		ExtraReadOnly: extraRO,
		Env:           sandbox.SafeEnv(),
		Limits: sandbox.Limits{
			Wall: iv.Tool.Backend.Timeout,
		},
	})
	if err != nil {
		// A sandbox setup failure is our fault, not the agent's. Surface it as
		// an infrastructure error so the runtime can retry rather than letting
		// the model conclude its script was wrong.
		return Outcome{}, fmt.Errorf("sandbox: %w", err)
	}
	return Outcome{
		Content: formatExecResult(res),
		IsError: res.ExitCode != 0,
		Meta: map[string]any{
			"driver": res.Driver, "exit_code": res.ExitCode,
			"cold_start_ms": res.ColdStartMS, "oom_killed": res.OOMKilled,
			"timed_out": res.TimedOut, "truncated": res.Truncated,
		},
		Truncated: res.Truncated,
	}, nil
}

func (in *Invoker) execArgv(iv Invocation) ([]string, map[string]string, error) {
	name := iv.Tool.Schema.Name
	if name == "doc.convert" {
		input, _ := iv.Args["input"].(string)
		output, _ := iv.Args["output"].(string)
		from, _ := iv.Args["from"].(string)
		to, _ := iv.Args["to"].(string)
		if from == "" {
			from = "markdown"
		}
		if to == "" {
			to = "html"
		}
		for _, p := range []string{input, output} {
			if err := validateRelPath(p); err != nil {
				return nil, nil, err
			}
		}
		// Prefer the real converter when the image has it; otherwise use our
		// own, so the demo works on a machine without pandoc installed.
		if pandoc, err := findPandoc(); err == nil {
			return []string{pandoc, "-f", from, "-t", to, "-o", output, input}, nil, nil
		}
		helper := sandbox.HelperBinDir + "/aoconvert"
		return []string{helper, "--from", from, "--to", to, "--in", input, "--out", output},
			map[string]string{helper: in.selfBin}, nil
	}
	script, _ := iv.Args["script"].(string)
	bin := iv.Tool.Backend.Binary
	if len(bin) == 0 {
		return nil, nil, errors.New("tool has no configured binary")
	}
	return append(append([]string{}, bin...), script), nil, nil
}

func findPandoc() (string, error) {
	for _, p := range []string{"/usr/bin/pandoc", "/usr/local/bin/pandoc"} {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", os.ErrNotExist
}

// formatExecResult renders a sandbox result for the model.
//
// Being explicit about *why* something failed matters: "killed after exceeding
// its 256 MiB memory limit" leads a competent model to process the file in
// chunks, whereas a bare "exit 137" leads it to retry the same thing.
func formatExecResult(r sandbox.Result) string {
	var b strings.Builder
	if r.Stdout != "" {
		b.WriteString(r.Stdout)
	}
	if r.Stderr != "" {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("[stderr]\n")
		b.WriteString(r.Stderr)
	}
	switch {
	case r.TimedOut:
		fmt.Fprintf(&b, "\n\n[sandbox] Killed after exceeding the wall-clock limit (%s).",
			r.Duration.Truncate(time.Millisecond))
	case r.OOMKilled:
		b.WriteString("\n\n[sandbox] Killed by the kernel for exceeding its memory limit. " +
			"Process the data in smaller chunks or stream it.")
	case r.ExitCode != 0:
		fmt.Fprintf(&b, "\n\n[sandbox] Exited with status %d.", r.ExitCode)
	}
	if b.Len() == 0 {
		return "[sandbox] Command produced no output and exited 0."
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// fs: workspace access, path-traversal safe
// ---------------------------------------------------------------------------

func (in *Invoker) invokeFS(_ context.Context, iv Invocation) (Outcome, error) {
	path, _ := iv.Args["path"].(string)
	switch iv.Tool.Schema.Name {
	case "fs.write":
		content, _ := iv.Args["content"].(string)
		if err := in.workspace.Write(iv.Workspace, path, content); err != nil {
			return Outcome{Content: err.Error(), IsError: true}, nil
		}
		return Outcome{Content: fmt.Sprintf("Wrote %d bytes to %s.", len(content), path)}, nil
	case "fs.read":
		content, err := in.workspace.Read(iv.Workspace, path)
		if err != nil {
			return Outcome{Content: err.Error(), IsError: true}, nil
		}
		return Outcome{Content: content}, nil
	case "fs.list":
		listing, err := in.workspace.List(iv.Workspace, path)
		if err != nil {
			return Outcome{Content: err.Error(), IsError: true}, nil
		}
		return Outcome{Content: listing}, nil
	}
	return Outcome{}, fmt.Errorf("tools: unhandled fs tool %q", iv.Tool.Schema.Name)
}

// ---------------------------------------------------------------------------
// http: the gateway makes the call; the agent never holds the credential
// ---------------------------------------------------------------------------

func (in *Invoker) invokeHTTP(ctx context.Context, iv Invocation) (Outcome, error) {
	rawURL, _ := iv.Args["url"].(string)
	body, _ := iv.Args["body"].(string)
	method := iv.Tool.Backend.Method
	if method == "" {
		method = http.MethodGet
	}

	ctx, cancel := context.WithTimeout(ctx, iv.Tool.Backend.Timeout)
	defer cancel()

	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		return Outcome{Content: "invalid request: " + err.Error(), IsError: true}, nil
	}
	req.Header.Set("User-Agent", "agentorch-tool-gateway/1.0")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	meta := map[string]any{"method": method, "url": rawURL}
	if ref := iv.Tool.Backend.CredRef; ref != "" {
		cred, err := in.broker.Mint(ctx, iv.TenantID, ref, 60*time.Second)
		if err != nil {
			// A missing credential is a configuration problem. Report it
			// without the ref's value and without implying the agent did
			// anything wrong.
			if errors.Is(err, creds.ErrNoSuchRef) {
				return Outcome{
					Content: fmt.Sprintf("This tool needs a credential (%s) that is not configured for your tenant.", ref),
					IsError: true,
				}, nil
			}
			return Outcome{}, fmt.Errorf("mint credential: %w", err)
		}
		// The ONLY place the plaintext leaves the broker: straight into a
		// header on a request the gateway itself makes. It is never written to
		// the event log, never returned, never placed in an environment.
		req.Header.Set("Authorization", "Bearer "+cred.Value.Reveal())
		meta["credential_ref"] = cred.Ref
		meta["credential_id"] = cred.ID
	}

	resp, err := in.http.Do(req)
	if err != nil {
		return Outcome{Content: "request failed: " + err.Error(), IsError: true}, nil
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, int64(in.maxResultLen)))
	if err != nil {
		return Outcome{Content: "reading response failed: " + err.Error(), IsError: true}, nil
	}
	meta["status"] = resp.StatusCode
	meta["response_bytes"] = len(raw)
	return Outcome{
		Content: fmt.Sprintf("HTTP %d\n\n%s", resp.StatusCode, raw),
		IsError: resp.StatusCode >= 400,
		Meta:    meta,
	}, nil
}

// ---------------------------------------------------------------------------
// cli: the interesting one
// ---------------------------------------------------------------------------

// invokeCLI answers the question "how does gh get a token without the agent
// being able to read it?".
//
// The answer is that gh does not run in the agent's sandbox at all. It runs in
// a SECOND sandbox that the agent has no handle on:
//
//	agent sandbox                     broker sandbox
//	-------------                     --------------
//	model-authored code               a trusted binary from our image
//	no network                        egress to the allowlisted API only
//	no credential in env              GH_TOKEN in env
//	/work bind-mounted rw             the same /work bind-mounted rw
//	separate pid/user/mount ns        separate pid/user/mount ns
//
// Because they are different processes in different PID and user namespaces,
// the agent cannot read the broker's /proc/<pid>/environ, cannot ptrace it, and
// never receives the token in any response. What crosses the boundary is argv
// in and stdout out - and argv has already been through parameter policy, which
// is what blocks `gh auth token` while permitting `gh pr create`.
//
// The workspace is shared deliberately: `gh pr create --body-file report.md`
// has to be able to read what the agent wrote.
func (in *Invoker) invokeCLI(ctx context.Context, iv Invocation) (Outcome, error) {
	argv := toStringSlice(iv.Args["argv"])
	if len(argv) == 0 {
		return Outcome{Content: "argv must be a non-empty array of strings", IsError: true}, nil
	}

	cred, err := in.broker.Mint(ctx, iv.TenantID, iv.Tool.Backend.CredRef, 60*time.Second)
	if err != nil {
		if errors.Is(err, creds.ErrNoSuchRef) {
			return Outcome{
				Content: fmt.Sprintf("The %s tool needs a credential that is not configured for your tenant.",
					iv.Tool.Schema.Name),
				IsError: true,
			}, nil
		}
		return Outcome{}, fmt.Errorf("mint credential: %w", err)
	}
	// Short TTL plus explicit revocation: even if the broker sandbox were
	// compromised, the stolen credential is useful for seconds, not days.
	defer func() {
		revokeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = in.broker.Revoke(revokeCtx, cred.ID)
	}()

	guestBin := sandbox.HelperBinDir + "/" + iv.Tool.Backend.Binary[0]
	res, err := in.sandbox.Run(ctx, sandbox.Spec{
		RunID: iv.RunID + "-broker", TenantID: iv.TenantID,
		Argv:         append([]string{guestBin}, argv...),
		WorkspaceDir: iv.Workspace,
		// Egress, because this sandbox has to reach the API. See the note in
		// jail_linux.go about how this is narrowed in production.
		Network: sandbox.NetworkProxy,
		// Our own binary, bound in read-only under the CLI's name. main()
		// dispatches on argv[0], the busybox pattern.
		ExtraReadOnly: map[string]string{guestBin: in.selfBin},
		Env: sandbox.SafeEnv(
			// The plaintext credential enters THIS process environment only.
			// The agent's sandbox is a different process in a different PID
			// namespace and cannot read it.
			"GH_TOKEN="+cred.Value.Reveal(),
			"GITHUB_API_BASE="+in.githubAPI,
			"AGENTORCH_TENANT="+iv.TenantID,
		),
		Limits: sandbox.Limits{Wall: iv.Tool.Backend.Timeout},
	})
	if err != nil {
		return Outcome{}, fmt.Errorf("broker sandbox: %w", err)
	}

	content := formatExecResult(res)
	// Belt and braces. Policy should already prevent any command that prints a
	// credential, but if one ever slips through, scrubbing here means the value
	// still never reaches the event log or the model context.
	content = creds.Scrub(content, cred.Value)

	return Outcome{
		Content: content,
		IsError: res.ExitCode != 0,
		Meta: map[string]any{
			"driver": res.Driver, "exit_code": res.ExitCode,
			"credential_ref": cred.Ref, "credential_id": cred.ID,
			"credential_ttl_s": int(time.Until(cred.ExpiresAt).Seconds()),
			"argv0":            argv[0],
		},
		Truncated: res.Truncated,
	}, nil
}

func toStringSlice(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func validateRelPath(p string) error {
	if p == "" {
		return errors.New("path must not be empty")
	}
	if filepath.IsAbs(p) {
		return fmt.Errorf("path %q must be relative to the workspace root", p)
	}
	if strings.Contains(p, "..") {
		return fmt.Errorf("path %q must not contain '..'", p)
	}
	return nil
}
