package config

import (
	"strings"
	"testing"
	"time"
)

// TestDepthParsing: a depth is a duration or one of the two words. Zero is
// refused either way round: an operator who means the whole chain has to
// say so, and one who typed a stray zero does not start a full-chain
// replay by accident.
func TestDepthParsing(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Depth
	}{
		{"720h", Depth(720 * time.Hour)},
		{" 1h30m ", Depth(90 * time.Minute)},
		{"genesis", Genesis},
		{"GENESIS", Genesis},
		{"hold", Hold},
		{" HOLD ", Hold},
	} {
		var d Depth
		if err := d.UnmarshalText([]byte(tc.in)); err != nil || d != tc.want {
			t.Fatalf("%q = %v %v, want %v", tc.in, d, err, tc.want)
		}
	}
	for _, in := range []string{"0", "0s", "-1h", "", "all", "everything", "720"} {
		var d Depth
		if err := d.UnmarshalText([]byte(in)); err == nil {
			t.Fatalf("%q must be refused, got %v", in, d)
		}
	}
	if Genesis.String() != GenesisWord || Hold.String() != HoldWord || Depth(time.Hour).String() != "1h0m0s" {
		t.Fatalf("string: %q %q %q", Genesis, Hold, Depth(time.Hour))
	}
	if !Genesis.IsGenesis() || Depth(time.Hour).IsGenesis() || Depth(time.Hour).Duration() != time.Hour {
		t.Fatal("genesis and duration accessors")
	}
	if !Hold.IsHold() || Genesis.IsHold() || Depth(time.Hour).IsHold() || Hold.IsGenesis() {
		t.Fatal("hold accessors")
	}
}

// TestLoadBackfillDepth: the field is decoded by hand, so the file, the
// default and a rejection all have to be exercised through Load.
func TestLoadBackfillDepth(t *testing.T) {
	base := `database:
  url: postgres://x
networks:
  - name: a
    chain_id: 1
    rpc_url: http://x
    enabled: true
`
	for _, tc := range []struct {
		name string
		yaml string
		want Depth
	}{
		{"default", base, Depth(720 * time.Hour)},
		{"duration", base + "collector:\n  backfill_depth: 1h\n", Depth(time.Hour)},
		{"genesis", base + "collector:\n  backfill_depth: genesis\n", Genesis},
		{"hold", base + "collector:\n  backfill_depth: hold\n", Hold},
	} {
		cfg, err := LoadWith(Options{Path: writeYAML(t, tc.yaml), Getenv: envOf(nil)})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if cfg.Collector.BackfillDepth != tc.want {
			t.Fatalf("%s = %v, want %v", tc.name, cfg.Collector.BackfillDepth, tc.want)
		}
	}
	_, err := LoadWith(Options{Path: writeYAML(t, base+"collector:\n  backfill_depth: soon\n"), Getenv: envOf(nil)})
	if err == nil || !strings.Contains(err.Error(), "backfill_depth") {
		t.Fatalf("an unparseable depth must name the setting: %v", err)
	}
	cfg := &Config{}
	if err := cfg.Validate(false); err == nil || !strings.Contains(err.Error(), "backfill_depth must be positive") {
		t.Fatalf("an unset depth stays invalid: %v", err)
	}
	held, err := LoadWith(Options{Path: writeYAML(t, base+"collector:\n  backfill_depth: hold\n"), Getenv: envOf(nil)})
	if err != nil {
		t.Fatal(err)
	}
	if err := held.Validate(false); err != nil && strings.Contains(err.Error(), "backfill_depth") {
		t.Fatalf("hold is a valid depth: %v", err)
	}
}
