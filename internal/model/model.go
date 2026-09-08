// Package model holds the JSON shapes shared by the collector (which builds
// the LiveSnapshot it publishes with NOTIFY) and the API (which serves the
// same shapes over REST and WebSocket). Field names follow
// docs/ARCHITECTURE.md section 6 exactly.
package model

import (
	"encoding/json"
	"time"
)

// Model names.
const (
	ModelConstraints = "constraints"
	ModelLegacy      = "legacy"
	ModelUnknown     = "unknown"
)

// Health status values used by /status.
const (
	StatusHealthy  = "healthy"
	StatusDegraded = "degraded"
	StatusDisabled = "disabled"
)

// Series bucket completeness states.
const (
	SeriesComplete = "complete"
	SeriesPartial  = "partial"
	SeriesUnknown  = "unknown"
)

// Constraint set sources.
const (
	SourceGenesis     = "genesis"
	SourceOwnerAction = "owner_action"
	SourceObserved    = "observed"
)

// Network is a configured chain and its collector head. HeadAt and
// LagSeconds are null until the collector has produced a head.
type Network struct {
	Name        string  `json:"name"`
	DisplayName string  `json:"displayName"`
	ChainID     uint64  `json:"chainId"`
	ExplorerURL string  `json:"explorerUrl"`
	Model       string  `json:"model"`
	HeadBlock   uint64  `json:"headBlock"`
	HeadAt      *string `json:"headAt"`
	LagSeconds  *int64  `json:"lagSeconds"`
	Enabled     bool    `json:"enabled"`
}

// Constraint is a live pricing constraint with its exponent share.
type Constraint struct {
	Target       uint64 `json:"target"`
	Window       uint64 `json:"window"`
	Backlog      uint64 `json:"backlog"`
	ExponentBips int64  `json:"exponentBips"`
}

// ConstraintSetEntry is one constraint of a ConstraintSet.
type ConstraintSetEntry struct {
	Target          uint64 `json:"target"`
	Window          uint64 `json:"window"`
	StartingBacklog uint64 `json:"startingBacklog"`
}

// ConstraintSet is a constraint configuration in force from EffectiveBlock.
type ConstraintSet struct {
	ID             int64                `json:"id"`
	EffectiveBlock uint64               `json:"effectiveBlock"`
	EffectiveAt    string               `json:"effectiveAt"`
	Source         string               `json:"source"`
	Constraints    []ConstraintSetEntry `json:"constraints"`
}

// LiveBlock is the head block summary inside a LiveSnapshot.
type LiveBlock struct {
	Number    uint64  `json:"number"`
	TS        uint64  `json:"ts"`
	GasUsed   uint64  `json:"gasUsed"`
	PosterGas *uint64 `json:"posterGas"`
	BaseFee   string  `json:"baseFee"`
	TxCount   int     `json:"txCount"`
}

// LegacyParams is the legacy pricer state.
type LegacyParams struct {
	SpeedLimit uint64 `json:"speedLimit"`
	Inertia    uint64 `json:"inertia"`
	Tolerance  uint64 `json:"tolerance"`
	Backlog    uint64 `json:"backlog"`
}

// Prices is the getPricesInWei tuple.
type Prices struct {
	PerL2Tx             string `json:"perL2Tx"`
	PerL1CalldataByte   string `json:"perL1CalldataByte"`
	PerL2Storage        string `json:"perL2Storage"`
	PerArbGasBase       string `json:"perArbGasBase"`
	PerArbGasCongestion string `json:"perArbGasCongestion"`
	PerArbGasTotal      string `json:"perArbGasTotal"`
}

// GasPerSecond holds rolling gas rates.
type GasPerSecond struct {
	S10 uint64 `json:"s10"`
	S60 uint64 `json:"s60"`
}

// NullableGasPerSecond holds rolling rates that are unknown until every
// source block has authoritative receipt data.
type NullableGasPerSecond struct {
	S10 *uint64 `json:"s10"`
	S60 *uint64 `json:"s60"`
}

