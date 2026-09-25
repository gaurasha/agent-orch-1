// Package authz decides whether a tool call may proceed.
//
// Position on "where does authorization live": at the gateway, authoritatively;
// in the tool schema, for usability; at the tool itself, for defence in depth.
//
//   - Compiling grants into the tool schema the model sees is a UX and cost
//     optimisation (the model does not waste tokens on tools it cannot use,
//     and mostly does not try). It is NOT security: a model can emit any tool
//     name it likes, and an injected prompt will actively try.
//   - The gateway is the enforcement point because it is the only place that
//     sees every call, cannot be bypassed (network policy permits the agent
//     egress to nothing else), and can be audited as one small component.
//   - The tool's own provider-side scoping (a GitHub fine-grained token limited
//     to one repository, an IAM role with one action) bounds the damage if the
//     gateway's policy is itself wrong. It is not sufficient alone: it gives no
//     central audit trail and delegates correctness to N third parties.
//
// Everything here fails closed. An unknown tool, an unparsable policy, a
// missing definition and an expired token all produce Deny.
package authz

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/gaurasha/agent-orch/backend/internal/types"
)

type Effect string

const (
	Allow         Effect = "ALLOW"
	Deny          Effect = "DENY"
	NeedsApproval Effect = "NEEDS_APPROVAL"
)

// Decision is the result of evaluating one tool call.
type Decision struct {
	Effect Effect
	// Reason is shown to the operator in the audit log AND returned to the
	// agent. Telling the model precisely why it was denied is deliberate: it
	// stops the loop where an agent retries the same forbidden call twenty
	// times, which is a real cost and availability problem.
	Reason string
	// Rule names the check that fired, so an operator can find it in code.
	Rule string
}

func deny(rule, format string, args ...any) Decision {
	return Decision{Effect: Deny, Rule: rule, Reason: fmt.Sprintf(format, args...)}
}

// Request is everything the policy engine needs. It deliberately does not
// contain credentials: authorization is decided before any secret is fetched,
// so a denied call never causes a credential to be minted at all.
type Request struct {
	Tool     string
	Args     map[string]any
	Run      types.Run
	Spec     types.Spec
	Approved bool
}

// Engine evaluates policy.
type Engine struct {
	// Tools is the registry of known tools, used to reject unknown names and
	// to apply per-tool intrinsic rules.
	Tools ToolInfoSource
}

// ToolInfoSource lets authz ask about a tool without importing the tools
// package (which imports the sandbox, which would be a dependency cycle).
type ToolInfoSource interface {
	Lookup(name string) (ToolInfo, bool)
}

// ToolInfo is the policy-relevant subset of a tool definition.
type ToolInfo struct {
	Name string
	// Dangerous tools always require an explicit grant AND, unless the tenant
	// opts out, human approval.
	Dangerous bool
	// RequiredArgs must be present and non-empty.
	RequiredArgs []string
}

func New(tools ToolInfoSource) *Engine { return &Engine{Tools: tools} }

// Evaluate runs the checks in order of cheapness and of how badly a failure
// would hurt. Budget and lifecycle come first so a runaway agent is stopped
// before we do any parsing work on its behalf.
func (e *Engine) Evaluate(req Request) Decision {
	// 1. Lifecycle. A cancelled or finished run must not be able to keep
	//    reaching the outside world through a straggler worker.
	if req.Run.State.Terminal() {
		return deny("run.terminal", "run is %s and can no longer call tools", req.Run.State)
	}

	// 2. Budget. Enforced here, not merely recorded, because this is the only
	//    choke point a poison agent cannot route around.
	if reason := req.Run.Budget.ExceedsReason(req.Run.Usage, req.Run.Elapsed()); reason != "" {
		return deny("budget.exhausted", "%s; this run will now be stopped", reason)
	}

	// 3. The tool must exist. An unknown name is usually a hallucination, and
	//    occasionally an attempt to find an undocumented internal tool.
	info, known := e.Tools.Lookup(req.Tool)
	if !known {
		return deny("tool.unknown", "no tool named %q exists", req.Tool)
	}

	// 4. The grant. This is the core capability check: the agent definition
	//    pinned by this run must list the tool. Note we consult req.Spec, which
	//    the caller loaded by the run's pinned definition DIGEST - so editing
	//    the agent definition cannot retroactively widen a running agent's
	//    permissions.
	if !req.Spec.Grants(req.Tool) {
		return deny("tool.not_granted",
			"tool %q is not in this agent's granted tool set (granted: %s)",
			req.Tool, strings.Join(req.Spec.Tools, ", "))
	}

	// 5. Required arguments.
	for _, a := range info.RequiredArgs {
		v, ok := req.Args[a]
		if !ok || v == nil || v == "" {
			return deny("args.missing", "argument %q is required for %s", a, req.Tool)
		}
	}

	// 6. Parameter-level policy. "May call http.get" must not mean "may GET
	//    anything": the first prompt injection would exfiltrate the workspace
	//    to an attacker's domain. This is where tool-level grants stop being
	//    enough.
	pp := req.Spec.ToolParams[req.Tool]
	if d := checkParams(req, pp); d.Effect != Allow {
		return d
	}

	// 7. Human approval for high-blast-radius calls.
	if (pp.RequiresApproval || info.Dangerous) && !req.Approved {
		return Decision{
			Effect: NeedsApproval, Rule: "approval.required",
			Reason: fmt.Sprintf("%q requires human approval before it runs", req.Tool),
		}
	}

	return Decision{Effect: Allow, Rule: "default.granted"}
}

