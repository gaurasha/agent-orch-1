// Package tools defines what an agent can ask the platform to do.
//
// The central split in this file is between the two halves of a tool
// definition:
//
//	Schema     - what the MODEL sees. Name, description, parameter shapes.
//	             Goes into the LLM request. Must contain nothing sensitive.
//	Backend    - what the GATEWAY sees. Which credential to mint, which URL
//	             template to call, which binary to execute, which sandbox
//	             profile to use. Never serialised towards the model.
//
// Keeping these in one struct but two fields, with a test that asserts the
// model-facing projection never contains backend fields, is how "credentials
// never enter the model context" stops being a promise and starts being a
// property. An agent cannot leak a URL template or a credential reference it
// was never shown.
package tools

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/gaurasha/agent-orch/backend/internal/authz"
)

type Kind string

const (
	KindExec Kind = "exec" // run code in a sandbox
	KindFS   Kind = "fs"   // manipulate the run workspace
	KindHTTP Kind = "http" // call an HTTP API with an injected credential
	KindCLI  Kind = "cli"  // run a credentialed CLI outside the agent sandbox
)

// Param is one model-visible parameter.
type Param struct {
	Type        string   `json:"type"`
	Description string   `json:"description"`
	Enum        []string `json:"enum,omitempty"`
	Items       *Param   `json:"items,omitempty"`
}

// Schema is the model-facing description of a tool. This is exactly what gets
// serialised into the LLM request, and nothing else.
type Schema struct {
	Name        string           `json:"name"`
	Description string           `json:"description"`
	Parameters  map[string]Param `json:"parameters"`
	Required    []string         `json:"required,omitempty"`
}

// Backend is the server-side half. It has no JSON tags that would make it
// travel by accident, and Tool.MarshalJSON drops it outright.
type Backend struct {
	Kind Kind
	// CredRef names the credential to mint for this call. The agent never sees
	// this string, let alone the credential.
	CredRef string
	// URLTemplate is the HTTP target, with {arg} placeholders filled from
	// validated arguments. Templating server side means the agent supplies
	// values, never the endpoint.
	URLTemplate string
	Method      string
	// Binary is the executable for exec/cli tools.
	Binary []string
	// Timeout bounds one invocation.
	Timeout time.Duration
	// Dangerous marks tools whose blast radius warrants human approval.
	Dangerous bool
	// UnsafeRetry marks tools that cannot be safely retried because the
	// provider offers no idempotency key. On an ambiguous failure the agent is
	// told the outcome is unknown rather than being allowed to retry blindly.
	UnsafeRetry bool
	// NeedsNetwork selects a sandbox profile with egress. Most exec tools do
	// not need it and must not get it.
	NeedsNetwork bool
}

// Tool is a registry entry.
type Tool struct {
	Schema  Schema
	Backend Backend
}

// MarshalJSON emits only the model-facing schema.
//
// This is the load-bearing line of the package: any code path that serialises
// a Tool - building an LLM request, rendering the UI, writing an event - gets
// the schema and cannot get the backend, even by mistake.
func (t Tool) MarshalJSON() ([]byte, error) { return json.Marshal(t.Schema) }

// Registry holds the tool catalogue.
type Registry struct{ byName map[string]Tool }

func NewRegistry(ts ...Tool) *Registry {
	r := &Registry{byName: make(map[string]Tool, len(ts))}
	for _, t := range ts {
		r.byName[t.Schema.Name] = t
	}
	return r
}

func (r *Registry) Get(name string) (Tool, bool) { t, ok := r.byName[name]; return t, ok }

