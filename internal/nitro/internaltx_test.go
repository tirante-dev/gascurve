package nitro

import (
	"errors"
	"math"
	"math/big"
	"testing"
)

func TestInternalTxRoundTrip(t *testing.T) {
	sb := StartBlock{L1BaseFee: big.NewInt(0), L1BlockNumber: 23_000_000, L2BlockNumber: 55_800_000, TimePassed: 1}
	got, err := DecodeInternalTx(EncodeStartBlock(sb))
	if err != nil {
		t.Fatal(err)
	}
	if s, ok := got.(*StartBlock); !ok || s.L1BlockNumber != 23_000_000 || s.L2BlockNumber != 55_800_000 || s.TimePassed != 1 || s.L1BaseFee.Sign() != 0 {
		t.Fatalf("startBlock: %+v", got)
	}

	r := BatchPostingReport{Version: 2, BatchTimestamp: 1_757_000_000, Poster: "0xdaa5260800000000000000000000000000000000", BatchNumber: 201_900,
		CalldataLen: 137, CalldataNonZeros: 113, ExtraGas: 27_132, L1BaseFee: big.NewInt(73_500_000)}
	got, err = DecodeInternalTx(EncodeBatchPostingReport(r))
	if err != nil {
		t.Fatal(err)
	}
	br, ok := got.(*BatchPostingReport)
	if !ok || br.BatchNumber != r.BatchNumber || br.Poster != r.Poster || br.CalldataLen != 137 || br.CalldataNonZeros != 113 || br.ExtraGas != 27_132 {
		t.Fatalf("batch report: %+v", got)
	}
	if br.L1BaseFee.Cmp(r.L1BaseFee) != 0 || br.BatchTimestamp != r.BatchTimestamp || br.Version != 2 {
		t.Fatalf("batch report fields: %+v", br)
	}
	v1 := BatchPostingReport{Version: 1, BatchTimestamp: 5, Poster: "0x0000000000000000000000000000000000000001", BatchNumber: 9, ExtraGas: 1000, L1BaseFee: big.NewInt(2)}
	got, err = DecodeInternalTx(EncodeBatchPostingReport(v1))
	if err != nil {
		t.Fatal(err)
	}
	b1, ok := got.(*BatchPostingReport)
	if !ok || b1.Version != 1 || b1.BatchNumber != 9 || b1.Poster != v1.Poster {
		t.Fatalf("v1: %+v", b1)
	}
}

// These expectations are direct vectors from Nitro's version-aware
// ApplyInternalTxUpdate and LegacyCostForStats formulas at commit
// a618155919315241665356fe60f3cd00d66d5e46:
// github.com/OffchainLabs/nitro/arbos/internal_tx.go and
// github.com/OffchainLabs/nitro/arbos/arbostypes/incomingmessage.go.
func TestBatchPostingReportCostNitroVectors(t *testing.T) {
	tests := []struct {
		name    string
		report  BatchPostingReport
		params  BatchPostingCostParams
		wantGas uint64
		wantWei string
	}{
		{
			name:    "v2 production sample includes legacy overhead and per-batch charge",
			report:  BatchPostingReport{Version: 2, CalldataLen: 137, CalldataNonZeros: 113, ExtraGas: 27_132, L1BaseFee: big.NewInt(73_500_000)},
			params:  BatchPostingCostParams{ArbOSVersion: 61, PerBatchGasCharge: 210_000, ParentGasFloorPerToken: 10},
			wantGas: 279_096, wantWei: "20513556000000",
		},
		{
			name:    "v2 before ArbOS 50 does not apply parent floor",
			report:  BatchPostingReport{Version: 2, CalldataLen: 10_000, CalldataNonZeros: 10_000, L1BaseFee: big.NewInt(2)},
			params:  BatchPostingCostParams{ArbOSVersion: 49, PerBatchGasCharge: 210_000, ParentGasFloorPerToken: 10},
			wantGas: 411_908, wantWei: "823816",
		},
		{
			name:    "v2 ArbOS 50 parent floor binds",
			report:  BatchPostingReport{Version: 2, CalldataLen: 10_000, CalldataNonZeros: 10_000, L1BaseFee: big.NewInt(2)},
			params:  BatchPostingCostParams{ArbOSVersion: 50, PerBatchGasCharge: 210_000, ParentGasFloorPerToken: 10},
			wantGas: 422_720, wantWei: "845440",
		},
		{
			name:    "v2 negative per-batch charge is ignored",
			report:  BatchPostingReport{Version: 2, CalldataLen: 1, CalldataNonZeros: 1, L1BaseFee: big.NewInt(1)},
			params:  BatchPostingCostParams{ArbOSVersion: 49, PerBatchGasCharge: -1},
			wantGas: 40_052, wantWei: "40052",
		},
		{
			name:    "v1 signed per-batch charge reduces data gas",
			report:  BatchPostingReport{Version: 1, ExtraGas: 1_000, L1BaseFee: big.NewInt(2)},
			params:  BatchPostingCostParams{ArbOSVersion: 49, PerBatchGasCharge: -400},
			wantGas: 600, wantWei: "1200",
		},
		{
			name:    "v1 negative total clamps to zero",
			report:  BatchPostingReport{Version: 1, ExtraGas: 1_000, L1BaseFee: big.NewInt(2)},
			params:  BatchPostingCostParams{ArbOSVersion: 49, PerBatchGasCharge: -1_200},
			wantGas: 0, wantWei: "0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.report.Cost(tt.params)
			if err != nil {
				t.Fatal(err)
			}
			if got.GasSpent != tt.wantGas || got.WeiSpent.String() != tt.wantWei {
				t.Fatalf("cost = %d/%s, want %d/%s", got.GasSpent, got.WeiSpent, tt.wantGas, tt.wantWei)
			}
		})
	}
}

