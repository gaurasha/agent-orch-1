// Package fakegithub is a stand-in for a third-party API that requires a
// tenant credential.
//
// Its job in the demo is to be a real HTTP service that ACTUALLY VERIFIES the
// credential it is given. Without that, "the platform injected a credential"
// would be an unverifiable claim - the demo would look identical if the token
// were empty. Here the server rejects a missing, malformed, expired, revoked
// or wrong-tenant token, so a successful `gh pr create` is evidence that a
// valid, correctly scoped, freshly minted credential reached it.
package fakegithub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gaurasha/agent-orch/backend/internal/creds"
	"github.com/gaurasha/agent-orch/backend/internal/obs"
)

type PullRequest struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	Tenant    string    `json:"tenant"`
	URL       string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
}

type Server struct {
	mu      sync.Mutex
	broker  *creds.DerivedBroker
	prs     []PullRequest
	nextNum int
	log     *obs.Logger
	// authAttempts records every credential presentation so the demo can show
	// that a real token arrived and was checked.
	authAttempts []AuthAttempt
}

type AuthAttempt struct {
	At        time.Time `json:"at"`
	Path      string    `json:"path"`
	Presented bool      `json:"credential_presented"`
	Valid     bool      `json:"credential_valid"`
	Reason    string    `json:"reason,omitempty"`
	Tenant    string    `json:"tenant,omitempty"`
	// TokenFingerprint is a short prefix/suffix so a human can eyeball that
	// each call used a DIFFERENT, freshly minted token. Never the whole value.
	TokenFingerprint string `json:"token_fingerprint,omitempty"`
}

func New(broker *creds.DerivedBroker, log *obs.Logger) *Server {
	return &Server{broker: broker, nextNum: 41, log: log}
}

func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /repos/{owner}/{repo}/pulls", s.createPR)
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls", s.listPRs)
	mux.HandleFunc("GET /user", s.getUser)
	// Demo-only introspection, so the README can show the evidence.
	mux.HandleFunc("GET /_demo/auth-attempts", s.listAuthAttempts)
}

// authenticate verifies the presented credential and records the attempt.
func (s *Server) authenticate(r *http.Request) (string, error) {
	raw := r.Header.Get("Authorization")
	token := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(raw, "Bearer"), "token"))
	attempt := AuthAttempt{At: time.Now().UTC(), Path: r.URL.Path, Presented: token != ""}
	defer func() {
		s.mu.Lock()
		s.authAttempts = append(s.authAttempts, attempt)
		s.mu.Unlock()
	}()

	if token == "" {
		attempt.Reason = "no Authorization header"
		return "", fmt.Errorf("authentication required")
	}
	attempt.TokenFingerprint = fingerprint(token)
	// The token carries its tenant; the broker proves it was really issued by
	// us, for this credential reference, and has not expired or been revoked.
	tenant := ""
	if rest, ok := strings.CutPrefix(token, "aot_"); ok {
		if i := strings.IndexByte(rest, '.'); i > 0 {
			tenant = rest[:i]
		}
	}
	attempt.Tenant = tenant
	if err := s.broker.Verify(token, tenant, "github/token"); err != nil {
		attempt.Reason = err.Error()
		return "", fmt.Errorf("invalid credential: %w", err)
	}
	attempt.Valid = true
	return tenant, nil
}

func fingerprint(tok string) string {
	if len(tok) < 16 {
		return "short"
	}
	return tok[:10] + "..." + tok[len(tok)-6:]
}

func (s *Server) createPR(w http.ResponseWriter, r *http.Request) {
	tenant, err := s.authenticate(r)
	if err != nil {
		s.log.Warn("github API rejected a call", "path", r.URL.Path, "err", err)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": err.Error()})
		return
	}
	var body struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	s.mu.Lock()
	s.nextNum++
	pr := PullRequest{
		Number: s.nextNum, Title: body.Title, Body: body.Body, Tenant: tenant,
		URL:       fmt.Sprintf("https://github.example.com/%s/%s/pull/%d", r.PathValue("owner"), r.PathValue("repo"), s.nextNum),
		CreatedAt: time.Now().UTC(),
	}
	s.prs = append(s.prs, pr)
	s.mu.Unlock()

	s.log.Info("github API accepted a credentialed call", "tenant", tenant, "pr", pr.Number)
	writeJSON(w, http.StatusCreated, pr)
}

func (s *Server) listPRs(w http.ResponseWriter, r *http.Request) {
	tenant, err := s.authenticate(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": err.Error()})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Tenancy at the far end too: one tenant's credential cannot list another's
	// pull requests.
	out := []PullRequest{}
	for _, pr := range s.prs {
		if pr.Tenant == tenant {
			out = append(out, pr)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getUser(w http.ResponseWriter, r *http.Request) {
	tenant, err := s.authenticate(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"login": "agentorch-bot-" + tenant, "type": "Bot",
	})
}

func (s *Server) listAuthAttempts(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"attempts": s.authAttempts})
}

// AuthAttempts exposes the record for tests.
func (s *Server) AuthAttempts() []AuthAttempt {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]AuthAttempt, len(s.authAttempts))
	copy(out, s.authAttempts)
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
