package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/tirante-dev/gascurve/internal/config"
	"github.com/tirante-dev/gascurve/internal/db"
	"github.com/tirante-dev/gascurve/internal/db/dbtest"
	"github.com/tirante-dev/gascurve/internal/logger"
	"github.com/tirante-dev/gascurve/internal/nitro"
)

// chainServer is a JSON-RPC server for a synthetic Nitro chain that
// advances ten blocks per second from the moment it starts and counts every
// call it answers (each item inside a batch), with the largest batch seen.
type chainServer struct {
	srv   *httptest.Server
	start time.Time

	mu       sync.Mutex
	offset   uint64 // blocks added by jump
	items    int
	requests int
	maxItems int
	samples  int // state samples answered (getGasPricingConstraints)
}

func word(v uint64) []byte {
	out := make([]byte, 32)
	new(big.Int).SetUint64(v).FillBytes(out)
	return out
}

func words(vs ...uint64) []byte {
	out := make([]byte, 0, 32*len(vs))
	for _, v := range vs {
		out = append(out, word(v)...)
	}
	return out
}

func addrWord(addr string) []byte {
	b, _ := nitro.DecodeHex(addr)
	out := make([]byte, 32)
	copy(out[12:], b)
	return out
}

func newChainServer(t *testing.T) *chainServer {
	t.Helper()
	c := &chainServer{start: time.Now()}
	c.srv = httptest.NewServer(http.HandlerFunc(c.serve))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *chainServer) head() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return 1000 + c.offset + uint64(time.Since(c.start).Seconds()*10)
}

// jump advances the head by n blocks at once, so the next catch-up asks
// for a header batch larger than the bucket holds.
func (c *chainServer) jump(n uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += n
}

func (c *chainServer) block(n uint64, full bool) map[string]any {
	txs := []any{"0x1", "0x2", "0x3"}
	if full {
		txs = []any{
			map[string]any{"hash": "0x1", "type": "0x2", "to": "0xdead", "input": "0x"},
			map[string]any{"hash": "0x2", "type": "0x2", "to": "0xdead", "input": "0x"},
			map[string]any{"hash": "0x3", "type": "0x2", "to": "0xdead", "input": "0x"},
		}
	}
	return map[string]any{
		"number": fmt.Sprintf("0x%x", n), "hash": fmt.Sprintf("0x%x", n), "parentHash": fmt.Sprintf("0x%x", n-1),
		"timestamp": fmt.Sprintf("0x%x", c.start.Unix()-100+int64(n/10)), "gasUsed": "0xf4240", "gasLimit": "0x1e84800",
		"baseFeePerGas": "0x1312d00", "l1BlockNumber": "0x32", "transactions": txs,
	}
}

// answer handles one call. Errors are returned as *nitro.RPCError.
func (c *chainServer) answer(method string, params []json.RawMessage) any {
	switch method {
	case "eth_chainId":
		return "0x1237"
	case "eth_blockNumber":
		return fmt.Sprintf("0x%x", c.head())
	case "eth_getBlockByNumber":
		var tag string
		var full bool
		_ = json.Unmarshal(params[0], &tag)
		_ = json.Unmarshal(params[1], &full)
		n := c.head()
		if tag != "latest" {
			if v, err := nitro.HexUint64(tag); err == nil {
				n = v
			}
		}
		if n > c.head() {
			return nil
		}
		return c.block(n, full)
	case "eth_getBlockReceipts":
		var tag string
		_ = json.Unmarshal(params[0], &tag)
		n := c.head()
		if tag != "latest" {
			if v, err := nitro.HexUint64(tag); err == nil {
				n = v
			}
		}
		if n > c.head() {
			return nil
		}
		blockHash := fmt.Sprintf("0x%x", n)
		return []any{
			map[string]any{"blockHash": blockHash, "blockNumber": tag, "transactionHash": "0x1", "transactionIndex": "0x0", "gasUsed": "0x493e0", "cumulativeGasUsed": "0x493e0", "gasUsedForL1": "0x0"},
			map[string]any{"blockHash": blockHash, "blockNumber": tag, "transactionHash": "0x2", "transactionIndex": "0x1", "gasUsed": "0x493e0", "cumulativeGasUsed": "0x927c0", "gasUsedForL1": "0x0"},
			map[string]any{"blockHash": blockHash, "blockNumber": tag, "transactionHash": "0x3", "transactionIndex": "0x2", "gasUsed": "0x61a80", "cumulativeGasUsed": "0xf4240", "gasUsedForL1": "0x0"},
		}
	case "eth_getLogs":
		return []any{}
	case "eth_getBalance":
		return "0x64"
	case "eth_call":
		var arg struct {
			Data string `json:"data"`
		}
		_ = json.Unmarshal(params[0], &arg)
		if len(arg.Data) < 10 {
			return &nitro.RPCError{Code: -32602, Message: "bad call"}
		}
		if arg.Data[:10] == nitro.SelectorHex(nitro.SigGetGasPricingConstraints) {
			c.mu.Lock()
			c.samples++
			c.mu.Unlock()
		}
		return nitro.EncodeHex(c.callResult(arg.Data[:10]))
	}
	return &nitro.RPCError{Code: -32601, Message: "method not found: " + method}
}

