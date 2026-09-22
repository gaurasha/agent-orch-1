// Package k8s is a small Kubernetes REST client.
//
// Why not client-go: client-go and its transitive dependencies are roughly
// 200 modules and tens of megabytes. This system needs six verbs against three
// resource kinds. Pulling in the full client would make the dependency tree -
// and therefore the supply-chain attack surface of a component that executes
// untrusted code - dramatically larger for no benefit. If we later need
// informers, leader election or CRD codegen, that trade flips and client-go is
// the right answer; see DEEP_DIVE.md "D9".
package k8s

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	saDir       = "/var/run/secrets/kubernetes.io/serviceaccount"
	tokenPath   = saDir + "/token"
	caPath      = saDir + "/ca.crt"
	nsPath      = saDir + "/namespace"
	defaultHost = "https://kubernetes.default.svc"
)

// Client talks to the API server using the pod's ServiceAccount.
type Client struct {
	base      string
	http      *http.Client
	tokenFile string
	namespace string
}

// InCluster builds a client from the mounted ServiceAccount. It returns a
// descriptive error (not a panic, not a silent fallback) when run outside a
// cluster, because "sandbox driver silently degraded" is exactly the class of
// failure this system must not have.
func InCluster() (*Client, error) {
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("k8s: not running in a cluster (%s unreadable): %w", caPath, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("k8s: service account CA bundle is not valid PEM")
	}
	nsBytes, err := os.ReadFile(nsPath)
	if err != nil {
		return nil, fmt.Errorf("k8s: read namespace: %w", err)
	}
	host := defaultHost
	if h, p := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT"); h != "" && p != "" {
		host = "https://" + h + ":" + p
	}
	return &Client{
		base:      host,
		tokenFile: tokenPath,
		namespace: strings.TrimSpace(string(nsBytes)),
		http: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
			},
		},
	}, nil
}

func (c *Client) Namespace() string { return c.namespace }

// token is read per request. Projected ServiceAccount tokens are short-lived
// and rotated by the kubelet, so caching one at startup means the driver stops
// working an hour later - a genuinely common bug.
func (c *Client) token() (string, error) {
	b, err := os.ReadFile(c.tokenFile)
	if err != nil {
		return "", fmt.Errorf("k8s: read service account token: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("k8s: marshal request: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return fmt.Errorf("k8s: build request: %w", err)
	}
	tok, err := c.token()
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("k8s: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("k8s: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Surface the API server's own message; it is almost always the exact
		// reason (admission webhook, quota, RBAC) and guessing wastes time.
		return fmt.Errorf("k8s: %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(raw)))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("k8s: decode response: %w", err)
		}
	}
	return nil
}

func (c *Client) Post(ctx context.Context, path string, body, out any) error {
	return c.do(ctx, http.MethodPost, path, body, out)
}
func (c *Client) Get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, out)
}
func (c *Client) Delete(ctx context.Context, path string) error {
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

// GetRaw fetches a non-JSON endpoint such as pod logs.
func (c *Client) GetRaw(ctx context.Context, path string, maxBytes int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	tok, err := c.token()
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("k8s: GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("k8s: GET %s: %s: %s", path, resp.Status, strings.TrimSpace(string(b)))
	}
	return b, nil
}
