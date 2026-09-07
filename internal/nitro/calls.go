package nitro

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"time"
)

// Sample is everything the fast tick reads in one batch.
type Sample struct {
	SampledAt   time.Time
	Header      Header
	Constraints []Constraint
	Prices      Prices
	MinBaseFee  *big.Int
	Legacy      *LegacyParams
}

// IsLegacy reports whether the chain runs the legacy pricer.
func (s *Sample) IsLegacy() bool { return len(s.Constraints) == 0 }

func callBytes(r Result) ([]byte, error) {
	if r.Err != nil {
		return nil, r.Err
	}
	var s string
	if err := json.Unmarshal(r.Raw, &s); err != nil {
		return nil, fmt.Errorf("decode call result: %w", err)
	}
	return DecodeHex(s)
}

// BlockNumber returns the latest block number.
func (c *Client) BlockNumber(ctx context.Context) (uint64, error) {
	return c.quantity(ctx, "eth_blockNumber")
}

// ChainID returns the chain id the node reports (eth_chainId).
func (c *Client) ChainID(ctx context.Context) (uint64, error) {
	return c.quantity(ctx, "eth_chainId")
}

func (c *Client) quantity(ctx context.Context, method string) (uint64, error) {
	raw, err := c.Call(ctx, method)
	if err != nil {
		return 0, err
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, fmt.Errorf("decode %s: %w", method, err)
	}
	return HexUint64(s)
}

func blockTag(number uint64) string {
	return fmt.Sprintf("0x%x", number)
}

// HeaderByNumber fetches one header (transaction hashes only).
func (c *Client) HeaderByNumber(ctx context.Context, number uint64) (*Header, error) {
	raw, err := c.Call(ctx, methodGetBlockByNumber, blockTag(number), false)
	if err != nil {
		return nil, err
	}
	b, err := parseHeader(raw)
	if err != nil {
		return nil, err
	}
	return &b.Header, nil
}

// HeadersByNumbers fetches headers in batches of at most MaxBatch, in order.
func (c *Client) HeadersByNumbers(ctx context.Context, numbers []uint64) ([]Header, error) {
	blocks, err := blocksByNumbers(ctx, numbers, false, c.chunk)
	if err != nil {
		return nil, err
	}
	out := make([]Header, len(blocks))
	for i := range blocks {
		out[i] = blocks[i].Header
	}
	return out, nil
}

// BlockWithTxs fetches a block including full transactions.
func (c *Client) BlockWithTxs(ctx context.Context, number uint64) (*Block, error) {
	blocks, err := c.BlocksWithTxs(ctx, []uint64{number})
	if err != nil {
		return nil, err
	}
	return &blocks[0], nil
}

// BlocksWithTxs fetches several blocks with full transactions in one batch
// per MaxBatch items.
func (c *Client) BlocksWithTxs(ctx context.Context, numbers []uint64) ([]Block, error) {
	return blocksByNumbers(ctx, numbers, true, c.chunk)
}

// TransactionReceipts fetches receipts in batches of at most MaxBatch, in
// the same order as hashes.
func (c *Client) TransactionReceipts(ctx context.Context, hashes []string) ([]Receipt, error) {
	return transactionReceipts(ctx, hashes, c.chunk)
}

// batcher sends a request list of any length, splitting it into HTTP
// batches as it sees fit, and returns one Result per request in order.
type batcher func(ctx context.Context, reqs []Request) ([]Result, error)

// batchCapped is the Client's batcher: batches of at most what the pacer
// can hold at once for the calling class, so a typed request list longer
// than the budget is split instead of overdrawing the bucket.
func (c *Client) batchCapped(ctx context.Context, reqs []Request) ([]Result, error) {
	out := make([]Result, 0, len(reqs))
	class := ClassOf(ctx)
	for start := 0; start < len(reqs); {
		size := chunkSize(c.pacer.MaxBatchFor(class), len(reqs)-start)
		results, err := c.Batch(ctx, reqs[start:start+size])
		if err != nil {
			return nil, err
		}
		out = append(out, results...)
		start += size
	}
	return out, nil
}

// blocksByNumbers fetches eth_getBlockByNumber for every number through
// send, with or without full transactions, and parses the results.
func blocksByNumbers(ctx context.Context, numbers []uint64, full bool, send batcher) ([]Block, error) {
	reqs := make([]Request, len(numbers))
	for i, n := range numbers {
		reqs[i] = Request{Method: methodGetBlockByNumber, Params: []any{blockTag(n), full}}
	}
	results, err := send(ctx, reqs)
	if err != nil {
		return nil, err
	}
	out := make([]Block, 0, len(numbers))
	for i, r := range results {
		if r.Err != nil {
			return nil, fmt.Errorf("block %d: %w", numbers[i], r.Err)
		}
		b, err := parseHeader(r.Raw)
		if err != nil {
			return nil, fmt.Errorf("block %d: %w", numbers[i], err)
		}
		out = append(out, *b)
	}
	return out, nil
}

