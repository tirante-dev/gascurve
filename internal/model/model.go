// Package model holds the JSON shapes shared by the collector (which builds
// the LiveSnapshot it publishes with NOTIFY) and the API (which serves the
// same shapes over REST and WebSocket). Field names follow
// docs/ARCHITECTURE.md section 6 exactly.
package model

import "encoding/json"

// Model names.
const (
	ModelConstraints = "constraints"
	ModelLegacy      = "legacy"
	ModelUnknown     = "unknown"
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
	Number  uint64 `json:"number"`
	TS      uint64 `json:"ts"`
	GasUsed uint64 `json:"gasUsed"`
	BaseFee string `json:"baseFee"`
	TxCount int    `json:"txCount"`
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

// EthUsd is the ETH/USD spot the collector's slow loop fetched (server
// side, never the browser). Price is a decimal string with two decimal
// places, At is when it was fetched and Source names the provider
// (coinbase, coingecko or custom, never a URL). A snapshot carries null
// instead once the quote is older than collector.eth_usd_max_age.
type EthUsd struct {
	Price  string `json:"price"`
	At     string `json:"at"`
	Source string `json:"source"`
}

// LiveSnapshot is the per-tick state, published with NOTIFY and served by
// /live and the WebSocket tick message.
type LiveSnapshot struct {
	ChainID         uint64        `json:"chainId"`
	SampledAt       string        `json:"sampledAt"`
	Block           LiveBlock     `json:"block"`
	BaseFee         string        `json:"baseFee"`
	MinBaseFee      string        `json:"minBaseFee"`
	MultiplierBips  int64         `json:"multiplierBips"`
	ExponentBips    int64         `json:"exponentBips"`
	Model           string        `json:"model"`
	Constraints     []Constraint  `json:"constraints"`
	Legacy          *LegacyParams `json:"legacy,omitempty"`
	Prices          Prices        `json:"prices"`
	GasPerSecond    GasPerSecond  `json:"gasPerSecond"`
	L1              *L1           `json:"l1,omitempty"`
	Accounts        *Accounts     `json:"accounts,omitempty"`
	ReplayErrorBips int64         `json:"replayErrorBips"`
	// EthUsd is null when no spot was fetched or the last one is stale.
	EthUsd *EthUsd `json:"ethUsd"`
}

// BlockPoint is one replayed block. Backlogs are the end-of-block values,
// ConstraintBips the start-of-block per-constraint exponents that priced
// the block (they sum to ExponentBips; null for a block stored before they
// were recorded, an empty array for a legacy block) and MinBaseFee the
// floor in force, null for that same history (pricing version 0), where
// the floor was never recorded.
type BlockPoint struct {
	Number           uint64   `json:"number"`
	TS               uint64   `json:"ts"`
	GasUsed          uint64   `json:"gasUsed"`
	BaseFee          string   `json:"baseFee"`
	PredictedBaseFee string   `json:"predictedBaseFee"`
	Backlogs         []uint64 `json:"backlogs"`
	ConstraintBips   []int64  `json:"constraintBips"`
	ExponentBips     int64    `json:"exponentBips"`
	MinBaseFee       *string  `json:"minBaseFee"`
	Anchored         bool     `json:"anchored"`
}

// SeriesPoint is one bucket of a Series. ExponentBips, ConstraintBips,
// Backlogs and MinBaseFee describe the bucket's last block; FloorFeesWei
// is the sum of gasUsed times the minimum base fee per block and
// SurplusFeesWei is FeesWei minus that. ConstraintBips, MinBaseFee,
// FloorFeesWei and SurplusFeesWei are null for history whose pricing
// breakdown was never recorded (pricing version 0),
// including a bucket any of whose source blocks is such history; they are
// never null otherwise.
type SeriesPoint struct {
	T       int64  `json:"t"`
	Blocks  int64  `json:"blocks"`
	GasUsed uint64 `json:"gasUsed"`
	// GasPerSecond is the rate over the covered span of the bucket. Coverage
	// is the share of the bucket that span is after subtracting bounded missing
	// intervals, or null when the missing-range time bounds cannot measure it.
	// Completeness distinguishes a whole aggregate, a known partial aggregate
	// and an aggregate whose completeness cannot be located in time.
	GasPerSecond    uint64   `json:"gasPerSecond"`
	Coverage        *float64 `json:"coverage"`
	Completeness    string   `json:"completeness"`
	FeesWei         string   `json:"feesWei"`
	BaseFeeMin      string   `json:"baseFeeMin"`
	BaseFeeAvg      string   `json:"baseFeeAvg"`
	BaseFeeMax      string   `json:"baseFeeMax"`
	ExponentBips    int64    `json:"exponentBips"`
	ConstraintBips  []int64  `json:"constraintBips"`
	Backlogs        []uint64 `json:"backlogs"`
	BacklogsMax     []uint64 `json:"backlogsMax"`
	MinBaseFee      *string  `json:"minBaseFee"`
	FloorFeesWei    *string  `json:"floorFeesWei"`
	SurplusFeesWei  *string  `json:"surplusFeesWei"`
	ConstraintSetID int64    `json:"constraintSetId"`
	ReplayErrorBips int64    `json:"replayErrorBips"`
}

// Reorg is the WebSocket reorg message: the collector replaced blocks at
// or below the ring's tip, so the client drops every block above Ancestor
// and appends Blocks (the canonical replacements, oldest first, never
// null) before any later blocks message.
type Reorg struct {
	ChainID  uint64       `json:"chainId"`
	Ancestor uint64       `json:"ancestor"`
	Blocks   []BlockPoint `json:"blocks"`
}

// Series is a history response.
type Series struct {
	Range      string `json:"range"`
	Resolution string `json:"resolution"`
	// From and To bound the requested window in unix seconds, whatever the
	// points cover: a chart draws the whole window and shows what is not
	// indexed as missing rather than stretching the data across it. For
	// the all range From is the first indexed point (To when there is none).
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

// EndpointStatus describes one RPC endpoint of a network in /status.
// Index 0 is the primary; URLs are never exposed because they can carry
// keys. Error is why a disabled endpoint was disabled, sanitized the same
// way (chain ids, never a URL or a credential), and null while the
// endpoint is usable. WSCooling and WSError report the endpoint's
// WebSocket separately from its JSON-RPC: a socket that cannot be dialed,
// cannot be subscribed to or will not stay up is cooled down and the head
// subscription moves to another endpoint, while the endpoint keeps serving
// ordinary calls.
type EndpointStatus struct {
	Index     int     `json:"index"`
	WS        bool    `json:"ws"`
	Archive   bool    `json:"archive"`
	Disabled  bool    `json:"disabled"`
	Error     *string `json:"error"`
	WSCooling bool    `json:"wsCooling"`
	WSError   *string `json:"wsError"`
}

// EndpointsStatus is the routing state of a network's endpoint pool: the
// endpoint ordinary calls go to, how often the collector failed over and
// every endpoint's capabilities. The collector stores it in
// collector_state under the endpoints key; Endpoints is never null.
type EndpointsStatus struct {
	ActiveEndpoint int              `json:"activeEndpoint"`
	Failovers      uint64           `json:"failovers"`
	Endpoints      []EndpointStatus `json:"endpoints"`
}

// Missing-range lifecycle states. Pending work is ready now, retrying work is
// delayed until its durable retry time, and blocked work has no replay state
// to continue from. Blocked ranges are re-examined because later backfill or a
// newly configured archive endpoint can make them recoverable.
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
	// HoleReasonNoState marks a hole no replay can fill right now: no
	// block before it is stored with a pricing state and the hole carries
	// no replay state of its own, so nothing can be replayed forward into
	// it. The gap filler skips these and only looks at them again once
	// something before the range appears.
	HoleReasonNoState = "no state"
	// HoleReasonExpired is accepted only while importing the old bounded JSON
	// checkpoint. Those ranges become pending again in durable storage.
	HoleReasonExpired = "expired"
)

// HoleState is the pricer state at the end of the last block a filler
// committed for a hole, carried in the hole record itself so a
// continuation never depends on a stored block at Next-1, which retention
// may have pruned. Block is that last block and Hash its hash, so a
// stored predecessor can be cross-checked against it; PrevTS is its
// timestamp, the previous timestamp the next replay step needs.
// Constraints carry the end-of-block backlogs of the constraints model and
// Legacy the whole legacy state (parameters and backlog); exactly one of
// them is set. SetID is the constraint set in force at Block, 0 when none
// is recorded.
type HoleState struct {
	Block       uint64        `json:"block"`
	Hash        string        `json:"hash,omitempty"`
	PrevTS      uint64        `json:"prevTs"`
	MinBaseFee  string        `json:"minBaseFee,omitempty"`
	Constraints []Constraint  `json:"constraints,omitempty"`
	Legacy      *LegacyParams `json:"legacy,omitempty"`
	SetID       int64         `json:"setId,omitempty"`
}

// Hole is the collector's domain shape for one durable missing_ranges row and
// the decoder for the legacy JSON checkpoint. Next is the first block still
// missing, State describes Next-1, and Folded prevents additive bucket work
// from being counted twice after a rewind. Lifecycle, reason and retry fields
// describe recovery. The three time bounds let the API map the remaining
// interval without decoding State.
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
	Pending          int    `json:"pending"`
	Blocks           uint64 `json:"blocks"`
	Unfillable       int    `json:"unfillable"`
	Retrying         int    `json:"retrying"`
	OldestAgeSeconds uint64 `json:"oldestAgeSeconds"`
	CheckpointError  bool   `json:"checkpointError"`
}

