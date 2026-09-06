package db

import (
	"encoding/json"
	"math/big"
	"testing"
	"time"
)

func TestWei(t *testing.T) {
	w := NewWei(nil)
	if w.String() != "0" || w.BigInt().Sign() != 0 {
		t.Fatal("nil wei")
	}
	if (Wei{}).String() != "0" || (Wei{}).BigInt().Sign() != 0 {
		t.Fatal("zero value wei")
	}
	w = WeiFromUint64(42)
	v, err := w.Value()
	if err != nil || v != "42" {
		t.Fatalf("Value: %v %v", v, err)
	}
	for _, src := range []any{nil, int64(7), []byte("12345678901234567890123456789"), "99"} {
		var s Wei
		if err := s.Scan(src); err != nil {
			t.Fatalf("Scan(%v): %v", src, err)
		}
	}
	var s Wei
	if err := s.Scan(3.5); err == nil {
		t.Fatal("float should fail")
	}
	if err := s.Scan("abc"); err == nil {
		t.Fatal("invalid numeric should fail")
	}
	b, err := json.Marshal(struct{ W Wei }{NewWei(big.NewInt(5))})
	if err != nil || string(b) != `{"W":"5"}` {
		t.Fatalf("MarshalJSON: %s %v", b, err)
	}
	var back struct{ W Wei }
	if err := json.Unmarshal([]byte(`{"W":"6"}`), &back); err != nil || back.W.Int64() != 6 {
		t.Fatalf("UnmarshalJSON string: %v %v", back, err)
	}
	if err := json.Unmarshal([]byte(`{"W":7}`), &back); err != nil || back.W.Int64() != 7 {
		t.Fatalf("UnmarshalJSON number: %v %v", back, err)
	}
	if err := json.Unmarshal([]byte(`{"W":"x"}`), &back); err == nil {
		t.Fatal("bad json wei")
	}
}

func TestJSONB(t *testing.T) {
	var j JSONB
	if v, err := j.Value(); err != nil || v != nil {
		t.Fatalf("nil Value: %v %v", v, err)
	}
	j = JSONB(`{"a":1}`)
	if v, err := j.Value(); err != nil || v != `{"a":1}` {
		t.Fatalf("Value: %v %v", v, err)
	}
	if _, err := JSONB(`{bad`).Value(); err == nil {
		t.Fatal("invalid json should fail")
	}
	for _, src := range []any{nil, []byte(`[1]`), `[2]`} {
		var s JSONB
		if err := s.Scan(src); err != nil {
			t.Fatalf("Scan(%v): %v", src, err)
		}
	}
	var s JSONB
	if err := s.Scan(1); err == nil {
		t.Fatal("int should fail")
	}
	b, err := json.Marshal(struct {
		A JSONB
		B JSONB
	}{A: JSONB(`{"x":true}`), B: nil})
	if err != nil || string(b) != `{"A":{"x":true},"B":null}` {
		t.Fatalf("MarshalJSON: %s %v", b, err)
	}
	var back struct {
		A JSONB
		B JSONB
	}
	if err := json.Unmarshal(b, &back); err != nil || string(back.A) != `{"x":true}` || back.B != nil {
		t.Fatalf("UnmarshalJSON: %v %v", back, err)
	}
	m, err := MarshalJSONB(map[string]int{"k": 1})
	if err != nil || string(m) != `{"k":1}` {
		t.Fatalf("MarshalJSONB: %s %v", m, err)
	}
	if m, err := MarshalJSONB(nil); err != nil || m != nil {
		t.Fatal("MarshalJSONB(nil)")
	}
	if _, err := MarshalJSONB(make(chan int)); err == nil {
		t.Fatal("unmarshalable value should fail")
	}
	var out map[string]int
	if err := JSONB(`{"k":2}`).Unmarshal(&out); err != nil || out["k"] != 2 {
		t.Fatalf("Unmarshal: %v %v", out, err)
	}
	if err := JSONB(nil).Unmarshal(&out); err != nil {
		t.Fatal("nil Unmarshal should be a no-op")
	}
}

func TestConversions(t *testing.T) {
	if got := Int64s([]uint64{1, 2}); len(got) != 2 || got[1] != 2 {
		t.Fatal("Int64s")
	}
	if got := Uint64s([]int64{1, -1, 3}); len(got) != 3 || got[1] != 0 || got[2] != 3 {
		t.Fatal("Uint64s")
	}
	if got := Uint64s(nil); got == nil || len(got) != 0 {
		t.Fatal("Uint64s(nil) should be empty, not nil")
	}
	if PQArray([]uint64{5})[0] != 5 {
		t.Fatal("PQArray")
	}
	if NullTime(time.Time{}).Valid || !NullTime(time.Now()).Valid {
		t.Fatal("NullTime")
	}
}

func TestOpen(t *testing.T) {
	d, err := Open("postgres://localhost:1/x?sslmode=disable", 5, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if d.Stats().MaxOpenConnections != 5 {
		t.Fatal("max open not applied")
	}
	if _, err := Open("postgres://localhost:1/x", 0, 0); err != nil {
		t.Fatal(err)
	}
}
