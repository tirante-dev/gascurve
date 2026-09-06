package nitro

import (
	"fmt"
	"math/big"
)

// OwnerAction is a decoded OwnerActs event.
type OwnerAction struct {
	BlockNumber uint64
	Timestamp   uint64
	TxHash      string
	LogIndex    uint64
	Owner       string
	Selector    string
	Method      string
	// Args is the decoded argument map (JSON friendly values: uint64 and
	// int64 as numbers, uint256 as decimal strings, addresses as strings).
	// Unknown selectors get {"raw": "0x..."} with the full calldata.
	Args map[string]any
	// Constraints is set for setGasPricingConstraints.
	Constraints []ConstraintParam
	// MinBaseFee is set for setMinimumL2BaseFee.
	MinBaseFee *big.Int
}

// ConstraintParam is one element of a setGasPricingConstraints call.
type ConstraintParam struct {
	GasTargetPerSecond      uint64 `json:"gasTargetPerSecond"`
	AdjustmentWindowSeconds uint64 `json:"adjustmentWindowSeconds"`
	StartingBacklog         uint64 `json:"startingBacklog"`
}

// Method and parameter names shared by several table entries.
const (
	methodSetMinimumL2BaseFee = "setMinimumL2BaseFee"
	paramLimit                = "limit"
	paramTimestamp            = "timestamp"
)

type argKind int

const (
	kindUint64 argKind = iota
	kindUint256
	kindAddress
	kindInt64
)

type ownerMethod struct {
	name   string
	params []string
	kinds  []argKind
}

// ownerMethods lists the ArbOwner functions decoded by name. Keys are the
// canonical signatures; selectors are computed once at init.
var ownerMethods = map[string]ownerMethod{
	"setMinimumL2BaseFee(uint256)":            {methodSetMinimumL2BaseFee, []string{"priceInWei"}, []argKind{kindUint256}},
	"setSpeedLimit(uint64)":                   {"setSpeedLimit", []string{paramLimit}, []argKind{kindUint64}},
	"setL2GasPricingInertia(uint64)":          {"setL2GasPricingInertia", []string{"sec"}, []argKind{kindUint64}},
	"setL2GasBacklogTolerance(uint64)":        {"setL2GasBacklogTolerance", []string{"sec"}, []argKind{kindUint64}},
	"setL1PricePerUnit(uint256)":              {"setL1PricePerUnit", []string{"pricePerUnit"}, []argKind{kindUint256}},
	"setL1PricingRewardRate(uint64)":          {"setL1PricingRewardRate", []string{"weiPerUnit"}, []argKind{kindUint64}},
	"setL1PricingRewardRecipient(address)":    {"setL1PricingRewardRecipient", []string{"recipient"}, []argKind{kindAddress}},
	"setNetworkFeeAccount(address)":           {"setNetworkFeeAccount", []string{"newNetworkFeeAccount"}, []argKind{kindAddress}},
	"setInfraFeeAccount(address)":             {"setInfraFeeAccount", []string{"newInfraFeeAccount"}, []argKind{kindAddress}},
	"addChainOwner(address)":                  {"addChainOwner", []string{"newOwner"}, []argKind{kindAddress}},
	"removeChainOwner(address)":               {"removeChainOwner", []string{"ownerToRemove"}, []argKind{kindAddress}},
	"scheduleArbOSUpgrade(uint64,uint64)":     {"scheduleArbOSUpgrade", []string{"newVersion", paramTimestamp}, []argKind{kindUint64, kindUint64}},
	"setMaxTxGasLimit(uint64)":                {"setMaxTxGasLimit", []string{paramLimit}, []argKind{kindUint64}},
	"setMaxBlockGasLimit(uint64)":             {"setMaxBlockGasLimit", []string{paramLimit}, []argKind{kindUint64}},
	"setL1PricingEquilibrationUnits(uint256)": {"setL1PricingEquilibrationUnits", []string{"equilibrationUnits"}, []argKind{kindUint256}},
	"setPerBatchGasCharge(int64)":             {"setPerBatchGasCharge", []string{"cost"}, []argKind{kindInt64}},
	"setAmortizedCostCapBips(uint64)":         {"setAmortizedCostCapBips", []string{"cap"}, []argKind{kindUint64}},
	"setTransactionFilteringFrom(uint64)":     {"setTransactionFilteringFrom", []string{paramTimestamp}, []argKind{kindUint64}},
	"addTransactionFilterer(address)":         {"addTransactionFilterer", []string{"filterer"}, []argKind{kindAddress}},
}

var (
	ownerMethodsBySelector = map[[4]byte]ownerMethod{}
	selSetGasPricingConstr = Selector(SigSetGasPricingConstraints)
)

func init() {
	for sig, m := range ownerMethods {
		ownerMethodsBySelector[Selector(sig)] = m
	}
}

