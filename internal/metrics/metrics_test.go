package metrics

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// scrape renders g the way a Prometheus server would see it.
func scrape(t *testing.T, g prometheus.Gatherer) string {
	t.Helper()
	rec := httptest.NewRecorder()
	Handler(g).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Path, http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape status %d: %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// hasSeries fails unless the exposition text carries the sample exactly.
func hasSeries(t *testing.T, body, sample string) {
	t.Helper()
	for line := range strings.SplitSeq(body, "\n") {
		if strings.TrimSpace(line) == sample {
			return
		}
	}
	t.Fatalf("missing series %q in:\n%s", sample, body)
}

// lacksSeries fails when any sample of the named series is present.
func lacksSeries(t *testing.T, body, name string) {
	t.Helper()
	for line := range strings.SplitSeq(body, "\n") {
		if strings.HasPrefix(line, name+"{") || strings.HasPrefix(line, name+" ") {
			t.Fatalf("unexpected series %q in:\n%s", name, body)
		}
	}
}

func TestNewRegistryCarriesRuntimeCollectors(t *testing.T) {
	body := scrape(t, NewRegistry())
	for _, want := range []string{"go_goroutines", "process_start_time_seconds"} {
		if !strings.Contains(body, want) {
			t.Fatalf("registry has no %s:\n%s", want, body)
		}
	}
}

// brokenCollector always fails to collect, so Gather returns an error.
type brokenCollector struct{}

func (brokenCollector) desc() *prometheus.Desc {
	return prometheus.NewDesc("gascurve_broken", "broken", nil, nil)
}

func (b brokenCollector) Describe(ch chan<- *prometheus.Desc) { ch <- b.desc() }

func (b brokenCollector) Collect(ch chan<- prometheus.Metric) {
	ch <- prometheus.NewInvalidMetric(b.desc(), errors.New("boom"))
}

func TestHandlerReportsGatherErrors(t *testing.T) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(brokenCollector{})
	rec := httptest.NewRecorder()
	Handler(reg).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Path, http.NoBody))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestMonotonicMirrorsGrowth(t *testing.T) {
	c := prometheus.NewCounter(prometheus.CounterOpts{Name: "x", Help: "x"})
	m := monotonic{c: c}
	m.observe(0)
	if got := testutil.ToFloat64(c); got != 0 {
		t.Fatalf("zero observation moved the counter to %v", got)
	}
	m.observe(5)
	m.observe(5)
	if got := testutil.ToFloat64(c); got != 5 {
		t.Fatalf("counter = %v, want 5", got)
	}
	m.observe(9)
	if got := testutil.ToFloat64(c); got != 9 {
		t.Fatalf("counter = %v, want 9", got)
	}
	// A source that started over is counted from zero again, not ignored.
	m.observe(2)
	if got := testutil.ToFloat64(c); got != 11 {
		t.Fatalf("counter after a restarted source = %v, want 11", got)
	}
	// The documented limit: a source replaced by one that has already
	// passed the last value read looks exactly like ordinary growth, and
	// the difference is lost. Nothing in this repository is exposed to it,
	// because the counters mirrored here live as long as the process, but
	// the helper must not be relied on for more than it does.
	m.observe(20)
	if got := testutil.ToFloat64(c); got != 29 {
		t.Fatalf("counter = %v, want 29", got)
	}
}

func TestBoolValue(t *testing.T) {
	if boolValue(true) != 1 || boolValue(false) != 0 {
		t.Fatal("boolValue must be 1 and 0")
	}
}

func TestHandlerServesText(t *testing.T) {
	reg := prometheus.NewRegistry()
	g := prometheus.NewGauge(prometheus.GaugeOpts{Name: "gascurve_probe", Help: "probe"})
	g.Set(3)
	reg.MustRegister(g)
	rec := httptest.NewRecorder()
	Handler(reg).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Path, http.NoBody))
	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	hasSeries(t, string(body), "gascurve_probe 3")
}
