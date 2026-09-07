package db

import (
	"crypto/sha256"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

const (
	productionRelease       = "v1.0.1"
	productionSchemaVersion = uint(1)
)

var immutableMigrationSHA256 = map[string]string{
	"000001_init.down.sql": "3a02068ff4a96c8c34405ffb1941751130a089573fef24717a70273fa15fd963",
	"000001_init.up.sql":   "12ecf190ff9c1359f4d470c03cdcf030e257981257e9ebc9f4f2eea57a0bc26d",
}

func TestReleasedMigrationsImmutable(t *testing.T) {
	t.Logf("production baseline %s uses schema version %d", productionRelease, productionSchemaVersion)
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		_, err := migrationVersion(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := immutableMigrationSHA256[name]; !ok {
			t.Errorf("migration %s is not protected by a checksum", name)
		}
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

func migrationVersion(name string) (uint, error) {
	prefix, _, ok := strings.Cut(name, "_")
	if !ok {
		return 0, fmt.Errorf("migration %s has no numeric prefix", name)
	}
	version, err := strconv.ParseUint(prefix, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("migration %s has an invalid version: %w", name, err)
	}
	return uint(version), nil
}
