package nitro

import (
	"fmt"
	"math/big"
)

// JSON-RPC method names and tags.
const (
	methodGetBlockByNumber = "eth_getBlockByNumber"
	methodGetBlockReceipts = "eth_getBlockReceipts"
	latestTag              = "latest"
)

// Precompile addresses.
const (
	ArbSysAddress         = "0x0000000000000000000000000000000000000064"
	ArbOwnerPublicAddress = "0x000000000000000000000000000000000000006b"
	ArbGasInfoAddress     = "0x000000000000000000000000000000000000006c"
	ArbOwnerAddress       = "0x0000000000000000000000000000000000000070"
	// ArbosAddress is the sender and recipient of ArbOS internal transactions.
	ArbosAddress = "0x00000000000000000000000000000000000a4b05"
	// InternalTxType is the EIP-2718 type of ArbOS internal transactions.
	InternalTxType = 0x6a
	// ArbOSVersionOffset is what ArbSys.arbOSVersion() adds to the ArbOS version.
	ArbOSVersionOffset = 55
)

// OwnerActsTopic is topic0 of ArbOwner's OwnerActs(bytes4,address,bytes) event.
const OwnerActsTopic = "0x3c9e6a772755407311e3b35b3ee56799df8f87395941b3a658eee9e08a67ebda"

// Precompile function signatures. Selectors are derived with keccak256.
const (
	SigGetGasPricingConstraints      = "getGasPricingConstraints()"
	SigGetPricesInWei                = "getPricesInWei()"
	SigGetMinimumGasPrice            = "getMinimumGasPrice()"
	SigGetGasBacklog                 = "getGasBacklog()"
	SigGetPricingInertia             = "getPricingInertia()"
	SigGetGasBacklogTolerance        = "getGasBacklogTolerance()"
	SigGetGasAccountingParams        = "getGasAccountingParams()"
	SigGetL1BaseFeeEstimate          = "getL1BaseFeeEstimate()"
	SigGetL1PricingSurplus           = "getL1PricingSurplus()"
	SigGetL1FeesAvailable            = "getL1FeesAvailable()"
	SigGetL1PricingUnitsSinceUpdate  = "getL1PricingUnitsSinceUpdate()"
	SigGetLastL1PricingUpdateTime    = "getLastL1PricingUpdateTime()"
	SigGetL1PricingEquilibrationUnit = "getL1PricingEquilibrationUnits()"
	SigGetPerBatchGasCharge          = "getPerBatchGasCharge()"
	SigGetL1RewardRate               = "getL1RewardRate()"
	SigGetL1RewardRecipient          = "getL1RewardRecipient()"
	SigGetNetworkFeeAccount          = "getNetworkFeeAccount()"
	SigGetInfraFeeAccount            = "getInfraFeeAccount()"
	SigGetParentGasFloorPerToken     = "getParentGasFloorPerToken()"
	SigArbOSVersion                  = "arbOSVersion()"

	SigSetGasPricingConstraints  = "setGasPricingConstraints(uint64[3][])"
	SigSetParentGasFloorPerToken = "setParentGasFloorPerToken(uint64)"
	SigStartBlock                = "startBlock(uint256,uint64,uint64,uint64)"
	SigBatchPostingReportV2      = "batchPostingReportV2(uint256,address,uint64,uint64,uint64,uint64,uint256)"
	SigBatchPostingReportV1      = "batchPostingReport(uint256,address,uint64,uint64,uint256)"
)

// Constraint is one gas pricing constraint as returned by
// getGasPricingConstraints: the third element is the live backlog.
type Constraint struct {
	Target  uint64
	Window  uint64
	Backlog uint64
}

// Prices is the getPricesInWei tuple in nitro's order.
type Prices struct {
	PerL2Tx             *big.Int
	PerL1CalldataByte   *big.Int
	PerL2Storage        *big.Int
	PerArbGasBase       *big.Int
	PerArbGasCongestion *big.Int
	PerArbGasTotal      *big.Int
}

// LegacyParams are the pre-constraint pricer parameters.
type LegacyParams struct {
	SpeedLimit uint64
	Inertia    uint64
	Tolerance  uint64
	Backlog    uint64
}

