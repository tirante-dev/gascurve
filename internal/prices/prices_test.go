package prices

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tirante-dev/gascurve/internal/version"
)

var fixedNow = time.Date(2026, 9, 6, 7, 0, 0, 0, time.UTC)

// serve answers every request with body and status, recording the last
// request seen.
func serve(t *testing.T, status int, body string) (*httptest.Server, *http.Request) {
	t.Helper()
	var last http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		last = *r
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &last
}

func fetcher(t *testing.T, source, endpoint string) *Fetcher {
	t.Helper()
	f, err := NewFetcher(source, WithEndpoint(endpoint), WithClock(func() time.Time { return fixedNow }))
	if err != nil {
		t.Fatalf("NewFetcher(%q): %v", source, err)
	}
	return f
}

// TestFetchProviders: every supported shape yields the same normalized
// quote, with two decimal places and the provider's name.
func TestFetchProviders(t *testing.T) {
	cases := []struct {
		name    string
		source  string
		body    string
		want    string
		source2 string
	}{
		{"coinbase", SourceCoinbase, `{"data":{"base":"ETH","currency":"USD","amount":"4523.4"}}`, "4523.40", SourceCoinbase},
		{"coingecko", SourceCoinGecko, `{"ethereum":{"usd":4523.456}}`, "4523.46", SourceCoinGecko},
		{"custom number", "https://prices.example/eth", `{"price":4523}`, "4523.00", SourceCustom},
		{"custom string", "https://prices.example/eth", `{"price":"4523.421"}`, "4523.42", SourceCustom},
		{"custom exponent", "https://prices.example/eth", `{"price":4.5234e3}`, "4523.40", SourceCustom},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, last := serve(t, http.StatusOK, tc.body)
			p, err := fetcher(t, tc.source, srv.URL).Fetch(context.Background())
			if err != nil {
				t.Fatalf("fetch: %v", err)
			}
			if p.Price != tc.want || p.Source != tc.source2 || !p.At.Equal(fixedNow) {
				t.Fatalf("got %+v, want price %q source %q", p, tc.want, tc.source2)
			}
			if got := last.Header.Get("User-Agent"); got != version.UserAgent() {
				t.Fatalf("User-Agent = %q", got)
			}
			if got := last.Header.Get("Accept"); got != "application/json" {
				t.Fatalf("Accept = %q", got)
			}
		})
	}
}

// TestFetchMalformed: nothing that is not a positive price is accepted, and
// the error says so without becoming a copy of the response.
func TestFetchMalformed(t *testing.T) {
	cases := map[string]struct {
		source string
		body   string
	}{
		"not json":         {SourceCoinbase, `<html>nope</html>`},
		"missing field":    {SourceCoinbase, `{"data":{"currency":"USD"}}`},
		"null field":       {SourceCoinbase, `{"data":{"amount":null}}`},
		"empty string":     {SourceCoinbase, `{"data":{"amount":""}}`},
		"not a number":     {SourceCoinbase, `{"data":{"amount":"tuppence"}}`},
		"nested object":    {SourceCoinbase, `{"data":{"amount":{"usd":1}}}`},
		"zero":             {SourceCoinGecko, `{"ethereum":{"usd":0}}`},
		"negative":         {SourceCoinGecko, `{"ethereum":{"usd":-4523.4}}`},
		"wrong provider":   {SourceCoinGecko, `{"data":{"amount":"4523.40"}}`},
		"fraction":         {"https://prices.example/eth", `{"price":"9/2"}`},
		"hex":              {"https://prices.example/eth", `{"price":"0x1f"}`},
		"digit separators": {"https://prices.example/eth", `{"price":"4_523.40"}`},
		"empty body":       {"https://prices.example/eth", ``},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv, _ := serve(t, http.StatusOK, tc.body)
			_, err := fetcher(t, tc.source, srv.URL).Fetch(context.Background())
			if !errors.Is(err, ErrMalformed) {
				t.Fatalf("got %v, want ErrMalformed", err)
			}
		})
	}
}

// TestFetchErrorBodyBounded: a provider answering with a page is reported
// with at most maxErrorBody bytes of it.
func TestFetchErrorBodyBounded(t *testing.T) {
	body := strings.Repeat("x", 5_000)
	srv, _ := serve(t, http.StatusTooManyRequests, body)
	_, err := fetcher(t, SourceCoinbase, srv.URL).Fetch(context.Background())
	if err == nil {
		t.Fatal("expected an error for http 429")
	}
	msg := err.Error()
	if !strings.Contains(msg, "http 429") || !strings.Contains(msg, "(truncated)") {
		t.Fatalf("error should name the status and say it truncated: %q", msg)
	}
	if strings.Count(msg, "x") != maxErrorBody {
		t.Fatalf("error quotes %d body bytes, want %d", strings.Count(msg, "x"), maxErrorBody)
	}
	// A malformed body is quoted under the same cap.
	srv2, _ := serve(t, http.StatusOK, body)
	_, err = fetcher(t, SourceCoinbase, srv2.URL).Fetch(context.Background())
	if err == nil || strings.Count(err.Error(), "x") != maxErrorBody {
		t.Fatalf("malformed body not bounded: %v", err)
	}
}

