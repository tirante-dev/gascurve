package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/tirante-dev/gascurve/internal/db"
	"github.com/tirante-dev/gascurve/internal/model"
	"github.com/tirante-dev/gascurve/internal/pricer"
)

// Cache-Control values per endpoint.
const (
	cacheNone    = "no-store"
	cacheShort   = "public, max-age=1"
	cacheNetwork = "public, max-age=5"
	cacheHour    = "public, max-age=5"
	cacheDay     = "public, max-age=30"
	cacheMonth   = "public, max-age=300"
	cacheAll     = "public, max-age=600"
)

var errNoData = errors.New("no data yet")

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, cacheNone, map[string]string{"status": "ok", "version": s.version})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "not_ready", "database unavailable")
		return
	}
	if s.listener != nil && !s.listener.Status().Ready {
		writeError(w, http.StatusServiceUnavailable, "not_ready", "notification listener unavailable")
		return
	}
	writeJSON(w, http.StatusOK, cacheNone, map[string]string{"status": "ready"})
}

// resolveNetwork looks up {network} by name or chain id.
func (s *Server) resolveNetwork(w http.ResponseWriter, r *http.Request) (*db.Network, bool) {
	ref := chi.URLParam(r, "network")
	n, err := s.store.NetworkByRef(r.Context(), ref)
	if err != nil {
		s.internal(w, err)
		return nil, false
	}
	if n == nil {
		writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("unknown network %q", ref))
		return nil, false
	}
	return n, true
}

func (s *Server) internal(w http.ResponseWriter, err error) {
	s.log.Error("request failed", "err", err.Error())
	writeError(w, http.StatusInternalServerError, "internal", "internal error")
}

// networkModel renders a network row, deriving the model from the latest
// sample.
func (s *Server) networkModel(ctx context.Context, n db.Network) (model.Network, error) {
	out := model.Network{
		Name: n.Name, DisplayName: n.DisplayName, ChainID: n.ChainID, ExplorerURL: n.ExplorerURL,
		Model: model.ModelUnknown, Enabled: n.Enabled,
	}
	if n.HeadBlock.Valid {
		out.HeadBlock = uint64(max(n.HeadBlock.Int64, 0))
	}
	out.HeadAt, out.LagSeconds = s.headTimes(n.HeadAt)
	sample, err := s.store.LatestStateSample(ctx, n.ChainID, false)
	if err != nil {
		return out, err
	}
	if sample != nil {
		out.Model = sampleModel(sample)
	}
	return out, nil
}

// headTimes renders a nullable head time and the lag behind now; both are
// nil until the collector has produced a head.
func (s *Server) headTimes(t sql.NullTime) (headAt *string, lag *int64) {
	if !t.Valid {
		return nil, nil
	}
	at := t.Time.UTC().Format(time.RFC3339)
	secs := max(int64(s.now().Sub(t.Time).Seconds()), 0)
	return &at, &secs
}

func sampleModel(sample *db.StateSample) string {
	if sample.Legacy != nil {
		return model.ModelLegacy
	}
	var constraints []model.Constraint
	if err := sample.Constraints.Unmarshal(&constraints); err == nil && len(constraints) > 0 {
		return model.ModelConstraints
	}
	return model.ModelUnknown
}

func (s *Server) handleNetworks(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.Networks(r.Context())
	if err != nil {
		s.internal(w, err)
		return
	}
	out := make([]model.Network, 0, len(rows))
	for _, n := range rows {
		m, err := s.networkModel(r.Context(), n)
		if err != nil {
			s.internal(w, err)
			return
		}
		out = append(out, m)
	}
	writeJSON(w, http.StatusOK, cacheNetwork, out)
}

func (s *Server) handleNetwork(w http.ResponseWriter, r *http.Request) {
	n, ok := s.resolveNetwork(w, r)
	if !ok {
		return
	}
	m, err := s.networkModel(r.Context(), *n)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cacheNetwork, m)
}