// RPCCapacity is the latest estimate of the calls per second required to
// sample the head and fetch intervening headers compared with the active
// endpoint's configured budget. A configured value of zero is unlimited.
// Saturated means observed ingress demand exceeded that finite budget.
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
	out := HolesStatus{}
	for _, h := range holes {
		switch h.Lifecycle {
		case MissingRangeRetrying:
			out.Pending++
			out.Retrying++
		case MissingRangeBlocked:
			out.Unfillable++
		case MissingRangePending:
			out.Pending++
		default:
			// Compatibility with the legacy checkpoint, where an empty reason
			// was pending and any reason meant unfillable.
			if h.Reason == "" || h.Reason == HoleReasonExpired {
				out.Pending++
			} else {
				out.Unfillable++
			}
		}
		out.Blocks += h.Blocks()
	}
	return out
}

// NetworkStatus is the collector status of one network. HeadAt,
// LagSeconds and LastSampleAt are null before the first head.
type NetworkStatus struct {
	Name            string      `json:"name"`
	ChainID         uint64      `json:"chainId"`
	Enabled         bool        `json:"enabled"`
	HeadBlock       uint64      `json:"headBlock"`
	HeadAt          *string     `json:"headAt"`
	LagSeconds      *int64      `json:"lagSeconds"`
	LastSampleAt    *string     `json:"lastSampleAt"`
	LastError       *string     `json:"lastError"`
	RateLimitEvents uint64      `json:"rateLimitEvents"`
	Last429At       *string     `json:"last429At"`
	BackfillCursor  *string     `json:"backfillCursor"`
	ArbOSVersion    *string     `json:"arbosVersion"`
	Degraded        bool        `json:"degraded"`
	Capacity        RPCCapacity `json:"capacity"`
	Holes           HolesStatus `json:"holes"`
	EndpointsStatus
}

// ListenerStatus is the API's PostgreSQL notification feed status. LastError
// is null while the listener is ready.
type ListenerStatus struct {
	Ready      bool    `json:"ready"`
	Reconnects uint64  `json:"reconnects"`
	LastError  *string `json:"lastError"`
}

// Status is the /status response. Listener is omitted only for an API server
// built without a live WebSocket feed.
type Status struct {
	Version  string          `json:"version"`
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