// L1 holds the L1 pricer getters.
type L1 struct {
	BaseFeeEstimate    string `json:"baseFeeEstimate"`
	Surplus            string `json:"surplus"`
	FeesAvailable      string `json:"feesAvailable"`
	UnitsSinceUpdate   uint64 `json:"unitsSinceUpdate"`
	LastUpdateAt       string `json:"lastUpdateAt"`
	EquilibrationUnits uint64 `json:"equilibrationUnits"`
	PerBatchGasCharge  int64  `json:"perBatchGasCharge"`
	RewardRate         uint64 `json:"rewardRate"`
}

// Account is an address and balance.
type Account struct {
	Address string `json:"address"`
	Balance string `json:"balance"`
}

// Accounts are the fee destinations.
type Accounts struct {
	Infra    Account `json:"infra"`
	Network  Account `json:"network"`
	L1Reward Account `json:"l1Reward"`
}

// EthUsd is the ETH/USD spot the collector's slow loop fetched. Source names the provider
// (coinbase, coingecko or custom, never a URL). A snapshot carries null instead once the quote is
// older than collector.eth_usd_max_age.
type EthUsd struct {
	Price  string `json:"price"`
	At     string `json:"at"`
	Source string `json:"source"`
}

// LiveSnapshot is the per-tick state, published with NOTIFY and served by
// /live and the WebSocket tick message.
type LiveSnapshot struct {
	ChainID        uint64        `json:"chainId"`
	SampledAt      string        `json:"sampledAt"`
	Block          LiveBlock     `json:"block"`
	BaseFee        string        `json:"baseFee"`
	MinBaseFee     string        `json:"minBaseFee"`
	MultiplierBips int64         `json:"multiplierBips"`
	ExponentBips   int64         `json:"exponentBips"`
	Model          string        `json:"model"`
	Constraints    []Constraint  `json:"constraints"`
	Legacy         *LegacyParams `json:"legacy,omitempty"`
	Prices         Prices        `json:"prices"`
	// GasPerSecond retains the original total-gas API contract.
	GasPerSecond        GasPerSecond         `json:"gasPerSecond"`
	ComputeGasPerSecond NullableGasPerSecond `json:"computeGasPerSecond"`
	L1                  *L1                  `json:"l1,omitempty"`
	Accounts            *Accounts            `json:"accounts,omitempty"`
	ReplayErrorBips     int64                `json:"replayErrorBips"`
	// EthUsd is null when no spot was fetched or the last one is stale.
	EthUsd *EthUsd `json:"ethUsd"`
}

// BlockPoint is one replayed block. Backlogs are end-of-block values, ConstraintBips the
// start-of-block per-constraint exponents that priced it (summing to ExponentBips), and MinBaseFee
// the floor in force. The last two are null for history stored before they were recorded.
type BlockPoint struct {
	Number           uint64   `json:"number"`
	TS               uint64   `json:"ts"`
	GasUsed          uint64   `json:"gasUsed"`
	PosterGas        *uint64  `json:"posterGas"`
	BaseFee          string   `json:"baseFee"`
	PredictedBaseFee *string  `json:"predictedBaseFee"`
	Backlogs         []uint64 `json:"backlogs"`
	ConstraintBips   []int64  `json:"constraintBips"`
	ExponentBips     int64    `json:"exponentBips"`
	MinBaseFee       *string  `json:"minBaseFee"`
	Anchored         bool     `json:"anchored"`
}

