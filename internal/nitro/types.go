package nitro

import (
	"encoding/json"
	"fmt"
	"math/big"
)

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
	TxCount       int
	TxHashes      []string
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
	LogIndex       uint64
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
	Transactions  []json.RawMessage `json:"transactions"`
}

type rawLog struct {
	Address        string   `json:"address"`
	Topics         []string `json:"topics"`
	Data           string   `json:"data"`
	BlockNumber    string   `json:"blockNumber"`
	BlockTimestamp string   `json:"blockTimestamp"`
	TxHash         string   `json:"transactionHash"`
	LogIndex       string   `json:"logIndex"`
}

func parseHeader(raw json.RawMessage) (*Block, error) {
	if len(raw) == 0 || string(raw) == "null" {
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
		if rl.BlockTimestamp != "" {
			if l.BlockTimestamp, err = HexUint64(rl.BlockTimestamp); err != nil {
				return nil, fmt.Errorf("log blockTimestamp: %w", err)
			}
		}
		out = append(out, l)
	}
	return out, nil
}
