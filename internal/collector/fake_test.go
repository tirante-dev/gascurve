package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/tirante-dev/gascurve/internal/config"
	"github.com/tirante-dev/gascurve/internal/db/dbtest"
	"github.com/tirante-dev/gascurve/internal/logger"
	"github.com/tirante-dev/gascurve/internal/nitro"
)

var (
	errRPC   = errors.New("rpc failure")
	baseTime = time.Date(2026, 9, 6, 7, 0, 0, 0, time.UTC)
)

// fakeRPC is a deterministic chain: block n has timestamp base+n/10 (ten
// blocks per second), gas 1M and a base fee derived from n.
type fakeRPC struct {
	mu      sync.Mutex
	head    uint64
	chainID uint64
	// forks reorganize the chain: every block at or above a fork's block
	// carries that fork's tag in its hash (later forks override earlier
	// ones), so stored hashes stop matching the node's.
	forks       []fakeFork
	constraints []nitro.Constraint
	legacy      *nitro.LegacyParams
	minFee      *big.Int
	l1          *nitro.L1Sample
	accounts    *nitro.FeeAccounts
	arbos       uint64
	logs        []nitro.Log
	fullBlocks  map[uint64]nitro.Block
	receipts    map[string]nitro.Receipt
	available   int
	stats       nitro.Stats
	errs        map[string]error
	// hooks run when the named method is called, before it answers, so a
	// test can change the database (commit a rewind, say) while a fetch is
	// in flight.
	hooks       map[string]func()
	calls       map[string]int
	logRanges   [][2]uint64
	headerCalls [][]uint64
	sampleAt    []uint64 // block numbers passed to FastSampleAt
	// classes records the pacer class each method was last called with.
	classes map[string]nitro.Class
	// backlogsAt and constraintsAt, when set, supply the backlogs and the
	// constraint set FastSampleAt reports at a block (an archive node's
	// real state at that height); nil means the live ones.
	backlogsAt    func(uint64) []uint64
	constraintsAt func(uint64) []nitro.Constraint
	txCount       func(uint64) int
	posterGas     func(uint64) uint64
	// parentOverride replaces the parent hash a header reports, so a test
	// can break the links a fetched range has to satisfy without moving
	// the blocks themselves onto a fork.
	parentOverride map[uint64]string
	// noHeaders makes HeadersByNumbers answer with nothing, the way an
	// endpoint that has not seen the range does, and headerShift makes it
	// answer with the wrong blocks.
	noHeaders   bool
	headerShift uint64
	sampledAt   time.Time
	// posterGasCalls records the block numbers of each PosterGasByNumbers
	// batch, so a test can see how the repair sized and narrowed its reads.
	posterGasCalls [][]uint64
}

func newFakeRPC(head uint64) *fakeRPC {
	return &fakeRPC{
		head:    head,
		chainID: 4663,
		constraints: []nitro.Constraint{
			{Target: 60_000_000, Window: 15, Backlog: 3_111_506},
			{Target: 40_000_000, Window: 86_400, Backlog: 11_194_391_810_886},
		},
		minFee: big.NewInt(20_000_000),
		l1: &nitro.L1Sample{BaseFeeEstimate: big.NewInt(2_369_608), Surplus: big.NewInt(-5), FeesAvailable: big.NewInt(190),
			UnitsSinceUpdate: 1, LastUpdateTime: 1_700_000_000, EquilibrationUnits: 160_000_000, PerBatchGasCharge: 210_000, RewardRate: 10,
			ArbOSVersion: 61, ParentGasFloorPerToken: 10},
		accounts: &nitro.FeeAccounts{
			Infra: nitro.Account{Address: "0x1", Balance: big.NewInt(402)}, Network: nitro.Account{Address: "0x2", Balance: big.NewInt(10706)},
			L1Reward: nitro.Account{Address: "0x3", Balance: big.NewInt(1)},
		},
		arbos:      61,
		fullBlocks: map[uint64]nitro.Block{},
		receipts:   map[string]nitro.Receipt{},
		available:  1000,
		errs:       map[string]error{},
		hooks:      map[string]func(){},
		calls:      map[string]int{},
		classes:    map[string]nitro.Class{},
		txCount:    func(uint64) int { return 3 },
		posterGas:  func(uint64) uint64 { return 0 },
		sampledAt:  baseTime,
	}
}

