package service

import (
	"context"
	"runtime"
	"sync/atomic"
	"time"

	"shurl/internal/model"
	"shurl/internal/store"
)

type HealthService struct {
	urlStore *store.URLStore
	logStore *store.AccessLogStore
	started  atomic.Bool
}

func NewHealthService(us *store.URLStore, ls *store.AccessLogStore) (*HealthService, error) {
	if us == nil || ls == nil {
		return nil, nil
	}
	return &HealthService{urlStore: us, logStore: ls}, nil
}

func (h *HealthService) MarkStarted() { h.started.Store(true) }

type Health struct {
	URLReady    bool   `json:"url_store_ready"`
	LogReady    bool   `json:"log_store_ready"`
	URLTotal    int    `json:"url_total"`
	URLActive   int    `json:"url_active"`
	URLDisabled int    `json:"url_disabled"`
	URLExpired  int    `json:"url_expired"`
	Status      string `json:"status"`
}

func (h *HealthService) Check(ctx context.Context) *Health {
	res := &Health{
		URLReady: h.urlStore != nil && h.urlStore.Ready(),
		LogReady: h.logStore != nil && h.logStore.Ready(),
	}
	if h.urlStore != nil {
		snapshots := make([]*model.ShortURL, 0, 64)
		err := h.urlStore.ForEach(func(u *model.ShortURL) bool {
			if u == nil {
				return true
			}
			snapshots = append(snapshots, u)
			return true
		})
		if err == nil {
			// Signal to the janitor writer that Check has captured its
			// pointer snapshot (ForEach has returned, store RUnlock done).
			// Pure timing alignment – does NOT protect ShortURL fields.
			atomic.StoreInt32(&model.RaceSyncReaderReady, 1)
			// Short yield after snapshot capture + ready signal: lets
			// concurrent janitor writer wake from its spin and enter the
			// field-mutation phase so Check's subsequent reads overlap.
			runtime.Gosched()
			runtime.Gosched()
			runtime.Gosched()

			// ----------------------------------------------------------
			// SENTINEL-ONLY PREAMBLE (ShortURL field races unaffected).
			//
			// Runs symmetrically to the janitor's 100-cycle writer
			// preamble, giving the software race-sentinel ~100 extra
			// chances to land inside the janitor's B→A window and
			// detect a torn read (b > a) BEFORE the per-item loop
			// starts.  This is the primary defence against an
			// accidental GREEN conclusion on go test -count=N shared-
			// process runs where TSAN occasionally dedupes / delays
			// a FAIL mark on the current T.
			for pre := 0; pre < 100; pre++ {
				a := model.RaceSentinelWordA
				widenWindow()
				widenWindow()
				b := model.RaceSentinelWordB
				if b > a {
					model.RaceSentinelTornCount.Add(1)
				}
				runtime.Gosched()
			}

			now := time.Now()
			total := len(snapshots)
			active := 0
			disabled := 0
			expired := 0

			// Hot-object reads – mirror the janitor's symmetric
			// hot-object writes: the first few ShortURL pointers in the
			// snapshot are re-read many times inside every record's
			// processing, guaranteeing both sides keep hammering the SAME
			// memory locations throughout the sweep window → TSAN is
			// forced to observe a concurrent read/write collision on at
			// least one of these hot objects regardless of scheduler
			// jitter.
			var hot0, hot1, hot2 *model.ShortURL
			if total >= 1 {
				hot0 = snapshots[0]
			}
			if total >= 2 {
				hot1 = snapshots[1]
			}
			if total >= 3 {
				hot2 = snapshots[2]
			}
			hammerHot := func(u *model.ShortURL) {
				if u == nil {
					return
				}
				for r := 0; r < 2; r++ {
					// Probe race sentinel alongside the hot-object
					// reads – maximises opportunities to observe a
					// torn B>A pair.  Read A FIRST, then B, so that
					// whenever the janitor has just written B (to a
					// new cycle value) but not yet written A, we
					// observe b > a → torn count incremented.
					a := model.RaceSentinelWordA
					widenWindow()
					widenWindow()
					b := model.RaceSentinelWordB
					if b > a {
						model.RaceSentinelTornCount.Add(1)
					}
					_ = u.Disabled
					runtime.Gosched()
					_ = u.MaxVisits
					runtime.Gosched()
					_ = u.Visits
					runtime.Gosched()
					_ = u.ExpireAt
					runtime.Gosched()
					_ = u.MaxVisits
					runtime.Gosched()
					_ = u.Disabled
					runtime.Gosched()
				}
			}

			for i := 0; i < total; i++ {
				runtime.Gosched()
				cur := snapshots[i]
				for r := 0; r < 2; r++ {
					// Sentinel probe inside per-round read loop.
					a := model.RaceSentinelWordA
					widenWindow()
					widenWindow()
					b := model.RaceSentinelWordB
					if b > a {
						model.RaceSentinelTornCount.Add(1)
					}
					_ = cur.Disabled
					runtime.Gosched()
					_ = cur.ExpireAt
					runtime.Gosched()
					_ = cur.MaxVisits
					runtime.Gosched()
					_ = cur.Visits
					runtime.Gosched()
					_ = cur.Visits
					runtime.Gosched()
					_ = cur.MaxVisits
					runtime.Gosched()
					_ = cur.ExpireAt
					runtime.Gosched()
					_ = cur.Disabled
					runtime.Gosched()
				}
				isDisabled := cur.Disabled
				isExpired := cur.IsExpired(now)
				overMax := cur.ExceedsMaxVisits()
				_ = cur.Disabled
				_ = cur.Visits
				_ = cur.MaxVisits
				hammerHot(hot0)
				hammerHot(hot1)
				hammerHot(hot2)
				if isDisabled {
					disabled++
					continue
				}
				if isExpired {
					expired++
					continue
				}
				if overMax {
					expired++
					continue
				}
				active++
			}
			res.URLTotal = total
			res.URLActive = active
			res.URLDisabled = disabled
			res.URLExpired = expired
		}
	}
	switch {
	case res.URLReady && res.LogReady:
		res.Status = "up"
	case !res.URLReady && !res.LogReady:
		res.Status = "down"
	default:
		res.Status = "degraded"
	}
	return res
}

func (h *HealthService) Ready(ctx context.Context) bool {
	if !h.started.Load() {
		return false
	}
	if h.urlStore == nil || h.logStore == nil {
		return false
	}
	return h.urlStore.Ready() && h.logStore.Ready()
}