// buildLive assembles the LiveSnapshot from the latest sample and the
// block that sample was taken at, inside one repeatable-read transaction,
// so every field describes the same database moment: the tick that wrote
// the selected sample, even when the collector commits meanwhile.
func (s *Server) buildLive(ctx context.Context, chainID uint64) (*model.LiveSnapshot, error) {
	var snap *model.LiveSnapshot
	err := s.store.WithSnapshotTx(ctx, func(st db.Store) error {
		var err error
		snap, err = buildLiveIn(ctx, st, chainID, s.now(), s.ethUsdMaxAge)
		return err
	})
	return snap, err
}

// buildLiveIn is buildLive against one store view. now and maxAge apply the
// collector's staleness rule to the recorded ETH/USD spot.
func buildLiveIn(ctx context.Context, store db.Store, chainID uint64, now time.Time, maxAge time.Duration) (*model.LiveSnapshot, error) {
	sample, err := store.LatestStateSample(ctx, chainID, false)
	if err != nil {
		return nil, err
	}
	if sample == nil {
		return nil, errNoData
	}
	slow := sample
	if sample.L1 == nil {
		if slow, err = store.LatestStateSample(ctx, chainID, true); err != nil {
			return nil, err
		}
	}
	block, err := store.BlockByNumber(ctx, chainID, sample.BlockNumber)
	if err != nil {
		return nil, err
	}
	snap := &model.LiveSnapshot{
		ChainID:     chainID,
		SampledAt:   sample.SampledAt.UTC().Format(time.RFC3339),
		BaseFee:     sample.BaseFee.String(),
		MinBaseFee:  sample.MinBaseFee.String(),
		Model:       sampleModel(sample),
		Constraints: []model.Constraint{},
	}
	if err := sample.Constraints.Unmarshal(&snap.Constraints); err != nil {
		return nil, fmt.Errorf("decode constraints: %w", err)
	}
	if snap.Constraints == nil {
		snap.Constraints = []model.Constraint{}
	}
	for _, c := range snap.Constraints {
		snap.ExponentBips += c.ExponentBips
	}
	if sample.Legacy != nil {
		snap.Legacy = &model.LegacyParams{}
		if err := sample.Legacy.Unmarshal(snap.Legacy); err != nil {
			return nil, fmt.Errorf("decode legacy: %w", err)
		}
		// The exponent the sampled backlog yields, exactly as the collector
		// computes it for the WebSocket tick.
		snap.ExponentBips = legacyExponent(snap.Legacy)
	}
	if err := sample.Prices.Unmarshal(&snap.Prices); err != nil {
		return nil, fmt.Errorf("decode prices: %w", err)
	}
	snap.MultiplierBips = multiplierBips(sample.BaseFee.BigInt(), sample.MinBaseFee.BigInt())
	if snap.EthUsd, err = ethUsdFromState(ctx, store, chainID, now, maxAge); err != nil {
		return nil, err
	}
	if slow != nil && slow.L1 != nil {
		snap.L1 = &model.L1{}
		if err := slow.L1.Unmarshal(snap.L1); err != nil {
			return nil, fmt.Errorf("decode l1: %w", err)
		}
		if slow.Accounts != nil {
			snap.Accounts = &model.Accounts{}
			if err := slow.Accounts.Unmarshal(snap.Accounts); err != nil {
				return nil, fmt.Errorf("decode accounts: %w", err)
			}
		}
	}
	if block != nil {
		snap.Block = model.LiveBlock{Number: block.Number, TS: uint64(block.TS.Unix()), GasUsed: block.GasUsed, BaseFee: block.BaseFee.String(), TxCount: block.TxCount}
		snap.ReplayErrorBips = replayError(*block)
		g10, err := store.GasUsedBetween(ctx, chainID, block.TS.Add(-10*time.Second), block.TS)
		if err != nil {
			return nil, err
		}
		g60, err := store.GasUsedBetween(ctx, chainID, block.TS.Add(-60*time.Second), block.TS)
		if err != nil {
			return nil, err
		}
		snap.GasPerSecond = model.GasPerSecond{S10: g10 / 10, S60: g60 / 60}
	} else {
		snap.Block = model.LiveBlock{Number: sample.BlockNumber, BaseFee: sample.BaseFee.String()}
	}
	return snap, nil
}

