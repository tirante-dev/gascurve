package nitro

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"strings"
)

const nullJSON = "null"

// Header is the subset of an L2 block header the collector needs.
type Header struct {
	Number        uint64
	Hash          string
	ParentHash    string
	Timestamp     uint64
	GasUsed       uint64
	GasLimit      uint64
	BaseFee       *big.Int
	L1BlockNumber uint64
	ArbOSVersion  uint64
	TxCount       int
	TxHashes      []string
	// PosterGas is the sum of gasUsedForL1 from the block's receipts. It is
	// nil only for header-only lookups that do not need fee accounting.
	PosterGas *uint64
	// computeGasBefore holds the cumulative compute gas before each
	// transaction. It comes from the same validated receipt set as PosterGas
	// and lets replay place owner actions at the correct compute-gas boundary.
	computeGasBefore []uint64
}

// ComputeGas is the gas Nitro applies to the L2 pricer and splits between
// the infrastructure and network fee accounts. A header-only lookup has no
// receipt input and falls back to total gas because callers such as ancestry
// checks do not use the value for fee accounting.
func (h Header) ComputeGas() uint64 {
	if h.PosterGas == nil {
		return h.GasUsed
	}
	if *h.PosterGas > h.GasUsed {
		return 0
	}
	return h.GasUsed - *h.PosterGas
}

// ComputeGasBeforeTx returns the cumulative compute gas before transaction
// index when the header was joined with its authoritative receipt set.
func (h Header) ComputeGasBeforeTx(index uint64) (uint64, bool) {
	if index >= uint64(len(h.computeGasBefore)) {
		return 0, false
	}
	return h.computeGasBefore[index], true
}

// Tx is the subset of a transaction the collector needs.
type Tx struct {
	Hash  string
	Type  uint64
	From  string
	To    string
	Input []byte
}

// Block is a header plus its full transactions.
type Block struct {
	Header
	Txs []Tx
}

// Log is an eth_getLogs entry.
type Log struct {
	Address        string
	Topics         []string
	Data           []byte
	BlockNumber    uint64
	BlockTimestamp uint64
	TxHash         string
	TxIndex        uint64
	LogIndex       uint64
}

// Receipt is the transaction-ordering and gas-accounting subset of an
// eth_getTransactionReceipt response.
type Receipt struct {
	TxHash            string
	BlockNumber       uint64
	TxIndex           uint64
	GasUsed           uint64
	CumulativeGasUsed uint64
}

type rawTx struct {
	Hash  string `json:"hash"`
	Type  string `json:"type"`
	From  string `json:"from"`
	To    string `json:"to"`
	Input string `json:"input"`
}

type rawBlock struct {
	Number        string            `json:"number"`
	Hash          string            `json:"hash"`
	ParentHash    string            `json:"parentHash"`
	Timestamp     string            `json:"timestamp"`
	GasUsed       string            `json:"gasUsed"`
	GasLimit      string            `json:"gasLimit"`
	BaseFee       string            `json:"baseFeePerGas"`
	L1BlockNumber string            `json:"l1BlockNumber"`
	MixHash       string            `json:"mixHash"`
	Transactions  []json.RawMessage `json:"transactions"`
}

type rawLog struct {
	Address        string   `json:"address"`
	Topics         []string `json:"topics"`
	Data           string   `json:"data"`
	BlockNumber    string   `json:"blockNumber"`
	BlockTimestamp string   `json:"blockTimestamp"`
	TxHash         string   `json:"transactionHash"`
	TxIndex        string   `json:"transactionIndex"`
	LogIndex       string   `json:"logIndex"`
}

type rawReceipt struct {
	BlockHash         string  `json:"blockHash"`
	BlockNumber       string  `json:"blockNumber"`
	TxHash            string  `json:"transactionHash"`
	TxIndex           string  `json:"transactionIndex"`
	GasUsed           *string `json:"gasUsed"`
	CumulativeGasUsed *string `json:"cumulativeGasUsed"`
	GasUsedForL1      *string `json:"gasUsedForL1"`
}

