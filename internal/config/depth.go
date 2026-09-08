package config

import (
	"fmt"
	"strings"
	"time"
)

// Depth is a history window: how far back from the head the owner scan and the backfill reach. It is
// a duration, or Genesis for the whole chain however old it is.
type Depth time.Duration

// Genesis is the depth that reaches the first block. It is negative rather than zero so that an
// unset field stays invalid: a bare 0 must not quietly mean a full-chain replay taking days.
const Genesis Depth = -1

// GenesisWord is what an operator writes for Genesis.
const GenesisWord = "genesis"

// IsGenesis reports whether the depth reaches the first block.
func (d Depth) IsGenesis() bool { return d == Genesis }

// Duration is the window as a duration, meaningless for Genesis.
func (d Depth) Duration() time.Duration { return time.Duration(d) }

func (d Depth) String() string {
	if d.IsGenesis() {
		return GenesisWord
	}
	return time.Duration(d).String()
}

// UnmarshalText parses a duration or the word genesis. A non-positive duration is refused rather
// than read as either endpoint of the range: an operator who means the whole chain writes genesis.
func (d *Depth) UnmarshalText(text []byte) error {
	s := strings.TrimSpace(string(text))
	if strings.EqualFold(s, GenesisWord) {
		*d = Genesis
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("%q is neither a duration nor %q", s, GenesisWord)
	}
	if v <= 0 {
		return fmt.Errorf("%q must be positive, or %q for the whole chain", s, GenesisWord)
	}
	*d = Depth(v)
	return nil
}
