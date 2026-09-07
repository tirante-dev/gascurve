package nitro

import (
	"context"
	"encoding/json"
	"math/big"
	"testing"
	"time"

	"github.com/tirante-dev/gascurve/internal/config"
)

func wordsOf(vals ...uint64) []byte {
	out := make([]byte, 0, 32*len(vals))
	for _, v := range vals {
		out = append(out, encodeUint64(v)...)
	}
	return out
}

func addrWord(a string) []byte {
	b, _ := DecodeHex(a)
	return padWord(b)
}

const (
	deadAddr = "0x000000000000000000000000000000000000dead"
	beefAddr = "0x000000000000000000000000000000000000beef"
)

func setupChain(f *fakeRPC) {
	blocks := map[string]map[string]any{}
	for n := uint64(100); n <= 320; n++ {
		blocks[blockTag(n)] = blockJSON(n, 1_700_000_000+n/10, 1_000_000*n, "0x1312d00", []any{"0xaa", "0xbb"}, 50)
	}
	f.handlers["eth_blockNumber"] = func([]json.RawMessage) any { return blockTag(320) }
	f.handlers["eth_getBlockByNumber"] = func(params []json.RawMessage) any {
		tag := paramString(params[0])
		if tag == "latest" {
			tag = blockTag(320)
		}
		var full bool
		_ = json.Unmarshal(params[1], &full)
		b, ok := blocks[tag]
		if !ok {
			return nil
		}
		if full {
			b = blockJSON(320, 1_700_000_032, 5, "0x1312d00", []any{
				map[string]any{"hash": "0x01", "type": "0x6a", "from": ArbosAddress, "to": ArbosAddress, "input": EncodeHex(EncodeStartBlock(StartBlock{L1BaseFee: big.NewInt(0), L1BlockNumber: 7, L2BlockNumber: 320, TimePassed: 1}))},
				map[string]any{"hash": "0x02", "type": "0x2", "to": "0xdead", "input": "0x"},
			}, 50)
		}
		return b
	}
	f.handlers["eth_getLogs"] = func(params []json.RawMessage) any {
		var filter map[string]any
		_ = json.Unmarshal(params[0], &filter)
		if filter["address"] != ArbOwnerAddress {
			return []any{}
		}
		return []any{map[string]any{
			"address": ArbOwnerAddress,
			"topics": []string{
				OwnerActsTopic,
				"0xa0188cdb00000000000000000000000000000000000000000000000000000000",
				"0x0000000000000000000000002a153c6a1b66dbc930a8d7017230ab0253005c09",
			},
			"data": "0x00", "blockNumber": "0x10", "transactionHash": "0xtx", "transactionIndex": "0x2", "logIndex": "0x1", "blockTimestamp": "0x5",
		}}
	}
	f.handlers["eth_getTransactionReceipt"] = func(params []json.RawMessage) any {
		hash := paramString(params[0])
		if hash == "0xmissing" {
			return nil
		}
		return map[string]any{
			"transactionHash": hash, "blockNumber": "0x140", "transactionIndex": "0x1",
			"gasUsed": "0x3", "cumulativeGasUsed": "0x5",
		}
	}
	f.handlers["eth_getBalance"] = func(params []json.RawMessage) any {
		switch paramString(params[0]) {
		case deadAddr:
			return &RPCError{Code: -32000, Message: "nope"}
		case beefAddr:
			return 12
		}
		return "0x64"
	}
	f.setCall(SigGetGasPricingConstraints, EncodeSetGasPricingConstraints([]ConstraintParam{{60_000_000, 15, 3_111_506}, {40_000_000, 86_400, 11_194_391_810_886}})[4:])
	f.setCall(SigGetPricesInWei, wordsOf(1, 2, 3, 4, 5, 6))
	f.setCall(SigGetMinimumGasPrice, wordsOf(20_000_000))
	f.setCall(SigGetGasBacklog, wordsOf(0))
	f.setCall(SigGetPricingInertia, wordsOf(102))
	f.setCall(SigGetGasBacklogTolerance, wordsOf(10))
	f.setCall(SigGetGasAccountingParams, wordsOf(7_000_000, 32_000_000, 32_000_000))
	f.setCall(SigGetL1BaseFeeEstimate, wordsOf(2_369_608))
	neg := make([]byte, 32)
	for i := range neg {
		neg[i] = 0xff
	}
	f.setCall(SigGetL1PricingSurplus, neg)
	f.setCall(SigGetL1FeesAvailable, wordsOf(190_000_000_000_000))
	f.setCall(SigGetL1PricingUnitsSinceUpdate, wordsOf(1234))
	f.setCall(SigGetLastL1PricingUpdateTime, wordsOf(1_700_000_000))
	f.setCall(SigGetL1PricingEquilibrationUnit, wordsOf(160_000_000))
	f.setCall(SigGetPerBatchGasCharge, wordsOf(210_000))
	f.setCall(SigGetL1RewardRate, wordsOf(10))
	f.setCall(SigGetL1RewardRecipient, addrWord("0x8f516b9921021522d74d08b94b9f1c22b4af82ea"))
	f.setCall(SigGetInfraFeeAccount, addrWord("0x5a2b80a9b7effc06129bd5462d77bc20a8a59be7"))
	f.setCall(SigGetNetworkFeeAccount, addrWord("0xbc5c3a7adecf54d34169fd90dbd1b7d3142df067"))
	f.setCall(SigArbOSVersion, wordsOf(116))
}

