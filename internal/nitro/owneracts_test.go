package nitro

import (
	"encoding/json"
	"math/big"
	"os"
	"testing"
)

type decodedFixture struct {
	Events []struct {
		Block       uint64            `json:"block"`
		Tx          string            `json:"tx"`
		Method      string            `json:"method"`
		Selector    string            `json:"selector"`
		Address     string            `json:"address"`
		Args        []json.Number     `json:"args"`
		Constraints []ConstraintParam `json:"constraints"`
	} `json:"events"`
}

func loadFixtureLogs(t *testing.T) []Log {
	t.Helper()
	raw, err := os.ReadFile("testdata/robinhood_owner_acts_logs.json")
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	logs, err := parseLogs(resp.Result)
	if err != nil {
		t.Fatal(err)
	}
	return logs
}

func TestDecodeOwnerActsFixture(t *testing.T) {
	logs := loadFixtureLogs(t)
	raw, err := os.ReadFile("../../data/robinhood/owner-actions.json")
	if err != nil {
		t.Fatal(err)
	}
	var want decodedFixture
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if len(logs) != len(want.Events) {
		t.Fatalf("%d logs vs %d decoded events", len(logs), len(want.Events))
	}
	for i, l := range logs {
		got, err := DecodeOwnerActs(l)
		if err != nil {
			t.Fatalf("log %d: %v", i, err)
		}
		w := want.Events[i]
		if got.BlockNumber != w.Block || got.TxHash != w.Tx || got.TxIndex != logs[i].TxIndex || got.Method != w.Method || got.Selector != w.Selector {
			t.Fatalf("log %d: got %+v want %+v", i, got, w)
		}
		if got.Owner != "0x2a153c6a1b66dbc930a8d7017230ab0253005c09" && got.BlockNumber != 11 {
			t.Fatalf("log %d owner %s", i, got.Owner)
		}
		if w.Address != "" {
			found := false
			for _, v := range got.Args {
				if v == w.Address {
					found = true
				}
			}
			if !found {
				t.Fatalf("log %d: address %s not in args %v", i, w.Address, got.Args)
			}
		}
		if len(w.Constraints) > 0 {
			if len(got.Constraints) != len(w.Constraints) {
				t.Fatalf("log %d: constraints %v", i, got.Constraints)
			}
			for k := range w.Constraints {
				if got.Constraints[k] != w.Constraints[k] {
					t.Fatalf("log %d constraint %d: %+v vs %+v", i, k, got.Constraints[k], w.Constraints[k])
				}
			}
		}
		switch got.Method {
		case "scheduleArbOSUpgrade":
			if got.Args["newVersion"] != uint64(61) || got.Args["timestamp"] != uint64(0) {
				t.Fatalf("scheduleArbOSUpgrade args %v", got.Args)
			}
		case "setMinimumL2BaseFee":
			if got.MinBaseFee.Cmp(big.NewInt(20_000_000)) != 0 || got.Args["priceInWei"] != "20000000" {
				t.Fatalf("setMinimumL2BaseFee: %v %v", got.MinBaseFee, got.Args)
			}
		case "setTransactionFilteringFrom":
			if got.Args["timestamp"] != uint64(1782295200) {
				t.Fatalf("setTransactionFilteringFrom args %v", got.Args)
			}
		}
		// Every args map must survive a JSON round trip (JSONB storage).
		if _, err := json.Marshal(got.Args); err != nil {
			t.Fatalf("log %d args not JSON: %v", i, err)
		}
	}
}