func (f *fakeRPC) fail(method string) error {
	f.mu.Lock()
	f.calls[method]++
	err, hook := f.errs[method], f.hooks[method]
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return err
}

// note records the pacer class a call carried.
func (f *fakeRPC) note(ctx context.Context, method string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.classes[method] = nitro.ClassOf(ctx)
}

// classOf returns the pacer class method was last called with.
func (f *fakeRPC) classOf(method string) nitro.Class {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.classes[method]
}

func tsFor(n uint64) uint64  { return uint64(baseTime.Unix()) + n/10 }
func gasFor(n uint64) uint64 { return 1_000_000 + n%7 }
func feeFor(n uint64) *big.Int {
	return big.NewInt(20_000_000 + int64(n%13))
}

type fakeFork struct {
	at  uint64
	tag string
}

// hashFor is the canonical hash of block n, or the hash of the latest fork
// covering it.
func (f *fakeRPC) hashFor(n uint64) string {
	tag := ""
	for _, fk := range f.forks {
		if n >= fk.at {
			tag = fk.tag
		}
	}
	return fmt.Sprintf("0x%s%x", tag, n)
}

// fork reorganizes the chain from block at on.
func (f *fakeRPC) fork(at uint64, tag string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forks = append(f.forks, fakeFork{at: at, tag: tag})
}

func (f *fakeRPC) header(n uint64) nitro.Header {
	parent := f.hashFor(n - 1)
	if h, ok := f.parentOverride[n]; ok {
		parent = h
	}
	posterGas := f.posterGas(n)
	return nitro.Header{Number: n, Hash: f.hashFor(n), ParentHash: parent, Timestamp: tsFor(n), GasUsed: gasFor(n), BaseFee: feeFor(n), L1BlockNumber: 50,
		ArbOSVersion: f.arbos, TxCount: f.txCount(n), TxHashes: []string{"0x1", "0x2", "0x3"}[:f.txCount(n)], PosterGas: &posterGas}
}

func (f *fakeRPC) ChainID(context.Context) (uint64, error) {
	if err := f.fail("ChainID"); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.chainID, nil
}

