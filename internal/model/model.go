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

// Constraint set sources.
const (
	SourceGenesis     = "genesis"
	SourceOwnerAction = "owner_action"
	SourceObserved    = "observed"
)

// Network is a configured chain and its collector head.
type Network struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	ChainID     uint64 `json:"chainId"`
	ExplorerURL string `json:"explorerUrl"`
	Model       string `json:"model"`
	HeadBlock   uint64 `json:"headBlock"`
	HeadAt      string `json:"headAt"`
	LagSeconds  int64  `json:"lagSeconds"`
	Enabled     bool   `json:"enabled"`
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
}

// BlockPoint is one replayed block.
type BlockPoint struct {
	Number           uint64   `json:"number"`
	TS               uint64   `json:"ts"`
	GasUsed          uint64   `json:"gasUsed"`
	BaseFee          string   `json:"baseFee"`
	PredictedBaseFee string   `json:"predictedBaseFee"`
	Backlogs         []uint64 `json:"backlogs"`
	ExponentBips     int64    `json:"exponentBips"`
	Anchored         bool     `json:"anchored"`
}

// SeriesPoint is one bucket of a Series.
type SeriesPoint struct {
	T               int64    `json:"t"`
	Blocks          int64    `json:"blocks"`
	GasUsed         uint64   `json:"gasUsed"`
	GasPerSecond    uint64   `json:"gasPerSecond"`
	FeesWei         string   `json:"feesWei"`
	BaseFeeMin      string   `json:"baseFeeMin"`
	BaseFeeAvg      string   `json:"baseFeeAvg"`
	BaseFeeMax      string   `json:"baseFeeMax"`
	ExponentBips    int64    `json:"exponentBips"`
	Backlogs        []uint64 `json:"backlogs"`
	BacklogsMax     []uint64 `json:"backlogsMax"`
	ConstraintSetID int64    `json:"constraintSetId"`
	ReplayErrorBips int64    `json:"replayErrorBips"`
}

// Series is a history response.
type Series struct {
	Range          string          `json:"range"`
	Resolution     string          `json:"resolution"`
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

// BatchPoint is one bucket of batch posting reports.
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
	Points []L1Point `json:"points"`
}

// NetworkStatus is the collector status of one network.
type NetworkStatus struct {
	Name            string  `json:"name"`
	ChainID         uint64  `json:"chainId"`
	Enabled         bool    `json:"enabled"`
	HeadBlock       uint64  `json:"headBlock"`
	HeadAt          string  `json:"headAt"`
	LagSeconds      int64   `json:"lagSeconds"`
	LastSampleAt    string  `json:"lastSampleAt"`
	LastError       *string `json:"lastError"`
	RateLimitEvents uint64  `json:"rateLimitEvents"`
	Last429At       *string `json:"last429At"`
	BackfillCursor  *string `json:"backfillCursor"`
	ArbOSVersion    *string `json:"arbosVersion"`
}

// Status is the /status response.
type Status struct {
	Version  string          `json:"version"`
	Networks []NetworkStatus `json:"networks"`
}

// OwnerActionNotification is the payload of the gascurve_owner_action
// NOTIFY channel.
type OwnerActionNotification struct {
	ChainID uint64      `json:"chainId"`
	Action  OwnerAction `json:"action"`
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