// SeriesPoint is one bucket of a Series, its pricing fields describing the bucket's last block.
// FloorFeesWei is compute gas times min(base fee, minimum base fee), SurplusFeesWei the remaining
// compute fee, PosterFeesWei poster gas times base fee. Poster and compute-per-second fields are
// null until every source block has authoritative receipt poster gas.
type SeriesPoint struct {
	T         int64   `json:"t"`
	Blocks    int64   `json:"blocks"`
	GasUsed   uint64  `json:"gasUsed"`
	PosterGas *uint64 `json:"posterGas"`
	// GasPerSecond is the rate over the covered span of the bucket. Coverage is the share of the
	// bucket that span is, or null when the missing-range time bounds cannot measure it.
	// Completeness separates a whole aggregate, a known partial one and one that cannot be located.
	GasPerSecond        uint64   `json:"gasPerSecond"`
	ComputeGasPerSecond *uint64  `json:"computeGasPerSecond"`
	Coverage            *float64 `json:"coverage"`
	Completeness        string   `json:"completeness"`
	FeesWei             string   `json:"feesWei"`
	PosterFeesWei       *string  `json:"posterFeesWei"`
	BaseFeeMin          string   `json:"baseFeeMin"`
	BaseFeeAvg          string   `json:"baseFeeAvg"`
	BaseFeeMax          string   `json:"baseFeeMax"`
	ExponentBips        int64    `json:"exponentBips"`
	ConstraintBips      []int64  `json:"constraintBips"`
	Backlogs            []uint64 `json:"backlogs"`
	BacklogsMax         []uint64 `json:"backlogsMax"`
	MinBaseFee          *string  `json:"minBaseFee"`
	FloorFeesWei        *string  `json:"floorFeesWei"`
	SurplusFeesWei      *string  `json:"surplusFeesWei"`
	ConstraintSetID     int64    `json:"constraintSetId"`
	ReplayErrorBips     int64    `json:"replayErrorBips"`
}

// Reorg is the WebSocket reorg message: the collector replaced blocks at or below the ring's tip,
// so the client drops everything above Ancestor and appends Blocks (oldest first, never null)
// before any later blocks message.
type Reorg struct {
	ChainID  uint64       `json:"chainId"`
	Ancestor uint64       `json:"ancestor"`
	Blocks   []BlockPoint `json:"blocks"`
}

// Series is a history response.
type Series struct {
	Range      string `json:"range"`
	Resolution string `json:"resolution"`
	// From and To bound the requested window in unix seconds, whatever the points cover, so a chart
	// draws the whole window and shows what is not indexed as missing. For the all range From is the
	// first indexed point.
	From           int64           `json:"from"`
	To             int64           `json:"to"`
	ConstraintSets []ConstraintSet `json:"constraintSets"`
	OwnerActions   []OwnerAction   `json:"ownerActions"`
	Points         []SeriesPoint   `json:"points"`
}

// OwnerAction is a decoded OwnerActs event.
type OwnerAction struct {
	Block    uint64          `json:"block"`
	At       string          `json:"at"`
	TxHash   string          `json:"txHash"`
	Method   string          `json:"method"`
	Selector string          `json:"selector"`
	Args     json.RawMessage `json:"args"`
}

// BatchPoint is one bucket of version-aware, ArbOS-attributed batch-poster
// spending. WeiSpent is not an Ethereum receipt total.
type BatchPoint struct {
	T             int64  `json:"t"`
	Batches       int64  `json:"batches"`
	GasSpent      uint64 `json:"gasSpent"`
	WeiSpent      string `json:"weiSpent"`
	L1BaseFeeAvg  string `json:"l1BaseFeeAvg"`
	CalldataBytes uint64 `json:"calldataBytes"`
}

// BatchSeries is the /batches response.
type BatchSeries struct {
	Range      string       `json:"range"`
	Resolution string       `json:"resolution"`
	From       int64        `json:"from"`
	To         int64        `json:"to"`
	Points     []BatchPoint `json:"points"`
}

// L1Point is one sample of the L1 pricer getters.
type L1Point struct {
	T                int64  `json:"t"`
	BaseFeeEstimate  string `json:"baseFeeEstimate"`
	Surplus          string `json:"surplus"`
	FeesAvailable    string `json:"feesAvailable"`
	UnitsSinceUpdate uint64 `json:"unitsSinceUpdate"`
}

// L1Series is the /l1 response.
type L1Series struct {
	Range  string    `json:"range"`
	From   int64     `json:"from"`
	To     int64     `json:"to"`
	Points []L1Point `json:"points"`
}

// EndpointStatus describes one RPC endpoint of a network in /status. Index 0 is the primary; URLs
// are never exposed because they can carry keys, and Error is sanitized the same way. WSCooling
// and WSError report the WebSocket separately from the JSON-RPC: a socket that will not stay up is
// cooled down and the head subscription moves on while the endpoint keeps serving ordinary calls.
type EndpointStatus struct {
	Index     int     `json:"index"`
	WS        bool    `json:"ws"`
	Archive   bool    `json:"archive"`
	Disabled  bool    `json:"disabled"`
	Error     *string `json:"error"`
	WSCooling bool    `json:"wsCooling"`
	WSError   *string `json:"wsError"`
}