// TestFetchTransportError: a failure to reach the provider never quotes the
// endpoint, which may carry a key in its path.
func TestFetchTransportError(t *testing.T) {
	srv, _ := serve(t, http.StatusOK, `{}`)
	endpoint := srv.URL + "/secret-key-abc"
	srv.Close()
	_, err := fetcher(t, SourceCoinbase, endpoint).Fetch(context.Background())
	if err == nil {
		t.Fatal("expected a transport error")
	}
	if strings.Contains(err.Error(), "secret-key-abc") {
		t.Fatalf("error leaks the endpoint: %q", err)
	}
	if !strings.Contains(err.Error(), SourceCoinbase) {
		t.Fatalf("error should name the source: %q", err)
	}
}

// TestFetchTruncatedBody: a response that ends early fails rather than
// being parsed as far as it got.
func TestFetchTruncatedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "512")
		_, _ = w.Write([]byte(`{"data":{"amount":"4523.40"}}`))
	}))
	t.Cleanup(srv.Close)
	_, err := fetcher(t, SourceCoinbase, srv.URL).Fetch(context.Background())
	if err == nil {
		t.Fatal("expected a read error for a short body")
	}
	if !strings.Contains(err.Error(), "read body") {
		t.Fatalf("got %v, want a read body error", err)
	}
}

// TestFetchContextCancelled: the caller's context cancels a fetch.
func TestFetchContextCancelled(t *testing.T) {
	srv, _ := serve(t, http.StatusOK, `{"data":{"amount":"1"}}`)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fetcher(t, SourceCoinbase, srv.URL).Fetch(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

// TestNewFetcher: the two providers resolve to their documented endpoints,
// an https URL is its own endpoint, and everything else is rejected without
// echoing the value.
func TestNewFetcher(t *testing.T) {
	cases := map[string]struct{ endpoint, source string }{
		SourceCoinbase:               {coinbaseURL, SourceCoinbase},
		SourceCoinGecko:              {coingeckoURL, SourceCoinGecko},
		"https://prices.example/eth": {"https://prices.example/eth", SourceCustom},
	}
	for in, want := range cases {
		f, err := NewFetcher(in)
		if err != nil {
			t.Fatalf("NewFetcher(%q): %v", in, err)
		}
		if f.endpoint != want.endpoint || f.Source() != want.source {
			t.Fatalf("NewFetcher(%q) = %q/%q, want %q/%q", in, f.endpoint, f.Source(), want.endpoint, want.source)
		}
		if f.client.Timeout != Timeout {
			t.Fatalf("client timeout = %v, want %v", f.client.Timeout, Timeout)
		}
	}
	for _, bad := range []string{"kraken", "http://insecure.example", "ftp://prices.example", "https://", "://", " "} {
		if _, err := NewFetcher(bad); !errors.Is(err, ErrUnsupportedSource) {
			t.Fatalf("NewFetcher(%q) = %v, want ErrUnsupportedSource", bad, err)
		}
	}
	// A rejected source is never echoed: it may be a URL carrying a key.
	_, err := NewFetcher("https://prices.example/key\x7f")
	if err == nil || strings.Contains(err.Error(), "prices.example") {
		t.Fatalf("the error must not echo the source: %v", err)
	}
}

// TestValidateSource: an empty source is valid and disables the fetch.
func TestValidateSource(t *testing.T) {
	for _, ok := range []string{"", SourceCoinbase, SourceCoinGecko, "https://prices.example/eth"} {
		if err := ValidateSource(ok); err != nil {
			t.Fatalf("ValidateSource(%q) = %v", ok, err)
		}
	}
	if err := ValidateSource("kraken"); !errors.Is(err, ErrUnsupportedSource) {
		t.Fatalf("ValidateSource(kraken) = %v", err)
	}
}

// TestFetchBadRequest: an endpoint that cannot become a request fails
// before any call is made.
func TestFetchBadRequest(t *testing.T) {
	f := fetcher(t, SourceCoinbase, "https://prices.example/\x7f")
	if _, err := f.Fetch(context.Background()); err == nil {
		t.Fatal("expected a request error")
	}
}
