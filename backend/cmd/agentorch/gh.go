package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// runGH is the `gh` personality.
//
// This binary is bind-mounted into the CREDENTIAL BROKER sandbox as
// /usr/local/bin/gh. It is not reachable from the agent's own sandbox, and the
// agent never sees the environment it runs in.
//
// It stands in for the real GitHub CLI. The property being demonstrated is not
// that we reimplemented gh, it is the plumbing around it: a trusted binary
// receives a freshly minted, short-lived, tenant-scoped token through its
// environment, in a process the agent cannot observe, and the agent gets only
// stdout back. Swapping this for the real gh binary in the sandbox image
// changes nothing about that plumbing.
func runGH(args []string) int {
	token := os.Getenv("GH_TOKEN")
	apiBase := os.Getenv("GITHUB_API_BASE")
	if apiBase == "" {
		apiBase = "https://api.github.com"
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: gh <command> [flags]")
		return 2
	}

	switch {
	case args[0] == "pr" && len(args) > 1 && args[1] == "create":
		return ghPRCreate(apiBase, token, args[2:])
	case args[0] == "pr" && len(args) > 1 && args[1] == "list":
		return ghGet(apiBase, token, "/repos/acme/platform/pulls")
	case args[0] == "auth" && len(args) > 1 && args[1] == "token":
		// Reached only if policy let it through. Defence in depth: the CLI
		// itself refuses to print the credential it was given. A tool that can
		// be asked to echo its own secret is a tool that will eventually be
		// asked to.
		fmt.Fprintln(os.Stderr,
			"gh: refusing to print the access token. This credential is injected by the "+
				"platform at the point of use and is not available to the caller.")
		return 1
	case args[0] == "api":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: gh api <path>")
			return 2
		}
		return ghGet(apiBase, token, "/"+strings.TrimPrefix(args[1], "/"))
	default:
		fmt.Fprintf(os.Stderr, "gh: unsupported command %q in this demo build "+
			"(supported: pr create, pr list, api, auth token)\n", strings.Join(args, " "))
		return 2
	}
}

func ghPRCreate(apiBase, token string, args []string) int {
	var title, body string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--title", "-t":
			if i+1 < len(args) {
				title = args[i+1]
				i++
			}
		case "--body", "-b":
			if i+1 < len(args) {
				body = args[i+1]
				i++
			}
		case "--body-file", "-F":
			if i+1 < len(args) {
				// Read from the SHARED workspace: this is why the broker
				// sandbox mounts the same /work the agent wrote to.
				p := args[i+1]
				if !filepath.IsAbs(p) {
					p = filepath.Join("/work", p)
				}
				b, err := os.ReadFile(p)
				if err != nil {
					fmt.Fprintf(os.Stderr, "gh: cannot read body file %s: %v\n", args[i+1], err)
					return 1
				}
				body = string(b)
				i++
			}
		}
	}
	if title == "" {
		fmt.Fprintln(os.Stderr, "gh: --title is required")
		return 2
	}
	payload, _ := json.Marshal(map[string]string{"title": title, "body": body})
	req, err := http.NewRequest(http.MethodPost, apiBase+"/repos/acme/platform/pulls", bytes.NewReader(payload))
	if err != nil {
		fmt.Fprintln(os.Stderr, "gh:", err)
		return 1
	}
	req.Header.Set("Content-Type", "application/json")
	// The one place the token is used. It came from the environment of THIS
	// process, which lives in a different pid and user namespace from the
	// agent's sandbox.
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gh: request failed: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		fmt.Fprintf(os.Stderr, "gh: API returned %s: %s\n", resp.Status, strings.TrimSpace(string(raw)))
		return 1
	}
	var pr struct {
		Number int    `json:"number"`
		URL    string `json:"html_url"`
		Title  string `json:"title"`
	}
	_ = json.Unmarshal(raw, &pr)
	fmt.Printf("Created pull request #%d: %s\n%s\n", pr.Number, pr.Title, pr.URL)
	return 0
}

func ghGet(apiBase, token, path string) int {
	req, err := http.NewRequest(http.MethodGet, apiBase+path, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gh:", err)
		return 1
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gh: request failed: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		fmt.Fprintf(os.Stderr, "gh: API returned %s: %s\n", resp.Status, strings.TrimSpace(string(raw)))
		return 1
	}
	fmt.Println(strings.TrimSpace(string(raw)))
	return 0
}