func TestTypedCalls(t *testing.T) {
	f := newFakeRPC(t)
	setupChain(f)
	c, _ := newTestClient(t, f, 1000)
	ctx := context.Background()

	if n, err := c.BlockNumber(ctx); err != nil || n != 320 {
		t.Fatalf("BlockNumber: %d %v", n, err)
	}
	h, err := c.HeaderByNumber(ctx, 100)
	if err != nil || h.Number != 100 || h.GasUsed != 100_000_000 || h.BaseFee.Int64() != 20_000_000 || h.L1BlockNumber != 50 || h.TxCount != 2 || h.TxHashes[1] != "0xbb" || h.Timestamp != 1_700_000_010 {
		t.Fatalf("HeaderByNumber: %+v %v", h, err)
	}
	if h.Hash != "0xabc" || h.ParentHash != "0xparent" {
		t.Fatalf("hashes: %+v", h)
	}
	if _, err := c.HeaderByNumber(ctx, 5); err == nil {
		t.Fatal("missing block should error")
	}
	nums := make([]uint64, 0, 150)
	for n := uint64(100); n < 250; n++ {
		nums = append(nums, n)
	}
	before := f.requests
	hs, err := c.HeadersByNumbers(ctx, nums)
	if err != nil || len(hs) != 150 || hs[149].Number != 249 {
		t.Fatalf("HeadersByNumbers: %d %v", len(hs), err)
	}
	if f.requests-before != 2 {
		t.Fatalf("expected 2 batches, got %d", f.requests-before)
	}
	if _, err := c.HeadersByNumbers(ctx, []uint64{100, 5}); err == nil {
		t.Fatal("missing block in batch should error")
	}
	b, err := c.BlockWithTxs(ctx, 320)
	if err != nil || len(b.Txs) != 2 || !IsInternalTx(b.Txs[0]) || IsInternalTx(b.Txs[1]) || b.TxHashes[0] != "0x01" {
		t.Fatalf("BlockWithTxs: %+v %v", b, err)
	}
	if _, err := c.BlocksWithTxs(ctx, []uint64{5}); err == nil {
		t.Fatal("missing full block should error")
	}
	receipts, err := c.TransactionReceipts(ctx, []string{"0x01", "0x02"})
	if err != nil || len(receipts) != 2 || receipts[1].TxHash != "0x02" || receipts[1].BlockNumber != 320 || receipts[1].TxIndex != 1 || receipts[1].GasUsed != 3 || receipts[1].CumulativeGasUsed != 5 {
		t.Fatalf("TransactionReceipts: %+v %v", receipts, err)
	}
	if _, err := c.TransactionReceipts(ctx, []string{"0xmissing"}); err == nil {
		t.Fatal("missing receipt should error")
	}
	logs, err := c.OwnerActsLogs(ctx, 0, 100)
	if err != nil || len(logs) != 1 || logs[0].BlockNumber != 16 || logs[0].TxIndex != 2 || logs[0].LogIndex != 1 || logs[0].BlockTimestamp != 5 {
		t.Fatalf("Logs: %+v %v", logs, err)
	}
	if logs, err := c.Logs(ctx, 0, 1, "0x1", nil); err != nil || len(logs) != 0 {
		t.Fatalf("empty logs: %v %v", logs, err)
	}
	bal, err := c.Balance(ctx, "0x1")
	if err != nil || bal.Int64() != 100 {
		t.Fatalf("Balance: %s %v", bal, err)
	}
	if _, err := c.Balance(ctx, deadAddr); err == nil {
		t.Fatal("balance error")
	}
	if _, err := c.Balance(ctx, beefAddr); err == nil {
		t.Fatal("balance decode error")
	}
	if v, err := c.ArbOSVersion(ctx); err != nil || v != 61 {
		t.Fatalf("ArbOSVersion: %d %v", v, err)
	}
	s := Selector(SigArbOSVersion)
	if data, err := c.CallContract(ctx, ArbSysAddress, s[:]); err != nil || len(data) != 32 {
		t.Fatalf("CallContract: %x %v", data, err)
	}
	if _, err := c.CallContract(ctx, ArbSysAddress, []byte{1, 2, 3, 4}); err == nil {
		t.Fatal("unknown selector should revert")
	}
}

