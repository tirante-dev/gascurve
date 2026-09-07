package collector

import (
	"context"
	"sync"
	"time"

	"github.com/tirante-dev/gascurve/internal/logger"
	"github.com/tirante-dev/gascurve/internal/model"
	"github.com/tirante-dev/gascurve/internal/prices"
)

// ethUsdLogEvery bounds how often a failing spot fetch is logged: the slow
// loop retries every network every slow_interval, and a provider that is
// down must not fill the log.
const ethUsdLogEvery = time.Minute

// defaultEthUsdMaxAge is the fallback for collector.eth_usd_max_age.
const defaultEthUsdMaxAge = 10 * time.Minute

// EthUsdFetcher reads the ETH/USD spot, a *prices.Fetcher in production.
type EthUsdFetcher interface {
	Fetch(ctx context.Context) (*prices.Price, error)
}

// ethUsdCache is the ETH/USD spot shared by every follower in the process:
// the price is the same for all of them, so it is fetched once per interval
// rather than once per network. The lock is held across the fetch, which is
// what makes concurrent slow ticks share one call instead of racing into
// several; prices.Timeout bounds how long a follower can wait for it.
//
// A failed fetch leaves the last value in place, so a provider blip does
// not blank the price: it disappears from the snapshots on its own once the
// tick's staleness rule finds it older than eth_usd_max_age.
type ethUsdCache struct {
	fetcher  EthUsdFetcher
	interval time.Duration

	mu sync.Mutex
	// last is the newest quote fetched, nil until the first success.
	last *prices.Price
	// fetchedAt is the last attempt, successful or not, so a provider that
	// is down is retried on the interval and not on every follower's tick.
	fetchedAt time.Time
	// loggedAt is the last failure logged, see ethUsdLogEvery.
	loggedAt time.Time
}

func newEthUsdCache(f EthUsdFetcher, interval time.Duration) *ethUsdCache {
	return &ethUsdCache{fetcher: f, interval: interval}
}

// value returns the current quote, fetching when the last attempt is at
// least interval old. Nil means nothing has been fetched successfully yet.
func (c *ethUsdCache) value(ctx context.Context, now time.Time, log *logger.Logger) *prices.Price {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.fetchedAt.IsZero() && now.Sub(c.fetchedAt) < c.interval {
		return c.last
	}
	c.fetchedAt = now
	p, err := c.fetcher.Fetch(ctx)
	if err != nil {
		if now.Sub(c.loggedAt) >= ethUsdLogEvery {
			c.loggedAt = now
			log.Debug("eth/usd fetch failed, keeping the last value until it ages out", "err", err.Error())
		}
		return c.last
	}
	c.last = p
	return p
}

// The process-wide cache, built on first use. Every follower in a process
// runs on one collector configuration, so the first caller's source and
// interval are the process's.
var (
	sharedEthUsdMu sync.Mutex
	sharedEthUsd   *ethUsdCache
)

// ethUsdCacheFor returns the process-wide cache for a source, nil when the
// source cannot be used (configuration validates it first, so this is a
// last line of defense rather than the normal path).
func ethUsdCacheFor(source string, interval time.Duration, log *logger.Logger) *ethUsdCache {
	sharedEthUsdMu.Lock()
	defer sharedEthUsdMu.Unlock()
	if sharedEthUsd != nil {
		return sharedEthUsd
	}
	f, err := prices.NewFetcher(source)
	if err != nil {
		log.Warn("eth/usd source unusable, the spot is disabled", "err", err.Error())
		return nil
	}
	sharedEthUsd = newEthUsdCache(f, interval)
	return sharedEthUsd
}

// ethUsdModel renders a quote for a snapshot: null once it is older than
// maxAge, so a stale price is never published as a live one.
func ethUsdModel(p *prices.Price, now time.Time, maxAge time.Duration) *model.EthUsd {
	if p == nil || now.Sub(p.At) > maxAge {
		return nil
	}
	return &model.EthUsd{Price: p.Price, At: p.At.UTC().Format(time.RFC3339), Source: p.Source}
}