// ethUsdFutureSkew is how far ahead of the serving clock a recorded quote
// may be stamped before it is unusable. The collector and the API can run
// on different hosts, so a small difference is expected; a quote from
// materially later than now says the two clocks disagree, and its age
// cannot be judged at all.
const ethUsdFutureSkew = 5 * time.Minute

// staleEthUsd reports whether a quote taken at cannot be served as live:
// older than maxAge, or stamped materially later than the serving clock.
func staleEthUsd(at, now time.Time, maxAge time.Duration) bool {
	return now.Sub(at) > maxAge || at.Sub(now) > ethUsdFutureSkew
}

// ethUsdFromState reads the ETH/USD spot the collector recorded for a chain
// and applies the same rule as the tick: a quote older than maxAge, or one
// from materially later than the serving clock, is null rather than served
// as live. The API never fetches a price itself.
func ethUsdFromState(ctx context.Context, store db.Store, chainID uint64, now time.Time, maxAge time.Duration) (*model.EthUsd, error) {
	raw, ok, err := store.GetState(ctx, chainID, db.StateEthUsd)
	if err != nil || !ok {
		return nil, err
	}
	var v model.EthUsd
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, fmt.Errorf("decode eth/usd: %w", err)
	}
	at, err := time.Parse(time.RFC3339, v.At)
	if err != nil {
		return nil, fmt.Errorf("decode eth/usd timestamp: %w", err)
	}
	if staleEthUsd(at, now, maxAge) {
		return nil, nil
	}
	return &v, nil
}

func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	n, ok := s.resolveNetwork(w, r)
	if !ok {
		return
	}
	snap, err := s.buildLive(r.Context(), n.ChainID)
	if errors.Is(err, errNoData) {
		writeError(w, http.StatusNotFound, "not_found", "no live data yet for "+n.Name)
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cacheNone, snap)
}

func (s *Server) handleBlocks(w http.ResponseWriter, r *http.Request) {
	n, ok := s.resolveNetwork(w, r)
	if !ok {
		return
	}
	limit := intParam(r, "limit", 120, 1, 1000)
	rows, err := s.store.RecentBlocks(r.Context(), n.ChainID, limit)
	if err != nil {
		s.internal(w, err)
		return
	}
	out := make([]model.BlockPoint, 0, len(rows))
	for _, b := range rows {
		out = append(out, blockPoint(b))
	}
	writeJSON(w, http.StatusOK, cacheShort, out)
}

func (s *Server) handleSeries(w http.ResponseWriter, r *http.Request) {
	n, ok := s.resolveNetwork(w, r)
	if !ok {
		return
	}
	rng, ok := parseRange(r.URL.Query().Get("range"))
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_request", "range must be one of 1h, 24h, 30d, all")
		return
	}
	series, err := s.buildSeries(r.Context(), n.ChainID, rng)
	if err != nil {
		s.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rng.cache, series)
}

// handleConstraints returns the set history and, only while the latest
// sample's model is the constraints model, the newest set as current.
func (s *Server) handleConstraints(w http.ResponseWriter, r *http.Request) {
	n, ok := s.resolveNetwork(w, r)
	if !ok {
		return
	}
	sets, err := s.store.ConstraintSets(r.Context(), n.ChainID)
	if err != nil {
		s.internal(w, err)
		return
	}
	history := make([]model.ConstraintSet, 0, len(sets))
	for i := len(sets) - 1; i >= 0; i-- {
		cs, err := constraintSetModel(sets[i])
		if err != nil {
			s.internal(w, err)
			return
		}
		history = append(history, cs)
	}
	var current *model.ConstraintSet
	if len(history) > 0 {
		sample, err := s.store.LatestStateSample(r.Context(), n.ChainID, false)
		if err != nil {
			s.internal(w, err)
			return
		}
		if sample != nil && sampleModel(sample) == model.ModelConstraints {
			current = &history[0]
		}
	}
	writeJSON(w, http.StatusOK, cacheDay, map[string]any{"current": current, "history": history})
}

