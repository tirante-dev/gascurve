package nitro

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// targetFor describes a block of the fake chain the way a stored row does:
// the hash its receipts carry, the transaction count and the gas total, and
// no transaction hashes at all.
func targetFor(n uint64) ReceiptTarget {
	return ReceiptTarget{Number: n, Hash: "0xabc", TxCount: 2, GasUsed: 1_000_000 * n}
}

// posterGasPerBlock is the sum of gasUsedForL1 the fake chain reports.
const posterGasPerBlock = 0x1e9 + 0x116

func TestPosterGasByNumbers(t *testing.T) {
	f := newFakeRPC(t)
	setupChain(f)
	c, _ := newTestClient(t, f, 1000)

	gas, err := c.PosterGasByNumbers(context.Background(), []ReceiptTarget{targetFor(200), targetFor(201), targetFor(202)})
	if err != nil {
		t.Fatal(err)
	}
	if len(gas) != 3 {
		t.Fatalf("poster gas for %d blocks, want 3", len(gas))
	}
	for _, n := range []uint64{200, 201, 202} {
		if gas[n] != posterGasPerBlock {
			t.Fatalf("block %d poster gas %d, want %d", n, gas[n], posterGasPerBlock)
		}
	}
}

func TestPosterGasByNumbersWithoutTargets(t *testing.T) {
	f := newFakeRPC(t)
	setupChain(f)
	c, _ := newTestClient(t, f, 1000)

	gas, err := c.PosterGasByNumbers(context.Background(), nil)
	if err != nil || len(gas) != 0 {
		t.Fatalf("no targets: %v %v", gas, err)
	}
}

func TestPosterGasByNumbersRejectsAMismatchedBlock(t *testing.T) {
	f := newFakeRPC(t)
	setupChain(f)
	c, _ := newTestClient(t, f, 1000)
	ctx := context.Background()

	for _, tc := range []struct {
		name   string
		target ReceiptTarget
		want   string
	}{
		{"another block's hash", ReceiptTarget{Number: 200, Hash: "0xdifferent", TxCount: 2, GasUsed: 200_000_000}, "does not match"},
		{"the wrong transaction count", ReceiptTarget{Number: 200, Hash: "0xabc", TxCount: 3, GasUsed: 200_000_000}, "want 3"},
		{"a gas total the receipts do not add to", ReceiptTarget{Number: 200, Hash: "0xabc", TxCount: 2, GasUsed: 7}, "does not match header gas"},
		{"a block the endpoint does not have", targetFor(99), "receipts not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.PosterGasByNumbers(ctx, []ReceiptTarget{tc.target}); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want one mentioning %q", err, tc.want)
			}
		})
	}
}

// A stored row has no transaction hashes, so the repair leaves HashesKnown
// false and the receipts are tied to their block by hash, index and totals
// instead. A header read keeps the stricter check.
func TestParseReceiptsChecksTransactionHashesOnlyWhenTheyAreKnown(t *testing.T) {
	raw := json.RawMessage(`[
		{"blockHash":"0xabc","blockNumber":"0xc8","transactionHash":"0xaa","transactionIndex":"0x0","gasUsed":"0x64","cumulativeGasUsed":"0x64","gasUsedForL1":"0xa"},
		{"blockHash":"0xabc","blockNumber":"0xc8","transactionHash":"0xbb","transactionIndex":"0x1","gasUsed":"0x64","cumulativeGasUsed":"0xc8","gasUsedForL1":"0xa"}
	]`)
	base := ReceiptTarget{Number: 200, Hash: "0xabc", TxCount: 2, GasUsed: 200}

	gas, _, err := parseReceipts(raw, base)
	if err != nil || gas != 20 {
		t.Fatalf("without hashes: %d %v", gas, err)
	}
	known := base
	known.HashesKnown = true
	known.TxHashes = []string{"0xaa", "0xbb"}
	if gas, _, err = parseReceipts(raw, known); err != nil || gas != 20 {
		t.Fatalf("with matching hashes: %d %v", gas, err)
	}
	wrong := known
	wrong.TxHashes = []string{"0xaa", "0xzz"}
	if _, _, err = parseReceipts(raw, wrong); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("with a mismatched hash: %v", err)
	}
	short := known
	short.TxHashes = []string{"0xaa"}
	if _, _, err = parseReceipts(raw, short); err == nil || !strings.Contains(err.Error(), "no transaction hash") {
		t.Fatalf("with a missing hash: %v", err)
	}
}

func TestParseReceiptsRejectsPosterGasAboveTheBlockTotal(t *testing.T) {
	raw := json.RawMessage(`[
		{"blockHash":"0xabc","blockNumber":"0xc8","transactionHash":"0xaa","transactionIndex":"0x0","gasUsed":"0x64","cumulativeGasUsed":"0x64","gasUsedForL1":"0xc8"}
	]`)
	if _, _, err := parseReceipts(raw, ReceiptTarget{Number: 200, Hash: "0xabc", TxCount: 1, GasUsed: 100}); err == nil || !strings.Contains(err.Error(), "exceeds gas used") {
		t.Fatalf("error %v, want one about poster gas exceeding the transaction", err)
	}
}
