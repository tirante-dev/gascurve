package db

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const (
	productionRelease       = "v1.0.1"
	productionSchemaVersion = uint(1)
)

var immutableMigrationSHA256 = map[string]string{
	"000001_init.down.sql":                      "3a02068ff4a96c8c34405ffb1941751130a089573fef24717a70273fa15fd963",
	"000001_init.up.sql":                        "12ecf190ff9c1359f4d470c03cdcf030e257981257e9ebc9f4f2eea57a0bc26d",
	"000002_state_samples_chain_block.down.sql": "031e18eb914da642209b67fc00cc443a496a72243f4aa3d59e985c49e501d4f4",
	"000002_state_samples_chain_block.up.sql":   "5f3d4651c07fdadb5503e3fb1097652e3ef6b1e9d9441f5f15f4f8c7c1399694",
	"000003_owner_action_tx_index.down.sql":     "923b26b4d547c3035a7cfb615ec2b1c28960df8ed0e614195dbee95cb964f761",
	"000003_owner_action_tx_index.up.sql":       "768092956fab6766fa127d3a8fc306fdbdd9d5bbe67262356a2406d2b24b2736",
	"000004_missing_ranges.down.sql":            "d93932d7e6c94bf147b57284f1be39dfe79719f6a83e770105af023420fa5eb3",
	"000004_missing_ranges.up.sql":              "478580c14ea88798fe4eb0472477c362a1f60e823f49933c37df86ed4ee540ef",
}

func TestReleasedMigrationsImmutable(t *testing.T) {
	t.Logf("production baseline %s uses schema version %d", productionRelease, productionSchemaVersion)
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}

	directions := map[uint]map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		version, direction, err := migrationVersion(name)
		if err != nil {
			t.Error(err)
			continue
		}
		if directions[version] == nil {
			directions[version] = map[string]bool{}
		}
		if directions[version][direction] {
			t.Errorf("migration %d has more than one %s file", version, direction)
		}
		directions[version][direction] = true
		if _, ok := immutableMigrationSHA256[name]; !ok {
			t.Errorf("migration %s is not protected by a checksum", name)
		}
	}

	versions := make([]uint, 0, len(directions))
	for version := range directions {
		versions = append(versions, version)
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i] < versions[j] })
	for i, version := range versions {
		if want := uint(i + 1); version != want {
			t.Errorf("migration versions must be sequential from 1 with no gaps: found %d, want %d", version, want)
		}
		for _, direction := range []string{"up", "down"} {
			if !directions[version][direction] {
				t.Errorf("migration %d has no %s file", version, direction)
			}
		}
	}
	if len(versions) > 0 && versions[len(versions)-1] < productionSchemaVersion {
		t.Errorf("head is version %d, below the deployed production version %d (%s)",
			versions[len(versions)-1], productionSchemaVersion, productionRelease)
	}

	for name, want := range immutableMigrationSHA256 {
		contents, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Errorf("released migration %s is missing: %v", name, err)
			continue
		}
		got := fmt.Sprintf("%x", sha256.Sum256(contents))
		if got != want {
			t.Errorf("released migration %s changed: sha256 = %s, want %s; add a forward migration instead", name, got, want)
		}
	}
}

// migrationVersion parses a golang-migrate file name into its schema version
// and direction.
func migrationVersion(name string) (version uint, direction string, err error) {
	prefix, rest, ok := strings.Cut(name, "_")
	if !ok {
		return 0, "", fmt.Errorf("migration %s has no numeric prefix", name)
	}
	if len(prefix) != 6 {
		return 0, "", fmt.Errorf("migration %s must use a six-digit version prefix", name)
	}
	n, err := strconv.ParseUint(prefix, 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("migration %s has an invalid version: %w", name, err)
	}
	switch {
	case strings.HasSuffix(rest, ".up.sql"):
		direction = "up"
	case strings.HasSuffix(rest, ".down.sql"):
		direction = "down"
	default:
		return 0, "", fmt.Errorf("migration %s must end in .up.sql or .down.sql", name)
	}
	return uint(n), direction, nil
}