func (s *Server) handleOwnerActions(w http.ResponseWriter, r *http.Request) {
	n, ok := s.resolveNetwork(w, r)
	if !ok {
		return
	}
	limit := intParam(r, "limit", 100, 1, 1000)
	rows, err := s.store.OwnerActions(r.Context(), n.ChainID, time.Time{}, time.Time{}, limit)
	if err != nil {
		s.internal(w, err)
		return
	}
	out := make([]model.OwnerAction, 0, len(rows))
	for _, a := range rows {
		out = append(out, ownerActionModel(a))
	}
	writeJSON(w, http.StatusOK, cacheDay, out)
}

func (s *Server) handleBatches(w http.ResponseWriter, r *http.Request) {
	n, ok := s.resolveNetwork(w, r)
	if !ok {
		return
	}
	rng, ok := parseRange(r.URL.Query().Get("range"))
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_request", "range must be one of 1h, 24h, 30d, all")
		return
	}
	from, to := rng.window(s.now())
	out := model.BatchSeries{Range: rng.name, Resolution: rng.batchResolution, Points: []model.BatchPoint{}}
	if rng.batchStep == 0 {
		// The batch resolution is exactly one point per report.
		reports, err := s.store.BatchReports(r.Context(), n.ChainID, from, to)
		if err != nil {
			s.internal(w, err)
			return
		}
		for _, br := range reports {
			out.Points = append(out.Points, model.BatchPoint{
				T: br.BatchTS.Unix(), Batches: 1, GasSpent: br.GasSpent, WeiSpent: br.WeiSpent.String(),
				L1BaseFeeAvg: br.L1BaseFee.String(), CalldataBytes: br.CalldataLen,
			})
		}
		// The bounds are the response's window, exactly as the grouped
		// resolutions report it.
		out.From, out.To = rng.bounds(from, to, firstBatch(out.Points), len(out.Points) > 0)
		writeJSON(w, http.StatusOK, rng.cache, out)
		return
	}
	rows, err := s.store.BatchBuckets(r.Context(), n.ChainID, from, to, rng.batchStep)
	if err != nil {
		s.internal(w, err)
		return
	}
	for _, b := range rows {
		out.Points = append(out.Points, model.BatchPoint{
			T: b.T.Unix(), Batches: b.Batches, GasSpent: b.GasSpent, WeiSpent: b.WeiSpent.String(),
			L1BaseFeeAvg: b.L1BaseFeeAvg.String(), CalldataBytes: b.CalldataBytes,
		})
	}
	out.From, out.To = rng.bounds(from, to, firstBatch(out.Points), len(out.Points) > 0)
	writeJSON(w, http.StatusOK, rng.cache, out)
}

