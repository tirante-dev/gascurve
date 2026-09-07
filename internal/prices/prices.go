// Package prices fetches the ETH/USD spot price the collector's slow loop
// attaches to every LiveSnapshot. It is the only outbound HTTP the backend
// makes besides the JSON-RPC endpoints, it is made server side (the browser
// never calls a price API) and it is optional: an empty source disables it
// and every snapshot then reports ethUsd: null.
//
// Three shapes are understood, all reduced to one decimal string: Coinbase's
// public spot endpoint (data.amount), CoinGecko's simple price
// (ethereum.usd) and, for any other https endpoint, a top-level price field
// holding a string or a number.
package prices

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tirante-dev/gascurve/internal/version"
)

// Source names accepted by collector.eth_usd_source, and the source
// reported in a Price.
const (
	SourceCoinbase  = "coinbase"
	SourceCoinGecko = "coingecko"
	// SourceCustom is what a configured URL reports as its source: the URL
	// itself is never published, because it may carry a key.
	SourceCustom = "custom"
)

const (
	coinbaseURL  = "https://api.coinbase.com/v2/prices/ETH-USD/spot"
	coingeckoURL = "https://api.coingecko.com/api/v3/simple/price?ids=ethereum&vs_currencies=usd"
	// Timeout bounds one fetch, connection to body included. The slow loop
	// runs every minute, so a provider that hangs must not hold a follower
	// for longer than a few seconds.
	Timeout = 5 * time.Second
	// maxBody is how much of a response is read at all; anything larger is
	// not a price document.
	maxBody = 64 << 10
	// maxErrorBody is the most of a response body an error may quote, so a
	// provider that answers with a page cannot flood the logs.
	maxErrorBody = 200
	// decimals is how many decimal places a price carries.
	decimals = 2
)

// ErrUnsupportedSource is returned for a source that is neither a known
// provider nor an https URL.
var ErrUnsupportedSource = errors.New("unsupported eth_usd_source")

// ErrMalformed is returned when a provider answers with something that is
// not a positive price.
var ErrMalformed = errors.New("malformed price response")

// Price is one spot quote.
type Price struct {
	// Price is the USD price as a decimal string with two decimal places.
	Price string
	// At is when the quote was fetched.
	At time.Time
	// Source is the provider name: coinbase, coingecko or custom.
	Source string
}

// Fetcher reads the spot price from one provider. It holds a single
// http.Client, so connections are reused across fetches, and is safe for
// concurrent use.
type Fetcher struct {
	client   *http.Client
	endpoint string
	source   string
	agent    string
	now      func() time.Time
}

// Option customizes a Fetcher.
type Option func(*Fetcher)

// WithEndpoint overrides the URL called while keeping the source's
// parsing and name (tests point a provider at an httptest server).
func WithEndpoint(u string) Option { return func(f *Fetcher) { f.endpoint = u } }

// WithClock overrides the clock (tests).
func WithClock(now func() time.Time) Option { return func(f *Fetcher) { f.now = now } }

// NewFetcher builds a fetcher for a source: "coinbase", "coingecko" or an
// https URL answering JSON with a top-level price.
func NewFetcher(source string, opts ...Option) (*Fetcher, error) {
	endpoint, name, err := resolve(source)
	if err != nil {
		return nil, err
	}
	f := &Fetcher{
		client:   &http.Client{Timeout: Timeout},
		endpoint: endpoint,
		source:   name,
		agent:    version.UserAgent(),
		now:      time.Now,
	}
	for _, o := range opts {
		o(f)
	}
	return f, nil
}

// Source is the provider name this fetcher reports.
func (f *Fetcher) Source() string { return f.source }

// ValidateSource checks a configured source: a provider name, an https URL,
// or "" to disable the fetch. The value is never echoed, since a URL may
// carry a key.
func ValidateSource(source string) error {
	if source == "" {
		return nil
	}
	_, _, err := resolve(source)
	return err
}

// resolve maps a configured source to the endpoint to call and the name
// reported in a Price.
func resolve(source string) (endpoint, name string, err error) {
	switch source {
	case SourceCoinbase:
		return coinbaseURL, SourceCoinbase, nil
	case SourceCoinGecko:
		return coingeckoURL, SourceCoinGecko, nil
	}
	u, perr := url.Parse(source)
	if perr != nil || u.Scheme != "https" || u.Host == "" {
		return "", "", fmt.Errorf("%w (want %q, %q or an https:// URL)", ErrUnsupportedSource, SourceCoinbase, SourceCoinGecko)
	}
	return source, SourceCustom, nil
}

// Fetch reads the current spot price. Errors name the source rather than
// the endpoint, which may carry a key, and quote at most maxErrorBody bytes
// of the response.
func (f *Fetcher) Fetch(ctx context.Context) (*Price, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.endpoint, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("eth/usd %s: %w", f.source, err)
	}
	req.Header.Set("User-Agent", f.agent)
	req.Header.Set("Accept", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("eth/usd %s: %w", f.source, transportError(err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("eth/usd %s: read body: %w", f.source, transportError(err))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("eth/usd %s: http %d: %s", f.source, resp.StatusCode, snippet(body))
	}
	amount, err := parse(f.source, body)
	if err != nil {
		return nil, fmt.Errorf("eth/usd %s: %w: %s", f.source, err, snippet(body))
	}
	return &Price{Price: amount.FloatString(decimals), At: f.now().UTC(), Source: f.source}, nil
}

// transportError strips the URL from a transport failure, so an endpoint
// carrying a key never reaches a log line.
func transportError(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) && uerr.Err != nil {
		return uerr.Err
	}
	return err
}

// snippet renders at most maxErrorBody bytes of a response body for an
// error message.
func snippet(body []byte) string {
	if len(body) > maxErrorBody {
		return fmt.Sprintf("%q (truncated)", body[:maxErrorBody])
	}
	return fmt.Sprintf("%q", body)
}

// document holds every field the three shapes are read from; only the one
// belonging to the source is used.
type document struct {
	Data struct {
		Amount json.RawMessage `json:"amount"`
	} `json:"data"`
	Ethereum struct {
		USD json.RawMessage `json:"usd"`
	} `json:"ethereum"`
	Price json.RawMessage `json:"price"`
}

// parse decodes one provider's body into a positive rational price.
func parse(source string, body []byte) (*big.Rat, error) {
	var doc document
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, ErrMalformed
	}
	switch source {
	case SourceCoinbase:
		return decimal(doc.Data.Amount)
	case SourceCoinGecko:
		return decimal(doc.Ethereum.USD)
	default:
		return decimal(doc.Price)
	}
}

// decimal reads a JSON scalar that is either a number or a decimal string
// into a positive rational. Fractions, hexadecimal and digit separators are
// not prices and are rejected along with everything else big.Rat would
// otherwise accept.
func decimal(raw json.RawMessage) (*big.Rat, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return nil, ErrMalformed
	}
	if strings.HasPrefix(s, `"`) {
		// raw comes out of a decoded document, so it is valid JSON and a
		// leading quote means a string; anything else stays empty and is
		// rejected below with everything that is not a number.
		var str string
		_ = json.Unmarshal(raw, &str)
		s = strings.TrimSpace(str)
	}
	if strings.ContainsAny(s, "/_xX") {
		return nil, ErrMalformed
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok || r.Sign() <= 0 {
		return nil, ErrMalformed
	}
	return r, nil
}