func (c *chainServer) callResult(selector string) []byte {
	switch selector {
	case nitro.SelectorHex(nitro.SigGetGasPricingConstraints):
		return nitro.EncodeSetGasPricingConstraints([]nitro.ConstraintParam{{GasTargetPerSecond: 60_000_000, AdjustmentWindowSeconds: 15, StartingBacklog: 3_111_506}, {GasTargetPerSecond: 40_000_000, AdjustmentWindowSeconds: 86_400, StartingBacklog: 11_194_391_810_886}})[4:]
	case nitro.SelectorHex(nitro.SigGetPricesInWei):
		return words(1, 2, 3, 4, 5, 6)
	case nitro.SelectorHex(nitro.SigGetMinimumGasPrice):
		return words(20_000_000)
	case nitro.SelectorHex(nitro.SigGetL1BaseFeeEstimate), nitro.SelectorHex(nitro.SigGetL1PricingSurplus), nitro.SelectorHex(nitro.SigGetL1FeesAvailable),
		nitro.SelectorHex(nitro.SigGetL1PricingUnitsSinceUpdate), nitro.SelectorHex(nitro.SigGetLastL1PricingUpdateTime), nitro.SelectorHex(nitro.SigGetL1PricingEquilibrationUnit),
		nitro.SelectorHex(nitro.SigGetPerBatchGasCharge), nitro.SelectorHex(nitro.SigGetL1RewardRate):
		return words(7)
	case nitro.SelectorHex(nitro.SigGetL1RewardRecipient), nitro.SelectorHex(nitro.SigGetInfraFeeAccount), nitro.SelectorHex(nitro.SigGetNetworkFeeAccount):
		return addrWord("0x5a2b80a9b7effc06129bd5462d77bc20a8a59be7")
	case nitro.SelectorHex(nitro.SigArbOSVersion):
		return words(116)
	}
	return words(0)
}

func (c *chainServer) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var reqs []struct {
		ID     uint64            `json:"id"`
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &reqs); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	c.requests++
	c.items += len(reqs)
	c.maxItems = max(c.maxItems, len(reqs))
	c.mu.Unlock()
	out := make([]map[string]any, 0, len(reqs))
	for _, req := range reqs {
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		res := c.answer(req.Method, req.Params)
		if e, ok := res.(*nitro.RPCError); ok {
			resp["error"] = e
		} else {
			resp["result"] = res
		}
		out = append(out, resp)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (c *chainServer) counts() (items, requests, maxItems int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.items, c.requests, c.maxItems
}

// sampled returns how many state samples the fast tick has taken.
func (c *chainServer) sampled() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.samples
}