func (s *Server) handleL1(w http.ResponseWriter, r *http.Request) {
	n, ok := s.resolveNetwork(w, r)
	if !ok {
		return
	}
	rng, ok := parseRange(r.URL.Query().Get("range"))
	if !ok {
		writeError(w, http.StatusBadRequest, "bad_request", "range must be one of 1h, 24h, 30d, all")
		return
	}
	from, to := rng.window(s.now())
	rows, err := s.store.L1Samples(r.Context(), n.ChainID, from, to, rng.l1Step)
	if err != nil {
		s.internal(w, err)
		return
	}
	out := model.L1Series{Range: rng.name, Points: make([]model.L1Point, 0, len(rows))}
	for _, sm := range rows {
		var l1 model.L1
		if err := sm.L1.Unmarshal(&l1); err != nil {
			continue
		}
		out.Points = append(out.Points, model.L1Point{
			T: sm.SampledAt.Unix(), BaseFeeEstimate: l1.BaseFeeEstimate, Surplus: l1.Surplus,
			FeesAvailable: l1.FeesAvailable, UnitsSinceUpdate: l1.UnitsSinceUpdate,
		})
	}
	out.From, out.To = rng.bounds(from, to, firstL1(out.Points), len(out.Points) > 0)
	writeJSON(w, http.StatusOK, rng.cache, out)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.Networks(r.Context())
	if err != nil {
		s.internal(w, err)
		return
	}
	out := model.Status{Version: s.version, Status: model.StatusHealthy, Networks: make([]model.NetworkStatus, 0, len(rows))}
	if s.listener != nil {
		status := s.listener.Status()
		listener := &model.ListenerStatus{Ready: status.Ready, Reconnects: status.Reconnects}
		if status.Error != "" {
			listener.LastError = &status.Error
		}
		out.Listener = listener
		if !status.Ready {
			out.Status = model.StatusDegraded
		}
	}
	for _, n := range rows {
		st, err := s.store.States(r.Context(), n.ChainID)
		if err != nil {
			s.internal(w, err)
			return
		}
		ns := model.NetworkStatus{Name: n.Name, ChainID: n.ChainID, Enabled: n.Enabled, Status: model.StatusHealthy, DegradedReasons: []string{}}
		if n.HeadBlock.Valid {
			ns.HeadBlock = uint64(max(n.HeadBlock.Int64, 0))
		}
		ns.HeadAt, ns.LagSeconds = s.headTimes(n.HeadAt)
		if n.LastSampleAt.Valid {
			at := n.LastSampleAt.Time.UTC().Format(time.RFC3339)
			ns.LastSampleAt = &at
		}
		if n.LastError.Valid {
			e := n.LastError.String
			ns.LastError = &e
		}
		if v, ok := st[db.StateRateLimitEvents]; ok {
			ns.RateLimitEvents, _ = strconv.ParseUint(v, 10, 64)
		}
		ns.Last429At = optString(st, db.StateLast429At)
		ns.BackfillCursor = optString(st, db.StateBackfillCursor)
		ns.ArbOSVersion = optString(st, db.StateArbOSVersion)
		ns.Holes, err = s.holesStatus(r.Context(), n.ChainID, st)
		if err != nil {
			s.internal(w, err)
			return
		}
		ns.Capacity = capacityStatus(st)
		ns.Degraded = ns.Capacity.Saturated || ns.Capacity.CheckpointError || ns.Holes.Blocks > 0 || ns.Holes.CheckpointError
		ns.EndpointsStatus = endpointsStatus(st)
		ns.Collector = collectorTelemetry(st, s.now())
		if ns.Collector != nil {
			ns.RateLimitEvents = ns.Collector.RPC.RateLimitEvents
			ns.Last429At = ns.Collector.RPC.Last429At
		}
		ns.Status, ns.DegradedReasons = networkHealth(ns, s.now())
		if ns.Status == model.StatusDegraded {
			out.Status = model.StatusDegraded
		}
		out.Networks = append(out.Networks, ns)
	}
	writeJSON(w, http.StatusOK, cacheNone, out)
}

// holesStatus summarizes durable missing ranges. During a rolling upgrade it
// also reads the legacy checkpoint until the collector imports it. A malformed
// checkpoint sets an explicit degradation signal instead of looking empty.
func (s *Server) holesStatus(ctx context.Context, chainID uint64, st map[string]string) (model.HolesStatus, error) {
	rows, err := s.store.MissingRanges(ctx, chainID)
	if err != nil {
		return model.HolesStatus{}, err
	}
	holes := make([]model.Hole, 0, len(rows))
	checkpointError := false
	for _, row := range rows {
		// The decode only detects corruption. Once one row is malformed the
		// remaining ones add nothing, and /status is polled often.
		if !checkpointError && row.ReplayState != nil {
			var state model.HoleState
			if err := row.ReplayState.Unmarshal(&state); err != nil {
				checkpointError = true
			}
		}
		holes = append(holes, model.Hole{
			From: row.From, To: row.To, At: row.DetectedAt.UTC().Format(time.RFC3339), Lifecycle: row.Lifecycle,
			Next: row.Cursor, Reason: row.Reason, RetryCount: row.RetryCount,
		})
	}
	if raw, ok := st[db.StateHoles]; ok {
		var legacy []model.Hole
		if err := json.Unmarshal([]byte(raw), &legacy); err != nil {
			checkpointError = true
		} else {
			holes = append(holes, legacy...)
		}
	}
	now := s.now()
	out := model.SummarizeHolesAt(holes, now)
	out.CheckpointError = checkpointError
	oldest := now
	known := false
	for _, h := range holes {
		at, err := time.Parse(time.RFC3339, h.At)
		if err != nil {
			out.CheckpointError = true
			continue
		}
		if !known || at.Before(oldest) {
			oldest, known = at, true
		}
	}
	if known && oldest.Before(now) {
		out.OldestAgeSeconds = uint64(now.Sub(oldest) / time.Second)
	}
	return out, nil
}