// EndpointsStatus is the routing state of a network's endpoint pool. The collector stores it in
// collector_state under the endpoints key; Endpoints is never null.
type EndpointsStatus struct {
	ActiveEndpoint int              `json:"activeEndpoint"`
	Failovers      uint64           `json:"failovers"`
	Endpoints      []EndpointStatus `json:"endpoints"`
}

// Missing-range lifecycle states. Pending work is ready now, retrying work is delayed until its
// durable retry time, and blocked work has no replay state to continue from. Blocked ranges are
// re-examined because later backfill or a new archive endpoint can make them recoverable.
const (
	MissingRangePending  = "pending"
	MissingRangeRetrying = "retrying"
	MissingRangeBlocked  = "blocked"

	// HoleReasonCatchUpLimit marks a range skipped because the endpoint's
	// configured ingress capacity could not keep the live follower current.
	HoleReasonCatchUpLimit = "catch up limit"
	// HoleReasonReplayDiscontinuity marks a range skipped because the model
	// changed without enough recorded state to replay through it safely.
	HoleReasonReplayDiscontinuity = "replay discontinuity"
	// HoleReasonNoState marks a hole nothing can be replayed into right now: no stored block before
	// it carries a pricing state and the hole carries none of its own.
	HoleReasonNoState = "no state"
	// HoleReasonExpired is accepted only while importing the old bounded JSON
	// checkpoint. Those ranges become pending again in durable storage.
	HoleReasonExpired = "expired"
)

// HoleState is the pricer state at the end of the last block a filler committed, carried in the
// hole record itself so a continuation never depends on a stored block at Next-1 that retention may
// have pruned. Hash lets a stored predecessor be cross-checked; PrevTS is the previous timestamp the
// next replay step needs. Exactly one of Constraints and Legacy is set.
type HoleState struct {
	Block       uint64        `json:"block"`
	Hash        string        `json:"hash,omitempty"`
	PrevTS      uint64        `json:"prevTs"`
	MinBaseFee  string        `json:"minBaseFee,omitempty"`
	Constraints []Constraint  `json:"constraints,omitempty"`
	Legacy      *LegacyParams `json:"legacy,omitempty"`
	SetID       int64         `json:"setId,omitempty"`
	// The pricing group Block computed, which prices Block+1: the first block the next fill batch
	// writes. Empty on the first batch of a hole, leaving that block without a prediction.
	PendingFee      string  `json:"pendingFee,omitempty"`
	PendingExponent int64   `json:"pendingExponent,omitempty"`
	PendingBips     []int64 `json:"pendingBips,omitempty"`
}

// Hole is the collector's domain shape for one durable missing_ranges row and the decoder for the
// legacy JSON checkpoint. Next is the first block still missing, State describes Next-1, and Folded
// stops additive bucket work being counted twice after a rewind. The three time bounds let the API
// map the remaining interval without decoding State.
type Hole struct {
	From          uint64     `json:"from"`
	To            uint64     `json:"to"`
	At            string     `json:"at"`
	Lifecycle     string     `json:"lifecycle,omitempty"`
	Next          uint64     `json:"next,omitempty"`
	State         *HoleState `json:"state,omitempty"`
	Folded        uint64     `json:"folded,omitempty"`
	Reason        string     `json:"reason,omitempty"`
	RetryCount    uint64     `json:"retryCount,omitempty"`
	LastAttemptAt string     `json:"lastAttemptAt,omitempty"`
	NextRetryAt   string     `json:"nextRetryAt,omitempty"`
	LastError     string     `json:"lastError,omitempty"`
	PredecessorAt string     `json:"predecessorAt,omitempty"`
	SuccessorAt   string     `json:"successorAt,omitempty"`
	CursorAt      string     `json:"cursorAt,omitempty"`
}

// Start is the first block still to fill: the cursor when it has moved
// into the range, the range's own start otherwise.
func (h Hole) Start() uint64 {
	if h.Next > h.From && h.Next <= h.To {
		return h.Next
	}
	return h.From
}

