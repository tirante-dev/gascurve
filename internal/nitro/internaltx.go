package nitro

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"strings"
)

// StartBlock is the ArbOS internal transaction that opens every block.
type StartBlock struct {
	L1BaseFee     *big.Int
	L1BlockNumber uint64
	L2BlockNumber uint64
	TimePassed    uint64
}

// BatchPostingReport is the internal transaction ArbOS receives for every
// batch posted to L1 (V2 for ArbOS 50+; V1 is used by older chains).
type BatchPostingReport struct {
	Version          int
	BatchTimestamp   uint64
	Poster           string
	BatchNumber      uint64
	CalldataLen      uint64
	CalldataNonZeros uint64
	// ExtraGas is batchExtraGas for V2 (blob gas expressed as L1 gas) and the
	// whole batchDataGas for V1 reports, whose calldata size is not reported.
	ExtraGas  uint64
	L1BaseFee *big.Int
}

// BatchPostingCostCalculationVersion identifies the persisted implementation
// of Nitro's version-aware batch-poster spending calculation.
const BatchPostingCostCalculationVersion = 1

const (
	arbOSVersionParentGasFloor = 50
	floorGasAdditionalTokens   = 172
	txGas                      = 21_000
	txDataZeroGas              = 4
	txDataNonZeroGas           = 16
	keccak256Gas               = 30
	keccak256WordGas           = 6
	sstoreSetGas               = 20_000
)

// BatchPostingCostParams is the ArbOS state used when a report executes.
// These values are not encoded in the internal transaction and must be
// retained with the decoded report for later recomputation.
type BatchPostingCostParams struct {
	ArbOSVersion           uint64 `json:"arbosVersion"`
	PerBatchGasCharge      int64  `json:"perBatchGasCharge"`
	ParentGasFloorPerToken uint64 `json:"parentGasFloorPerToken"`
}

// BatchPostingCost is the parent-chain spending ArbOS attributes to a batch.
type BatchPostingCost struct {
	GasSpent uint64
	WeiSpent *big.Int
}

// MaxAttributedGasSpent is the largest gas figure Cost reports. Attributed
// gas is persisted in a signed 64-bit column, so a malformed report whose
// saturating arithmetic runs past it clamps here rather than failing the
// write and stalling the scan. No report Nitro can produce comes near it.
const MaxAttributedGasSpent uint64 = math.MaxInt64

// Cost reproduces Nitro's ApplyInternalTxUpdate batch-report accounting. V1
// uses its signed saturating calculation. V2 uses LegacyCostForStats, adds
// extra gas and a nonnegative per-batch charge, then applies the ArbOS 50+
// parent calldata floor. Saturating arithmetic preserves Nitro's behavior at
// its explicit saturation points and prevents malformed uint64 inputs from
// wrapping in the remaining multiplications; the result is then clamped to
// MaxAttributedGasSpent.
func (r *BatchPostingReport) Cost(p BatchPostingCostParams) (BatchPostingCost, error) {
	var gas uint64
	switch r.Version {
	case 1:
		dataGas := int64(min(r.ExtraGas, uint64(math.MaxInt64)))
		signedGas := saturatingAddInt64(p.PerBatchGasCharge, dataGas)
		if signedGas > 0 {
			gas = uint64(signedGas)
		}
	case 2:
		gas = legacyCostForStats(r.CalldataLen, r.CalldataNonZeros)
		gas = saturatingAddUint64(gas, r.ExtraGas)
		if p.PerBatchGasCharge > 0 {
			gas = saturatingAddUint64(gas, uint64(p.PerBatchGasCharge))
		}
		if p.ArbOSVersion >= arbOSVersionParentGasFloor {
			nonZeros := min(r.CalldataNonZeros, r.CalldataLen)
			tokens := saturatingAddUint64(r.CalldataLen, saturatingMulUint64(nonZeros, 3))
			tokens = saturatingAddUint64(tokens, floorGasAdditionalTokens)
			floor := saturatingAddUint64(saturatingMulUint64(p.ParentGasFloorPerToken, tokens), txGas)
			gas = max(gas, floor)
		}
	default:
		return BatchPostingCost{}, fmt.Errorf("unsupported batch posting report version %d", r.Version)
	}

	gas = min(gas, MaxAttributedGasSpent)

	wei := new(big.Int)
	if r.L1BaseFee != nil {
		wei.Mul(r.L1BaseFee, new(big.Int).SetUint64(gas))
	}
	return BatchPostingCost{GasSpent: gas, WeiSpent: wei}, nil
}

func legacyCostForStats(length, nonZeros uint64) uint64 {
	nonZeros = min(nonZeros, length)
	zeros := length - nonZeros
	gas := saturatingAddUint64(saturatingMulUint64(zeros, txDataZeroGas), saturatingMulUint64(nonZeros, txDataNonZeroGas))
	words := length / 32
	if length%32 != 0 {
		words = saturatingAddUint64(words, 1)
	}
	gas = saturatingAddUint64(gas, keccak256Gas)
	gas = saturatingAddUint64(gas, saturatingMulUint64(words, keccak256WordGas))
	return saturatingAddUint64(gas, 2*sstoreSetGas)
}

func saturatingAddUint64(a, b uint64) uint64 {
	if math.MaxUint64-a < b {
		return math.MaxUint64
	}
	return a + b
}

func saturatingMulUint64(a, b uint64) uint64 {
	if a != 0 && b > math.MaxUint64/a {
		return math.MaxUint64
	}
	return a * b
}

func saturatingAddInt64(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	if b < 0 && a < math.MinInt64-b {
		return math.MinInt64
	}
	return a + b
}

