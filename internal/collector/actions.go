package collector

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/tirante-dev/gascurve/internal/nitro"
	"github.com/tirante-dev/gascurve/internal/pricer"
)

// actionBoundary is the point immediately before an owner action's transaction. GasBefore excludes that
// transaction, whose gas is charged to the state the action leaves.
type actionBoundary struct {
	txIndex   uint64
	gasBefore uint64
	changes   []pricingChange
}

type actionBlocks map[uint64][]actionBoundary

// resolveActionBlocks reads one receipt per pricing-action transaction and turns cumulative receipt gas
// into block-local action boundaries. It returns without an RPC when the headers contain no actions.
func (f *Follower) resolveActionBlocks(ctx context.Context, headers []nitro.Header, tl *timeline) (actionBlocks, error) {
	headerByNumber := make(map[uint64]nitro.Header, len(headers))
	changesByHash := map[string][]pricingChange{}
	hashes := make([]string, 0)
	for _, header := range headers {
		headerByNumber[header.Number] = header
		for _, change := range tl.changesAt(header.Number, true) {
			key := strings.ToLower(change.pos.txHash)
			if _, ok := changesByHash[key]; !ok {
				hashes = append(hashes, change.pos.txHash)
			}
			changesByHash[key] = append(changesByHash[key], change)
		}
	}
	if len(hashes) == 0 {
		return nil, nil
	}
	receipts, err := f.rpc.TransactionReceipts(ctx, hashes)
	if err != nil {
		return nil, fmt.Errorf("owner action receipts: %w", err)
	}
	if len(receipts) != len(hashes) {
		return nil, fmt.Errorf("owner action receipts: got %d receipts for %d transactions", len(receipts), len(hashes))
	}
	out := actionBlocks{}
	for i, receipt := range receipts {
		hash := hashes[i]
		if receipt.TxHash == "" || !strings.EqualFold(receipt.TxHash, hash) {
			return nil, fmt.Errorf("owner action receipt %s returned transaction %s", hash, receipt.TxHash)
		}
		changes := changesByHash[strings.ToLower(hash)]
		if len(changes) == 0 {
			return nil, fmt.Errorf("owner action receipt %s has no pricing change", hash)
		}
		block := changes[0].setBlock()
		header, ok := headerByNumber[block]
		if !ok || receipt.BlockNumber != block {
			return nil, fmt.Errorf("owner action receipt %s is in block %d, expected %d", hash, receipt.BlockNumber, block)
		}
		if header.TxCount > 0 && receipt.TxIndex >= uint64(header.TxCount) {
			return nil, fmt.Errorf("owner action receipt %s has transaction index %d outside block %d transaction count %d", hash, receipt.TxIndex, block, header.TxCount)
		}
		if receipt.GasUsed > receipt.CumulativeGasUsed {
			return nil, fmt.Errorf("owner action receipt %s gas used %d exceeds cumulative gas %d", hash, receipt.GasUsed, receipt.CumulativeGasUsed)
		}
		if receipt.CumulativeGasUsed > header.GasUsed {
			return nil, fmt.Errorf("owner action receipt %s cumulative gas %d exceeds block %d gas %d", hash, receipt.CumulativeGasUsed, block, header.GasUsed)
		}
		gasBefore := receipt.CumulativeGasUsed - receipt.GasUsed
		for _, change := range changes {
			if change.setBlock() != block {
				return nil, fmt.Errorf("owner action transaction %s spans blocks %d and %d", hash, block, change.setBlock())
			}
			if change.pos.txIndexKnown && change.pos.txIndex != receipt.TxIndex {
				return nil, fmt.Errorf("owner action %s/%d transaction index %d differs from receipt index %d", hash, change.pos.logIndex, change.pos.txIndex, receipt.TxIndex)
			}
		}
		sort.SliceStable(changes, func(i, j int) bool { return changes[i].pos.logIndex < changes[j].pos.logIndex })
		out[block] = append(out[block], actionBoundary{txIndex: receipt.TxIndex, gasBefore: gasBefore, changes: changes})
	}
	for block := range out {
		boundaries := out[block]
		sort.Slice(boundaries, func(i, j int) bool { return boundaries[i].txIndex < boundaries[j].txIndex })
		for i := 1; i < len(boundaries); i++ {
			if boundaries[i-1].txIndex == boundaries[i].txIndex {
				return nil, fmt.Errorf("owner action block %d has different transactions at index %d", block, boundaries[i].txIndex)
			}
			if boundaries[i-1].gasBefore > boundaries[i].gasBefore {
				return nil, fmt.Errorf("owner action block %d cumulative gas decreases between transaction indexes %d and %d", block, boundaries[i-1].txIndex, boundaries[i].txIndex)
			}
		}
		out[block] = boundaries
	}
	return out, nil
}

func (c pricingChange) setBlock() uint64 {
	switch {
	case c.set != nil:
		return c.set.block
	case c.fee != nil:
		return c.fee.block
	case c.legacy != nil:
		return c.legacy.block
	default:
		return 0
	}
}

// applyActionGas applies a block's gas in transaction order: gas before an action transaction goes to the
// old state, the action mutates the state, then that transaction and the ones after it add to the new one.
//
// A boundary is clamped to the gas the block contributes, so a boundary resolved against another gas basis
// can never subtract past zero and saturate a backlog. Clamping preserves the non-decreasing order.
func applyActionGas(st *pricer.State, gasUsed uint64, boundaries []actionBoundary) {
	var applied uint64
	for _, boundary := range boundaries {
		gasBefore := min(boundary.gasBefore, gasUsed)
		st.AddGas(gasBefore - applied)
		for _, change := range boundary.changes {
			applyPricingChange(st, change)
		}
		applied = gasBefore
	}
	st.AddGas(gasUsed - applied)
}
