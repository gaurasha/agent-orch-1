#!/usr/bin/env python3
"""Fail if the code contains an identifier the documentation never mentions.

Scans the Go sources for: environment variables (AGENTORCH_*), authorization
rule names (deny("rule", …)), event types and run/tool-call states, tool names,
metric series, Makefile targets, test functions, exported error sentinels and
the fields of the agent Spec / ParamPolicy / Budget; then checks that each
string occurs somewhere under docs/ or in the top-level markdown files.

  python3 docs/tools/check-coverage.py
"""
import glob, os, re, sys

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
GO = [f for f in glob.glob(ROOT + "/backend/**/*.go", recursive=True) if not f.endswith("_test.go")]
TESTS = [f for f in glob.glob(ROOT + "/backend/**/*_test.go", recursive=True)]
DOCS = " ".join(open(f, encoding="utf-8").read() for f in
                glob.glob(ROOT + "/docs/**/*.md", recursive=True) +
                [ROOT + "/README.md", ROOT + "/DESIGN.md", ROOT + "/DEEP_DIVE.md", ROOT + "/TUTORIAL.md", ROOT + "/AI_LOG.md"])

def src(files):
    return "\n".join(open(f, encoding="utf-8").read() for f in files)

code = src(GO)
groups = {
    "env var":        set(re.findall(r'"(AGENTORCH_[A-Z_]+)"', code)),
    "authz rule":     set(re.findall(r'deny\("([a-z_.]+)"', code)) | set(re.findall(r'Rule:\s*"([a-z_.]+)"', code)),
    "event type":     set(re.findall(r'EventType\s*=\s*"([A-Z_]+)"', code)),
    "run state":      set(re.findall(r'RunState\s*=\s*"([A-Z_]+)"', code)),
    "tool-call state": set(re.findall(r'ToolCallState\s*=\s*"([A-Z_]+)"', code)),
    "tool":           set(re.findall(r'Name:\s*"([a-z]+\.[a-z]+)"', code)),
    "metric":         set(re.findall(r'"(agentorch_[a-z_]+)"', code)),
    "error sentinel": set(re.findall(r'\b(Err[A-Z][A-Za-z]+)\s*=\s*errors\.New', code)),
    "spec field":     set(re.findall(r'json:"([a-z_]+)(?:,omitempty)?"', src([ROOT + "/backend/internal/types/types.go"]))),
    "make target":    set(re.findall(r'^([a-z][a-z-]*):', open(ROOT + "/Makefile").read(), re.M)) - {"help", "clean", "fmt"},
    "test":           set(re.findall(r'^func (Test[A-Za-z_0-9]+)', src(TESTS), re.M)) - {"TestMain"},
}
# JSON fields that are pure plumbing, not concepts a reader needs by name
groups["spec field"] -= {"id", "name", "seq", "type", "payload", "created_at", "updated_at", "run_id", "tenant_id", "digest", "spec", "text", "worker", "step", "state", "usage", "budget", "priority", "model", "args", "tool", "result", "reason", "decision", "is_error", "idem_key", "tool_call_id", "input_tokens", "output_tokens", "cost_usd", "stop_reason", "duration_ms", "status_reason", "next_seq", "lease_owner", "lease_expires_at", "wake_at", "agent_name", "triggering_user", "def_digest", "weight", "tokens_per_minute", "max_concurrent_runs", "steps", "tool_calls", "sandbox_ms", "tools"}

missing = []
total = 0
for kind, ids in groups.items():
    for ident in sorted(ids):
        total += 1
        if ident not in DOCS:
            missing.append((kind, ident))
print(f"{total} identifiers checked across {len(groups)} groups; {len(missing)} undocumented")
for kind, ident in missing:
    print(f"  MISSING {kind}: {ident}")
sys.exit(1 if missing else 0)
