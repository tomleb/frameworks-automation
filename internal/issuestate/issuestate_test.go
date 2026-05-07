package issuestate

import (
	"strings"
	"testing"
)

type sampleState struct {
	Items []sampleItem `yaml:"items,omitempty"`
	Note  string       `yaml:"note,omitempty"`
}

type sampleItem struct {
	Name  string `yaml:"name"`
	Value int    `yaml:"value,omitempty"`
}

var sampleMarker = Marker[sampleState]{
	Open:  "<!-- sample-state v1",
	Close: "-->",
}

func TestRoundTrip(t *testing.T) {
	in := sampleState{
		Items: []sampleItem{{Name: "alpha", Value: 1}, {Name: "beta", Value: 2}},
		Note:  "hello",
	}
	body, err := sampleMarker.Embed("preamble\n", in)
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	got, err := sampleMarker.Extract(body)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if got.Note != "hello" || len(got.Items) != 2 || got.Items[1].Value != 2 {
		t.Errorf("round-trip mismatch: got %+v", got)
	}
	if !strings.Contains(body, "preamble") {
		t.Errorf("preamble lost: %q", body)
	}
}

func TestExtractMissingBlock(t *testing.T) {
	got, err := sampleMarker.Extract("just a body\n")
	if err != nil {
		t.Fatalf("expected no error for missing block, got: %v", err)
	}
	if len(got.Items) != 0 || got.Note != "" {
		t.Errorf("expected zero state, got %+v", got)
	}
}

func TestExtractMissingClose(t *testing.T) {
	body := "header\n" + sampleMarker.Open + "\nitems: []\n"
	if _, err := sampleMarker.Extract(body); err == nil {
		t.Fatal("expected error for missing close fence")
	}
}

func TestEmbedReplacesExisting(t *testing.T) {
	first, err := sampleMarker.Embed("header\n", sampleState{Note: "one"})
	if err != nil {
		t.Fatalf("embed first: %v", err)
	}
	second, err := sampleMarker.Embed(first, sampleState{Note: "two"})
	if err != nil {
		t.Fatalf("embed second: %v", err)
	}
	got, _ := sampleMarker.Extract(second)
	if got.Note != "two" {
		t.Errorf("note after replace: got %q want two", got.Note)
	}
	if strings.Count(second, sampleMarker.Open) != 1 {
		t.Errorf("expected exactly one open fence after replace, got %d:\n%s",
			strings.Count(second, sampleMarker.Open), second)
	}
}

func TestEmbedAppendsNewline(t *testing.T) {
	body, err := sampleMarker.Embed("no trailing newline", sampleState{Note: "x"})
	if err != nil {
		t.Fatalf("embed: %v", err)
	}
	if !strings.Contains(body, "no trailing newline\n\n") {
		t.Errorf("expected normalized newline before fence: %q", body)
	}
}

// Different markers don't collide: extracting marker A from a body that
// contains marker B's block returns the zero value (block absent).
func TestMarkerIsolation(t *testing.T) {
	other := Marker[sampleState]{Open: "<!-- other-state v1", Close: "-->"}
	body, _ := sampleMarker.Embed("", sampleState{Note: "mine"})
	got, err := other.Extract(body)
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if got.Note != "" {
		t.Errorf("other marker should not see sample's block, got %+v", got)
	}
}