func (f *fakeRPC) FastSample(ctx context.Context) (*nitro.Sample, error) {
	f.note(ctx, "FastSample")
	if err := f.fail("FastSample"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sampleLocked(f.head, f.constraints, nil), nil
}

func (f *fakeRPC) FastSampleAt(ctx context.Context, n uint64) (*nitro.Sample, error) {
	f.note(ctx, "FastSampleAt")
	if err := f.fail("FastSampleAt"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sampleAt = append(f.sampleAt, n)
	if n > f.head {
		return nil, fmt.Errorf("block %d not found", n)
	}
	var backlogs []uint64
	if f.backlogsAt != nil {
		backlogs = f.backlogsAt(n)
	}
	constraints := f.constraints
	if f.constraintsAt != nil {
		constraints = f.constraintsAt(n)
	}
	return f.sampleLocked(n, constraints, backlogs), nil
}

func (f *fakeRPC) PricingSampleAt(ctx context.Context, n uint64) (*nitro.Sample, error) {
	f.note(ctx, "PricingSampleAt")
	if err := f.fail("PricingSampleAt"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sampleAt = append(f.sampleAt, n)
	if n > f.head {
		return nil, fmt.Errorf("block %d not found", n)
	}
	var backlogs []uint64
	if f.backlogsAt != nil {
		backlogs = f.backlogsAt(n)
	}
	constraints := f.constraints
	if f.constraintsAt != nil {
		constraints = f.constraintsAt(n)
	}
	return f.sampleLocked(n, constraints, backlogs), nil
}

// sampleLocked builds a sample for block n with the given constraints,
// overriding the backlogs when given.
func (f *fakeRPC) sampleLocked(n uint64, constraints []nitro.Constraint, backlogs []uint64) *nitro.Sample {
	s := &nitro.Sample{SampledAt: f.sampledAt, Header: f.header(n), MinBaseFee: new(big.Int).Set(f.minFee),
		Prices: nitro.Prices{PerL2Tx: big.NewInt(1), PerL1CalldataByte: big.NewInt(2), PerL2Storage: big.NewInt(3), PerArbGasBase: big.NewInt(4), PerArbGasCongestion: big.NewInt(5), PerArbGasTotal: big.NewInt(6)}}
	if f.legacy != nil {
		l := *f.legacy
		if len(backlogs) > 0 {
			l.Backlog = backlogs[0]
		}
		s.Legacy = &l
	} else {
		s.Constraints = append([]nitro.Constraint(nil), constraints...)
		for i := range s.Constraints {
			if i < len(backlogs) {
				s.Constraints[i].Backlog = backlogs[i]
			}
		}
	}
	f.sampledAt = f.sampledAt.Add(time.Second)
	return s
}

func (f *fakeRPC) HeadersByNumbers(ctx context.Context, numbers []uint64) ([]nitro.Header, error) {
	f.note(ctx, "HeadersByNumbers")
	if err := f.fail("HeadersByNumbers"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.headerCalls = append(f.headerCalls, append([]uint64(nil), numbers...))
	if f.noHeaders {
		return nil, nil
	}
	out := make([]nitro.Header, 0, len(numbers))
	for _, n := range numbers {
		if n > f.head {
			return nil, fmt.Errorf("block %d not found", n)
		}
		out = append(out, f.header(n+f.headerShift))
	}
	return out, nil
}

// PosterGasByNumbers answers the repair pass the way an endpoint does: the
// poster gas the fake chain reports for each block, checked against the
// target's own gas total so a test that scripts an impossible value sees the
// same rejection the real validator gives.
func (f *fakeRPC) PosterGasByNumbers(ctx context.Context, targets []nitro.ReceiptTarget) (map[uint64]uint64, error) {
	f.note(ctx, "PosterGasByNumbers")
	if err := f.fail("PosterGasByNumbers"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	numbers := make([]uint64, 0, len(targets))
	out := make(map[uint64]uint64, len(targets))
	for _, t := range targets {
		numbers = append(numbers, t.Number)
		if t.Number > f.head {
			return nil, fmt.Errorf("block %d not found", t.Number)
		}
		gas := f.posterGas(t.Number)
		if gas > t.GasUsed {
			return nil, fmt.Errorf("block %d poster gas %d exceeds total gas %d", t.Number, gas, t.GasUsed)
		}
		out[t.Number] = gas
	}
	f.posterGasCalls = append(f.posterGasCalls, numbers)
	return out, nil
}

func (f *fakeRPC) HeaderByNumber(_ context.Context, n uint64) (*nitro.Header, error) {
	if err := f.fail("HeaderByNumber"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	h := f.header(n)
	return &h, nil
}

func (f *fakeRPC) BlocksWithTxs(_ context.Context, numbers []uint64) ([]nitro.Block, error) {
	if err := f.fail("BlocksWithTxs"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]nitro.Block, 0, len(numbers))
	for _, n := range numbers {
		if b, ok := f.fullBlocks[n]; ok {
			out = append(out, b)
			continue
		}
		out = append(out, nitro.Block{Header: f.header(n)})
	}
	return out, nil
}

func (f *fakeRPC) TransactionReceipts(_ context.Context, hashes []string) ([]nitro.Receipt, error) {
	if err := f.fail("TransactionReceipts"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]nitro.Receipt, 0, len(hashes))
	for _, hash := range hashes {
		if receipt, ok := f.receipts[hash]; ok {
			out = append(out, receipt)
			continue
		}
		var log *nitro.Log
		for i := range f.logs {
			if f.logs[i].TxHash == hash {
				log = &f.logs[i]
				break
			}
		}
		if log == nil {
			return nil, fmt.Errorf("receipt %s not found", hash)
		}
		gas := f.header(log.BlockNumber).GasUsed
		out = append(out, nitro.Receipt{TxHash: hash, BlockNumber: log.BlockNumber, TxIndex: log.TxIndex, GasUsed: gas, CumulativeGasUsed: gas})
	}
	return out, nil
}

func (f *fakeRPC) OwnerActsLogs(_ context.Context, from, to uint64) ([]nitro.Log, error) {
	if err := f.fail("OwnerActsLogs"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logRanges = append(f.logRanges, [2]uint64{from, to})
	var out []nitro.Log
	for _, l := range f.logs {
		if l.BlockNumber >= from && l.BlockNumber <= to {
			out = append(out, l)
		}
	}
	return out, nil
}

func (f *fakeRPC) L1SampleAt(_ context.Context, _ uint64) (*nitro.L1Sample, error) {
	if err := f.fail("L1SampleAt"); err != nil {
		return nil, err
	}
	return f.l1, nil
}

func (f *fakeRPC) FeeAccounts(context.Context) (*nitro.FeeAccounts, error) {
	if err := f.fail("FeeAccounts"); err != nil {
		return nil, err
	}
	return f.accounts, nil
}

func (f *fakeRPC) ArbOSVersion(context.Context) (uint64, error) {
	if err := f.fail("ArbOSVersion"); err != nil {
		return 0, err
	}
	return f.arbos, nil
}

func (f *fakeRPC) BlockNumber(context.Context) (uint64, error) {
	if err := f.fail("BlockNumber"); err != nil {
		return 0, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.head, nil
}

func (f *fakeRPC) Stats() nitro.Stats {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stats
}

func (f *fakeRPC) Available() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.available
}

func (f *fakeRPC) setHead(n uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.head = n
}

func (f *fakeRPC) calledTimes(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[method]
}

// fixtureLogs loads the recorded Robinhood OwnerActs logs.
func fixtureLogs(t *testing.T) []nitro.Log {
	t.Helper()
	raw, err := os.ReadFile("../nitro/testdata/robinhood_owner_acts_logs.json")
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Result []struct {
			Address     string   `json:"address"`
			Topics      []string `json:"topics"`
			Data        string   `json:"data"`
			BlockNumber string   `json:"blockNumber"`
			TxHash      string   `json:"transactionHash"`
			TxIndex     string   `json:"transactionIndex"`
			LogIndex    string   `json:"logIndex"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	out := make([]nitro.Log, 0, len(resp.Result))
	for _, r := range resp.Result {
		data, err := nitro.DecodeHex(r.Data)
		if err != nil {
			t.Fatal(err)
		}
		bn, _ := nitro.HexUint64(r.BlockNumber)
		ti, _ := nitro.HexUint64(r.TxIndex)
		li, _ := nitro.HexUint64(r.LogIndex)
		out = append(out, nitro.Log{Address: r.Address, Topics: r.Topics, Data: data, BlockNumber: bn, TxHash: r.TxHash, TxIndex: ti, LogIndex: li})
	}
	return out
}

func testConfig() config.CollectorConfig {
	return config.CollectorConfig{
		TickInterval: time.Second, SlowInterval: time.Minute, HeaderBatchSize: 10,
		BlockRetention: 48 * time.Hour, SampleRetention: 168 * time.Hour, BackfillDepth: 700 * time.Second,
	}
}

func newTestFollower(t *testing.T, rpc *fakeRPC, store *dbtest.MemStore, opts ...func(*Options)) *Follower {
	t.Helper()
	clock := baseTime.Add(1000 * time.Second / 10)
	o := Options{
		Network:   config.NetworkConfig{Name: "robinhood", DisplayName: "Robinhood Chain", ChainID: 4663, ExplorerURL: "https://x", CallsPerSecond: 4, Enabled: true},
		Collector: testConfig(),
		RPC:       rpc,
		Store:     store,
		Log:       logger.Nop(),
		Now:       func() time.Time { return clock },
		Sleep:     func(ctx context.Context, _ time.Duration) error { return ctx.Err() },
	}
	for _, apply := range opts {
		apply(&o)
	}
	f := NewFollower(o)
	f.batchCostAnchor = &batchCostAnchor{block: ^uint64(0), params: nitro.BatchPostingCostParams{
		ArbOSVersion: rpc.l1.ArbOSVersion, PerBatchGasCharge: rpc.l1.PerBatchGasCharge,
		ParentGasFloorPerToken: rpc.l1.ParentGasFloorPerToken,
	}}
	return f
}
