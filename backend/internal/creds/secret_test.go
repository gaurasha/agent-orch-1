package creds_test

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/gaurasha/agent-orch/backend/internal/creds"
)

// The whole point of the Secret type is that an ordinary mistake cannot leak
// it. These tests enumerate the mistakes.
func TestSecret_NeverRendersItsValue(t *testing.T) {
	const plaintext = "ghp_SUPERSECRETVALUE1234567890"
	s := creds.Secret(plaintext)

	type wrapper struct {
		Name  string       `json:"name"`
		Token creds.Secret `json:"token"`
	}

	var buf strings.Builder
	logger := log.New(&buf, "", 0)
	logger.Printf("credential in a log line: %v / %s / %q / %#v", s, s, s, s)

	jsonBytes, err := json.Marshal(wrapper{Name: "gh", Token: s})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	wrapped := fmt.Errorf("upstream call failed with %v", s)

	for name, rendered := range map[string]string{
		"fmt.Sprint":   fmt.Sprint(s),
		"fmt.Sprintf":  fmt.Sprintf("%v|%s|%q|%#v|%x", s, s, s, s, s),
		"log output":   buf.String(),
		"json.Marshal": string(jsonBytes),
		"error wrap":   wrapped.Error(),
		"String()":     s.String(),
	} {
		if strings.Contains(rendered, plaintext) {
			t.Errorf("%s leaked the secret: %s", name, rendered)
		}
		if !strings.Contains(rendered, creds.Redacted) {
			t.Errorf("%s did not show the redaction marker: %s", name, rendered)
		}
	}

	// Reveal is the one deliberate escape hatch.
	if s.Reveal() != plaintext {
		t.Fatal("Reveal must return the real value")
	}
}

func TestSecret_SurvivesStructPrintingInsideAContainer(t *testing.T) {
	// The realistic accident: someone logs a whole request/response struct.
	const plaintext = "aot_secret_value"
	c := creds.Credential{ID: "cred_1", Ref: "github/bot", Kind: "bearer", Value: creds.Secret(plaintext)}
	out := fmt.Sprintf("%+v", c)
	if strings.Contains(out, plaintext) {
		t.Fatalf("printing the containing struct leaked the secret: %s", out)
	}
	b, _ := json.Marshal(map[string]any{"cred": c, "list": []creds.Credential{c}})
	if strings.Contains(string(b), plaintext) {
		t.Fatalf("marshalling a nested struct leaked the secret: %s", b)
	}
}

// A guard against the escape hatch spreading.
//
// Every .Reveal() is a place plaintext credential material is handled, so the
// set should stay small and each entry should be individually justifiable. If
// this count changes, that is a review event, not a number to bump.
//
// The expected sites, and why each is legitimate:
//
//	creds.Scrub              - compares against output to redact it; discloses nothing
//	tools.invokeHTTP         - injects the Authorization header on a call the
//	                           GATEWAY makes; the agent never sees the request
//	tools.invokeCLI          - places GH_TOKEN in the BROKER sandbox's environment,
//	                           a process in a different pid/user namespace from
//	                           the agent
func TestSecret_RevealCallSitesAreFewAndIntentional(t *testing.T) {
	const expected = 3
	found := map[string]int{}
	total := 0
	err := walkGoFiles("..", func(path, content string) {
		if strings.HasSuffix(path, "_test.go") {
			return
		}
		n := 0
		for _, line := range strings.Split(content, "\n") {
			trimmed := strings.TrimSpace(line)
			// Skip comments: an earlier version of this test counted the
			// doc comment that *describes* the grep, which made the number
			// meaningless as a guard.
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			n += strings.Count(line, ".Reveal()")
		}
		if n > 0 {
			found[path] = n
			total += n
		}
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	t.Logf("plaintext credential use sites: %d across %d files: %v", total, len(found), found)
	if total != expected {
		t.Fatalf("there are now %d .Reveal() call sites, expected %d: %v\n"+
			"Each one handles a plaintext credential. If this change is intentional, "+
			"justify the new site in this test's doc comment and in "+
			"docs/01-concepts/05-secrets.md, then update the expected count.",
			total, expected, found)
	}
}

func walkGoFiles(root string, fn func(path, content string)) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		p := root + "/" + e.Name()
		if e.IsDir() {
			if err := walkGoFiles(p, fn); err != nil {
				return err
			}
			continue
		}
		if !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		fn(p, string(b))
	}
	return nil
}
