package config

import (
	"fmt"
	"strings"
	"time"
)

// Depth is a history window: how far back from the head the owner scan and the backfill reach. It is
// a duration, Genesis for the whole chain however old it is, or Hold for the floor the backfill has
// already reached.
type Depth time.Duration

// Genesis is the depth that reaches the first block. It is negative rather than zero so that an
// unset field stays invalid: a bare 0 must not quietly mean a full-chain replay taking days.
const Genesis Depth = -1

// Hold keeps the floor already reached and abandons the rest of the descent. A shorter duration
// cannot say that: it is equally what a window measured back from a growing head looks like, so
// acting on one would hand history back on every restart of an unchanged configuration.
const Hold Depth = -2

// GenesisWord is what an operator writes for Genesis.
const GenesisWord = "genesis"

// HoldWord is what an operator writes for Hold.
const HoldWord = "hold"

// IsGenesis reports whether the depth reaches the first block.
func (d Depth) IsGenesis() bool { return d == Genesis }

// IsHold reports whether the depth is the floor the backfill has already reached.
func (d Depth) IsHold() bool { return d == Hold }

// Duration is the window as a duration, meaningless for Genesis and Hold.
func (d Depth) Duration() time.Duration { return time.Duration(d) }

func (d Depth) String() string {
	switch d {
	case Genesis:
		return GenesisWord
	case Hold:
		return HoldWord
	}
	return time.Duration(d).String()
}

// UnmarshalText parses a duration or one of the two words. A non-positive duration is refused rather
// than read as either endpoint of the range: an operator who means the whole chain writes genesis,
// and one who means no further descent writes hold.
func (d *Depth) UnmarshalText(text []byte) error {
	s := strings.TrimSpace(string(text))
	switch {
	case strings.EqualFold(s, GenesisWord):
		*d = Genesis
		return nil
	case strings.EqualFold(s, HoldWord):
		*d = Hold
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("%q is neither a duration nor %q or %q", s, GenesisWord, HoldWord)
	}
	if v <= 0 {
		return fmt.Errorf("%q must be positive, %q for the whole chain, or %q to stop where the backfill has reached", s, GenesisWord, HoldWord)
	}
	*d = Depth(v)
	return nil
}
