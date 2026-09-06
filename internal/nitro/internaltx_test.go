package nitro

import (
	"errors"
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

	// SPEC example: 137 calldata bytes, 113 non-zero, extra gas 27,132,
	// L1 base fee 0.0735 gwei: about 2.1e-6 ETH.
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
	if br.GasSpent() != 24*4+113*16+27_132 {
		t.Fatalf("gasSpent = %d", br.GasSpent())
	}
	if br.WeiSpent().Cmp(big.NewInt(73_500_000*29_036)) != 0 {
		t.Fatalf("weiSpent = %s", br.WeiSpent())
	}
	eth := new(big.Float).Quo(new(big.Float).SetInt(br.WeiSpent()), big.NewFloat(1e18))
	if f, _ := eth.Float64(); f < 2.0e-6 || f > 2.2e-6 {
		t.Fatalf("eth per batch = %g", f)
	}

	v1 := BatchPostingReport{Version: 1, BatchTimestamp: 5, Poster: "0x0000000000000000000000000000000000000001", BatchNumber: 9, ExtraGas: 1000, L1BaseFee: big.NewInt(2)}
	got, err = DecodeInternalTx(EncodeBatchPostingReport(v1))
	if err != nil {
		t.Fatal(err)
	}
	b1, ok := got.(*BatchPostingReport)
	if !ok || b1.Version != 1 || b1.GasSpent() != 1000 || b1.WeiSpent().Int64() != 2000 || b1.BatchNumber != 9 || b1.Poster != v1.Poster {
		t.Fatalf("v1: %+v", b1)
	}
	if (&BatchPostingReport{CalldataLen: 1, CalldataNonZeros: 5}).GasSpent() != 80 {
		t.Fatal("non-zero count above length is clamped")
	}
	if (&BatchPostingReport{}).WeiSpent().Sign() != 0 {
		t.Fatal("nil base fee")
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