func capacityStatus(st map[string]string) model.RPCCapacity {
	raw, ok := st[db.StateRPCCapacity]
	if !ok {
		return model.RPCCapacity{}
	}
	var out model.RPCCapacity
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		out.CheckpointError = true
	}
	return out
}

func collectorTelemetry(st map[string]string, now time.Time) *model.CollectorTelemetry {
	raw, ok := st[db.StateTelemetry]
	if !ok {
		return nil
	}
	var out model.CollectorTelemetry
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	if out.HeartbeatAt != nil {
		if at, err := time.Parse(time.RFC3339, *out.HeartbeatAt); err == nil {
			age := max(int64(now.Sub(at).Seconds()), 0)
			out.HeartbeatAgeSeconds = &age
		}
	}
	return &out
}

func networkHealth(n model.NetworkStatus, now time.Time) (status string, reasons []string) {
	if !n.Enabled {
		return model.StatusDisabled, []string{}
	}
	telemetry := n.Collector
	if telemetry == nil || telemetry.HeartbeatAt == nil || telemetry.HeartbeatAgeSeconds == nil {
		reasons = append(reasons, "collector heartbeat missing")
	} else if *telemetry.HeartbeatAgeSeconds > telemetry.HeartbeatStaleAfterSeconds {
		reasons = append(reasons, "collector heartbeat stale")
	}
	if telemetry != nil {
		for _, item := range []struct {
			name string
			loop model.LoopStatus
		}{
			{name: "fast", loop: telemetry.Loops.Fast},
			{name: "slow", loop: telemetry.Loops.Slow},
			{name: "history", loop: telemetry.Loops.History},
		} {
			name, loop := item.name, item.loop
			success := parseStatusTime(loop.LastSuccessAt)
			failure := parseStatusTime(loop.LastErrorAt)
			switch {
			case success.IsZero():
				reasons = append(reasons, name+" loop has not succeeded")
			case failure.After(success):
				reasons = append(reasons, name+" loop failing")
			case loop.StaleAfterSecs > 0 && now.Sub(success) > time.Duration(loop.StaleAfterSecs)*time.Second:
				reasons = append(reasons, name+" loop stale")
			}
		}
		if telemetry.HeadLagBlocks > 0 {
			reasons = append(reasons, "collector behind observed head")
		}
		if n.LagSeconds != nil && telemetry.Loops.Fast.StaleAfterSecs > 0 && *n.LagSeconds > telemetry.Loops.Fast.StaleAfterSecs {
			reasons = append(reasons, "chain head stale")
		}
	}
	for _, endpoint := range n.Endpoints {
		if endpoint.Disabled {
			reasons = append(reasons, "RPC endpoint disabled")
			break
		}
	}
	if n.Holes.OldestPendingAgeSeconds != nil && telemetry != nil && telemetry.Loops.History.StaleAfterSecs > 0 &&
		*n.Holes.OldestPendingAgeSeconds > telemetry.Loops.History.StaleAfterSecs {
		reasons = append(reasons, "pending gaps are stale")
	}
	if n.Holes.Blocks > 0 {
		reasons = append(reasons, "blocks missing from history")
	}
	if n.Holes.CheckpointError {
		reasons = append(reasons, "missing range checkpoint unreadable")
	}
	if n.Capacity.Saturated {
		reasons = append(reasons, "RPC capacity saturated")
	}
	if n.Capacity.CheckpointError {
		reasons = append(reasons, "RPC capacity checkpoint unreadable")
	}
	if len(reasons) > 0 {
		return model.StatusDegraded, reasons
	}
	return model.StatusHealthy, []string{}
}

func parseStatusTime(raw *string) time.Time {
	if raw == nil {
		return time.Time{}
	}
	at, _ := time.Parse(time.RFC3339, *raw)
	return at
}

