// Package id generates short, sortable, collision-resistant identifiers.
//
// We deliberately avoid a UUID dependency: a 64-bit millisecond timestamp
// prefix plus 64 bits of CSPRNG entropy gives us k-sortable ids (nice for
// Postgres B-tree locality and for reading logs chronologically) with a
// collision probability far below anything this system will ever reach.
package id

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"time"
)

// New returns a prefixed identifier such as "run_018f3a1c9d2b4e7a1f".
func New(prefix string) string {
	var b [14]byte
	binary.BigEndian.PutUint64(b[0:8], uint64(time.Now().UTC().UnixMilli()))
	if _, err := rand.Read(b[8:]); err != nil {
		// crypto/rand failing means the kernel CSPRNG is broken. There is no
		// safe fallback for a security-relevant identifier, so fail loudly.
		panic("id: crypto/rand unavailable: " + err.Error())
	}
	return prefix + "_" + hex.EncodeToString(b[2:])
}

// Token returns n bytes of hex-encoded CSPRNG output, for secrets.
func Token(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("id: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b)
}
