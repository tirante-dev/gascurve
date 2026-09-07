package nitro

import (
	"bytes"
	"math/big"
	"testing"
)

func TestSelectorsPinned(t *testing.T) {
	cases := map[string]string{
		SigGetGasPricingConstraints:           "0x232027d1",
		SigGetPricesInWei:                     "0x41b247a8",
		SigSetGasPricingConstraints:           "0xcc0d556a",
		SigGetParentGasFloorPerToken:          "0x49ccdaff",
		SigSetParentGasFloorPerToken:          "0x3a930b0b",
		SigArbOSVersion:                       "0x051038f2",
		SigStartBlock:                         "0x6bf6a42d",
		SigBatchPostingReportV2:               "0x9998269e",
		SigBatchPostingReportV1:               "0xb6693771",
		"setMinimumL2BaseFee(uint256)":        "0xa0188cdb",
		"setSpeedLimit(uint64)":               "0x4d7a060d",
		"setL1PricePerUnit(uint256)":          "0x2b352fae",
		"scheduleArbOSUpgrade(uint64,uint64)": "0xe388b381",
	}
	for sig, want := range cases {
		if got := SelectorHex(sig); got != want {
			t.Errorf("SelectorHex(%q) = %s, want %s", sig, got, want)
		}
	}
	if got := EventTopic("OwnerActs(bytes4,address,bytes)"); got != OwnerActsTopic {
		t.Errorf("OwnerActs topic = %s", got)
	}
}

func TestHexHelpers(t *testing.T) {
	if v, err := HexUint64("0x1a"); err != nil || v != 26 {
		t.Fatalf("HexUint64: %v %v", v, err)
	}
	if _, err := HexUint64("0x10000000000000000"); err == nil {
		t.Fatal("expected overflow")
	}
	if _, err := HexUint64("0x"); err == nil {
		t.Fatal("expected empty error")
	}
	if _, err := HexBig("0xzz"); err == nil {
		t.Fatal("expected invalid")
	}
	if b, err := DecodeHex("0xabc"); err != nil || len(b) != 2 || b[0] != 0x0a {
		t.Fatalf("odd length: %x %v", b, err)
	}
	if _, err := DecodeHex("0xgg"); err == nil {
		t.Fatal("expected decode error")
	}
	if EncodeHex([]byte{1, 255}) != "0x01ff" {
		t.Fatal("EncodeHex")
	}
}

func TestWords(t *testing.T) {
	data := make([]byte, 64)
	data[31] = 7
	for i := range 32 {
		data[32+i] = 0xff
	}
	if v, err := wordUint64(data, 0); err != nil || v != 7 {
		t.Fatalf("wordUint64: %d %v", v, err)
	}
	if _, err := wordUint64(data, 1); err == nil {
		t.Fatal("expected overflow")
	}
	if v, err := wordInt256(data, 1); err != nil || v.Cmp(big.NewInt(-1)) != 0 {
		t.Fatalf("wordInt256: %s %v", v, err)
	}
	if v, err := wordInt64(data, 1); err != nil || v != -1 {
		t.Fatalf("wordInt64: %d %v", v, err)
	}
	big1 := make([]byte, 32)
	big1[0] = 0x40
	if _, err := wordInt64(big1, 0); err == nil {
		t.Fatal("expected int64 overflow")
	}
	if _, err := word(data, 2); err == nil {
		t.Fatal("expected short data")
	}
	if _, err := wordBig(data, -1); err == nil {
		t.Fatal("expected negative index error")
	}
	if _, err := wordInt256(data, 5); err == nil {
		t.Fatal("expected short data")
	}
	if _, err := wordInt64(data, 5); err == nil {
		t.Fatal("expected short data")
	}
	if _, err := wordAddress(data, 5); err == nil {
		t.Fatal("expected short data")
	}
	if a, err := wordAddress(data, 1); err != nil || a != "0xffffffffffffffffffffffffffffffffffffffff" {
		t.Fatalf("wordAddress: %s %v", a, err)
	}
	if got := padWord(make([]byte, 40)); len(got) != 32 {
		t.Fatal("padWord long")
	}
}

func TestDynamicBytes(t *testing.T) {
	payload := []byte{1, 2, 3}
	data := append(encodeUint64(32), encodeUint64(3)...)
	data = append(data, padWord(nil)...)
	copy(data[64:], payload)
	got, err := dynamicBytes(data)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("dynamicBytes: %x %v", got, err)
	}
	bad := append(encodeUint64(33), encodeUint64(3)...)
	if _, err := dynamicBytes(bad); err == nil {
		t.Fatal("misaligned offset should fail")
	}
	if _, err := dynamicBytes(encodeUint64(32)); err == nil {
		t.Fatal("missing length word should fail")
	}
	short := append(encodeUint64(32), encodeUint64(100)...)
	if _, err := dynamicBytes(short); err == nil {
		t.Fatal("short payload should fail")
	}
	if _, err := dynamicBytes(nil); err == nil {
		t.Fatal("empty should fail")
	}
}

func TestUint64Triples(t *testing.T) {
	enc := EncodeSetGasPricingConstraints([]ConstraintParam{{1, 2, 3}, {4, 5, 6}})[4:]
	got, err := uint64Triples(enc)
	if err != nil || len(got) != 2 || got[1] != [3]uint64{4, 5, 6} {
		t.Fatalf("triples: %v %v", got, err)
	}
	if _, err := uint64Triples(nil); err == nil {
		t.Fatal("empty")
	}
	if _, err := uint64Triples(encodeUint64(1)); err == nil {
		t.Fatal("misaligned")
	}
	if _, err := uint64Triples(encodeUint64(32)); err == nil {
		t.Fatal("missing length")
	}
	huge := append(encodeUint64(32), encodeUint64(1000)...)
	if _, err := uint64Triples(huge); err == nil {
		t.Fatal("length beyond data")
	}
	trunc := append(encodeUint64(32), encodeUint64(1)...)
	trunc = append(trunc, encodeUint64(1)...)
	if _, err := uint64Triples(trunc); err == nil {
		t.Fatal("truncated triple")
	}
}
