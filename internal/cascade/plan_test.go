package cascade

import (
	"testing"

	"github.com/rancher/release-automation/internal/config"
)

func TestLeafBranchForDepVersion_Independent(t *testing.T) {
	cfg := &config.Config{
		Repos: map[string]config.Repo{
			"wrangler": {Kind: config.KindIndependent, Repo: "rancher/wrangler"},
		},
	}
	got, err := LeafBranchForDepVersion(cfg, "wrangler", "v0.5.6", nil, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "main" {
		t.Errorf("independent: got %q want main", got)
	}
}

func TestLeafBranchForDepVersion_Paired(t *testing.T) {
	cfg := &config.Config{
		Repos: map[string]config.Repo{
			"steve":   {Kind: config.KindPaired, Repo: "rancher/steve"},
			"rancher": {Kind: config.KindLeaf, Repo: "rancher/rancher"},
		},
	}
	depTable, err := config.ParseVersionTable("| Branch | Minor | Matching Rancher |\n|--|--|--|\n| release/v0.7 | v0.7 | v2.13 |\n| main | v0.8 | v2.16 |\n")
	if err != nil {
		t.Fatalf("parse depTable: %v", err)
	}
	leafTable, err := config.ParseVersionTable("| Branch | Minor | Matching Rancher |\n|--|--|--|\n| release/v2.13 | v2.13 | v2.13 |\n| main | v2.16 | v2.16 |\n")
	if err != nil {
		t.Fatalf("parse leafTable: %v", err)
	}

	cases := []struct {
		name, version, want string
	}{
		{"older minor → release branch", "v0.7.5", "release/v2.13"},
		{"current minor → main", "v0.8.0", "main"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := LeafBranchForDepVersion(cfg, "steve", c.version, leafTable, depTable)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if got != c.want {
				t.Errorf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestLeafBranchForDepVersion_PairedUnknownMinor(t *testing.T) {
	cfg := &config.Config{
		Repos: map[string]config.Repo{
			"steve": {Kind: config.KindPaired, Repo: "rancher/steve"},
		},
	}
	depTable, _ := config.ParseVersionTable("| Branch | Minor | Matching Rancher |\n|--|--|--|\n| main | v0.8 | v2.16 |\n")
	leafTable, _ := config.ParseVersionTable("| Branch | Minor | Matching Rancher |\n|--|--|--|\n| main | v2.16 | v2.16 |\n")
	_, err := LeafBranchForDepVersion(cfg, "steve", "v0.6.0", leafTable, depTable)
	if err == nil {
		t.Fatal("expected error for unknown minor")
	}
}

func TestLeafBranchForDepVersion_InvalidVersion(t *testing.T) {
	cfg := &config.Config{
		Repos: map[string]config.Repo{
			"steve": {Kind: config.KindPaired, Repo: "rancher/steve"},
		},
	}
	depTable, _ := config.ParseVersionTable("| Branch | Minor | Matching Rancher |\n|--|--|--|\n| main | v0.8 | v2.16 |\n")
	leafTable, _ := config.ParseVersionTable("| Branch | Minor | Matching Rancher |\n|--|--|--|\n| main | v2.16 | v2.16 |\n")
	_, err := LeafBranchForDepVersion(cfg, "steve", "garbage", leafTable, depTable)
	if err == nil {
		t.Fatal("expected error for invalid version")
	}
}

func TestLeafBranchForDepVersion_UnknownDep(t *testing.T) {
	cfg := &config.Config{Repos: map[string]config.Repo{}}
	_, err := LeafBranchForDepVersion(cfg, "ghost", "v0.1.0", nil, nil)
	if err == nil {
		t.Fatal("expected error for unknown dep")
	}
}