// L1Sample is the set of L1 pricer getters read on the slow tick.
type L1Sample struct {
	BaseFeeEstimate        *big.Int
	Surplus                *big.Int
	FeesAvailable          *big.Int
	UnitsSinceUpdate       uint64
	LastUpdateTime         uint64
	EquilibrationUnits     uint64
	PerBatchGasCharge      int64
	RewardRate             uint64
	ArbOSVersion           uint64
	ParentGasFloorPerToken uint64
}

// Account is an address with its balance.
type Account struct {
	Address string
	Balance *big.Int
}

// FeeAccounts are the three fee destinations.
type FeeAccounts struct {
	Infra    Account
	Network  Account
	L1Reward Account
}

// CallRequest builds an eth_call against a precompile at the latest block.
func CallRequest(to string, data []byte) Request {
	return CallRequestAt(to, data, latestTag)
}

// CallRequestAt builds an eth_call against a precompile at a block tag
// ("latest" or a hex block number).
func CallRequestAt(to string, data []byte, tag string) Request {
	return Request{Method: "eth_call", Params: []any{map[string]string{"to": to, "data": EncodeHex(data)}, tag}}
}

// SelectorCall builds an eth_call for a no-argument function at the latest
// block.
func SelectorCall(to, sig string) Request {
	return SelectorCallAt(to, sig, latestTag)
}

// SelectorCallAt builds an eth_call for a no-argument function at a block tag.
func SelectorCallAt(to, sig, tag string) Request {
	s := Selector(sig)
	return CallRequestAt(to, s[:], tag)
}

// DecodeConstraints decodes a getGasPricingConstraints() return value.
func DecodeConstraints(data []byte) ([]Constraint, error) {
	if len(data) == 0 {
		return nil, nil
	}
	triples, err := uint64Triples(data)
	if err != nil {
		return nil, fmt.Errorf("getGasPricingConstraints: %w", err)
	}
	out := make([]Constraint, len(triples))
	for i, t := range triples {
		out[i] = Constraint{Target: t[0], Window: t[1], Backlog: t[2]}
	}
	return out, nil
}

// DecodePrices decodes a getPricesInWei() return value.
func DecodePrices(data []byte) (*Prices, error) {
	vals := make([]*big.Int, 6)
	for i := range vals {
		v, err := wordBig(data, i)
		if err != nil {
			return nil, fmt.Errorf("getPricesInWei: %w", err)
		}
		vals[i] = v
	}
	return &Prices{
		PerL2Tx: vals[0], PerL1CalldataByte: vals[1], PerL2Storage: vals[2],
		PerArbGasBase: vals[3], PerArbGasCongestion: vals[4], PerArbGasTotal: vals[5],
	}, nil
}

// DecodeUint256 decodes a single uint256 return value.
func DecodeUint256(data []byte) (*big.Int, error) {
	return wordBig(data, 0)
}

// DecodeInt256 decodes a single int256 return value.
func DecodeInt256(data []byte) (*big.Int, error) {
	return wordInt256(data, 0)
}

// DecodeUint64 decodes a single uint64 return value.
func DecodeUint64(data []byte) (uint64, error) {
	return wordUint64(data, 0)
}

// DecodeInt64 decodes a single int64 return value.
func DecodeInt64(data []byte) (int64, error) {
	return wordInt64(data, 0)
}

// DecodeAddress decodes a single address return value.
func DecodeAddress(data []byte) (string, error) {
	return wordAddress(data, 0)
}

// DecodeGasAccountingParams decodes getGasAccountingParams() into
// (speedLimitPerSecond, gasPoolMax, maxTxGasLimit).
func DecodeGasAccountingParams(data []byte) (speedLimit, gasPoolMax, maxTxGasLimit uint64, err error) {
	if speedLimit, err = wordUint64(data, 0); err != nil {
		return 0, 0, 0, fmt.Errorf("getGasAccountingParams: %w", err)
	}
	if gasPoolMax, err = wordUint64(data, 1); err != nil {
		return 0, 0, 0, fmt.Errorf("getGasAccountingParams: %w", err)
	}
	if maxTxGasLimit, err = wordUint64(data, 2); err != nil {
		return 0, 0, 0, fmt.Errorf("getGasAccountingParams: %w", err)
	}
	return speedLimit, gasPoolMax, maxTxGasLimit, nil
}
