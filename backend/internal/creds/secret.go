// Package creds mints and holds tenant credentials.
//
// The requirement is "credentials never enter the model context". That is easy
// to state and easy to violate by accident: one %v in a log line, one
// json.Marshal of a struct that happens to embed a token, one error message
// that wraps a request object. Discipline does not scale across a codebase.
//
// So the secret value is a distinct type whose String, Format, GoString and
// MarshalJSON all redact. Printing it is safe by default; extracting it
// requires calling Reveal(), which is a single greppable token. `grep -rn
// '.Reveal()'` enumerates every place in the system where a plaintext secret
// is used - which is exactly the review question an auditor asks.
package creds

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gaurasha/agent-orch/backend/internal/id"
)

// Redacted is what a secret renders as anywhere it is printed or serialised.
const Redacted = "[REDACTED]"

// Secret is a string that refuses to print itself.
type Secret string

func (s Secret) String() string   { return Redacted }
func (s Secret) GoString() string { return Redacted }

// Format captures %v, %s, %q, %#v and friends, which String alone does not.
func (s Secret) Format(f fmt.State, verb rune) { _, _ = f.Write([]byte(Redacted)) }

// MarshalJSON means a Secret nested anywhere inside a logged or returned struct
// is still redacted.
func (s Secret) MarshalJSON() ([]byte, error) { return json.Marshal(Redacted) }

// Reveal returns the plaintext. Every call site is a deliberate decision.
func (s Secret) Reveal() string { return string(s) }

// Empty reports whether there is no secret material.
func (s Secret) Empty() bool { return len(s) == 0 }