func transactionReceipts(ctx context.Context, hashes []string, send batcher) ([]Receipt, error) {
	reqs := make([]Request, len(hashes))
	for i, hash := range hashes {
		reqs[i] = Request{Method: "eth_getTransactionReceipt", Params: []any{hash}}
	}
	results, err := send(ctx, reqs)
	if err != nil {
		return nil, err
	}
	if len(results) != len(hashes) {
		return nil, fmt.Errorf("receipts: got %d results for %d transactions", len(results), len(hashes))
	}
	out := make([]Receipt, 0, len(hashes))
	for i, result := range results {
		if result.Err != nil {
			return nil, fmt.Errorf("receipt %s: %w", hashes[i], result.Err)
		}
		receipt, err := parseReceipt(result.Raw)
		if err != nil {
			return nil, fmt.Errorf("receipt %s: %w", hashes[i], err)
		}
		out = append(out, *receipt)
	}
	return out, nil
}

// Logs runs eth_getLogs for an address and topics over [from, to].
func (c *Client) Logs(ctx context.Context, from, to uint64, address string, topics []string) ([]Log, error) {
	filter := map[string]any{
		"fromBlock": blockTag(from),
		"toBlock":   blockTag(to),
		"address":   address,
	}
	if len(topics) > 0 {
		filter["topics"] = topics
	}
	raw, err := c.Call(ctx, "eth_getLogs", filter)
	if err != nil {
		return nil, err
	}
	return parseLogs(raw)
}

// Balance returns an account balance at the latest block.
func (c *Client) Balance(ctx context.Context, address string) (*big.Int, error) {
	raw, err := c.Call(ctx, "eth_getBalance", address, latestTag)
	if err != nil {
		return nil, err
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("decode eth_getBalance: %w", err)
	}
	return HexBig(s)
}

// Call performs eth_call against `to` with calldata at the latest block and
// returns the raw return data.
func (c *Client) CallContract(ctx context.Context, to string, data []byte) ([]byte, error) {
	r := CallRequest(to, data)
	results, err := c.chunk(ctx, []Request{r})
	if err != nil {
		return nil, err
	}
	return callBytes(results[0])
}

// ArbOSVersion returns the ArbOS version (ArbSys.arbOSVersion() minus 55).
func (c *Client) ArbOSVersion(ctx context.Context) (uint64, error) {
	s := Selector(SigArbOSVersion)
	data, err := c.CallContract(ctx, ArbSysAddress, s[:])
	if err != nil {
		return 0, err
	}
	return decodeArbOSVersion(data)
}

func decodeArbOSVersion(data []byte) (uint64, error) {
	v, err := DecodeUint64(data)
	if err != nil {
		return 0, err
	}
	if v < ArbOSVersionOffset {
		return 0, fmt.Errorf("arbOSVersion %d below offset", v)
	}
	return v - ArbOSVersionOffset, nil
}

// FastSample performs the fast tick: it resolves the head number first
// (eth_blockNumber) and then samples header, constraints, prices and
// minimum base fee pinned to that block, so a batch whose items execute
// at different "latest" heights can never mix two blocks. When the
// constraints call reverts or returns an empty list a second batch reads
// the legacy pricer parameters at the same block.
func (c *Client) FastSample(ctx context.Context) (*Sample, error) {
	head, err := c.BlockNumber(ctx)
	if err != nil {
		return nil, err
	}
	return c.sampleAt(ctx, blockTag(head))
}

// FastSampleAt is FastSample with every call pinned to one block number, so
// the header and the state belong to the same block. Used when following
// newHeads over WebSocket and by the archive backfill anchors.
func (c *Client) FastSampleAt(ctx context.Context, number uint64) (*Sample, error) {
	return c.sampleAt(ctx, blockTag(number))
}