func checkParams(req Request, pp types.ParamPolicy) Decision {
	// Host allowlist for anything that names a URL.
	if len(pp.AllowedHosts) > 0 {
		raw, _ := req.Args["url"].(string)
		if raw != "" {
			host, d := parseHost(raw)
			if d.Effect != Allow {
				return d
			}
			if !hostAllowed(host, pp.AllowedHosts) {
				return deny("params.host_not_allowed",
					"host %q is not in this agent's allowed hosts (%s)",
					host, strings.Join(pp.AllowedHosts, ", "))
			}
		}
	}

	// Command allowlist for exec/CLI tools.
	if len(pp.AllowedCommands) > 0 {
		argv := stringSlice(req.Args["argv"])
		if len(argv) > 0 {
			if !contains(pp.AllowedCommands, argv[0]) {
				return deny("params.command_not_allowed",
					"command %q is not in this agent's allowed commands (%s)",
					argv[0], strings.Join(pp.AllowedCommands, ", "))
			}
		}
	}

	// Denied argument patterns. This is how a CLI is allowed for its intended
	// purpose while its credential-disclosing subcommands stay blocked -
	// `gh pr create` yes, `gh auth token` no.
	if len(pp.DeniedArgPatterns) > 0 {
		hay := flattenArgs(req.Args)
		for _, pat := range pp.DeniedArgPatterns {
			if pat != "" && strings.Contains(hay, strings.ToLower(pat)) {
				return deny("params.denied_pattern",
					"the argument pattern %q is forbidden for %s", pat, req.Tool)
			}
		}
	}
	return Decision{Effect: Allow}
}

// parseHost extracts and sanity-checks the host of a URL.
func parseHost(raw string) (string, Decision) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", deny("params.bad_url", "url is not parsable: %v", err)
	}
	// Only http(s). Without this, file:// and gopher:// become read primitives.
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", deny("params.bad_scheme", "scheme %q is not permitted; use http or https", u.Scheme)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", deny("params.bad_url", "url has no host")
	}
	// Block obvious SSRF targets even when an allowlist would already do it.
	// Defence in depth: a misconfigured allowlist entry like "*" should not
	// immediately expose the node's metadata service or the cluster's internal
	// services.
	if ip := net.ParseIP(host); ip != nil {
		if isBlockedIP(ip) {
			return "", deny("params.ssrf",
				"%s is a link-local, loopback or private address and cannot be reached from a tool call", host)
		}
	}
	if host == "metadata.google.internal" || strings.HasSuffix(host, ".internal") {
		return "", deny("params.ssrf", "%s is an instance metadata hostname", host)
	}
	return host, Decision{Effect: Allow}
}

// isBlockedIP reports addresses that a tenant tool call must never reach.
func isBlockedIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() {
		return true
	}
	// RFC1918 and CGNAT: the cluster's own network.
	if ip4 := ip.To4(); ip4 != nil {
		switch {
		case ip4[0] == 10:
			return true
		case ip4[0] == 172 && ip4[1]&0xf0 == 16:
			return true
		case ip4[0] == 192 && ip4[1] == 168:
			return true
		case ip4[0] == 100 && ip4[1]&0xc0 == 64:
			return true
		}
	}
	// IPv6 unique-local.
	if len(ip) == net.IPv6len && ip[0]&0xfe == 0xfc {
		return true
	}
	return false
}

// hostAllowed matches exactly, or as a suffix when the pattern starts with a
// dot (".example.com" matches "api.example.com" but not "notexample.com").
func hostAllowed(host string, allowed []string) bool {
	for _, a := range allowed {
		a = strings.ToLower(strings.TrimSpace(a))
		switch {
		case a == "":
			continue
		case strings.HasPrefix(a, "."):
			if strings.HasSuffix(host, a) {
				return true
			}
		case host == a:
			return true
		}
	}
	return false
}

// flattenArgs renders arguments to a single lowercase string for substring
// matching. Nested values are included so a denied pattern cannot be hidden
// one level down in an object or array.
func flattenArgs(args map[string]any) string {
	var b strings.Builder
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case string:
			b.WriteString(strings.ToLower(t))
			b.WriteByte(' ')
		case []any:
			for _, e := range t {
				walk(e)
			}
		case map[string]any:
			for k, e := range t {
				b.WriteString(strings.ToLower(k))
				b.WriteByte(' ')
				walk(e)
			}
		default:
			fmt.Fprintf(&b, "%v ", t)
		}
	}
	walk(map[string]any(args))
	return b.String()
}

func stringSlice(v any) []string {
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

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
