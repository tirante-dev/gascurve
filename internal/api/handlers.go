package api

import (
	"context"
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
	if n.HeadAt.Valid {
		out.HeadAt = n.HeadAt.Time.UTC().Format(time.RFC3339)
		out.LagSeconds = max(int64(s.now().Sub(n.HeadAt.Time).Seconds()), 0)
	}
	sample, err := s.store.LatestStateSample(ctx, n.ChainID, false)
	if err != nil {
		return out, err
	}
	if sample != nil {
		out.Model = sampleModel(sample)
	}
	return out, nil
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

// buildLive assembles the LiveSnapshot from the latest sample and block.
func (s *Server) buildLive(ctx context.Context, chainID uint64) (*model.LiveSnapshot, error) {
	sample, err := s.store.LatestStateSample(ctx, chainID, false)
	if err != nil {
		return nil, err
	}
	if sample == nil {
		return nil, errNoData
	}
	slow := sample
	if sample.L1 == nil {
		if slow, err = s.store.LatestStateSample(ctx, chainID, true); err != nil {
			return nil, err
		}
	}
	block, err := s.store.LatestBlock(ctx, chainID)
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
	}
	if err := sample.Prices.Unmarshal(&snap.Prices); err != nil {
		return nil, fmt.Errorf("decode prices: %w", err)
	}
	snap.MultiplierBips = multiplierBips(sample.BaseFee.BigInt(), sample.MinBaseFee.BigInt())
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
		if snap.Legacy != nil {
			snap.ExponentBips = block.ExponentBips
		}
		g10, err := s.store.GasUsedBetween(ctx, chainID, block.TS.Add(-10*time.Second), block.TS)
		if err != nil {
			return nil, err
		}
		g60, err := s.store.GasUsedBetween(ctx, chainID, block.TS.Add(-60*time.Second), block.TS)
		if err != nil {
			return nil, err
		}
		snap.GasPerSecond = model.GasPerSecond{S10: g10 / 10, S60: g60 / 60}
	} else {
		snap.Block = model.LiveBlock{Number: sample.BlockNumber, BaseFee: sample.BaseFee.String()}
	}
	return snap, nil
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
		current = &history[0]
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
	rows, err := s.store.BatchBuckets(r.Context(), n.ChainID, from, to, rng.batchStep)
	if err != nil {
		s.internal(w, err)
		return
	}
	out := model.BatchSeries{Range: rng.name, Resolution: rng.batchResolution, Points: make([]model.BatchPoint, 0, len(rows))}
	for _, b := range rows {
		out.Points = append(out.Points, model.BatchPoint{
			T: b.T.Unix(), Batches: b.Batches, GasSpent: b.GasSpent, WeiSpent: b.WeiSpent.String(),
			L1BaseFeeAvg: b.L1BaseFeeAvg.String(), CalldataBytes: b.CalldataBytes,
		})
	}
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
	writeJSON(w, http.StatusOK, rng.cache, out)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.Networks(r.Context())
	if err != nil {
		s.internal(w, err)
		return
	}
	out := model.Status{Version: s.version, Networks: make([]model.NetworkStatus, 0, len(rows))}
	for _, n := range rows {
		st, err := s.store.States(r.Context(), n.ChainID)
		if err != nil {
			s.internal(w, err)
			return
		}
		ns := model.NetworkStatus{Name: n.Name, ChainID: n.ChainID, Enabled: n.Enabled}
		if n.HeadBlock.Valid {
			ns.HeadBlock = uint64(max(n.HeadBlock.Int64, 0))
		}
		if n.HeadAt.Valid {
			ns.HeadAt = n.HeadAt.Time.UTC().Format(time.RFC3339)
			ns.LagSeconds = max(int64(s.now().Sub(n.HeadAt.Time).Seconds()), 0)
		}
		if n.LastSampleAt.Valid {
			ns.LastSampleAt = n.LastSampleAt.Time.UTC().Format(time.RFC3339)
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
		out.Networks = append(out.Networks, ns)
	}
	writeJSON(w, http.StatusOK, cacheNone, out)
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
		PredictedBaseFee: b.PredictedBaseFee.String(), Backlogs: db.Uint64s(b.Backlogs),
		ExponentBips: b.ExponentBips, Anchored: b.Anchored,
	}
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