func (c *Client) sampleAt(ctx context.Context, tag string) (*Sample, error) {
	results, err := c.chunk(ctx, []Request{
		{Method: methodGetBlockByNumber, Params: []any{tag, false}},
		SelectorCallAt(ArbGasInfoAddress, SigGetGasPricingConstraints, tag),
		SelectorCallAt(ArbGasInfoAddress, SigGetPricesInWei, tag),
		SelectorCallAt(ArbGasInfoAddress, SigGetMinimumGasPrice, tag),
	})
	if err != nil {
		return nil, err
	}
	s := &Sample{SampledAt: c.now()}
	if results[0].Err != nil {
		return nil, fmt.Errorf("block %s: %w", tag, results[0].Err)
	}
	b, err := parseHeader(results[0].Raw)
	if err != nil {
		return nil, err
	}
	s.Header = b.Header

	if results[1].Err == nil {
		data, err := callBytes(results[1])
		if err != nil {
			return nil, err
		}
		if s.Constraints, err = DecodeConstraints(data); err != nil {
			return nil, err
		}
	} else if !IsRevert(results[1].Err) {
		return nil, fmt.Errorf("getGasPricingConstraints: %w", results[1].Err)
	}

	data, err := callBytes(results[2])
	if err != nil {
		return nil, fmt.Errorf("getPricesInWei: %w", err)
	}
	prices, err := DecodePrices(data)
	if err != nil {
		return nil, err
	}
	s.Prices = *prices

	data, err = callBytes(results[3])
	if err != nil {
		return nil, fmt.Errorf("getMinimumGasPrice: %w", err)
	}
	if s.MinBaseFee, err = DecodeUint256(data); err != nil {
		return nil, fmt.Errorf("getMinimumGasPrice: %w", err)
	}

	if len(s.Constraints) == 0 {
		legacy, err := c.legacyParamsAt(ctx, tag)
		if err != nil {
			return nil, err
		}
		s.Legacy = legacy
	}
	return s, nil
}

// LegacyParams reads the legacy pricer parameters in one batch at the
// latest block.
func (c *Client) LegacyParams(ctx context.Context) (*LegacyParams, error) {
	return c.legacyParamsAt(ctx, latestTag)
}

func (c *Client) legacyParamsAt(ctx context.Context, tag string) (*LegacyParams, error) {
	results, err := c.chunk(ctx, []Request{
		SelectorCallAt(ArbGasInfoAddress, SigGetGasBacklog, tag),
		SelectorCallAt(ArbGasInfoAddress, SigGetPricingInertia, tag),
		SelectorCallAt(ArbGasInfoAddress, SigGetGasBacklogTolerance, tag),
		SelectorCallAt(ArbGasInfoAddress, SigGetGasAccountingParams, tag),
	})
	if err != nil {
		return nil, err
	}
	names := []string{SigGetGasBacklog, SigGetPricingInertia, SigGetGasBacklogTolerance, SigGetGasAccountingParams}
	datas := make([][]byte, len(results))
	for i, r := range results {
		if datas[i], err = callBytes(r); err != nil {
			return nil, fmt.Errorf("%s: %w", names[i], err)
		}
	}
	lp := &LegacyParams{}
	if lp.Backlog, err = DecodeUint64(datas[0]); err != nil {
		return nil, fmt.Errorf("%s: %w", names[0], err)
	}
	if lp.Inertia, err = DecodeUint64(datas[1]); err != nil {
		return nil, fmt.Errorf("%s: %w", names[1], err)
	}
	if lp.Tolerance, err = DecodeUint64(datas[2]); err != nil {
		return nil, fmt.Errorf("%s: %w", names[2], err)
	}
	if lp.SpeedLimit, _, _, err = DecodeGasAccountingParams(datas[3]); err != nil {
		return nil, err
	}
	return lp, nil
}

// L1Sample reads the L1 pricer getters at the latest block.
func (c *Client) L1Sample(ctx context.Context) (*L1Sample, error) {
	return c.l1SampleAt(ctx, latestTag)
}

// L1SampleAt reads the L1 pricer getters and batch-cost parameters at one
// block. The historical parameter snapshot is used to anchor report-cost
// reconstruction without making one state call per report.
func (c *Client) L1SampleAt(ctx context.Context, number uint64) (*L1Sample, error) {
	return c.l1SampleAt(ctx, blockTag(number))
}

