package cascade

import "testing"

func TestPredictNextPatch(t *testing.T) {
	tests := []struct {
		name  string
		tags  []string
		minor string
		want  string
	}{
		{"no tags on minor", []string{"v0.6.4", "v0.8.1"}, "v0.7", "v0.7.0"},
		{"highest patch wins", []string{"v0.7.1", "v0.7.5", "v0.7.3"}, "v0.7", "v0.7.6"},
		{"prerelease bumps implied patch", []string{"v0.7.5-rc.2"}, "v0.7", "v0.7.6"},
		{"junk tags ignored", []string{"latest", "v0.7.x"}, "v0.7", "v0.7.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PredictNextPatch(tt.tags, tt.minor)
			if got != tt.want {
				t.Errorf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestPredictNextRC(t *testing.T) {
	tests := []struct {
		name  string
		tags  []string
		minor string
		want  string
	}{
		{"rc bumps rc number", []string{"v0.7.5-rc.1"}, "v0.7", "v0.7.5-rc.2"},
		{"highest rc wins", []string{"v0.7.5-rc.1", "v0.7.5-rc.3", "v0.7.5-rc.2"}, "v0.7", "v0.7.5-rc.4"},
		{"GA falls back to next patch rc.1", []string{"v0.7.5"}, "v0.7", "v0.7.6-rc.1"},
		{"GA outranks earlier rc", []string{"v0.7.5-rc.1", "v0.7.5"}, "v0.7", "v0.7.6-rc.1"},
		{"rc on next patch outranks GA", []string{"v0.7.5", "v0.7.6-rc.1"}, "v0.7", "v0.7.6-rc.2"},
		{"no tags on minor", []string{"v0.6.0"}, "v0.7", "v0.7.0-rc.1"},
		{"non-rc prerelease falls back to patch+rc.1", []string{"v0.7.5-alpha.1"}, "v0.7", "v0.7.6-rc.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PredictNextRC(tt.tags, tt.minor)
			if got != tt.want {
				t.Errorf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestPredictUnRC(t *testing.T) {
	tests := []struct {
		name  string
		tags  []string
		minor string
		want  string
	}{
		{"rc drops suffix", []string{"v0.9.0-rc.4"}, "v0.9", "v0.9.0"},
		{"highest rc wins", []string{"v0.9.0-rc.1", "v0.9.0-rc.4", "v0.9.0-rc.2"}, "v0.9", "v0.9.0"},
		{"GA leaves nothing to unRC", []string{"v0.9.0"}, "v0.9", ""},
		{"GA outranks earlier rc", []string{"v0.9.0-rc.4", "v0.9.0"}, "v0.9", ""},
		{"no tags on minor", []string{"v0.8.0-rc.1"}, "v0.9", ""},
		{"non-rc prerelease is not unRC-able", []string{"v0.9.0-alpha.1"}, "v0.9", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PredictUnRC(tt.tags, tt.minor)
			if got != tt.want {
				t.Errorf("got %q want %q", got, tt.want)
			}
		})
	}
}

func TestSplitRC(t *testing.T) {
	tests := []struct {
		name      string
		tag       string
		wantBase  string
		wantN     int
		wantHasRC bool
	}{
		{"plain rc", "v0.7.5-rc.2", "v0.7.5", 2, true},
		{"no rc suffix", "v0.7.5", "v0.7.5", 0, false},
		{"alpha is not rc", "v0.7.5-alpha.1", "v0.7.5-alpha.1", 0, false},
		{"unparseable rc number", "v0.7.5-rc.x", "v0.7.5-rc.x", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base, n, hasRC := SplitRC(tt.tag)
			if base != tt.wantBase || n != tt.wantN || hasRC != tt.wantHasRC {
				t.Errorf("got (%q, %d, %v) want (%q, %d, %v)", base, n, hasRC, tt.wantBase, tt.wantN, tt.wantHasRC)
			}
		})
	}
}