func TestFastSample(t *testing.T) {
	f := newFakeRPC(t)
	setupChain(f)
	c, _ := newTestClient(t, f, 1000)
	ctx := context.Background()

	s, err := c.FastSample(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.Header.Number != 320 || len(s.Constraints) != 2 || s.Constraints[1].Backlog != 11_194_391_810_886 || s.IsLegacy() {
		t.Fatalf("sample: %+v", s)
	}
	if s.Prices.PerArbGasTotal.Int64() != 6 || s.Prices.PerL1CalldataByte.Int64() != 2 || s.MinBaseFee.Int64() != 20_000_000 || s.Legacy != nil {
		t.Fatalf("sample prices: %+v", s)
	}
	if s.SampledAt.IsZero() {
		t.Fatal("sampledAt")
	}

	// Empty constraints switch to the legacy getters.
	f.setCall(SigGetGasPricingConstraints, EncodeSetGasPricingConstraints(nil)[4:])
	s, err = c.FastSample(ctx)
	if err != nil || !s.IsLegacy() || s.Legacy == nil || s.Legacy.SpeedLimit != 7_000_000 || s.Legacy.Inertia != 102 || s.Legacy.Tolerance != 10 {
		t.Fatalf("legacy sample: %+v %v", s, err)
	}
	// A revert does too.
	f.setCallErr(SigGetGasPricingConstraints, &RPCError{Code: 3, Message: "execution reverted"})
	if s, err = c.FastSample(ctx); err != nil || !s.IsLegacy() {
		t.Fatalf("revert sample: %+v %v", s, err)
	}
	// Other errors on the constraints call propagate.
	f.setCallErr(SigGetGasPricingConstraints, &RPCError{Code: -32601, Message: "method missing"})
	if _, err = c.FastSample(ctx); err == nil {
		t.Fatal("expected constraints error")
	}
	delete(f.callErrs, SelectorHex(SigGetGasPricingConstraints))
	// Bad constraint data.
	f.setCall(SigGetGasPricingConstraints, []byte{1})
	if _, err = c.FastSample(ctx); err == nil {
		t.Fatal("expected decode error")
	}
	f.setCall(SigGetGasPricingConstraints, EncodeSetGasPricingConstraints(nil)[4:])
	// Legacy getter failures.
	for _, sig := range []string{SigGetGasBacklog, SigGetPricingInertia, SigGetGasBacklogTolerance, SigGetGasAccountingParams} {
		f.setCallErr(sig, &RPCError{Code: 1, Message: "x"})
		if _, err = c.FastSample(ctx); err == nil {
			t.Fatalf("%s error should propagate", sig)
		}
		delete(f.callErrs, SelectorHex(sig))
		f.setCall(sig, []byte{1})
		if _, err = c.FastSample(ctx); err == nil {
			t.Fatalf("%s decode error should propagate", sig)
		}
	}
	f.setCall(SigGetGasBacklog, wordsOf(0))
	f.setCall(SigGetPricingInertia, wordsOf(102))
	f.setCall(SigGetGasBacklogTolerance, wordsOf(10))
	f.setCall(SigGetGasAccountingParams, wordsOf(7_000_000, 32_000_000, 32_000_000))
	// Prices and min fee failures.
	f.setCallErr(SigGetPricesInWei, &RPCError{Code: 1, Message: "x"})
	if _, err = c.FastSample(ctx); err == nil {
		t.Fatal("prices error")
	}
	delete(f.callErrs, SelectorHex(SigGetPricesInWei))
	f.setCall(SigGetPricesInWei, []byte{1})
	if _, err = c.FastSample(ctx); err == nil {
		t.Fatal("prices decode error")
	}
	f.setCall(SigGetPricesInWei, wordsOf(1, 2, 3, 4, 5, 6))
	f.setCallErr(SigGetMinimumGasPrice, &RPCError{Code: 1, Message: "x"})
	if _, err = c.FastSample(ctx); err == nil {
		t.Fatal("min fee error")
	}
	delete(f.callErrs, SelectorHex(SigGetMinimumGasPrice))
	f.setCall(SigGetMinimumGasPrice, []byte{1})
	if _, err = c.FastSample(ctx); err == nil {
		t.Fatal("min fee decode error")
	}
	f.setCall(SigGetMinimumGasPrice, wordsOf(1))
	// Latest block failures.
	f.handlers["eth_getBlockByNumber"] = func([]json.RawMessage) any { return &RPCError{Code: 1, Message: "x"} }
	if _, err = c.FastSample(ctx); err == nil {
		t.Fatal("block error")
	}
	f.handlers["eth_getBlockByNumber"] = func([]json.RawMessage) any { return map[string]any{"number": "zz"} }
	if _, err = c.FastSample(ctx); err == nil {
		t.Fatal("block decode error")
	}
	f.server.Close()
	if _, err = c.FastSample(ctx); err == nil {
		t.Fatal("transport error")
	}
	if _, err = c.LegacyParams(ctx); err == nil {
		t.Fatal("transport error")
	}
}

func TestFastSampleAt(t *testing.T) {
	f := newFakeRPC(t)
	setupChain(f)
	c, _ := newTestClient(t, f, 1000)
	ctx := context.Background()

	s, err := c.FastSampleAt(ctx, 200)
	if err != nil {
		t.Fatal(err)
	}
	if s.Header.Number != 200 || len(s.Constraints) != 2 || s.MinBaseFee.Int64() != 20_000_000 {
		t.Fatalf("sample at 200: %+v", s)
	}
	// Every state call in the batch carries the head's block tag.
	if len(f.callTags) != 3 {
		t.Fatalf("call tags = %v", f.callTags)
	}
	for _, tag := range f.callTags {
		if tag != "0xc8" {
			t.Fatalf("state call not pinned to the block: %v", f.callTags)
		}
	}
	// Legacy getters are pinned too.
	f.callTags = nil
	f.setCall(SigGetGasPricingConstraints, EncodeSetGasPricingConstraints(nil)[4:])
	if s, err = c.FastSampleAt(ctx, 150); err != nil || !s.IsLegacy() || s.Header.Number != 150 {
		t.Fatalf("legacy sample at 150: %+v %v", s, err)
	}
	if len(f.callTags) != 7 || f.callTags[6] != "0x96" {
		t.Fatalf("legacy call tags = %v", f.callTags)
	}
	// A block the node does not have is an error, not a silent "latest".
	if _, err := c.FastSampleAt(ctx, 5); err == nil {
		t.Fatal("missing block should error")
	}
	// The plain FastSample resolves the head first and pins every call to
	// it, so a batch can never mix two "latest" heights.
	f.callTags = nil
	s, err = c.FastSample(ctx)
	if err != nil || s.Header.Number != 320 {
		t.Fatalf("FastSample: %+v %v", s, err)
	}
	for _, tag := range f.callTags {
		if tag != blockTag(320) {
			t.Fatalf("FastSample must pin state calls to the resolved head: %v", f.callTags)
		}
	}
	f.handlers["eth_blockNumber"] = func([]json.RawMessage) any { return &RPCError{Code: 1, Message: "x"} }
	if _, err := c.FastSample(ctx); err == nil {
		t.Fatal("head resolution error should propagate")
	}
}

func TestL1SampleAndFeeAccounts(t *testing.T) {
	f := newFakeRPC(t)
	setupChain(f)
	c, _ := newTestClient(t, f, 1000)
	ctx := context.Background()

	l1, err := c.L1Sample(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if l1.BaseFeeEstimate.Int64() != 2_369_608 || l1.Surplus.Int64() != -1 || l1.FeesAvailable.Int64() != 190_000_000_000_000 ||
		l1.UnitsSinceUpdate != 1234 || l1.LastUpdateTime != 1_700_000_000 || l1.EquilibrationUnits != 160_000_000 || l1.PerBatchGasCharge != 210_000 || l1.RewardRate != 10 {
		t.Fatalf("l1: %+v", l1)
	}
	acc, err := c.FeeAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if acc.Infra.Address != "0x5a2b80a9b7effc06129bd5462d77bc20a8a59be7" || acc.Network.Balance.Int64() != 100 || acc.L1Reward.Address != "0x8f516b9921021522d74d08b94b9f1c22b4af82ea" {
		t.Fatalf("accounts: %+v", acc)
	}

	sigs := []string{
		SigGetL1BaseFeeEstimate, SigGetL1PricingSurplus, SigGetL1FeesAvailable, SigGetL1PricingUnitsSinceUpdate,
		SigGetLastL1PricingUpdateTime, SigGetL1PricingEquilibrationUnit, SigGetPerBatchGasCharge, SigGetL1RewardRate,
	}
	for _, sig := range sigs {
		f.setCallErr(sig, &RPCError{Code: 1, Message: "x"})
		if _, err := c.L1Sample(ctx); err == nil {
			t.Fatalf("%s error should propagate", sig)
		}
		delete(f.callErrs, SelectorHex(sig))
		old := f.calls[SelectorHex(sig)]
		f.setCall(sig, []byte{1})
		if _, err := c.L1Sample(ctx); err == nil {
			t.Fatalf("%s decode error should propagate", sig)
		}
		f.setCall(sig, old)
	}
	for _, sig := range []string{SigGetInfraFeeAccount, SigGetNetworkFeeAccount, SigGetL1RewardRecipient} {
		f.setCallErr(sig, &RPCError{Code: 1, Message: "x"})
		if _, err := c.FeeAccounts(ctx); err == nil {
			t.Fatalf("%s error should propagate", sig)
		}
		delete(f.callErrs, SelectorHex(sig))
		old := f.calls[SelectorHex(sig)]
		f.setCall(sig, []byte{1})
		if _, err := c.FeeAccounts(ctx); err == nil {
			t.Fatalf("%s decode error should propagate", sig)
		}
		f.setCall(sig, old)
	}
	f.setCall(SigGetInfraFeeAccount, addrWord(deadAddr))
	if _, err := c.FeeAccounts(ctx); err == nil {
		t.Fatal("balance error should propagate")
	}
	f.setCall(SigGetInfraFeeAccount, addrWord(beefAddr))
	if _, err := c.FeeAccounts(ctx); err == nil {
		t.Fatal("balance decode error should propagate")
	}
	f.handlers["eth_getBalance"] = func([]json.RawMessage) any { return "0xzz" }
	f.setCall(SigGetInfraFeeAccount, addrWord("0x5a2b80a9b7effc06129bd5462d77bc20a8a59be7"))
	if _, err := c.FeeAccounts(ctx); err == nil {
		t.Fatal("balance hex error should propagate")
	}
	f.server.Close()
	if _, err := c.L1Sample(ctx); err == nil {
		t.Fatal("transport")
	}
	if _, err := c.FeeAccounts(ctx); err == nil {
		t.Fatal("transport")
	}
	if _, err := c.BlockNumber(ctx); err == nil {
		t.Fatal("transport")
	}
	if _, err := c.Logs(ctx, 0, 1, "0x1", nil); err == nil {
		t.Fatal("transport")
	}
	if _, err := c.HeadersByNumbers(ctx, []uint64{1}); err == nil {
		t.Fatal("transport")
	}
	if _, err := c.BlocksWithTxs(ctx, []uint64{1}); err == nil {
		t.Fatal("transport")
	}
	if _, err := c.TransactionReceipts(ctx, []string{"0x1"}); err == nil {
		t.Fatal("transport")
	}
	if _, err := c.ArbOSVersion(ctx); err == nil {
		t.Fatal("transport")
	}
}

func TestArbOSVersionErrors(t *testing.T) {
	f := newFakeRPC(t)
	c, _ := newTestClient(t, f, 1000)
	f.setCall(SigArbOSVersion, wordsOf(3))
	if _, err := c.ArbOSVersion(context.Background()); err == nil {
		t.Fatal("below offset")
	}
	f.setCall(SigArbOSVersion, []byte{1})
	if _, err := c.ArbOSVersion(context.Background()); err == nil {
		t.Fatal("decode")
	}
	f.handlers["eth_blockNumber"] = func([]json.RawMessage) any { return 12 }
	if _, err := c.BlockNumber(context.Background()); err == nil {
		t.Fatal("decode block number")
	}
	f.handlers["eth_call"] = func([]json.RawMessage) any { return 12 }
	if _, err := c.ArbOSVersion(context.Background()); err == nil {
		t.Fatal("decode call result")
	}
	f.handlers["eth_getLogs"] = func([]json.RawMessage) any { return "x" }
	if _, err := c.Logs(context.Background(), 0, 1, "0x1", nil); err == nil {
		t.Fatal("decode logs")
	}
}

func TestParseHeaderErrors(t *testing.T) {
	cases := []string{
		`null`,
		`"str"`,
		`{"number":"zz"}`,
		`{"number":"0x1","timestamp":"zz"}`,
		`{"number":"0x1","timestamp":"0x1","gasUsed":"zz"}`,
		`{"number":"0x1","timestamp":"0x1","gasUsed":"0x1","gasLimit":"zz"}`,
		`{"number":"0x1","timestamp":"0x1","gasUsed":"0x1","baseFeePerGas":"zz"}`,
		`{"number":"0x1","timestamp":"0x1","gasUsed":"0x1","l1BlockNumber":"zz"}`,
		`{"number":"0x1","timestamp":"0x1","gasUsed":"0x1","transactions":[1]}`,
		`{"number":"0x1","timestamp":"0x1","gasUsed":"0x1","transactions":["]}`,
		`{"number":"0x1","timestamp":"0x1","gasUsed":"0x1","transactions":[{"type":"zz"}]}`,
		`{"number":"0x1","timestamp":"0x1","gasUsed":"0x1","transactions":[{"input":"0xzz"}]}`,
	}
	for _, c := range cases {
		if _, err := parseHeader(json.RawMessage(c)); err == nil {
			t.Errorf("expected error for %s", c)
		}
	}
	b, err := parseHeader(json.RawMessage(`{"number":"0x1","timestamp":"0x1","gasUsed":"0x1"}`))
	if err != nil || b.BaseFee.Sign() != 0 || b.TxCount != 0 {
		t.Fatalf("minimal header: %+v %v", b, err)
	}
	for _, c := range []string{
		`[{"data":"0xzz"}]`,
		`[{"data":"0x","blockNumber":"zz"}]`,
		`[{"data":"0x","blockNumber":"0x1","logIndex":"zz"}]`,
		`[{"data":"0x","blockNumber":"0x1","logIndex":"0x1","transactionIndex":"zz"}]`,
		`[{"data":"0x","blockNumber":"0x1","logIndex":"0x1"}]`,
		`[{"data":"0x","blockNumber":"0x1","logIndex":"0x1","transactionIndex":"0x1","blockTimestamp":"zz"}]`,
	} {
		if _, err := parseLogs(json.RawMessage(c)); err == nil {
			t.Errorf("expected log error for %s", c)
		}
	}
	for _, c := range []string{
		`null`, `"str"`,
		`{"blockNumber":"zz"}`,
		`{"blockNumber":"0x1","transactionIndex":"zz"}`,
		`{"blockNumber":"0x1","transactionIndex":"0x1","gasUsed":"zz"}`,
		`{"blockNumber":"0x1","transactionIndex":"0x1","gasUsed":"0x1","cumulativeGasUsed":"zz"}`,
	} {
		if _, err := parseReceipt(json.RawMessage(c)); err == nil {
			t.Errorf("expected receipt error for %s", c)
		}
	}
}

// TestTypedBatchesAreChunked: every typed request list goes through the
// endpoint's chunker, not straight to Batch, so an eight-call L1 sample on
// a four calls per second budget is split into what the bulk share holds
// rather than overdrawing the bucket and eating the fast reserve.
func TestTypedBatchesAreChunked(t *testing.T) {
	ctx := context.Background()
	f := newFakeRPC(t)
	setupChain(f)
	f.handlers["eth_chainId"] = func([]json.RawMessage) any { return testChainIDHex }
	// A request carrying more than the burst is refused, the way an
	// endpoint refuses an oversized batch.
	f.maxItems = 8
	clock := newFakeClock()
	p := NewPool(PoolConfig{ChainID: testChainID, Endpoints: []config.EndpointConfig{endpointOf(f, 4)}, BatchSize: 100},
		WithPoolClientOptions(WithHTTPClient(f.server.Client())), withPoolClock(clock.Now, clock.Sleep))
	e := p.Endpoints()[0]
	pacer := e.Pacer()
	if pacer.MaxBatch() != 7 || pacer.MaxBatchFor(Fast) != 8 {
		t.Fatalf("bulk share %d, fast share %d", pacer.MaxBatch(), pacer.MaxBatchFor(Fast))
	}
	if err := p.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Hour)
	before := f.requestCount()
	if _, err := p.L1Sample(ctx); err != nil {
		t.Fatal(err)
	}
	// Eight getters, seven of which the bulk share holds: two requests, no
	// throttling, and the fast reserve untouched.
	if n := f.requestCount() - before; n != 2 {
		t.Fatalf("the eight L1 getters must be split: %d requests", n)
	}
	if p.Stats().RateLimitEvents != 0 {
		t.Fatalf("chunked batches must not be refused: %+v", p.Stats())
	}
	if tokens, reserve := pacer.levels(); tokens < 0 || reserve < 0 {
		t.Fatalf("the bucket must never go negative: tokens %v reserve %v", tokens, reserve)
	}
	// The fee accounts (three addresses then three balances) and the legacy
	// parameters fit and stay one request each.
	clock.Advance(time.Hour)
	before = f.requestCount()
	if _, err := p.FeeAccounts(ctx); err != nil {
		t.Fatal(err)
	}
	if n := f.requestCount() - before; n != 2 {
		t.Fatalf("fee accounts: %d requests", n)
	}
	// A fast sample may carry the reserve more than a bulk batch, so its
	// four calls still go in one request under bulk pressure.
	clock.Advance(time.Hour)
	before = f.requestCount()
	if _, err := p.FastSampleAt(WithClass(ctx, Fast), 200); err != nil {
		t.Fatal(err)
	}
	if n := f.requestCount() - before; n != 1 {
		t.Fatalf("a fast sample fits in one request: %d", n)
	}
	if tokens, _ := pacer.levels(); tokens < 0 {
		t.Fatalf("fast sampling must not overdraw: %v", tokens)
	}
}
