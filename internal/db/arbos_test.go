package db

import (
	"database/sql"
	"testing"
	"time"
)

func versionedBlock(number uint64, version sql.NullInt64) Block {
	return Block{
		ChainID: 1, Number: number, TS: time.Unix(int64(number), 0).UTC(), GasUsed: 1,
		BaseFee: NewWei(nil), MinBaseFee: NewNullWei(nil), PricingVersion: PricingFull,
		PosterGas: sql.NullInt64{Valid: true}, ArbOSVersion: version,
	}
}

func version(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: true} }

func foldedRange(t *testing.T, versions ...sql.NullInt64) Bucket {
	t.Helper()
	b := NewBucketBuilder(1, Resolution1m, time.Unix(0, 0).UTC())
	for i, v := range versions {
		b.Add(versionedBlock(uint64(i+1), v), sql.NullInt64{})
	}
	return b.Bucket()
}

func TestBucketArbOSRange(t *testing.T) {
	tests := []struct {
		name             string
		versions         []sql.NullInt64
		wantLo, wantHi   int64
		wantKnown, spans bool
	}{
		{"one version", []sql.NullInt64{version(61), version(61)}, 61, 61, true, false},
		{"an upgrade inside the bucket", []sql.NullInt64{version(51), version(61)}, 51, 61, true, true},
		{"out of order still bounds", []sql.NullInt64{version(61), version(51)}, 51, 61, true, true},
		{"one block without a version", []sql.NullInt64{version(61), {}}, 0, 0, false, false},
		{"the first block without a version", []sql.NullInt64{{}, version(61)}, 0, 0, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := foldedRange(t, tt.versions...)
			if b.ArbOSVersionMin.Valid != tt.wantKnown || b.ArbOSVersionMax.Valid != tt.wantKnown {
				t.Fatalf("range known = (%v, %v), want %v", b.ArbOSVersionMin.Valid, b.ArbOSVersionMax.Valid, tt.wantKnown)
			}
			if tt.wantKnown && (b.ArbOSVersionMin.Int64 != tt.wantLo || b.ArbOSVersionMax.Int64 != tt.wantHi) {
				t.Errorf("range = %d..%d, want %d..%d", b.ArbOSVersionMin.Int64, b.ArbOSVersionMax.Int64, tt.wantLo, tt.wantHi)
			}
			if got := b.SpansArbOSUpgrade(); got != tt.spans {
				t.Errorf("SpansArbOSUpgrade = %v, want %v", got, tt.spans)
			}
		})
	}
}

// MergeBuckets is the additive fold the backfill uses, and it has to reach the same range the SQL
// fold does: an empty stored bucket takes the incoming one whole, and either side without a version
// leaves the merged range unknown.
func TestMergeBucketsArbOSRange(t *testing.T) {
	empty := NewBucketBuilder(1, Resolution1m, time.Unix(0, 0).UTC()).Bucket()
	older := foldedRange(t, version(51))
	newer := foldedRange(t, version(61))
	unknown := foldedRange(t, sql.NullInt64{})

	if got := MergeBuckets(empty, newer); !got.ArbOSVersionMin.Valid || got.ArbOSVersionMin.Int64 != 61 {
		t.Errorf("empty + 61 = %v, want 61", got.ArbOSVersionMin)
	}
	if got := MergeBuckets(older, empty); !got.ArbOSVersionMax.Valid || got.ArbOSVersionMax.Int64 != 51 {
		t.Errorf("51 + empty = %v, want 51", got.ArbOSVersionMax)
	}
	merged := MergeBuckets(older, newer)
	if !merged.SpansArbOSUpgrade() || merged.ArbOSVersionMin.Int64 != 51 || merged.ArbOSVersionMax.Int64 != 61 {
		t.Errorf("51 + 61 = %d..%d, want a 51..61 span", merged.ArbOSVersionMin.Int64, merged.ArbOSVersionMax.Int64)
	}
	if got := MergeBuckets(older, unknown); got.ArbOSVersionMin.Valid || got.ArbOSVersionMax.Valid {
		t.Errorf("51 + unknown = %v..%v, want unknown", got.ArbOSVersionMin, got.ArbOSVersionMax)
	}
	if got := MergeBuckets(unknown, newer); got.ArbOSVersionMin.Valid || got.ArbOSVersionMax.Valid {
		t.Errorf("unknown + 61 = %v..%v, want unknown", got.ArbOSVersionMin, got.ArbOSVersionMax)
	}
}