// Blocks is how many blocks of the range are still not indexed.
func (h Hole) Blocks() uint64 {
	if h.To < h.Start() {
		return 0
	}
	return h.To - h.Start() + 1
}

// HolesStatus summarizes a network's missing ranges for /status.
type HolesStatus struct {
	Pending                 int     `json:"pending"`
	Blocks                  uint64  `json:"blocks"`
	Unfillable              int     `json:"unfillable"`
	Retrying                int     `json:"retrying"`
	OldestAgeSeconds        uint64  `json:"oldestAgeSeconds"`
	CheckpointError         bool    `json:"checkpointError"`
	PendingBlocks           uint64  `json:"pendingBlocks"`
	OldestPendingAt         *string `json:"oldestPendingAt"`
	OldestPendingAgeSeconds *int64  `json:"oldestPendingAgeSeconds"`
}

// RPCCapacity compares the calls per second needed to sample the head and fetch intervening headers
// with the active endpoint's configured budget (zero is unlimited). Saturated means observed demand
// exceeded a finite budget.
type RPCCapacity struct {
	ConfiguredCallsPerSecond float64  `json:"configuredCallsPerSecond"`
	RequiredCallsPerSecond   float64  `json:"requiredCallsPerSecond"`
	ObservedCallsPerSecond   float64  `json:"observedCallsPerSecond"`
	HeadroomCallsPerSecond   *float64 `json:"headroomCallsPerSecond"`
	Saturated                bool     `json:"saturated"`
	At                       *string  `json:"at"`
	CheckpointError          bool     `json:"checkpointError"`
}

// SummarizeHoles counts the recorded holes for /status.
func SummarizeHoles(holes []Hole) HolesStatus {
	return SummarizeHolesAt(holes, time.Time{})
}

// SummarizeHolesAt counts recorded holes and, when now is non-zero, reports
// the age of the oldest queued range. Unfillable history is still included
// in Blocks for compatibility, but not in PendingBlocks or pending age.
func SummarizeHolesAt(holes []Hole, now time.Time) HolesStatus {
	out := HolesStatus{}
	var oldest time.Time
	for _, h := range holes {
		pending := false
		switch h.Lifecycle {
		case MissingRangeRetrying:
			out.Pending++
			out.Retrying++
			pending = true
		case MissingRangeBlocked:
			out.Unfillable++
		case MissingRangePending:
			out.Pending++
			pending = true
		default:
			// Compatibility with the legacy checkpoint, where an empty reason
			// was pending and any reason meant unfillable.
			if h.Reason == "" || h.Reason == HoleReasonExpired {
				out.Pending++
				pending = true
			} else {
				out.Unfillable++
			}
		}
		if pending {
			out.PendingBlocks += h.Blocks()
			if at, err := time.Parse(time.RFC3339, h.At); err == nil && (oldest.IsZero() || at.Before(oldest)) {
				oldest = at
			}
		}
		out.Blocks += h.Blocks()
	}
	if !oldest.IsZero() {
		at := oldest.UTC().Format(time.RFC3339)
		out.OldestPendingAt = &at
		if !now.IsZero() {
			age := max(int64(now.Sub(oldest).Seconds()), 0)
			out.OldestPendingAgeSeconds = &age
		}
	}
	return out
}

// LoopStatus is the latest outcome of one collector loop. Both timestamps are retained so a
// recovered error stays diagnosable without marking the network degraded. ErrorStreak is what says
// a loop is failing: one error on a metered public RPC is routine and recovers on the next tick.
type LoopStatus struct {
	LastSuccessAt  *string `json:"lastSuccessAt"`
	LastErrorAt    *string `json:"lastErrorAt"`
	LastError      *string `json:"lastError"`
	ErrorStreak    int64   `json:"errorStreak"`
	LastDurationMS int64   `json:"lastDurationMs"`
	StaleAfterSecs int64   `json:"staleAfterSeconds"`
	// Phase names the long step the loop is inside, empty when it is not in one. It is what separates
	// a first pass that legitimately takes many minutes from a loop that is failing or wedged.
	Phase string `json:"phase,omitempty"`
}

// CollectorLoops reports the fast, slow and history loops separately.
type CollectorLoops struct {
	Fast    LoopStatus `json:"fast"`
	Slow    LoopStatus `json:"slow"`
	History LoopStatus `json:"history"`
}

