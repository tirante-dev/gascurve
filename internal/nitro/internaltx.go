package nitro

import (
	"errors"
	"fmt"
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
// batch posted to L1 (V2 since ArbOS 40; V1 is accepted for older chains).
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

// GasSpent is the L1 gas ArbOS attributes to the batch: 4 gas per zero
// calldata byte, 16 per non-zero byte, plus the extra gas. The EIP-7623
// calldata floor that newer ArbOS versions apply on top is not modeled
// yet, so this is a lower bound on chains where the floor binds.
func (r *BatchPostingReport) GasSpent() uint64 {
	zeros := r.CalldataLen - min(r.CalldataLen, r.CalldataNonZeros)
	return zeros*4 + r.CalldataNonZeros*16 + r.ExtraGas
}

// WeiSpent is l1BaseFee * GasSpent().
func (r *BatchPostingReport) WeiSpent() *big.Int {
	if r.L1BaseFee == nil {
		return new(big.Int)
	}
	return new(big.Int).Mul(r.L1BaseFee, new(big.Int).SetUint64(r.GasSpent()))
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
