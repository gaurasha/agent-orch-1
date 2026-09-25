package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// WorkspaceManager owns the per-run scratch directories.
//
// Tenancy is expressed in the path layout: <root>/<tenant>/<run>. A run can
// only ever be handed its own directory, and every path the agent supplies is
// resolved and then checked to still be inside it. Two independent checks
// (lexical, then symlink-resolved) because the lexical one alone is defeated by
// a symlink the agent itself planted on a previous tool call.
type WorkspaceManager struct {
	root       string
	maxFileLen int
}

func NewWorkspaceManager(root string) (*WorkspaceManager, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("workspace: create root: %w", err)
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("workspace: resolve root: %w", err)
	}
	return &WorkspaceManager{root: abs, maxFileLen: 8 << 20}, nil
}

// Dir returns (creating if needed) the workspace for a run.
func (w *WorkspaceManager) Dir(tenantID, runID string) (string, error) {
	if tenantID == "" || runID == "" {
		return "", fmt.Errorf("workspace: tenant and run are both required")
	}
	dir := filepath.Join(w.root, safeSegment(tenantID), safeSegment(runID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("workspace: create %s: %w", dir, err)
	}
	return dir, nil
}

// Remove deletes a run's workspace. Called when a run reaches a terminal state.
func (w *WorkspaceManager) Remove(tenantID, runID string) error {
	dir := filepath.Join(w.root, safeSegment(tenantID), safeSegment(runID))
	if !strings.HasPrefix(dir, w.root+string(os.PathSeparator)) {
		return fmt.Errorf("workspace: refusing to remove %s: outside the workspace root", dir)
	}
	return os.RemoveAll(dir)
}

// resolve turns an agent-supplied relative path into an absolute one, or
// refuses.
func (w *WorkspaceManager) resolve(wsDir, rel string) (string, error) {
	if err := validateRelPath(rel); err != nil {
		return "", err
	}
	base, err := filepath.Abs(wsDir)
	if err != nil {
		return "", fmt.Errorf("workspace: resolve base: %w", err)
	}
	full := filepath.Join(base, rel)
	// Check 1 (lexical): after cleaning, are we still under the base?
	if full != base && !strings.HasPrefix(full, base+string(os.PathSeparator)) {
		return "", fmt.Errorf("path %q escapes the workspace", rel)
	}
	// Check 2 (symlinks): the agent may have created a symlink in a previous
	// tool call pointing at /etc or another tenant's directory. EvalSymlinks
	// on the parent catches that; we check the parent because the leaf may not
	// exist yet on a write.
	parent := filepath.Dir(full)
	if realParent, err := filepath.EvalSymlinks(parent); err == nil {
		realBase, err := filepath.EvalSymlinks(base)
		if err != nil {
			return "", fmt.Errorf("workspace: resolve base symlinks: %w", err)
		}
		if realParent != realBase && !strings.HasPrefix(realParent, realBase+string(os.PathSeparator)) {
			return "", fmt.Errorf("path %q resolves outside the workspace through a symlink", rel)
		}
	}
	return full, nil
}

func (w *WorkspaceManager) Write(wsDir, rel, content string) error {
	if len(content) > w.maxFileLen {
		return fmt.Errorf("content is %d bytes; the per-file limit is %d", len(content), w.maxFileLen)
	}
	full, err := w.resolve(wsDir, rel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return fmt.Errorf("creating directory for %q: %w", rel, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		return fmt.Errorf("writing %q: %w", rel, err)
	}
	return nil
}

func (w *WorkspaceManager) Read(wsDir, rel string) (string, error) {
	full, err := w.resolve(wsDir, rel)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("no such file: %s", rel)
		}
		return "", fmt.Errorf("reading %q: %w", rel, err)
	}
	if fi.IsDir() {
		return "", fmt.Errorf("%s is a directory; use fs.list", rel)
	}
	if fi.Size() > int64(w.maxFileLen) {
		return "", fmt.Errorf("%s is %d bytes; the read limit is %d", rel, fi.Size(), w.maxFileLen)
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return "", fmt.Errorf("reading %q: %w", rel, err)
	}
	return string(b), nil
}

func (w *WorkspaceManager) List(wsDir, rel string) (string, error) {
	if rel == "" {
		rel = "."
	}
	full, err := w.resolve(wsDir, rel)
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("no such directory: %s", rel)
		}
		return "", fmt.Errorf("listing %q: %w", rel, err)
	}
	lines := make([]string, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		if e.IsDir() {
			lines = append(lines, fmt.Sprintf("%-40s  <dir>", e.Name()+"/"))
			continue
		}
		lines = append(lines, fmt.Sprintf("%-40s  %d bytes", e.Name(), info.Size()))
	}
	sort.Strings(lines)
	if len(lines) == 0 {
		return "(empty)", nil
	}
	return strings.Join(lines, "\n"), nil
}

// safeSegment makes an identifier safe to use as one path component. It cannot
// produce "..", an absolute path, or anything containing a separator.
func safeSegment(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "" || out == "." || out == ".." {
		return "_"
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}
