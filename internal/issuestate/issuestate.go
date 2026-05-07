// Package issuestate is the envelope for the "issue body as state store"
// pattern: a fenced YAML metadata block embedded in a GitHub issue body.
// Each tracker flavor (bump-op, cascade) declares its own versioned marker
// and Persistent type and uses Marker.Extract / Marker.Embed to round-trip
// state.
package issuestate

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// Marker is the open/close fence pair that wraps the YAML state block in an
// issue body. The Open marker is versioned (e.g. "<!-- bump-op-state v1") so
// a future schema bump can introduce a v2 envelope without disturbing v1
// issues already in the wild.
type Marker[P any] struct {
	Open  string
	Close string
}

// Extract pulls the marker's YAML block out of body and unmarshals it into
// P. Returns the zero value of P (no error) when the block is absent —
// useful for issues created out-of-band that the reconciler is adopting.
func (m Marker[P]) Extract(body string) (P, error) {
	var zero P
	start := strings.Index(body, m.Open)
	if start < 0 {
		return zero, nil
	}
	rest := body[start+len(m.Open):]
	end := strings.Index(rest, m.Close)
	if end < 0 {
		return zero, fmt.Errorf("metadata block missing closing %q", m.Close)
	}
	var p P
	if err := yaml.Unmarshal([]byte(rest[:end]), &p); err != nil {
		return zero, fmt.Errorf("parse metadata block: %w", err)
	}
	return p, nil
}

// Embed replaces (or appends) the marker's YAML block in body with the YAML
// encoding of p.
func (m Marker[P]) Embed(body string, p P) (string, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(p); err != nil {
		return "", fmt.Errorf("encode metadata: %w", err)
	}
	enc.Close()
	block := m.Open + "\n" + buf.String() + m.Close

	start := strings.Index(body, m.Open)
	if start < 0 {
		if !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		return body + "\n" + block + "\n", nil
	}
	end := strings.Index(body[start:], m.Close)
	if end < 0 {
		return "", fmt.Errorf("metadata block missing closing %q", m.Close)
	}
	end += start + len(m.Close)
	return body[:start] + block + body[end:], nil
}
