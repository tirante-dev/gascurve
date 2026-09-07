// Package db is the PostgreSQL layer: connection setup, embedded migrations,
// the Store interface the collector and API depend on, its sqlx
// implementation, and the LISTEN/NOTIFY abstraction.
package db

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	// Postgres driver.
	_ "github.com/lib/pq"
)

// Notify channels.
const (
	ChannelLive        = "gascurve_live"
	ChannelOwnerAction = "gascurve_owner_action"
)

// Collector state keys.
const (
	StateHead            = "head"
	StateBackfillCursor  = "backfill_cursor"
	StateOwnerLogCursor  = "owner_log_cursor"
	StateBatchScanCursor = "batch_scan_cursor"
	StateRateLimitEvents = "rate_limit_events"
	StateLast429At       = "last_429_at"
	StateArbOSVersion    = "arbos_version"
	// StateRPCCapacity is the latest live-ingress demand and configured RPC
	// capacity estimate (model.RPCCapacity), persisted for the API status.
	StateRPCCapacity = "rpc_capacity"
	// StateLiveStart records the first block the live loop stored
	// ({"block":n,"ts":unix}); buckets from its hour on are rebuilt from
	// block rows, older ones belong to the backfill alone.
	StateLiveStart = "live_start"
	// StateHoles is the legacy JSON checkpoint imported transactionally into
	// missing_ranges at collector startup. It remains only when decoding or
	// persistence failed, so operators can repair it without data loss.
	StateHoles = "holes"
	// StateEndpoints is the endpoint pool's routing state
	// (model.EndpointsStatus as JSON), refreshed by the slow loop.
	StateEndpoints = "endpoints"
	// StateOwnerScanThrough is the block through which the owner-action
	// timeline is complete: the slow loop writes it only when a scan pass
	// reached the head it started from, never per chunk. The backfill
	// starts a segment only when it covers the block before live_start.
	StateOwnerScanThrough = "owner_scan_through"
	// StateGeneration counts the canonical rewinds of a chain: every reorg
	// rewind bumps it inside its own transaction. A writer captures it
	// before its network calls and compares it inside its chain
	// transaction, so work fetched from a fork that has since been rewound
	// is discarded instead of committed on top of the canonical chain.
	StateGeneration = "generation"
	// StateOwnerScanOrigin describes where a deliberately truncated owner
	// scan began ({"block":n,"minBaseFee":"…","archive":bool}): the pricing
	// state in force there when an archive endpoint could sample it, or a
	// marker that nothing before the block can be reconstructed.
	StateOwnerScanOrigin = "owner_scan_origin"
	// StateEthUsd is the last ETH/USD spot the slow loop fetched
	// ({"price":"…","at":"RFC3339","source":"…"}), recorded per chain so the
	// API can serve it in /live without an outbound call of its own.
	StateEthUsd = "eth_usd"
	// StateHistoryEpoch is the network's history_epoch the collector last
	// rebuilt the reconstructed history at, as a decimal string. A
	// configured epoch above it drops the backfill's buckets and
	// checkpoints once and records the new value, so the rebuild runs on
	// a raised setting rather than on every restart.
	StateHistoryEpoch = "history_epoch"
)

// Open connects to Postgres and applies pool limits.
func Open(url string, maxOpen, maxIdle int) (*sqlx.DB, error) {
	d, err := sqlx.Open("postgres", url)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	if maxOpen > 0 {
		d.SetMaxOpenConns(maxOpen)
	}
	if maxIdle > 0 {
		d.SetMaxIdleConns(maxIdle)
	}
	d.SetConnMaxLifetime(30 * time.Minute)
	return d, nil
}

// Wei maps NUMERIC(40,0) columns to *big.Int and renders as a decimal
// string in JSON.
type Wei struct {
	*big.Int
}

// NewWei wraps v (nil is treated as zero).
func NewWei(v *big.Int) Wei {
	if v == nil {
		return Wei{new(big.Int)}
	}
	return Wei{v}
}

// WeiFromUint64 builds a Wei from an unsigned integer.
func WeiFromUint64(v uint64) Wei {
	return Wei{new(big.Int).SetUint64(v)}
}

// BigInt returns the value, never nil.
func (w Wei) BigInt() *big.Int {
	if w.Int == nil {
		return new(big.Int)
	}
	return w.Int
}

// String renders the decimal value ("0" for nil).
func (w Wei) String() string {
	return w.BigInt().String()
}

// Value implements driver.Valuer.
func (w Wei) Value() (driver.Value, error) {
	return w.String(), nil
}

// Scan implements sql.Scanner.
func (w *Wei) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		w.Int = new(big.Int)
		return nil
	case int64:
		w.Int = big.NewInt(v)
		return nil
	case []byte:
		return w.setString(string(v))
	case string:
		return w.setString(v)
	default:
		return fmt.Errorf("wei: cannot scan %T", src)
	}
}

func (w *Wei) setString(s string) error {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return fmt.Errorf("wei: invalid numeric %q", s)
	}
	w.Int = v
	return nil
}

// MarshalJSON renders the decimal string.
func (w Wei) MarshalJSON() ([]byte, error) {
	return json.Marshal(w.String())
}