// ErrNotInternal is returned for calldata that is not a known internal call.
var ErrNotInternal = errors.New("not an ArbOS internal transaction")

var (
	selStartBlock = Selector(SigStartBlock)
	selBatchV2    = Selector(SigBatchPostingReportV2)
	selBatchV1    = Selector(SigBatchPostingReportV1)
)

// IsInternalTx reports whether tx is an ArbOS internal transaction.
func IsInternalTx(tx Tx) bool {
	return tx.Type == InternalTxType && strings.EqualFold(tx.To, ArbosAddress)
}

// DecodeInternalTx decodes startBlock and batchPostingReport calldata. It
// returns *StartBlock or *BatchPostingReport.
func DecodeInternalTx(input []byte) (any, error) {
	if len(input) < 4 {
		return nil, ErrNotInternal
	}
	var sel [4]byte
	copy(sel[:], input[:4])
	body := input[4:]
	switch sel {
	case selStartBlock:
		return decodeStartBlock(body)
	case selBatchV2:
		return decodeBatchV2(body)
	case selBatchV1:
		return decodeBatchV1(body)
	default:
		return nil, ErrNotInternal
	}
}

func decodeStartBlock(body []byte) (*StartBlock, error) {
	var (
		s   StartBlock
		err error
	)
	if s.L1BaseFee, err = wordBig(body, 0); err != nil {
		return nil, fmt.Errorf("startBlock: %w", err)
	}
	if s.L1BlockNumber, err = wordUint64(body, 1); err != nil {
		return nil, fmt.Errorf("startBlock: %w", err)
	}
	if s.L2BlockNumber, err = wordUint64(body, 2); err != nil {
		return nil, fmt.Errorf("startBlock: %w", err)
	}
	if s.TimePassed, err = wordUint64(body, 3); err != nil {
		return nil, fmt.Errorf("startBlock: %w", err)
	}
	return &s, nil
}

func decodeBatchV2(body []byte) (*BatchPostingReport, error) {
	r := &BatchPostingReport{Version: 2}
	var err error
	if r.BatchTimestamp, err = wordUint64(body, 0); err != nil {
		return nil, fmt.Errorf("batchPostingReportV2: %w", err)
	}
	if r.Poster, err = wordAddress(body, 1); err != nil {
		return nil, fmt.Errorf("batchPostingReportV2: %w", err)
	}
	if r.BatchNumber, err = wordUint64(body, 2); err != nil {
		return nil, fmt.Errorf("batchPostingReportV2: %w", err)
	}
	if r.CalldataLen, err = wordUint64(body, 3); err != nil {
		return nil, fmt.Errorf("batchPostingReportV2: %w", err)
	}
	if r.CalldataNonZeros, err = wordUint64(body, 4); err != nil {
		return nil, fmt.Errorf("batchPostingReportV2: %w", err)
	}
	if r.ExtraGas, err = wordUint64(body, 5); err != nil {
		return nil, fmt.Errorf("batchPostingReportV2: %w", err)
	}
	if r.L1BaseFee, err = wordBig(body, 6); err != nil {
		return nil, fmt.Errorf("batchPostingReportV2: %w", err)
	}
	return r, nil
}

func decodeBatchV1(body []byte) (*BatchPostingReport, error) {
	r := &BatchPostingReport{Version: 1}
	var err error
	if r.BatchTimestamp, err = wordUint64(body, 0); err != nil {
		return nil, fmt.Errorf("batchPostingReport: %w", err)
	}
	if r.Poster, err = wordAddress(body, 1); err != nil {
		return nil, fmt.Errorf("batchPostingReport: %w", err)
	}
	if r.BatchNumber, err = wordUint64(body, 2); err != nil {
		return nil, fmt.Errorf("batchPostingReport: %w", err)
	}
	if r.ExtraGas, err = wordUint64(body, 3); err != nil {
		return nil, fmt.Errorf("batchPostingReport: %w", err)
	}
	if r.L1BaseFee, err = wordBig(body, 4); err != nil {
		return nil, fmt.Errorf("batchPostingReport: %w", err)
	}
	return r, nil
}

// EncodeStartBlock builds startBlock calldata, for tests and tooling.
func EncodeStartBlock(s StartBlock) []byte {
	out := append([]byte{}, selStartBlock[:]...)
	out = append(out, padWord(s.L1BaseFee.Bytes())...)
	out = append(out, encodeUint64(s.L1BlockNumber)...)
	out = append(out, encodeUint64(s.L2BlockNumber)...)
	out = append(out, encodeUint64(s.TimePassed)...)
	return out
}

// EncodeBatchPostingReport builds V2 (or V1 when r.Version == 1) calldata,
// for tests and tooling.
func EncodeBatchPostingReport(r BatchPostingReport) []byte {
	addr, _ := DecodeHex(r.Poster)
	if r.Version == 1 {
		out := append([]byte{}, selBatchV1[:]...)
		out = append(out, encodeUint64(r.BatchTimestamp)...)
		out = append(out, padWord(addr)...)
		out = append(out, encodeUint64(r.BatchNumber)...)
		out = append(out, encodeUint64(r.ExtraGas)...)
		out = append(out, padWord(r.L1BaseFee.Bytes())...)
		return out
	}
	out := append([]byte{}, selBatchV2[:]...)
	out = append(out, encodeUint64(r.BatchTimestamp)...)
	out = append(out, padWord(addr)...)
	out = append(out, encodeUint64(r.BatchNumber)...)
	out = append(out, encodeUint64(r.CalldataLen)...)
	out = append(out, encodeUint64(r.CalldataNonZeros)...)
	out = append(out, encodeUint64(r.ExtraGas)...)
	out = append(out, padWord(r.L1BaseFee.Bytes())...)
	return out
}