// DecodeOwnerActs decodes an OwnerActs(bytes4 indexed method, address
// indexed owner, bytes data) log. The data payload is the full calldata of
// the owner call (selector plus ABI-encoded arguments).
func DecodeOwnerActs(l Log) (*OwnerAction, error) {
	if len(l.Topics) != 3 || l.Topics[0] != OwnerActsTopic {
		return nil, fmt.Errorf("log %s/%d: not an OwnerActs event", l.TxHash, l.LogIndex)
	}
	methodTopic, err := DecodeHex(l.Topics[1])
	if err != nil || len(methodTopic) != 32 {
		return nil, fmt.Errorf("log %s/%d: bad method topic", l.TxHash, l.LogIndex)
	}
	owner, err := wordAddress(mustDecode(l.Topics[2]), 0)
	if err != nil {
		return nil, fmt.Errorf("log %s/%d: bad owner topic: %w", l.TxHash, l.LogIndex, err)
	}
	calldata, err := dynamicBytes(l.Data)
	if err != nil {
		return nil, fmt.Errorf("log %s/%d: data: %w", l.TxHash, l.LogIndex, err)
	}
	a := &OwnerAction{
		BlockNumber: l.BlockNumber,
		Timestamp:   l.BlockTimestamp,
		TxHash:      l.TxHash,
		LogIndex:    l.LogIndex,
		Owner:       owner,
		Selector:    EncodeHex(methodTopic[:4]),
	}
	a.Method, a.Args, a.Constraints, a.MinBaseFee, err = DecodeOwnerCalldata(calldata)
	if err != nil {
		return nil, fmt.Errorf("log %s/%d: %w", l.TxHash, l.LogIndex, err)
	}
	if a.Method == "" {
		a.Method = a.Selector
	}
	return a, nil
}

func mustDecode(s string) []byte {
	b, err := DecodeHex(s)
	if err != nil {
		return make([]byte, 32)
	}
	return padWord(b)
}

// DecodeOwnerCalldata decodes an ArbOwner call. Unknown selectors yield an
// empty method name and {"raw": calldata}. A known selector whose
// arguments do not decode is an error: such an event may change the
// pricer and must not be stored as an opaque raw action.
func DecodeOwnerCalldata(calldata []byte) (method string, args map[string]any, constraints []ConstraintParam, minBaseFee *big.Int, err error) {
	raw := map[string]any{"raw": EncodeHex(calldata)}
	if len(calldata) < 4 {
		return "", raw, nil, nil, nil
	}
	var sel [4]byte
	copy(sel[:], calldata[:4])
	body := calldata[4:]

	if sel == selSetGasPricingConstr {
		triples, err := uint64Triples(body)
		if err != nil {
			return "", nil, nil, nil, fmt.Errorf("setGasPricingConstraints: %w", err)
		}
		constraints = make([]ConstraintParam, len(triples))
		for i, t := range triples {
			constraints[i] = ConstraintParam{GasTargetPerSecond: t[0], AdjustmentWindowSeconds: t[1], StartingBacklog: t[2]}
		}
		return "setGasPricingConstraints", map[string]any{"constraints": constraints}, constraints, nil, nil
	}

	m, ok := ownerMethodsBySelector[sel]
	if !ok {
		return "", raw, nil, nil, nil
	}
	args = make(map[string]any, len(m.params))
	for i, kind := range m.kinds {
		var (
			v   any
			err error
		)
		switch kind {
		case kindUint64:
			v, err = wordUint64(body, i)
		case kindInt64:
			v, err = wordInt64(body, i)
		case kindAddress:
			v, err = wordAddress(body, i)
		case kindUint256:
			var b *big.Int
			b, err = wordBig(body, i)
			if err == nil {
				v = b.String()
				if m.name == methodSetMinimumL2BaseFee {
					minBaseFee = b
				}
			}
		}
		if err != nil {
			return "", nil, nil, nil, fmt.Errorf("%s argument %s: %w", m.name, m.params[i], err)
		}
		args[m.params[i]] = v
	}
	return m.name, args, nil, minBaseFee, nil
}

// EncodeSetGasPricingConstraints builds the calldata for
// setGasPricingConstraints(uint64[3][]), used by tests and tooling.
func EncodeSetGasPricingConstraints(constraints []ConstraintParam) []byte {
	out := make([]byte, 0, 4+wordSize*(2+3*len(constraints)))
	out = append(out, selSetGasPricingConstr[:]...)
	out = append(out, encodeUint64(wordSize)...)
	out = append(out, encodeUint64(uint64(len(constraints)))...)
	for _, c := range constraints {
		out = append(out, encodeUint64(c.GasTargetPerSecond)...)
		out = append(out, encodeUint64(c.AdjustmentWindowSeconds)...)
		out = append(out, encodeUint64(c.StartingBacklog)...)
	}
	return out
}
