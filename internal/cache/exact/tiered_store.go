package exact

import (
	"context"
	"sync"
	"time"

	"github.com/primaybr/liltok/internal/cache"
	"github.com/primaybr/liltok/internal/db"
	"github.com/primaybr/liltok/internal/telemetry"
)

type asyncTaskType int

const (
	taskInsert asyncTaskType = iota
	taskHit
)

type asyncTask struct {
	taskType asyncTaskType
	entry    *cache.CacheEntry
	hash     string
}

// TieredStore implements cache.Store by combining L1 Memory LRU with L2 SQLite WAL storage.
type TieredStore struct {
	l1       *MemoryLRU
	l2       *SQLiteStore
	taskChan chan asyncTask
	wg       sync.WaitGroup
	stopChan chan struct{}
}

// NewTieredStore initializes the multi-tier cache store and starts the background async worker.
func NewTieredStore(database *db.DB, l1Capacity int) (*TieredStore, error) {
	l1, err := NewMemoryLRU(l1Capacity)
	if err != nil {
		return nil, err
	}

	l2 := NewSQLiteStore(database)

	ts := &TieredStore{
		l1:       l1,
		l2:       l2,
		taskChan: make(chan asyncTask, 5000),
		stopChan: make(chan struct{}),
	}

	ts.wg.Add(1)
	go ts.backgroundWorker()

	return ts, nil
}

// Get checks L1 memory first, falling back to L2 SQLite on a miss.
func (t *TieredStore) Get(ctx context.Context, hash string) (*cache.CacheEntry, bool, error) {
	// 1. Check L1 Memory LRU (< 50us)
	if entry, ok := t.l1.Get(hash); ok {
		t.queueHitIncrement(hash)
		return entry, true, nil
	}

	// 2. Check L2 SQLite (< 3ms)
	entry, ok, err := t.l2.Get(ctx, hash)
	if err != nil {
		telemetry.Log.Warn().Err(err).Str("hash", hash).Msg("L2 SQLite cache read error")
		return nil, false, nil // Fail-open principle
	}
	if !ok {
		return nil, false, nil
	}

	// Promote cold entry back to L1
	t.l1.Set(entry)
	t.queueHitIncrement(hash)

	return entry, true, nil
}

// Set saves entry in L1 memory immediately and schedules async persistence to SQLite.
func (t *TieredStore) Set(ctx context.Context, entry *cache.CacheEntry) error {
	if entry == nil {
		return nil
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now()
	}
	if entry.LastAccessedAt.IsZero() {
		entry.LastAccessedAt = time.Now()
	}

	// Instant L1 memory write
	t.l1.Set(entry)

	// Asynchronous persistence queue (never blocks client responses)
	select {
	case t.taskChan <- asyncTask{taskType: taskInsert, entry: entry}:
	default:
		telemetry.Log.Warn().Str("hash", entry.Hash).Msg("Async cache queue full, dropping L2 write")
	}

	return nil
}

// Delete evicts an entry from both L1 and L2 caches.
func (t *TieredStore) Delete(ctx context.Context, hash string) error {
	t.l1.Remove(hash)
	return t.l2.Delete(ctx, hash)
}

// Purge removes all cached entries (or entries for a specific model) across L1 and L2.
func (t *TieredStore) Purge(ctx context.Context, model string) (int64, error) {
	t.l1.Purge()
	return t.l2.Purge(ctx, model)
}

// Stats returns aggregated statistics across memory and disk tiers.
func (t *TieredStore) Stats(ctx context.Context) (cache.CacheStats, error) {
	totalEntries, totalHits, err := t.l2.Stats(ctx)
	if err != nil {
		return cache.CacheStats{}, err
	}
	return cache.CacheStats{
		TotalEntries:  totalEntries,
		MemoryEntries: t.l1.Len(),
		TotalHits:     totalHits,
	}, nil
}

// Close gracefully stops the background persistence worker.
func (t *TieredStore) Close() error {
	close(t.stopChan)
	t.wg.Wait()
	return nil
}

func (t *TieredStore) queueHitIncrement(hash string) {
	select {
	case t.taskChan <- asyncTask{taskType: taskHit, hash: hash}:
	default:
	}
}

func (t *TieredStore) backgroundWorker() {
	defer t.wg.Done()
	ctx := context.Background()

	for {
		select {
		case <-t.stopChan:
			// Drain remaining tasks before exiting
			for {
				select {
				case task := <-t.taskChan:
					t.processTask(ctx, task)
				default:
					return
				}
			}
		case task := <-t.taskChan:
			t.processTask(ctx, task)
		}
	}
}

func (t *TieredStore) processTask(ctx context.Context, task asyncTask) {
	switch task.taskType {
	case taskInsert:
		if err := t.l2.Set(ctx, task.entry); err != nil {
			telemetry.Log.Warn().Err(err).Str("hash", task.entry.Hash).Msg("Async L2 cache insert error")
		}
	case taskHit:
		_ = t.l2.IncrementHit(ctx, task.hash)
	}
}