// Credential is a short-lived, single-purpose credential.
type Credential struct {
	ID string `json:"id"`
	// Ref is the tenant-scoped name of the underlying credential, e.g.
	// "github/deploy-bot". Safe to log; it is a pointer, not a secret.
	Ref       string    `json:"ref"`
	Kind      string    `json:"kind"` // bearer | basic | github_token
	Value     Secret    `json:"value"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (c Credential) Expired() bool { return time.Now().After(c.ExpiresAt) }

// Broker mints credentials.
//
// Production note: the real implementation should call a secrets manager that
// issues genuinely dynamic credentials (Vault database/AWS/GitHub App secret
// engines, or a GitHub App installation token) so that "short-lived" is
// enforced by the issuer and not by us remembering to expire it. See
// DEEP_DIVE.md "D6 - credential boundary".
type Broker interface {
	// Mint issues a credential for (tenant, ref) valid for at most ttl.
	Mint(ctx context.Context, tenantID, ref string, ttl time.Duration) (Credential, error)
	// Revoke invalidates a minted credential immediately. Called when a run is
	// cancelled or a sandbox is suspected of being compromised.
	Revoke(ctx context.Context, credID string) error
	// Refs lists the credential names a tenant has, without values. Used to
	// validate agent definitions at registration time.
	Refs(ctx context.Context, tenantID string) []string
}

var (
	ErrNoSuchRef = errors.New("creds: no such credential reference for this tenant")
	ErrRevoked   = errors.New("creds: credential revoked")
)

// DerivedBroker mints short-lived, verifiable tokens derived from a per-tenant
// root secret.
//
// This is a stand-in for a real secrets manager, but it is not a toy: the
// minted token is an HMAC over (tenant, ref, credID, expiry), so the downstream
// service can verify it without a shared database, it is bound to one
// credential reference, and it genuinely expires. What it does NOT do, and what
// a real broker must, is give the downstream service a way to revoke early or
// scope the credential's permissions at the provider. See "known gaps".
type DerivedBroker struct {
	mu      sync.RWMutex
	roots   map[string]map[string]rootSecret // tenant -> ref -> root
	revoked map[string]time.Time
}

type rootSecret struct {
	kind string
	key  []byte
}

func NewDerivedBroker() *DerivedBroker {
	return &DerivedBroker{
		roots:   map[string]map[string]rootSecret{},
		revoked: map[string]time.Time{},
	}
}

// AddRoot registers a tenant's root secret for a credential reference. In
// production this is the point where a Vault path or cloud secret ARN is
// configured, not a literal value.
func (b *DerivedBroker) AddRoot(tenantID, ref, kind string, key []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.roots[tenantID] == nil {
		b.roots[tenantID] = map[string]rootSecret{}
	}
	b.roots[tenantID][ref] = rootSecret{kind: kind, key: key}
}

func (b *DerivedBroker) Mint(_ context.Context, tenantID, ref string, ttl time.Duration) (Credential, error) {
	b.mu.RLock()
	root, ok := b.roots[tenantID][ref]
	b.mu.RUnlock()
	if !ok {
		// Do not echo the ref back in a way that confirms which refs exist for
		// other tenants; the caller already knows what it asked for.
		return Credential{}, fmt.Errorf("%w: tenant=%s ref=%s", ErrNoSuchRef, tenantID, ref)
	}
	// Cap the TTL here rather than trusting the caller. A bug upstream that
	// requests a 30-day credential must not be able to get one.
	const maxTTL = 10 * time.Minute
	if ttl <= 0 || ttl > maxTTL {
		ttl = maxTTL
	}
	credID := id.New("cred")
	exp := time.Now().UTC().Add(ttl).Truncate(time.Second)

	mac := hmac.New(sha256.New, root.key)
	fmt.Fprintf(mac, "%s\n%s\n%s\n%d", tenantID, ref, credID, exp.Unix())
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	// The token carries its own scope so a downstream service can see exactly
	// what it is being asked to authorise.
	token := fmt.Sprintf("aot_%s.%s.%d.%s", tenantID, credID, exp.Unix(), sig)

	return Credential{
		ID: credID, Ref: ref, Kind: root.kind,
		Value: Secret(token), ExpiresAt: exp,
	}, nil
}

func (b *DerivedBroker) Revoke(_ context.Context, credID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.revoked[credID] = time.Now().UTC()
	return nil
}

func (b *DerivedBroker) Refs(_ context.Context, tenantID string) []string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]string, 0, len(b.roots[tenantID]))
	for ref := range b.roots[tenantID] {
		out = append(out, ref)
	}
	return out
}

// Verify checks a minted token. A downstream service (in the demo, the fake
// GitHub API) uses this to prove the credential really was issued by us, is
// bound to the expected tenant and reference, and has not expired or been
// revoked.
func (b *DerivedBroker) Verify(token, wantTenant, wantRef string) error {
	parts := strings.Split(strings.TrimPrefix(token, "aot_"), ".")
	if len(parts) != 4 {
		return errors.New("creds: malformed token")
	}
	tenantID, credID, expStr, sig := parts[0], parts[1], parts[2], parts[3]
	var expUnix int64
	if _, err := fmt.Sscanf(expStr, "%d", &expUnix); err != nil {
		return errors.New("creds: malformed expiry")
	}
	if time.Now().Unix() >= expUnix {
		return fmt.Errorf("creds: token expired at %s", time.Unix(expUnix, 0).UTC())
	}
	b.mu.RLock()
	root, ok := b.roots[tenantID][wantRef]
	_, isRevoked := b.revoked[credID]
	b.mu.RUnlock()
	if !ok {
		return ErrNoSuchRef
	}
	if isRevoked {
		return ErrRevoked
	}
	if wantTenant != "" && tenantID != wantTenant {
		return fmt.Errorf("creds: token is for tenant %s, not %s", tenantID, wantTenant)
	}
	mac := hmac.New(sha256.New, root.key)
	fmt.Fprintf(mac, "%s\n%s\n%s\n%d", tenantID, wantRef, credID, expUnix)
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(want)) {
		return errors.New("creds: signature mismatch")
	}
	return nil
}

var _ Broker = (*DerivedBroker)(nil)

// Scrub removes any occurrence of a secret's plaintext from a string.
//
// This is a last line of defence, not a control: anything that relies on Scrub
// to stay safe has a bug upstream. It exists because tool output is attacker
// influenced (a CLI can be made to echo its environment), and the cost of one
// string replacement is nothing compared to a token reaching the event log,
// where it would be durably stored and replayed into the model's context on
// every subsequent turn.
func Scrub(s string, secrets ...Secret) string {
	for _, sec := range secrets {
		if v := sec.Reveal(); len(v) >= 8 {
			s = strings.ReplaceAll(s, v, Redacted)
		}
	}
	return s
}
