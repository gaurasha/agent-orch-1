// Package jwtmini is a deliberately tiny HS256 JWT implementation.
//
// Scope note: this exists so the proof of concept has a real, verifiable
// run-scoped token without pulling in a JWT library. Production would use
// asymmetric keys (RS256/EdDSA) issued by the control plane and, better,
// SPIFFE/SPIRE mTLS identities for service-to-service auth. See
// DEEP_DIVE.md "Workload identity" for why a shared HMAC secret is not
// acceptable beyond a demo.
package jwtmini

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrMalformed = errors.New("jwt: malformed token")
	ErrSignature = errors.New("jwt: signature mismatch")
	ErrExpired   = errors.New("jwt: token expired")
	ErrAudience  = errors.New("jwt: wrong audience")
)

// Claims is the fixed claim set this system uses. Keeping it closed (rather
// than a free-form map) means a typo in a claim name is a compile error.
type Claims struct {
	Subject   string `json:"sub"` // run id
	Tenant    string `json:"ten"`
	AgentName string `json:"agn"`
	DefDigest string `json:"dig"`
	User      string `json:"usr"` // triggering human
	Audience  string `json:"aud"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
}

type header struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

var enc = base64.RawURLEncoding

// Sign returns a compact HS256 JWT.
func Sign(secret []byte, c Claims) (string, error) {
	if len(secret) < 16 {
		return "", errors.New("jwt: signing secret too short (min 16 bytes)")
	}
	hb, err := json.Marshal(header{Alg: "HS256", Typ: "JWT"})
	if err != nil {
		return "", err
	}
	cb, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	signing := enc.EncodeToString(hb) + "." + enc.EncodeToString(cb)
	return signing + "." + enc.EncodeToString(mac(secret, signing)), nil
}

// Verify checks the signature, expiry and audience, then returns the claims.
func Verify(secret []byte, token, audience string) (Claims, error) {
	var c Claims
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return c, ErrMalformed
	}
	hb, err := enc.DecodeString(parts[0])
	if err != nil {
		return c, ErrMalformed
	}
	var h header
	if err := json.Unmarshal(hb, &h); err != nil {
		return c, ErrMalformed
	}
	// Reject "alg": "none" and algorithm confusion outright.
	if h.Alg != "HS256" {
		return c, fmt.Errorf("jwt: unsupported alg %q", h.Alg)
	}
	signing := parts[0] + "." + parts[1]
	sig, err := enc.DecodeString(parts[2])
	if err != nil {
		return c, ErrMalformed
	}
	if !hmac.Equal(sig, mac(secret, signing)) {
		return c, ErrSignature
	}
	cb, err := enc.DecodeString(parts[1])
	if err != nil {
		return c, ErrMalformed
	}
	if err := json.Unmarshal(cb, &c); err != nil {
		return c, ErrMalformed
	}
	if time.Now().Unix() >= c.ExpiresAt {
		return c, ErrExpired
	}
	if audience != "" && c.Audience != audience {
		return c, ErrAudience
	}
	return c, nil
}

func mac(secret []byte, msg string) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(msg))
	return m.Sum(nil)
}