func parseHeader(raw json.RawMessage) (*Block, error) {
	if len(raw) == 0 || string(raw) == nullJSON {
		return nil, fmt.Errorf("block not found")
	}
	var rb rawBlock
	if err := json.Unmarshal(raw, &rb); err != nil {
		return nil, fmt.Errorf("decode block: %w", err)
	}
	b := &Block{Header: Header{Hash: rb.Hash, ParentHash: rb.ParentHash}}
	var err error
	if b.Number, err = HexUint64(rb.Number); err != nil {
		return nil, fmt.Errorf("block number: %w", err)
	}
	if b.Timestamp, err = HexUint64(rb.Timestamp); err != nil {
		return nil, fmt.Errorf("block %d timestamp: %w", b.Number, err)
	}
	if b.GasUsed, err = HexUint64(rb.GasUsed); err != nil {
		return nil, fmt.Errorf("block %d gasUsed: %w", b.Number, err)
	}
	if rb.GasLimit != "" {
		if b.GasLimit, err = HexUint64(rb.GasLimit); err != nil {
			return nil, fmt.Errorf("block %d gasLimit: %w", b.Number, err)
		}
	}
	if rb.BaseFee == "" {
		b.BaseFee = new(big.Int)
	} else if b.BaseFee, err = HexBig(rb.BaseFee); err != nil {
		return nil, fmt.Errorf("block %d baseFeePerGas: %w", b.Number, err)
	}
	if rb.L1BlockNumber != "" {
		if b.L1BlockNumber, err = HexUint64(rb.L1BlockNumber); err != nil {
			return nil, fmt.Errorf("block %d l1BlockNumber: %w", b.Number, err)
		}
	}
	if rb.MixHash != "" {
		mixHash, err := DecodeHex(rb.MixHash)
		if err != nil || len(mixHash) != 32 {
			return nil, fmt.Errorf("block %d mixHash: expected 32 bytes", b.Number)
		}
		// Nitro HeaderInfo stores ArbOSFormatVersion in bytes 16 through 23
		// of the mix digest. This is the version used to process the block.
		b.ArbOSVersion = binary.BigEndian.Uint64(mixHash[16:24])
	}
	b.TxCount = len(rb.Transactions)
	b.TxHashes = make([]string, 0, len(rb.Transactions))
	for _, t := range rb.Transactions {
		if len(t) > 0 && t[0] == '"' {
			var h string
			if err := json.Unmarshal(t, &h); err != nil {
				return nil, fmt.Errorf("block %d tx hash: %w", b.Number, err)
			}
			b.TxHashes = append(b.TxHashes, h)
			continue
		}
		var rt rawTx
		if err := json.Unmarshal(t, &rt); err != nil {
			return nil, fmt.Errorf("block %d tx: %w", b.Number, err)
		}
		tx := Tx{Hash: rt.Hash, From: rt.From, To: rt.To}
		if rt.Type != "" {
			if tx.Type, err = HexUint64(rt.Type); err != nil {
				return nil, fmt.Errorf("block %d tx type: %w", b.Number, err)
			}
		}
		if rt.Input != "" {
			if tx.Input, err = DecodeHex(rt.Input); err != nil {
				return nil, fmt.Errorf("block %d tx input: %w", b.Number, err)
			}
		}
		b.TxHashes = append(b.TxHashes, rt.Hash)
		b.Txs = append(b.Txs, tx)
	}
	return b, nil
}

func parseLogs(raw json.RawMessage) ([]Log, error) {
	var rls []rawLog
	if err := json.Unmarshal(raw, &rls); err != nil {
		return nil, fmt.Errorf("decode logs: %w", err)
	}
	out := make([]Log, 0, len(rls))
	for _, rl := range rls {
		l := Log{Address: rl.Address, Topics: rl.Topics, TxHash: rl.TxHash}
		var err error
		if l.Data, err = DecodeHex(rl.Data); err != nil {
			return nil, fmt.Errorf("log data: %w", err)
		}
		if l.BlockNumber, err = HexUint64(rl.BlockNumber); err != nil {
			return nil, fmt.Errorf("log blockNumber: %w", err)
		}
		if l.LogIndex, err = HexUint64(rl.LogIndex); err != nil {
			return nil, fmt.Errorf("log logIndex: %w", err)
		}
		if l.TxIndex, err = HexUint64(rl.TxIndex); err != nil {
			return nil, fmt.Errorf("log transactionIndex: %w", err)
		}
		if rl.BlockTimestamp != "" {
			if l.BlockTimestamp, err = HexUint64(rl.BlockTimestamp); err != nil {
				return nil, fmt.Errorf("log blockTimestamp: %w", err)
			}
		}
		out = append(out, l)
	}
	return out, nil
}

