package traffic

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"hazuki-go/internal/metrics"
	"hazuki-go/internal/storage"
)

type Options struct {
	FlushInterval   time.Duration
	CleanupInterval time.Duration
}

type Persister struct {
	db      *sql.DB
	metrics *metrics.Registry

	mu   sync.Mutex
	last map[string]metrics.Snapshot

	totals map[string]storage.TrafficCounts
}

func NewPersister(db *sql.DB, reg *metrics.Registry) *Persister {
	return &Persister{
		db:      db,
		metrics: reg,
		last:    map[string]metrics.Snapshot{},
		totals:  map[string]storage.TrafficCounts{},
	}
}

func (p *Persister) Init(ctx context.Context) error {
	if p == nil || p.db == nil {
		return nil
	}
	totals, err := storage.GetTrafficTotals(ctx, p.db)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.totals = totals
	p.mu.Unlock()
	return nil
}

func (p *Persister) Start(ctx context.Context, opts Options) (stop func()) {
	if p == nil {
		return func() {}
	}

	flushEvery := opts.FlushInterval
	if flushEvery <= 0 {
		flushEvery = 5 * time.Second
	}
	cleanupEvery := opts.CleanupInterval
	if cleanupEvery <= 0 {
		cleanupEvery = 1 * time.Hour
	}

	stopCh := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer func() {
			if r := recover(); r != nil {
				log.Printf("traffic: persister goroutine panic: %v", r)
			}
		}()

		flushTicker := time.NewTicker(flushEvery)
		defer flushTicker.Stop()

		cleanupTicker := time.NewTicker(cleanupEvery)
		defer cleanupTicker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-stopCh:
				return
			case <-flushTicker.C:
				if err := p.Flush(context.Background(), time.Now()); err != nil {
					log.Printf("traffic: flush failed: %v", err)
				}
			case <-cleanupTicker.C:
				if err := p.Cleanup(context.Background(), time.Now()); err != nil {
					log.Printf("traffic: cleanup failed: %v", err)
				}
			}
		}
	}()

	return func() {
		close(stopCh)
		wg.Wait()
	}
}

func (p *Persister) TotalsNow() map[string]storage.TrafficCounts {
	if p == nil || p.metrics == nil {
		return map[string]storage.TrafficCounts{}
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	out := make(map[string]storage.TrafficCounts, len(p.totals))
	for k, v := range p.totals {
		out[k] = v
	}

	// Add unflushed deltas (current snapshot - last flushed snapshot).
	cur := p.metrics.Snapshot()
	for key, c := range cur {
		prev := p.last[key]
		d := diffSnapshot(c, prev)
		if d.BytesIn == 0 && d.BytesOut == 0 && d.Requests == 0 {
			continue
		}
		t := out[key]
		t.BytesIn += d.BytesIn
		t.BytesOut += d.BytesOut
		t.Requests += d.Requests
		out[key] = t
	}

	return out
}

func (p *Persister) ResetBaseline() {
	if p == nil || p.metrics == nil {
		return
	}
	p.mu.Lock()
	p.last = p.metrics.Snapshot()
	p.mu.Unlock()
}

func (p *Persister) ClearAll(ctx context.Context) error {
	if p == nil {
		return nil
	}
	if p.db == nil {
		return errors.New("traffic: db is nil")
	}
	if p.metrics == nil {
		return errors.New("traffic: metrics registry is nil")
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if err := storage.ClearTrafficStats(ctx, p.db); err != nil {
		return err
	}
	clear(p.totals)
	p.last = p.metrics.Snapshot()
	return nil
}

func (p *Persister) Flush(ctx context.Context, now time.Time) error {
	if p == nil || p.db == nil || p.metrics == nil {
		return nil
	}
	now = now.UTC()

	p.mu.Lock()
	defer p.mu.Unlock()

	cur := p.metrics.Snapshot()

	keys := make([]string, 0, len(cur))
	for k := range cur {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	deltas := make([]storage.TrafficDelta, 0, len(keys))
	for _, key := range keys {
		c := cur[key]
		prev := p.last[key]
		d := diffSnapshot(c, prev)
		if d.BytesIn == 0 && d.BytesOut == 0 && d.Requests == 0 {
			continue
		}
		deltas = append(deltas, storage.TrafficDelta{
			Service: key,
			TrafficCounts: storage.TrafficCounts{
				BytesIn:  d.BytesIn,
				BytesOut: d.BytesOut,
				Requests: d.Requests,
			},
		})
	}

	// Always advance baseline even if there's nothing to flush.
	// This keeps unflushed delta small when the admin polls frequently.
	if len(deltas) == 0 {
		p.last = cur
		return nil
	}

	starts := computeBucketStarts(now)
	if err := storage.AddTrafficSample(ctx, p.db, starts, deltas, now); err != nil {
		return err
	}

	for _, d := range deltas {
		svc := strings.TrimSpace(d.Service)
		if svc == "" {
			continue
		}
		t := p.totals[svc]
		t.BytesIn += d.BytesIn
		t.BytesOut += d.BytesOut
		t.Requests += d.Requests
		p.totals[svc] = t
	}
	p.last = cur
	return nil
}

func (p *Persister) Cleanup(ctx context.Context, now time.Time) error {
	if p == nil || p.db == nil {
		return nil
	}
	now = now.UTC()

	p.mu.Lock()
	defer p.mu.Unlock()

	ret, err := storage.GetTrafficRetention(ctx, p.db)
	if err != nil {
		return err
	}
	_, err = storage.CleanupTrafficBuckets(ctx, p.db, ret, now)
	return err
}

func computeBucketStarts(now time.Time) map[string]int64 {
	now = now.UTC()
	h0 := now.Truncate(time.Hour)
	d0 := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	m0 := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	y0 := time.Date(now.Year(), 1, 1, 0, 0, 0, 0, time.UTC)

	return map[string]int64{
		"hour":  h0.Unix(),
		"day":   d0.Unix(),
		"month": m0.Unix(),
		"year":  y0.Unix(),
	}
}

func diffSnapshot(cur, prev metrics.Snapshot) metrics.Snapshot {
	dIn := cur.BytesIn - prev.BytesIn
	dOut := cur.BytesOut - prev.BytesOut
	dReq := cur.Requests - prev.Requests

	// Guard against counter resets.
	if dIn < 0 {
		dIn = cur.BytesIn
	}
	if dOut < 0 {
		dOut = cur.BytesOut
	}
	if dReq < 0 {
		dReq = cur.Requests
	}

	return metrics.Snapshot{BytesIn: dIn, BytesOut: dOut, Requests: dReq}
}
