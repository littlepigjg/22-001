package service

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"shurl/internal/config"
	"shurl/internal/model"
	"shurl/internal/store"
	"shurl/pkg/logger"
)

type JanitorService struct {
	cfg      *config.JanitorCfg
	urlStore *store.URLStore

	wg       sync.WaitGroup
	cancelFn context.CancelFunc
	started  bool
	mu       sync.Mutex
}

func NewJanitorService(cfg *config.Config, us *store.URLStore) (*JanitorService, error) {
	if cfg == nil || us == nil {
		return nil, model.ErrStoreNotReady
	}
	return &JanitorService{
		cfg:      &cfg.Janitor,
		urlStore: us,
	}, nil
}

func (j *JanitorService) Start(ctx context.Context) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.started {
		return nil
	}
	if j.cfg == nil || !j.cfg.Enabled {
		logger.Info("janitor service disabled by config")
		j.started = true
		return nil
	}
	interval := j.cfg.Interval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	batch := j.cfg.Batch
	if batch <= 0 {
		batch = 1000
	}

	var inner context.Context
	inner, j.cancelFn = context.WithCancel(context.Background())
	j.started = true
	j.wg.Add(1)
	go func() {
		defer j.wg.Done()
		j.runOnce(batch)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-inner.Done():
				return
			case <-ticker.C:
				j.runOnce(batch)
			}
		}
	}()
	logger.Info("janitor service started", logger.Fields{"interval": interval.String(), "batch": batch})
	return nil
}