func parseReceipt(raw json.RawMessage) (*Receipt, error) {
	if len(raw) == 0 || string(raw) == nullJSON {
		return nil, fmt.Errorf("receipt not found")
	}
	var rr rawReceipt
	if err := json.Unmarshal(raw, &rr); err != nil {
		return nil, fmt.Errorf("decode receipt: %w", err)
	}
	r := &Receipt{TxHash: rr.TxHash}
	var err error
	if r.BlockNumber, err = HexUint64(rr.BlockNumber); err != nil {
		return nil, fmt.Errorf("receipt %s blockNumber: %w", rr.TxHash, err)
	}
	if r.TxIndex, err = HexUint64(rr.TxIndex); err != nil {
		return nil, fmt.Errorf("receipt %s transactionIndex: %w", rr.TxHash, err)
	}
	if rr.GasUsed == nil || *rr.GasUsed == "" {
		return nil, fmt.Errorf("receipt %s has no gasUsed", rr.TxHash)
	}
	if r.GasUsed, err = HexUint64(*rr.GasUsed); err != nil {
		return nil, fmt.Errorf("receipt %s gasUsed: %w", rr.TxHash, err)
	}
	if rr.CumulativeGasUsed == nil || *rr.CumulativeGasUsed == "" {
		return nil, fmt.Errorf("receipt %s has no cumulativeGasUsed", rr.TxHash)
	}
	if r.CumulativeGasUsed, err = HexUint64(*rr.CumulativeGasUsed); err != nil {
		return nil, fmt.Errorf("receipt %s cumulativeGasUsed: %w", rr.TxHash, err)
	}
	return r, nil
}

// parsePosterGas validates a block receipt set and returns the sum of its
// authoritative gasUsedForL1 fields. Matching the receipt count, block number
// and hash keeps a reorg during a batched header/receipt read from joining two
// different blocks.
func parsePosterGas(raw json.RawMessage, h Header) (uint64, error) {
	posterGas, _, err := parseReceiptGas(raw, h)
	return posterGas, err
}

// ReceiptTarget is the block a receipt set has to belong to and the totals it
// has to reproduce.
type ReceiptTarget struct {
	Number  uint64
	Hash    string
	TxCount int
	GasUsed uint64
	// TxHashes are the block's transaction hashes, in order.
	TxHashes []string
	// HashesKnown says whether TxHashes is authoritative. A header read has
	// the hashes and sets it, so every receipt is matched against its own and
	// a header missing a hash it should carry is an error. The poster-gas
	// repair validates receipts against a stored block row, which records the
	// transaction count but not the hashes, and leaves this false: the block
	// hash on every receipt, the index order and the gas totals still have to
	// agree, which is what rules out joining a different block.
	HashesKnown bool
}

// receiptTarget describes a header for the receipt validator.
func receiptTarget(h Header) ReceiptTarget {
	return ReceiptTarget{Number: h.Number, Hash: h.Hash, TxCount: h.TxCount, GasUsed: h.GasUsed, TxHashes: h.TxHashes, HashesKnown: true}
}

// parseReceiptGas also records the cumulative compute gas before each
// transaction. Owner-action replay uses these boundaries so poster gas from
// transactions before an action is not added to the compute pricer.
func parseReceiptGas(raw json.RawMessage, h Header) (posterGas uint64, computeGasBefore []uint64, err error) {
	return parseReceipts(raw, receiptTarget(h))
}