func (c *Client) l1SampleAt(ctx context.Context, tag string) (*L1Sample, error) {
	calls := []struct {
		address string
		sig     string
	}{
		{ArbGasInfoAddress, SigGetL1BaseFeeEstimate},
		{ArbGasInfoAddress, SigGetL1PricingSurplus},
		{ArbGasInfoAddress, SigGetL1FeesAvailable},
		{ArbGasInfoAddress, SigGetL1PricingUnitsSinceUpdate},
		{ArbGasInfoAddress, SigGetLastL1PricingUpdateTime},
		{ArbGasInfoAddress, SigGetL1PricingEquilibrationUnit},
		{ArbGasInfoAddress, SigGetPerBatchGasCharge},
		{ArbGasInfoAddress, SigGetL1RewardRate},
		{ArbSysAddress, SigArbOSVersion},
		{ArbOwnerPublicAddress, SigGetParentGasFloorPerToken},
	}
	reqs := make([]Request, len(calls))
	for i, call := range calls {
		reqs[i] = SelectorCallAt(call.address, call.sig, tag)
	}
	results, err := c.chunk(ctx, reqs)
	if err != nil {
		return nil, err
	}
	datas := make([][]byte, len(results))
	// The parent floor getter is unavailable before ArbOS 50. Decode every
	// earlier result first, including the version that controls that gate.
	for i, r := range results[:len(results)-1] {
		if datas[i], err = callBytes(r); err != nil {
			return nil, fmt.Errorf("%s: %w", calls[i].sig, err)
		}
	}
	l := &L1Sample{}
	if l.BaseFeeEstimate, err = DecodeUint256(datas[0]); err != nil {
		return nil, fmt.Errorf("%s: %w", calls[0].sig, err)
	}
	if l.Surplus, err = DecodeInt256(datas[1]); err != nil {
		return nil, fmt.Errorf("%s: %w", calls[1].sig, err)
	}
	if l.FeesAvailable, err = DecodeUint256(datas[2]); err != nil {
		return nil, fmt.Errorf("%s: %w", calls[2].sig, err)
	}
	if l.UnitsSinceUpdate, err = DecodeUint64(datas[3]); err != nil {
		return nil, fmt.Errorf("%s: %w", calls[3].sig, err)
	}
	if l.LastUpdateTime, err = DecodeUint64(datas[4]); err != nil {
		return nil, fmt.Errorf("%s: %w", calls[4].sig, err)
	}
	if l.EquilibrationUnits, err = DecodeUint64(datas[5]); err != nil {
		return nil, fmt.Errorf("%s: %w", calls[5].sig, err)
	}
	if l.PerBatchGasCharge, err = DecodeInt64(datas[6]); err != nil {
		return nil, fmt.Errorf("%s: %w", calls[6].sig, err)
	}
	if l.RewardRate, err = DecodeUint64(datas[7]); err != nil {
		return nil, fmt.Errorf("%s: %w", calls[7].sig, err)
	}
	if l.ArbOSVersion, err = decodeArbOSVersion(datas[8]); err != nil {
		return nil, fmt.Errorf("%s: %w", calls[8].sig, err)
	}
	if l.ArbOSVersion >= arbOSVersionParentGasFloor {
		floorData, err := callBytes(results[9])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", calls[9].sig, err)
		}
		if l.ParentGasFloorPerToken, err = DecodeUint64(floorData); err != nil {
			return nil, fmt.Errorf("%s: %w", calls[9].sig, err)
		}
	}
	return l, nil
}

// FeeAccounts reads the infra, network and L1 reward accounts and their
// balances (two batches: addresses, then balances).
func (c *Client) FeeAccounts(ctx context.Context) (*FeeAccounts, error) {
	results, err := c.chunk(ctx, []Request{
		SelectorCall(ArbOwnerPublicAddress, SigGetInfraFeeAccount),
		SelectorCall(ArbOwnerPublicAddress, SigGetNetworkFeeAccount),
		SelectorCall(ArbGasInfoAddress, SigGetL1RewardRecipient),
	})
	if err != nil {
		return nil, err
	}
	names := []string{SigGetInfraFeeAccount, SigGetNetworkFeeAccount, SigGetL1RewardRecipient}
	addrs := make([]string, 3)
	for i, r := range results {
		data, err := callBytes(r)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", names[i], err)
		}
		if addrs[i], err = DecodeAddress(data); err != nil {
			return nil, fmt.Errorf("%s: %w", names[i], err)
		}
	}
	balReqs := make([]Request, 3)
	for i, a := range addrs {
		balReqs[i] = Request{Method: "eth_getBalance", Params: []any{a, latestTag}}
	}
	balResults, err := c.chunk(ctx, balReqs)
	if err != nil {
		return nil, err
	}
	accounts := make([]Account, 3)
	for i, r := range balResults {
		if r.Err != nil {
			return nil, fmt.Errorf("eth_getBalance %s: %w", addrs[i], r.Err)
		}
		var s string
		if err := json.Unmarshal(r.Raw, &s); err != nil {
			return nil, fmt.Errorf("eth_getBalance %s: %w", addrs[i], err)
		}
		bal, err := HexBig(s)
		if err != nil {
			return nil, fmt.Errorf("eth_getBalance %s: %w", addrs[i], err)
		}
		accounts[i] = Account{Address: addrs[i], Balance: bal}
	}
	return &FeeAccounts{Infra: accounts[0], Network: accounts[1], L1Reward: accounts[2]}, nil
}

// OwnerActsLogs fetches OwnerActs events from the ArbOwner precompile over
// [from, to].
func (c *Client) OwnerActsLogs(ctx context.Context, from, to uint64) ([]Log, error) {
	return c.Logs(ctx, from, to, ArbOwnerAddress, []string{OwnerActsTopic})
}