func TestDecodeOwnerActsErrors(t *testing.T) {
	good := loadFixtureLogs(t)[0]
	bad := good
	bad.Topics = good.Topics[:2]
	if _, err := DecodeOwnerActs(bad); err == nil {
		t.Fatal("missing topics")
	}
	bad = good
	bad.Topics = []string{"0x1", good.Topics[1], good.Topics[2]}
	if _, err := DecodeOwnerActs(bad); err == nil {
		t.Fatal("wrong topic0")
	}
	bad = good
	bad.Topics = []string{good.Topics[0], "0xzz", good.Topics[2]}
	if _, err := DecodeOwnerActs(bad); err == nil {
		t.Fatal("bad method topic")
	}
	bad = good
	bad.Topics = []string{good.Topics[0], "0x01", good.Topics[2]}
	if _, err := DecodeOwnerActs(bad); err == nil {
		t.Fatal("short method topic")
	}
	bad = good
	bad.Topics = []string{good.Topics[0], good.Topics[1], "0xzz"}
	if a, err := DecodeOwnerActs(bad); err != nil || a.Owner != "0x0000000000000000000000000000000000000000" {
		t.Fatalf("undecodable owner topic should fall back to zero: %v %v", a, err)
	}
	bad = good
	bad.Data = []byte{1}
	if _, err := DecodeOwnerActs(bad); err == nil {
		t.Fatal("bad data")
	}
}

func TestDecodeOwnerCalldata(t *testing.T) {
	method, args, _, _, err := DecodeOwnerCalldata([]byte{1, 2})
	if err != nil || method != "" || args["raw"] != "0x0102" {
		t.Fatalf("short calldata: %s %v %v", method, args, err)
	}
	unknown := Selector("frobnicate(uint256)")
	method, args, _, _, err = DecodeOwnerCalldata(append(unknown[:], encodeUint64(1)...))
	if err != nil || method != "" || args["raw"] == nil {
		t.Fatalf("unknown selector: %s %v %v", method, args, err)
	}
	// Truncated arguments of a known selector are malformed, not raw: the
	// event could change the pricer and must not be silently skipped.
	sel := Selector("setSpeedLimit(uint64)")
	if _, _, _, _, err := DecodeOwnerCalldata(sel[:]); err == nil {
		t.Fatal("truncated known selector should fail")
	}
	sel = Selector(SigSetGasPricingConstraints)
	if _, _, _, _, err := DecodeOwnerCalldata(sel[:]); err == nil {
		t.Fatal("truncated constraints should fail")
	}
	good := loadFixtureLogs(t)[0]
	bad := good
	bad.Data = append(encodeUint64(32), encodeUint64(4)...)
	bad.Data = append(bad.Data, append(selSetGasPricingConstr[:], make([]byte, 28)...)...)
	if _, err := DecodeOwnerActs(bad); err == nil {
		t.Fatal("malformed known event should fail to decode")
	}
	// int64 argument.
	neg := make([]byte, 32)
	for i := range neg {
		neg[i] = 0xff
	}
	sel = Selector("setPerBatchGasCharge(int64)")
	method, args, _, _, err = DecodeOwnerCalldata(append(sel[:], neg...))
	if err != nil || method != "setPerBatchGasCharge" || args["cost"] != int64(-1) {
		t.Fatalf("int64: %s %v %v", method, args, err)
	}
	sel = Selector("setL1PricingEquilibrationUnits(uint256)")
	method, args, _, mbf, err := DecodeOwnerCalldata(append(sel[:], encodeUint64(5)...))
	if err != nil || method != "setL1PricingEquilibrationUnits" || args["equilibrationUnits"] != "5" || mbf != nil {
		t.Fatalf("uint256: %s %v %v %v", method, args, mbf, err)
	}
	// Unknown selector data in a log yields the selector as the method name.
	l := Log{Topics: []string{
		OwnerActsTopic,
		"0xdeadbeef00000000000000000000000000000000000000000000000000000000",
		"0x0000000000000000000000002a153c6a1b66dbc930a8d7017230ab0253005c09",
	}}
	l.Data = append(encodeUint64(32), encodeUint64(4)...)
	l.Data = append(l.Data, padWord([]byte{0xde, 0xad, 0xbe, 0xef})...)
	a, err := DecodeOwnerActs(l)
	if err != nil || a.Method != "0xdeadbeef" || a.Args["raw"] == nil {
		t.Fatalf("unknown method log: %+v %v", a, err)
	}
}