func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.byName))
	for n := range r.byName {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// SchemasFor returns the model-facing schemas for the granted tool names only.
//
// Sending the model only the tools it may actually call is a usability and cost
// choice, not a security control - the gateway re-checks every call. But it
// matters: an agent that is never shown a tool rarely tries to call it, which
// keeps audit logs free of noise and stops wasted round trips.
func (r *Registry) SchemasFor(granted []string) []Schema {
	out := make([]Schema, 0, len(granted))
	for _, n := range granted {
		if t, ok := r.byName[n]; ok {
			out = append(out, t.Schema)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Lookup implements authz.ToolInfoSource.
func (r *Registry) Lookup(name string) (authz.ToolInfo, bool) {
	t, ok := r.byName[name]
	if !ok {
		return authz.ToolInfo{}, false
	}
	return authz.ToolInfo{
		Name:         t.Schema.Name,
		Dangerous:    t.Backend.Dangerous,
		RequiredArgs: t.Schema.Required,
	}, true
}

var _ authz.ToolInfoSource = (*Registry)(nil)

// ---------------------------------------------------------------------------
// The standard catalogue
// ---------------------------------------------------------------------------

// DefaultRegistry is the tool catalogue the demo ships with. Each entry is a
// small, specific capability rather than a general one: "read a file in your
// workspace" instead of "run any filesystem operation". Narrow tools make
// parameter policy meaningful and make the audit log readable.
func DefaultRegistry(githubAPIBase string) *Registry {
	return NewRegistry(
		Tool{
			Schema: Schema{
				Name:        "exec.bash",
				Description: "Run a bash script in an isolated sandbox with no network access. The working directory is /work and is the only writable location that persists.",
				Parameters: map[string]Param{
					"script": {Type: "string", Description: "The bash script to execute."},
				},
				Required: []string{"script"},
			},
			Backend: Backend{Kind: KindExec, Binary: []string{"/bin/bash", "-c"}, Timeout: 30 * time.Second},
		},
		Tool{
			Schema: Schema{
				Name:        "exec.python",
				Description: "Run a Python 3 script in an isolated sandbox with no network access. The working directory is /work.",
				Parameters: map[string]Param{
					"script": {Type: "string", Description: "The Python source to execute."},
				},
				Required: []string{"script"},
			},
			Backend: Backend{Kind: KindExec, Binary: []string{"python3", "-c"}, Timeout: 30 * time.Second},
		},
		Tool{
			Schema: Schema{
				Name:        "fs.write",
				Description: "Write a UTF-8 text file into the run workspace.",
				Parameters: map[string]Param{
					"path":    {Type: "string", Description: "Path relative to the workspace root."},
					"content": {Type: "string", Description: "File contents."},
				},
				Required: []string{"path", "content"},
			},
			Backend: Backend{Kind: KindFS, Timeout: 5 * time.Second},
		},
		Tool{
			Schema: Schema{
				Name:        "fs.read",
				Description: "Read a UTF-8 text file from the run workspace.",
				Parameters: map[string]Param{
					"path": {Type: "string", Description: "Path relative to the workspace root."},
				},
				Required: []string{"path"},
			},
			Backend: Backend{Kind: KindFS, Timeout: 5 * time.Second},
		},
		Tool{
			Schema: Schema{
				Name:        "fs.list",
				Description: "List files in the run workspace.",
				Parameters: map[string]Param{
					"path": {Type: "string", Description: "Directory relative to the workspace root. Defaults to the root."},
				},
			},
			Backend: Backend{Kind: KindFS, Timeout: 5 * time.Second},
		},
		Tool{
			Schema: Schema{
				Name:        "doc.convert",
				Description: "Convert a document in the workspace between formats (for example Markdown to HTML). Runs the converter inside the sandbox.",
				Parameters: map[string]Param{
					"input":  {Type: "string", Description: "Input file, relative to the workspace root."},
					"output": {Type: "string", Description: "Output file, relative to the workspace root."},
					"from":   {Type: "string", Description: "Source format.", Enum: []string{"markdown", "html"}},
					"to":     {Type: "string", Description: "Target format.", Enum: []string{"html", "plain"}},
				},
				Required: []string{"input", "output"},
			},
			Backend: Backend{Kind: KindExec, Timeout: 30 * time.Second},
		},
		Tool{
			Schema: Schema{
				Name:        "http.get",
				Description: "Perform an HTTP GET against an allowed host. Authentication is handled by the platform; do not include credentials.",
				Parameters: map[string]Param{
					"url": {Type: "string", Description: "Absolute https URL."},
				},
				Required: []string{"url"},
			},
			Backend: Backend{Kind: KindHTTP, Method: "GET", CredRef: "http/default", Timeout: 20 * time.Second},
		},
		Tool{
			Schema: Schema{
				Name:        "http.post",
				Description: "Perform an HTTP POST against an allowed host. Authentication is handled by the platform; do not include credentials.",
				Parameters: map[string]Param{
					"url":  {Type: "string", Description: "Absolute https URL."},
					"body": {Type: "string", Description: "Request body."},
				},
				Required: []string{"url"},
			},
			// POST changes state somewhere we do not control, and few APIs
			// accept an idempotency key, so a replay after an ambiguous failure
			// could duplicate the effect. Mark it and tell the agent the truth.
			Backend: Backend{Kind: KindHTTP, Method: "POST", CredRef: "http/default",
				Timeout: 20 * time.Second, Dangerous: true, UnsafeRetry: true},
		},
		Tool{
			Schema: Schema{
				Name: "github.cli",
				Description: "Run a GitHub CLI command (gh). Authentication is injected by the platform - " +
					"you neither have nor need a token, and there is no way for you to read one.",
				Parameters: map[string]Param{
					"argv": {Type: "array", Description: "Arguments to gh, e.g. [\"pr\",\"create\",\"--title\",\"Fix\"].",
						Items: &Param{Type: "string"}},
				},
				Required: []string{"argv"},
			},
			// The star of the design: a real credentialed CLI whose token the
			// agent can never observe, because the CLI does not run in the
			// agent's sandbox at all. See invoke.go.
			Backend: Backend{
				Kind: KindCLI, CredRef: "github/token",
				URLTemplate: githubAPIBase, Binary: []string{"gh"},
				Timeout: 20 * time.Second, NeedsNetwork: true,
			},
		},
	)
}

// ValidateArgs checks argument presence and primitive types against the schema.
//
// This is not a full JSON Schema implementation on purpose: the parameter
// shapes we accept are deliberately simple, and a small explicit validator is
// easier to reason about at a security boundary than a large general one.
func (t Tool) ValidateArgs(args map[string]any) error {
	for _, r := range t.Schema.Required {
		v, ok := args[r]
		if !ok {
			return fmt.Errorf("missing required argument %q", r)
		}
		if s, isStr := v.(string); isStr && s == "" {
			return fmt.Errorf("argument %q must not be empty", r)
		}
	}
	for name, v := range args {
		p, known := t.Schema.Parameters[name]
		if !known {
			// Reject rather than ignore: an unexpected argument usually means
			// the model is improvising, and silently dropping it produces a
			// confusing result the agent will not understand.
			return fmt.Errorf("unknown argument %q for %s", name, t.Schema.Name)
		}
		if err := checkType(name, p, v); err != nil {
			return err
		}
	}
	return nil
}

func checkType(name string, p Param, v any) error {
	switch p.Type {
	case "string":
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("argument %q must be a string, got %T", name, v)
		}
		if len(p.Enum) > 0 {
			for _, e := range p.Enum {
				if s == e {
					return nil
				}
			}
			return fmt.Errorf("argument %q must be one of %v, got %q", name, p.Enum, s)
		}
	case "array":
		items, ok := v.([]any)
		if !ok {
			if _, isStrSlice := v.([]string); isStrSlice {
				return nil
			}
			return fmt.Errorf("argument %q must be an array, got %T", name, v)
		}
		if p.Items != nil {
			for i, e := range items {
				if err := checkType(fmt.Sprintf("%s[%d]", name, i), *p.Items, e); err != nil {
					return err
				}
			}
		}
	case "number", "integer":
		switch v.(type) {
		case float64, int, int64, json.Number:
		default:
			return fmt.Errorf("argument %q must be a number, got %T", name, v)
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("argument %q must be a boolean, got %T", name, v)
		}
	}
	return nil
}