// endpointsStatus decodes the collector's endpoint routing state; a
// network without one (or with an unreadable one) reports the primary
// alone with no endpoints listed.
func endpointsStatus(st map[string]string) model.EndpointsStatus {
	out := model.EndpointsStatus{Endpoints: []model.EndpointStatus{}}
	if raw, ok := st[db.StateEndpoints]; ok {
		var decoded model.EndpointsStatus
		if err := json.Unmarshal([]byte(raw), &decoded); err == nil && decoded.Endpoints != nil {
			out = decoded
		}
	}
	return out
}

func optString(m map[string]string, key string) *string {
	if v, ok := m[key]; ok {
		return &v
	}
	return nil
}

// intParam parses a bounded integer query parameter.
func intParam(r *http.Request, name string, def, lo, hi int) int {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return def
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return min(max(v, lo), hi)
}

func blockPoint(b db.Block) model.BlockPoint {
	return model.BlockPoint{
		Number: b.Number, TS: uint64(b.TS.Unix()), GasUsed: b.GasUsed, BaseFee: b.BaseFee.String(),
		PredictedBaseFee: b.PredictedBaseFee.String(), Backlogs: b.Backlogs.Uint64s(), ConstraintBips: int64s(b.ConstraintBips),
		ExponentBips: b.ExponentBips, MinBaseFee: b.MinBaseFee.StringPtr(), Anchored: b.Anchored,
	}
}

// int64s copies a BIGINT[]: a stored array (even empty) becomes a JSON
// array, a NULL (nil, a value that was never recorded) stays null.
func int64s(v []int64) []int64 {
	if v == nil {
		return nil
	}
	return append([]int64{}, v...)
}

// legacyExponent is the pricer exponent the legacy parameters and backlog
// yield at the start of the next block, the value the collector publishes.
func legacyExponent(l *model.LegacyParams) int64 {
	st := &pricer.State{MinBaseFee: new(big.Int), Legacy: &pricer.Legacy{SpeedLimit: l.SpeedLimit, Inertia: l.Inertia, Tolerance: l.Tolerance, Backlog: l.Backlog}}
	_, exponent, _ := st.Step(0)
	return int64(exponent)
}

func constraintSetModel(cs db.ConstraintSet) (model.ConstraintSet, error) {
	out := model.ConstraintSet{
		ID: cs.ID, EffectiveBlock: cs.EffectiveBlock, EffectiveAt: cs.EffectiveAt.UTC().Format(time.RFC3339),
		Source: cs.Source, Constraints: []model.ConstraintSetEntry{},
	}
	if err := cs.Constraints.Unmarshal(&out.Constraints); err != nil {
		return out, fmt.Errorf("constraint set %d: %w", cs.ID, err)
	}
	if out.Constraints == nil {
		out.Constraints = []model.ConstraintSetEntry{}
	}
	return out, nil
}

func ownerActionModel(a db.OwnerAction) model.OwnerAction {
	args := a.Args
	if args == nil {
		args = db.JSONB(`{}`)
	}
	return model.OwnerAction{
		Block: a.BlockNumber, At: a.TS.UTC().Format(time.RFC3339), TxHash: a.TxHash,
		Method: a.Method, Selector: a.Selector, Args: []byte(args),
	}
}

// replayError is |predicted - actual| in bips for a stored block.
func replayError(b db.Block) int64 {
	return pricer.ErrorBips(b.PredictedBaseFee.BigInt(), b.BaseFee.BigInt())
}

func multiplierBips(baseFee, minFee *big.Int) int64 {
	if minFee.Sign() == 0 {
		return 0
	}
	m := new(big.Int).Mul(baseFee, big.NewInt(int64(pricer.OneInBips)))
	m.Div(m, minFee)
	if !m.IsInt64() {
		return 1<<63 - 1
	}
	return m.Int64()
}

func firstBatch(points []model.BatchPoint) int64 {
	if len(points) == 0 {
		return 0
	}
	return points[0].T
}

func firstL1(points []model.L1Point) int64 {
	if len(points) == 0 {
		return 0
	}
	return points[0].T
}