// parseReceipts validates a block receipt set against the block it claims to
// belong to and returns the sum of its authoritative gasUsedForL1 fields.
func parseReceipts(raw json.RawMessage, t ReceiptTarget) (posterGas uint64, computeGasBefore []uint64, err error) {
	if len(raw) == 0 || string(raw) == nullJSON {
		return 0, nil, fmt.Errorf("block %d receipts not found", t.Number)
	}
	var receipts []rawReceipt
	if err := json.Unmarshal(raw, &receipts); err != nil {
		return 0, nil, fmt.Errorf("decode block %d receipts: %w", t.Number, err)
	}
	if len(receipts) != t.TxCount {
		return 0, nil, fmt.Errorf("block %d receipts: got %d, want %d", t.Number, len(receipts), t.TxCount)
	}
	computeGasBefore = make([]uint64, len(receipts))
	var total, totalGas uint64
	for i, receipt := range receipts {
		computeGasBefore[i] = totalGas - total
		if receipt.BlockHash == "" || !strings.EqualFold(receipt.BlockHash, t.Hash) {
			return 0, nil, fmt.Errorf("block %d receipt %d hash %q does not match %q", t.Number, i, receipt.BlockHash, t.Hash)
		}
		number, err := HexUint64(receipt.BlockNumber)
		if err != nil {
			return 0, nil, fmt.Errorf("block %d receipt %d number: %w", t.Number, i, err)
		}
		if number != t.Number {
			return 0, nil, fmt.Errorf("block %d receipt %d belongs to block %d", t.Number, i, number)
		}
		if t.HashesKnown {
			if i >= len(t.TxHashes) {
				return 0, nil, fmt.Errorf("block %d has no transaction hash for receipt %d", t.Number, i)
			}
			if receipt.TxHash == "" || !strings.EqualFold(receipt.TxHash, t.TxHashes[i]) {
				return 0, nil, fmt.Errorf("block %d receipt %d transaction %q does not match %q", t.Number, i, receipt.TxHash, t.TxHashes[i])
			}
		}
		index, err := HexUint64(receipt.TxIndex)
		if err != nil {
			return 0, nil, fmt.Errorf("block %d receipt %d transactionIndex: %w", t.Number, i, err)
		}
		if index != uint64(i) {
			return 0, nil, fmt.Errorf("block %d receipt %d has transaction index %d", t.Number, i, index)
		}
		if receipt.GasUsed == nil || *receipt.GasUsed == "" {
			return 0, nil, fmt.Errorf("block %d receipt %d has no gasUsed", t.Number, i)
		}
		gasUsed, err := HexUint64(*receipt.GasUsed)
		if err != nil {
			return 0, nil, fmt.Errorf("block %d receipt %d gasUsed: %w", t.Number, i, err)
		}
		if gasUsed > math.MaxUint64-totalGas {
			return 0, nil, fmt.Errorf("block %d receipt gas overflows uint64", t.Number)
		}
		totalGas += gasUsed
		if receipt.CumulativeGasUsed == nil || *receipt.CumulativeGasUsed == "" {
			return 0, nil, fmt.Errorf("block %d receipt %d has no cumulativeGasUsed", t.Number, i)
		}
		cumulative, err := HexUint64(*receipt.CumulativeGasUsed)
		if err != nil {
			return 0, nil, fmt.Errorf("block %d receipt %d cumulativeGasUsed: %w", t.Number, i, err)
		}
		if cumulative != totalGas {
			return 0, nil, fmt.Errorf("block %d receipt %d cumulative gas %d, want %d", t.Number, i, cumulative, totalGas)
		}
		if receipt.GasUsedForL1 == nil || *receipt.GasUsedForL1 == "" {
			return 0, nil, fmt.Errorf("block %d receipt %d has no gasUsedForL1", t.Number, i)
		}
		gas, err := HexUint64(*receipt.GasUsedForL1)
		if err != nil {
			return 0, nil, fmt.Errorf("block %d receipt %d gasUsedForL1: %w", t.Number, i, err)
		}
		if gas > gasUsed {
			return 0, nil, fmt.Errorf("block %d receipt %d poster gas %d exceeds gas used %d", t.Number, i, gas, gasUsed)
		}
		if gas > math.MaxUint64-total {
			return 0, nil, fmt.Errorf("block %d poster gas overflows uint64", t.Number)
		}
		total += gas
	}
	if totalGas != t.GasUsed {
		return 0, nil, fmt.Errorf("block %d receipt gas %d does not match header gas %d", t.Number, totalGas, t.GasUsed)
	}
	if total > t.GasUsed {
		return 0, nil, fmt.Errorf("block %d poster gas %d exceeds total gas %d", t.Number, total, t.GasUsed)
	}
	return total, computeGasBefore, nil
}
