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

// Int64s converts unsigned backlogs into the BIGINT[] representation.
func Int64s(v []uint64) []int64 {
	out := make([]int64, len(v))
	for i, x := range v {
		out[i] = int64(x)
	}
	return out
}

// Uint64s converts a BIGINT[] back into unsigned values (never nil).
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