func TestBatchPostingReportCostOverflowSafety(t *testing.T) {
	v2 := BatchPostingReport{Version: 2, CalldataLen: math.MaxUint64, CalldataNonZeros: math.MaxUint64, ExtraGas: math.MaxUint64, L1BaseFee: big.NewInt(1)}
	cost, err := v2.Cost(BatchPostingCostParams{ArbOSVersion: 50, PerBatchGasCharge: math.MaxInt64, ParentGasFloorPerToken: math.MaxUint64})
	if err != nil || cost.GasSpent != math.MaxUint64 || cost.WeiSpent.String() != "18446744073709551615" {
		t.Fatalf("v2 saturation: %+v %v", cost, err)
	}
	v1 := BatchPostingReport{Version: 1, ExtraGas: math.MaxUint64}
	cost, err = v1.Cost(BatchPostingCostParams{PerBatchGasCharge: math.MaxInt64})
	if err != nil || cost.GasSpent != math.MaxInt64 || cost.WeiSpent.Sign() != 0 {
		t.Fatalf("v1 saturation: %+v %v", cost, err)
	}
	clamped, err := (&BatchPostingReport{Version: 2, CalldataLen: 1, CalldataNonZeros: 5}).Cost(BatchPostingCostParams{ArbOSVersion: 49})
	if err != nil || clamped.GasSpent != 40_052 {
		t.Fatalf("nonzero count clamp: %+v %v", clamped, err)
	}
	if _, err := (&BatchPostingReport{Version: 3}).Cost(BatchPostingCostParams{}); err == nil {
		t.Fatal("unsupported report version")
	}
}

func TestInternalTxErrors(t *testing.T) {
	if _, err := DecodeInternalTx([]byte{1}); !errors.Is(err, ErrNotInternal) {
		t.Fatal("short")
	}
	if _, err := DecodeInternalTx([]byte{1, 2, 3, 4, 5}); !errors.Is(err, ErrNotInternal) {
		t.Fatal("unknown selector")
	}
	encoded := [][]byte{
		EncodeStartBlock(StartBlock{L1BaseFee: big.NewInt(1)}),
		EncodeBatchPostingReport(BatchPostingReport{Version: 2, L1BaseFee: big.NewInt(1)}),
		EncodeBatchPostingReport(BatchPostingReport{Version: 1, L1BaseFee: big.NewInt(1)}),
	}
	for i, full := range encoded {
		// Every truncation point must produce an error, never a panic.
		for cut := 4; cut < len(full); cut += 32 {
			if _, err := DecodeInternalTx(full[:cut]); err == nil {
				t.Fatalf("encoding %d cut %d: expected error", i, cut)
			}
		}
		if _, err := DecodeInternalTx(full); err != nil {
			t.Fatalf("encoding %d full: %v", i, err)
		}
	}
	if IsInternalTx(Tx{Type: 2, To: ArbosAddress}) || !IsInternalTx(Tx{Type: InternalTxType, To: "0x00000000000000000000000000000000000A4B05"}) {
		t.Fatal("IsInternalTx")
	}
}