// UnmarshalJSON accepts a decimal string or a JSON number.
func (w *Wei) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		s = string(b)
	}
	return w.setString(s)
}

// NullWei is a nullable Wei: Valid is false for SQL NULL, which marks a
// value that was never recorded (history written before the column
// existed) as opposed to a zero.
type NullWei struct {
	Wei   Wei
	Valid bool
}

// NewNullWei wraps a known value (nil is a known zero).
func NewNullWei(v *big.Int) NullWei {
	return NullWei{Wei: NewWei(v), Valid: true}
}

// NullWeiFromUint64 builds a known NullWei from an unsigned integer.
func NullWeiFromUint64(v uint64) NullWei {
	return NullWei{Wei: WeiFromUint64(v), Valid: true}
}

// Value implements driver.Valuer.
func (n NullWei) Value() (driver.Value, error) {
	if !n.Valid {
		return nil, nil
	}
	return n.Wei.Value()
}

// Scan implements sql.Scanner.
func (n *NullWei) Scan(src any) error {
	if src == nil {
		*n = NullWei{}
		return nil
	}
	n.Valid = true
	return n.Wei.Scan(src)
}

// StringPtr renders the value as a decimal string, nil when unknown.
func (n NullWei) StringPtr() *string {
	if !n.Valid {
		return nil
	}
	s := n.Wei.String()
	return &s
}

// JSONB maps JSONB columns. A nil value is SQL NULL and JSON null.
type JSONB []byte

// Value implements driver.Valuer, sending the document as text.
func (j JSONB) Value() (driver.Value, error) {
	if j == nil {
		return nil, nil
	}
	if !json.Valid(j) {
		return nil, fmt.Errorf("jsonb: invalid document")
	}
	return string(j), nil
}

// Scan implements sql.Scanner.
func (j *JSONB) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*j = nil
	case []byte:
		*j = append(JSONB(nil), v...)
	case string:
		*j = JSONB(v)
	default:
		return fmt.Errorf("jsonb: cannot scan %T", src)
	}
	return nil
}

// MarshalJSON emits the raw document.
func (j JSONB) MarshalJSON() ([]byte, error) {
	if j == nil {
		return []byte("null"), nil
	}
	return j, nil
}

// UnmarshalJSON stores the raw document.
func (j *JSONB) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*j = nil
		return nil
	}
	*j = append(JSONB(nil), b...)
	return nil
}

// MarshalJSONB encodes v into a JSONB value.
func MarshalJSONB(v any) (JSONB, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return JSONB(b), nil
}

// Unmarshal decodes the document into v; a nil document leaves v untouched.
func (j JSONB) Unmarshal(v any) error {
	if j == nil {
		return nil
	}
	return json.Unmarshal(j, v)
}

// Uint64Array maps NUMERIC(20,0)[] columns to unsigned 64-bit values
// exactly: backlogs saturate at 2^64-1 in the pricer, which BIGINT cannot
// hold. It scans the array text form ("{1,2}") and renders the same.
type Uint64Array []uint64

// Value implements driver.Valuer.
func (a Uint64Array) Value() (driver.Value, error) {
	if len(a) == 0 {
		return "{}", nil
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, v := range a {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatUint(v, 10))
	}
	b.WriteByte('}')
	return b.String(), nil
}

// Scan implements sql.Scanner.
func (a *Uint64Array) Scan(src any) error {
	var s string
	switch v := src.(type) {
	case nil:
		*a = Uint64Array{}
		return nil
	case []byte:
		s = string(v)
	case string:
		s = v
	default:
		return fmt.Errorf("uint64 array: cannot scan %T", src)
	}
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") || !strings.HasSuffix(s, "}") {
		return fmt.Errorf("uint64 array: invalid literal %q", s)
	}
	s = s[1 : len(s)-1]
	if s == "" {
		*a = Uint64Array{}
		return nil
	}
	parts := strings.Split(s, ",")
	out := make(Uint64Array, len(parts))
	for i, p := range parts {
		v, err := strconv.ParseUint(strings.Trim(strings.TrimSpace(p), `"`), 10, 64)
		if err != nil {
			return fmt.Errorf("uint64 array: element %d: %w", i, err)
		}
		out[i] = v
	}
	*a = out
	return nil
}

// Uint64s returns a non-nil copy of the array (JSON arrays are never null).
func (a Uint64Array) Uint64s() []uint64 {
	return append([]uint64{}, a...)
}

// Int64s converts values into a BIGINT[] parameter (bips, never backlogs).
func Int64s(v []uint64) []int64 {
	out := make([]int64, len(v))
	for i, x := range v {
		out[i] = int64(x)
	}
	return out
}

// Uint64s converts a BIGINT[] of non-negative values (never nil).
func Uint64s(v []int64) []uint64 {
	out := make([]uint64, len(v))
	for i, x := range v {
		if x > 0 {
			out[i] = uint64(x)
		}
	}
	return out
}

// NullTime returns a sql.NullTime for t (zero means NULL).
func NullTime(t time.Time) sql.NullTime {
	if t.IsZero() {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: t, Valid: true}
}
