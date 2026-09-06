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
	raw, err := c.Call(ctx, "eth_blockNumber")
	if err != nil {
		return 0, err
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, fmt.Errorf("decode eth_blockNumber: %w", err)
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
	out := make([]Header, 0, len(numbers))
	for start := 0; start < len(numbers); start += MaxBatch {
		end := min(start+MaxBatch, len(numbers))
		reqs := make([]Request, 0, end-start)
		for _, n := range numbers[start:end] {
			reqs = append(reqs, Request{Method: methodGetBlockByNumber, Params: []any{blockTag(n), false}})
		}
		results, err := c.Batch(ctx, reqs)
		if err != nil {
			return nil, err
		}
		for i, r := range results {
			if r.Err != nil {
				return nil, fmt.Errorf("block %d: %w", numbers[start+i], r.Err)
			}
			b, err := parseHeader(r.Raw)
			if err != nil {
				return nil, fmt.Errorf("block %d: %w", numbers[start+i], err)
			}
			out = append(out, b.Header)
		}
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
	out := make([]Block, 0, len(numbers))
	for start := 0; start < len(numbers); start += MaxBatch {
		end := min(start+MaxBatch, len(numbers))
		reqs := make([]Request, 0, end-start)
		for _, n := range numbers[start:end] {
			reqs = append(reqs, Request{Method: methodGetBlockByNumber, Params: []any{blockTag(n), true}})
		}
		results, err := c.Batch(ctx, reqs)
		if err != nil {
			return nil, err
		}
		for i, r := range results {
			if r.Err != nil {
				return nil, fmt.Errorf("block %d: %w", numbers[start+i], r.Err)
			}
			b, err := parseHeader(r.Raw)
			if err != nil {
				return nil, fmt.Errorf("block %d: %w", numbers[start+i], err)
			}
			out = append(out, *b)
		}
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
	results, err := c.Batch(ctx, []Request{r})
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
	v, err := DecodeUint64(data)
	if err != nil {
		return 0, err
	}
	if v < ArbOSVersionOffset {
		return 0, fmt.Errorf("arbOSVersion %d below offset", v)
	}
	return v - ArbOSVersionOffset, nil
}

// FastSample performs the fast tick batch: latest header, constraints,
// prices and minimum base fee. When the constraints call reverts or returns
// an empty list a second batch reads the legacy pricer parameters.
func (c *Client) FastSample(ctx context.Context) (*Sample, error) {
	results, err := c.Batch(ctx, []Request{
		{Method: methodGetBlockByNumber, Params: []any{latestTag, false}},
		SelectorCall(ArbGasInfoAddress, SigGetGasPricingConstraints),
		SelectorCall(ArbGasInfoAddress, SigGetPricesInWei),
		SelectorCall(ArbGasInfoAddress, SigGetMinimumGasPrice),
	})
	if err != nil {
		return nil, err
	}
	s := &Sample{SampledAt: c.now()}
	if results[0].Err != nil {
		return nil, fmt.Errorf("latest block: %w", results[0].Err)
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
		legacy, err := c.LegacyParams(ctx)
		if err != nil {
			return nil, err
		}
		s.Legacy = legacy
	}
	return s, nil
}

// LegacyParams reads the legacy pricer parameters in one batch.
func (c *Client) LegacyParams(ctx context.Context) (*LegacyParams, error) {
	results, err := c.Batch(ctx, []Request{
		SelectorCall(ArbGasInfoAddress, SigGetGasBacklog),
		SelectorCall(ArbGasInfoAddress, SigGetPricingInertia),
		SelectorCall(ArbGasInfoAddress, SigGetGasBacklogTolerance),
		SelectorCall(ArbGasInfoAddress, SigGetGasAccountingParams),
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

// L1Sample reads the L1 pricer getters in one batch.
func (c *Client) L1Sample(ctx context.Context) (*L1Sample, error) {
	sigs := []string{
		SigGetL1BaseFeeEstimate, SigGetL1PricingSurplus, SigGetL1FeesAvailable, SigGetL1PricingUnitsSinceUpdate,
		SigGetLastL1PricingUpdateTime, SigGetL1PricingEquilibrationUnit, SigGetPerBatchGasCharge, SigGetL1RewardRate,
	}
	reqs := make([]Request, len(sigs))
	for i, sig := range sigs {
		reqs[i] = SelectorCall(ArbGasInfoAddress, sig)
	}
	results, err := c.Batch(ctx, reqs)
	if err != nil {
		return nil, err
	}
	datas := make([][]byte, len(results))
	for i, r := range results {
		if datas[i], err = callBytes(r); err != nil {
			return nil, fmt.Errorf("%s: %w", sigs[i], err)
		}
	}
	l := &L1Sample{}
	if l.BaseFeeEstimate, err = DecodeUint256(datas[0]); err != nil {
		return nil, fmt.Errorf("%s: %w", sigs[0], err)
	}
	if l.Surplus, err = DecodeInt256(datas[1]); err != nil {
		return nil, fmt.Errorf("%s: %w", sigs[1], err)
	}
	if l.FeesAvailable, err = DecodeUint256(datas[2]); err != nil {
		return nil, fmt.Errorf("%s: %w", sigs[2], err)
	}
	if l.UnitsSinceUpdate, err = DecodeUint64(datas[3]); err != nil {
		return nil, fmt.Errorf("%s: %w", sigs[3], err)
	}
	if l.LastUpdateTime, err = DecodeUint64(datas[4]); err != nil {
		return nil, fmt.Errorf("%s: %w", sigs[4], err)
	}
	if l.EquilibrationUnits, err = DecodeUint64(datas[5]); err != nil {
		return nil, fmt.Errorf("%s: %w", sigs[5], err)
	}
	if l.PerBatchGasCharge, err = DecodeInt64(datas[6]); err != nil {
		return nil, fmt.Errorf("%s: %w", sigs[6], err)
	}
	if l.RewardRate, err = DecodeUint64(datas[7]); err != nil {
		return nil, fmt.Errorf("%s: %w", sigs[7], err)
	}
	return l, nil
}

// FeeAccounts reads the infra, network and L1 reward accounts and their
// balances (two batches: addresses, then balances).
func (c *Client) FeeAccounts(ctx context.Context) (*FeeAccounts, error) {
	results, err := c.Batch(ctx, []Request{
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
	balResults, err := c.Batch(ctx, balReqs)
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