// RPCMetrics is cumulative request accounting from the collector process.
// AverageLatencyMS measures HTTP round trips and excludes pacer waits.
type RPCMetrics struct {
	Calls              uint64  `json:"calls"`
	Requests           uint64  `json:"requests"`
	Errors             uint64  `json:"errors"`
	CallsLast10Seconds int     `json:"callsLast10Seconds"`
	RateLimitEvents    uint64  `json:"rateLimitEvents"`
	Last429At          *string `json:"last429At"`
	AverageLatencyMS   float64 `json:"averageLatencyMs"`
}

// DatabaseMetrics is process-wide PostgreSQL operation accounting. It is
// repeated in each network's durable collector checkpoint because
// collector_state is keyed by chain.
type DatabaseMetrics struct {
	Operations       uint64  `json:"operations"`
	Errors           uint64  `json:"errors"`
	AverageLatencyMS float64 `json:"averageLatencyMs"`
	LastLatencyMS    float64 `json:"lastLatencyMs"`
}

// CollectorTelemetry is the collector's durable heartbeat and in-process
// progress snapshot. HeartbeatAgeSeconds is derived by the API at serve time
// and is omitted from the stored checkpoint.
type CollectorTelemetry struct {
	HeartbeatAt                *string         `json:"heartbeatAt"`
	HeartbeatAgeSeconds        *int64          `json:"heartbeatAgeSeconds,omitempty"`
	HeartbeatStaleAfterSeconds int64           `json:"heartbeatStaleAfterSeconds"`
	ObservedHead               uint64          `json:"observedHead"`
	IndexedHead                uint64          `json:"indexedHead"`
	HeadLagBlocks              uint64          `json:"headLagBlocks"`
	Loops                      CollectorLoops  `json:"loops"`
	RPC                        RPCMetrics      `json:"rpc"`
	Database                   DatabaseMetrics `json:"database"`
}

// NetworkStatus is the collector status of one network. HeadAt,
// LagSeconds and LastSampleAt are null before the first head.
type NetworkStatus struct {
	Name            string              `json:"name"`
	ChainID         uint64              `json:"chainId"`
	Enabled         bool                `json:"enabled"`
	HeadBlock       uint64              `json:"headBlock"`
	HeadAt          *string             `json:"headAt"`
	LagSeconds      *int64              `json:"lagSeconds"`
	LastSampleAt    *string             `json:"lastSampleAt"`
	LastError       *string             `json:"lastError"`
	RateLimitEvents uint64              `json:"rateLimitEvents"`
	Last429At       *string             `json:"last429At"`
	BackfillCursor  *string             `json:"backfillCursor"`
	ArbOSVersion    *string             `json:"arbosVersion"`
	Degraded        bool                `json:"degraded"`
	Capacity        RPCCapacity         `json:"capacity"`
	Holes           HolesStatus         `json:"holes"`
	Status          string              `json:"status"`
	DegradedReasons []string            `json:"degradedReasons"`
	Collector       *CollectorTelemetry `json:"collector"`
	EndpointsStatus
}

// ListenerStatus is the API's PostgreSQL notification feed status. LastError
// is null while the listener is ready.
type ListenerStatus struct {
	Ready      bool    `json:"ready"`
	Reconnects uint64  `json:"reconnects"`
	LastError  *string `json:"lastError"`
}

// Status is the /status response. Status degrades when the notification
// listener or any enabled collector network is degraded. Listener is omitted
// only for an API server built without a live WebSocket feed.
type Status struct {
	Version  string          `json:"version"`
	Status   string          `json:"status"`
	Listener *ListenerStatus `json:"listener,omitempty"`
	Networks []NetworkStatus `json:"networks"`
}

// OwnerActionNotification is the payload of the gascurve_owner_action
// NOTIFY channel. LogIndex completes the (block, tx hash, log index) key
// the API de-duplicates deliveries by.
type OwnerActionNotification struct {
	ChainID  uint64      `json:"chainId"`
	LogIndex uint64      `json:"logIndex"`
	Action   OwnerAction `json:"action"`
}

// ErrorBody is the error envelope.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

// ErrorDetail is the inner error.
type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
