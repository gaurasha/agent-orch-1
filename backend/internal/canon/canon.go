// Package canon produces a canonical byte representation of arbitrary JSON
// values so that they can be hashed reproducibly.
//
// Why this exists: agent definitions are content-addressed by digest, and the
// audit log is a hash chain. Both need "the same logical value always hashes
// to the same bytes". Go's encoding/json already sorts map keys, but it does
// not normalise number formatting or guarantee stability across types, so we
// re-marshal through a normalised interface{} tree.
package canon

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
)

// Bytes returns the canonical JSON encoding of v.
func Bytes(v any) ([]byte, error) {
	// Round-trip through encoding/json first so that structs, maps and
	// pointers all collapse to the same primitive tree.
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("canon: marshal: %w", err)
	}
	var tree any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // keep numbers exact rather than forcing float64
	if err := dec.Decode(&tree); err != nil {
		return nil, fmt.Errorf("canon: decode: %w", err)
	}
	var buf bytes.Buffer
	if err := write(&buf, tree); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Digest returns "sha256:<hex>" over the canonical encoding of v.
func Digest(v any) (string, error) {
	b, err := Bytes(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// HashHex returns the bare hex sha256 of raw bytes.
func HashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func write(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		buf.WriteString(strconv.FormatBool(t))
	case json.Number:
		// Normalise: integers lose any ".0", floats use shortest round-trip form.
		if i, err := t.Int64(); err == nil {
			buf.WriteString(strconv.FormatInt(i, 10))
			return nil
		}
		f, err := t.Float64()
		if err != nil {
			return fmt.Errorf("canon: bad number %q: %w", t.String(), err)
		}
		buf.WriteString(strconv.FormatFloat(f, 'g', -1, 64))
	case string:
		b, err := json.Marshal(t)
		if err != nil {
			return err
		}
		buf.Write(b)
	case []any:
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := write(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			kb, err := json.Marshal(k)
			if err != nil {
				return err
			}
			buf.Write(kb)
			buf.WriteByte(':')
			if err := write(buf, t[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("canon: unsupported type %T", v)
	}
	return nil
}
