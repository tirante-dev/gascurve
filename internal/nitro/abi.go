// Package nitro is the JSON-RPC client for Arbitrum Nitro chains: batching,
// per-network pacing, 429 back-off, and hand-rolled ABI codecs for the
// precompiles, OwnerActs events and ArbOS internal transactions.
package nitro

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"golang.org/x/crypto/sha3"
)

const wordSize = 32

var (
	errShortData = errors.New("abi: data too short")
	errOverflow  = errors.New("abi: value does not fit")
)

func Keccak256(data []byte) []byte {
	h := sha3.NewLegacyKeccak256()
	h.Write(data)
	return h.Sum(nil)
}

// Selector returns the 4-byte function selector for a canonical signature such as "getPricesInWei()".
func Selector(sig string) [4]byte {
	var out [4]byte
	copy(out[:], Keccak256([]byte(sig))[:4])
	return out
}

func SelectorHex(sig string) string {
	s := Selector(sig)
	return "0x" + hex.EncodeToString(s[:])
}

func EventTopic(sig string) string {
	return "0x" + hex.EncodeToString(Keccak256([]byte(sig)))
}

// DecodeHex parses a 0x-prefixed hex string (odd lengths are left padded).
func DecodeHex(s string) ([]byte, error) {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	if len(s)%2 == 1 {
		s = "0" + s
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("decode hex: %w", err)
	}
	return b, nil
}

func EncodeHex(b []byte) string {
	return "0x" + hex.EncodeToString(b)
}

// HexUint64 parses a JSON-RPC quantity ("0x1a") into a uint64.
func HexUint64(s string) (uint64, error) {
	b, err := HexBig(s)
	if err != nil {
		return 0, err
	}
	if !b.IsUint64() {
		return 0, fmt.Errorf("%w: %s as uint64", errOverflow, s)
	}
	return b.Uint64(), nil
}

// HexBig parses a JSON-RPC quantity into a big.Int.
func HexBig(s string) (*big.Int, error) {
	t := strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
	if t == "" {
		return nil, fmt.Errorf("empty quantity %q", s)
	}
	b, ok := new(big.Int).SetString(t, 16)
	if !ok {
		return nil, fmt.Errorf("invalid quantity %q", s)
	}
	return b, nil
}

func word(data []byte, i int) ([]byte, error) {
	start := i * wordSize
	if start < 0 || start+wordSize > len(data) {
		return nil, fmt.Errorf("%w: word %d of %d bytes", errShortData, i, len(data))
	}
	return data[start : start+wordSize], nil
}

func wordBig(data []byte, i int) (*big.Int, error) {
	w, err := word(data, i)
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(w), nil
}

// wordInt256 decodes word i as a two's complement signed integer.
func wordInt256(data []byte, i int) (*big.Int, error) {
	w, err := word(data, i)
	if err != nil {
		return nil, err
	}
	v := new(big.Int).SetBytes(w)
	if w[0]&0x80 != 0 {
		v.Sub(v, new(big.Int).Lsh(big.NewInt(1), 256))
	}
	return v, nil
}

// wordUint64 decodes word i as a uint64, rejecting larger values.
func wordUint64(data []byte, i int) (uint64, error) {
	v, err := wordBig(data, i)
	if err != nil {
		return 0, err
	}
	if !v.IsUint64() {
		return 0, fmt.Errorf("%w: word %d as uint64", errOverflow, i)
	}
	return v.Uint64(), nil
}

// wordInt64 decodes word i as an int64 (two's complement).
func wordInt64(data []byte, i int) (int64, error) {
	v, err := wordInt256(data, i)
	if err != nil {
		return 0, err
	}
	if !v.IsInt64() {
		return 0, fmt.Errorf("%w: word %d as int64", errOverflow, i)
	}
	return v.Int64(), nil
}

// wordAddress decodes word i as a 0x-prefixed lowercase address.
func wordAddress(data []byte, i int) (string, error) {
	w, err := word(data, i)
	if err != nil {
		return "", err
	}
	return "0x" + hex.EncodeToString(w[12:]), nil
}

// dynamicBytes decodes a `bytes` value whose head slot is word 0.
func dynamicBytes(data []byte) ([]byte, error) {
	offset, err := wordUint64(data, 0)
	if err != nil {
		return nil, err
	}
	if offset%wordSize != 0 || offset+wordSize > uint64(len(data)) {
		return nil, fmt.Errorf("%w: bytes offset %d", errShortData, offset)
	}
	length, err := wordUint64(data, int(offset/wordSize))
	if err != nil {
		return nil, err
	}
	start := offset + wordSize
	if start+length > uint64(len(data)) {
		return nil, fmt.Errorf("%w: bytes length %d", errShortData, length)
	}
	return data[start : start+length], nil
}

// uint64Triples decodes a `uint64[3][]` whose head slot is word 0.
func uint64Triples(data []byte) ([][3]uint64, error) {
	offset, err := wordUint64(data, 0)
	if err != nil {
		return nil, err
	}
	if offset%wordSize != 0 {
		return nil, fmt.Errorf("%w: array offset %d", errShortData, offset)
	}
	base := int(offset / wordSize)
	length, err := wordUint64(data, base)
	if err != nil {
		return nil, err
	}
	if length > uint64(len(data))/wordSize {
		return nil, fmt.Errorf("%w: array length %d", errShortData, length)
	}
	out := make([][3]uint64, 0, length)
	for n := 0; n < int(length); n++ {
		var t [3]uint64
		for k := 0; k < 3; k++ {
			v, err := wordUint64(data, base+1+n*3+k)
			if err != nil {
				return nil, err
			}
			t[k] = v
		}
		out = append(out, t)
	}
	return out, nil
}

func padWord(b []byte) []byte {
	if len(b) >= wordSize {
		return b[len(b)-wordSize:]
	}
	out := make([]byte, wordSize)
	copy(out[wordSize-len(b):], b)
	return out
}

func encodeUint64(v uint64) []byte {
	return padWord(new(big.Int).SetUint64(v).Bytes())
}