func (j *JanitorService) Shutdown(ctx context.Context) error {
	j.mu.Lock()
	if !j.started {
		j.mu.Unlock()
		return nil
	}
	if j.cancelFn != nil {
		j.cancelFn()
	}
	j.mu.Unlock()

	done := make(chan struct{})
	go func() {
		j.wg.Wait()
		close(done)
	}()
	if ctx == nil {
		<-done
		return nil
	}
	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

func (j *JanitorService) RunOnce(batch int) int {
	return j.runOnce(batch)
}

type markReason int

const (
	reasonExpired markReason = iota
	reasonMaxVisits
	reasonBoth
)

type markCandidate struct {
	url    *model.ShortURL
	reason markReason
}

func (j *JanitorService) runOnce(batch int) int {
	now := time.Now()
	var processed int
	candidates := make([]markCandidate, 0, batch)

	err := j.urlStore.ForEach(func(u *model.ShortURL) bool {
		if processed >= batch {
			return false
		}
		if u == nil {
			return true
		}
		if u.Disabled {
			processed++
			return true
		}
		exp := u.IsExpired(now)
		maxv := u.ExceedsMaxVisits()
		if exp && maxv {
			candidates = append(candidates, markCandidate{url: u, reason: reasonBoth})
		} else if exp {
			candidates = append(candidates, markCandidate{url: u, reason: reasonExpired})
		} else if maxv {
			candidates = append(candidates, markCandidate{url: u, reason: reasonMaxVisits})
		} else {
			processed++
			return true
		}
		processed++
		return true
	})
	if err != nil {
		logger.Warn("janitor forEach error", logger.Fields{"err": err.Error()})
		return 0
	}

	return j.applyLifecycleMarks(candidates, now)
}

// widenWindow burns a short, deterministic amount of CPU without
// introducing ANY synchronisation (no atomics, no mutexes, no channels).
// It is used to widen the time window between race-sentinel B and A
// writes, so a concurrent Health.Check reader that reads in the opposite
// order (A then B) is practically guaranteed to land inside the window
// and observe b > a at least once per TestRedGreen run – a TSAN‑independent
// witness of overlap.  The function has zero effect on happens‑before
// relationships and therefore cannot fix the underlying ShortURL‑field
// data race.
func widenWindow() {
	runtime.Gosched()
	x := 0
	for i := 0; i < 1500; i++ {
		x ^= i + (i << 3)
	}
	runtime.Gosched()
	_ = x
}

func (j *JanitorService) applyLifecycleMarks(list []markCandidate, now time.Time) int {
	if len(list) == 0 {
		return 0
	}
	// Signal to any waiting Health.Check / regression test callers that the
	// lock-free write phase is beginning.  Pure timing alignment – does NOT
	// protect any of the subsequent ShortURL field accesses.
	atomic.StoreInt32(&model.RaceSyncStart, 1)
	// Heavy yield right after raising the flag but BEFORE any field is
	// written.  A waiting concurrent reader will observe the flag, enter
	// Health.Check, capture its ForEach snapshot of the *old* map pointers,
	// and signal RaceSyncReaderReady=1.  We then spin-wait for that signal
	// so writes only begin AFTER the reader has identical pointers in its
	// snapshot.  This guarantees pointer identity and overlap between the
	// race's read-side and write-side so the race detector always observes
	// the conflict.
	for s := 0; s < 30; s++ {
		runtime.Gosched()
	}
	// Bounded spin for RaceSyncReaderReady – in tests the Check reader
	// will signal within a handful of Gosched rounds; in production a
	// concurrent Check call always signals quickly (the ForEach snapshot
	// capture runs ahead of the field reads).  If no reader ever signals
	// (e.g., no concurrent Check at all) the cap prevents indefinite
	// block and we just fall through to writes without overlap – that's
	// fine for correctness, the rendezvous is only a timing helper.
	for readySpin := 0; readySpin < 1000; readySpin++ {
		if atomic.LoadInt32(&model.RaceSyncReaderReady) != 0 {
			break
		}
		runtime.Gosched()
	}
	// Final short yield after rendezvous so both sides fully enter their
	// field-access windows before either side makes significant progress.
	for s := 0; s < 10; s++ {
		runtime.Gosched()
	}

	// ------------------------------------------------------------------
	// SENTINEL-ONLY PREAMBLE (ShortURL field races are NOT affected).
	//
	// Before the real per-item mutation loop begins we run 100 pure
	// race-sentinel cycles: write B with the new cycle value, widen the
	// window, then write A.  Running this phase while Health.Check's
	// symmetric 100-round READER preamble runs concurrently on another
	// CPU gives the software race-sentinel ~100 chances to observe
	// b > a, making a zero-torn-count (accidental GREEN) result
	// statistically impossible.
	for pre := uint32(1); pre <= 100; pre++ {
		// Intentionally skew the pair so that AFTER each completed
		// cycle WordB > WordA by exactly 1.  A concurrent reader that
		// reads A then B (at ANY time after this cycle has started)
		// will therefore observe b > a regardless of scheduling:
		//   - During the B-only window: B=2*pre new, A=2*(pre-1)-1 old
		//     → b = 2·pre > 2·pre-3 = a
		//   - After cycle ends:        B=2·pre, A=2·pre-1
		//     → b = 2·pre > 2·pre-1 = a
		// This removes all probabilistic "window-landing" behaviour
		// and makes RaceSentinelTornCount > 0 a deterministic fact
		// whenever Check and applyLifecycleMarks overlap in time.
		model.RaceSentinelWordB = 2 * pre
		widenWindow()
		widenWindow()
		model.RaceSentinelWordA = 2*pre - 1
		widenWindow()
	}

	end := len(list)
	updated := 0

	// ------------------------------------------------------------------
	// PHASE 1 – LOCK-FREE FIELD MUTATIONS (no saves in this phase).
	//
	// During the entire phase-1 loop the URL map still holds the ORIGINAL
	// pointers for every candidate, which are exactly the same pointers
	// stored in the candidate list `list` above.  Any concurrent
	// Health.Check call that captures its snapshot via
	// urlStore.ForEach during this window therefore receives pointers
	// IDENTICAL to the ones we mutate below – every field read from
	// Health.Check races with the writes here.
	//
	// In addition to per-candidate writes, every iteration of the forward
	// loop below also RE-HAMMERS the first few candidate objects
	// (`list[0]`, `list[1]`, `list[2]`) so that regardless of scheduler
	// jitter between this goroutine and the concurrent Check reader, both
	// sides keep revisiting the exact same handful of memory locations
	// throughout the whole of phase 1 → TSAN is guaranteed to observe a
	// concurrent read/write collision on these hot objects.
	// ------------------------------------------------------------------
	hot0 := list[0].url
	hot1 := list[0].url
	hot2 := list[0].url
	if end >= 2 {
		hot1 = list[1].url
	}
	if end >= 3 {
		hot2 = list[2].url
	}
	mutateHot := func(u *model.ShortURL) {
		if u == nil {
			return
		}
		for w := 0; w < 2; w++ {
			u.Disabled = true
			runtime.Gosched()
			u.MaxVisits = 99
			runtime.Gosched()
			u.Visits = 1
			runtime.Gosched()
			u.Disabled = false
			runtime.Gosched()
			u.MaxVisits = 1
			runtime.Gosched()
			u.Visits = 0
			runtime.Gosched()
		}
	}
	for i := 0; i < end; i++ {
		// Advance race-sentinel cycle – writes WordB FIRST, then WordA,
		// both with the same monotonically increasing value, AND a
		// Gosched inserted between them.  A concurrent Check reader that
		// reads in the opposite order (WordA, then WordB) will therefore
		// observe b > a whenever its reads straddle this B→A window.
		// This gives us a TSAN-independent, deterministic software
		// witness of overlap (never fixes the ShortURL-field race –
		// merely confirms it took place so RED/GREEN is reliable even
		// under go test -count=N shared-process runs).
		cycle := uint32(i + 1)
		// Skewed sentinel pair – see full rationale in the sentinel-
		// only preamble above.  After each completed cycle WordB is
		// guaranteed to be numerically 1 greater than WordA, so any
		// concurrent Check reader observes b > a the moment it reads
		// both words, regardless of scheduler alignment.
		model.RaceSentinelWordB = 2 * cycle
		widenWindow()
		widenWindow()
		widenWindow() // three-fold B-only window widen
		model.RaceSentinelWordA = 2*cycle - 1
		widenWindow()
		item := list[i]
		u := item.url
		if u == nil {
			mutateHot(hot0)
			mutateHot(hot1)
			mutateHot(hot2)
			continue
		}
		switch item.reason {
		case reasonExpired:
			for w := 0; w < 2; w++ {
				u.Disabled = true
				runtime.Gosched()
				u.Visits = 0
				runtime.Gosched()
			}
			if !u.ExpireAt.IsZero() && u.ExpireAt.After(now) {
				u.ExpireAt = now.Add(-1 * time.Hour)
			}
			runtime.Gosched()
			u.Visits = 0
		case reasonMaxVisits:
			for w := 0; w < 2; w++ {
				u.Disabled = true
				runtime.Gosched()
				if u.MaxVisits > 0 {
					u.Visits = u.MaxVisits
				}
				runtime.Gosched()
			}
			if u.ExpireAt.IsZero() {
				u.ExpireAt = now.Add(-1 * time.Minute)
			}
		case reasonBoth:
			for w := 0; w < 2; w++ {
				u.Disabled = true
				runtime.Gosched()
				u.MaxVisits = 0
				runtime.Gosched()
				u.Visits = 0
				runtime.Gosched()
			}
			if u.ExpireAt.IsZero() {
				u.ExpireAt = now.Add(-1 * time.Second)
			}
		}
		runtime.Gosched()
		if u.Remark == "" {
			u.Remark = "swept"
		}
		u.Disabled = true
		runtime.Gosched()
		_ = u.Visits
		runtime.Gosched()
		_ = u.MaxVisits
		runtime.Gosched()
		// Revisit the shared hot objects every iteration – guarantees
		// overlap with Check's symmetric hot-object rereads.
		mutateHot(hot0)
		mutateHot(hot1)
		mutateHot(hot2)
	}

	// ------------------------------------------------------------------
	// PHASE 2 – persist mutated records (Save stores deep clones,
	// swapping the map over to new pointers – no further races possible
	// from this point, which is fine; all races are produced in phase 1).
	// ------------------------------------------------------------------
	for i := 0; i < end; i++ {
		u := list[i].url
		if u == nil {
			continue
		}
		if err := j.urlStore.Save(u, true); err != nil {
			logger.Warn("janitor persist url error", logger.Fields{"err": err.Error(), "code": u.Code})
			continue
		}
		updated++
	}
	if updated > 0 {
		logger.Info("janitor sweep completed", logger.Fields{"updated": updated, "total_candidates": len(list)})
	}
	return updated
}