// TestLoopsRespectEndpointBudget: the fast loop, the slow loop and the backfill running together never
// put more calls on the wire than the endpoint's bucket allows, never send a batch it cannot hold at
// once, and all still make progress. The network's own 250 ms tick makes the fast loop demanding (six
// calls per tick, four ticks a second, over the 20/s budget), which under strict fast priority would
// have starved the bulk loops.
func TestLoopsRespectEndpointBudget(t *testing.T) {
	chain := newChainServer(t)
	const rate = 20.0
	burst := int(2 * rate)
	t0 := time.Now()
	pool := nitro.NewPool(nitro.PoolConfig{
		ChainID:   4663,
		Endpoints: []config.EndpointConfig{{RPCURL: chain.srv.URL, CallsPerSecond: rate}},
		BatchSize: 100,
	}, nitro.WithPoolClientOptions(nitro.WithHTTPClient(chain.srv.Client())))
	store := dbtest.New()
	f := NewFollower(Options{
		Network: config.NetworkConfig{Name: "robinhood", ChainID: 4663, Enabled: true, CallsPerSecond: rate, TickInterval: 250 * time.Millisecond},
		Collector: config.CollectorConfig{
			TickInterval: time.Second, SlowInterval: 500 * time.Millisecond, HeaderBatchSize: 100,
			BlockRetention: time.Hour, SampleRetention: time.Hour, BackfillDepth: 30 * time.Second,
		},
		RPC:   pool,
		Store: store,
		Log:   logger.Nop(),
		Sleep: quickSleep,
	})
	if f.TickInterval() != 250*time.Millisecond {
		t.Fatalf("the network's tick_interval must win: %v", f.TickInterval())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// A jump of 45 blocks makes the catch-up ask for more headers than the
	// bucket holds at once: the batch is chunked to the burst minus the fast
	// reserve and waits its turn for a full bucket (two seconds at this
	// rate). The run ends once
	// the follower has caught up past the jump.
	done := make(chan struct{})
	go func() {
		_ = f.Run(ctx)
		close(done)
	}()
	time.Sleep(300 * time.Millisecond)
	chain.jump(45)
	// The run ends once the follower has caught up past the jump, the
	// backfill, sharing the budget with the demanding tick, has folded at
	// least one batch (its cursor moved past the segment start, or a
	// segment is finished), and the tick has kept sampling beside it.
	progressed := func() bool {
		c, err := f.loadCursor(ctx)
		return err == nil && (c.Done || c.Next > c.SegStart || (c.Top > 0 && c.End < c.Top))
	}
	const minSamples = 5
	deadline := time.Now().Add(20 * time.Second)
	for (f.Head() < 1045 || !progressed() || chain.sampled() < minSamples) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	elapsed := time.Since(t0)
	items, requests, maxItems := chain.counts()
	budget := burst + int(rate*elapsed.Seconds()) + 1
	if items > budget {
		t.Fatalf("%d calls in %s exceed the budget of %d (burst %d + %v/s)", items, elapsed, budget, burst, rate)
	}
	if maxItems > burst {
		t.Fatalf("a batch of %d items exceeds what the bucket holds (%d)", maxItems, burst)
	}
	// Every loop ran: several fast ticks, the slow loop's checkpoints, and
	// backfill batches paced by what the bucket had to spare.
	if f.Head() < 1045 || requests < 10 {
		t.Fatalf("fast loop must catch up past the jump: head %d, %d requests", f.Head(), requests)
	}
	if blocks, _ := store.BlocksAfter(context.Background(), 4663, 1000, 1000); len(blocks) < 45 {
		t.Fatalf("catch-up headers: %d blocks", len(blocks))
	}
	if bulkMax := pool.Endpoints()[0].Pacer().MaxBatch(); maxItems != bulkMax || bulkMax != burst-int(rate/4) {
		t.Fatalf("the oversized header batch must be chunked to exactly the burst minus the fast reserve (%d): %d", bulkMax, maxItems)
	}
	if _, ok, _ := store.GetState(ctx, 4663, db.StateArbOSVersion); !ok {
		t.Fatal("slow loop did not run")
	}
	if v, _, _ := store.GetState(ctx, 4663, db.StateOwnerScanThrough); v == "" {
		t.Fatal("owner scan did not complete a pass")
	}
	c, err := f.loadCursor(context.Background())
	if err != nil || !progressed() {
		t.Fatalf("backfill made no progress beside the demanding tick: %+v %v", c, err)
	}
	// The tick kept sampling while the bulk loops ran, at least once a
	// second from the reserve alone.
	samples := chain.sampled()
	// A fast sample now costs six calls because receipt poster gas is part
	// of the atomic head sample. Under simultaneous history work the loop
	// must still complete at least one sample every five seconds.
	if samples < minSamples || float64(samples) < elapsed.Seconds()/5 {
		t.Fatalf("the fast loop stalled behind bulk work: %d samples in %s", samples, elapsed)
	}
	t.Logf("%d calls in %d requests over %s (budget %d, largest batch %d, head %d, %d samples, backfill next %d of %d..%d)", items, requests, elapsed, budget, maxItems, f.Head(), samples, c.Next, c.SegStart, c.End)
}
