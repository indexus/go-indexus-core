package core

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/indexus/go-indexus-core/domain"
)

type shardState uint8

const (
	shardEvicted shardState = iota
	shardLoaded
)

type shardEntry struct {
	state      shardState
	lastAccess time.Time
	dirty      int32 // 0 = clean, 1 = dirty (atomic CAS so cntDirty stays in sync)
	pinCount   int32
}

// ShardStats is a snapshot of the shard manager counters, returned by Stats().
type ShardStats struct {
	Loaded    int64
	Evicted   int64
	Dirty     int64
	Hits      int64
	Misses    int64
	Loads     int64
	Evictions int64
}

// ShardManager tracks per-shard lifecycle: Loaded ↔ Evicted, dirty bit, last
// access for LRU eviction, singleflight loading, and cache hit/miss counters.
// It is intentionally decoupled from the storage and collection layers so it
// can be tested and reasoned about in isolation.
//
// Concurrency model: the mu guard protects entries and inflight; the atomic
// counters (cnt*) may be read / incremented without holding mu.
type ShardManager struct {
	mu       sync.Mutex
	entries  map[domain.Key]*shardEntry
	inflight map[domain.Key]chan struct{}

	maxLoaded int
	idleTTL   time.Duration

	cntLoaded    int64
	cntEvicted   int64
	cntDirty     int64
	cntHits      int64
	cntMisses    int64
	cntLoads     int64
	cntEvictions int64
}

// NewShardManager creates a manager with the given LRU budget and idle TTL.
// maxLoaded <= 0 means unlimited (no eviction by count). idleTTL == 0 disables
// idle-based eviction.
func NewShardManager(maxLoaded int, idleTTL time.Duration) *ShardManager {
	return &ShardManager{
		entries:   make(map[domain.Key]*shardEntry),
		inflight:  make(map[domain.Key]chan struct{}),
		maxLoaded: maxLoaded,
		idleTTL:   idleTTL,
	}
}

// Register records a shard as Evicted. Called for every shard found on disk
// during Restore so that EnsureLoaded can trigger a lazy load on first access.
// If the shard was already registered as Loaded (e.g. by MarkLoaded during the
// cluster-replay phase), it is demoted back to Evicted.
func (m *ShardManager) Register(k domain.Key) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if e, ok := m.entries[k]; ok {
		if e.state == shardLoaded {
			atomic.AddInt64(&m.cntLoaded, -1)
			atomic.AddInt64(&m.cntEvicted, 1)
		}
		e.state = shardEvicted
		return
	}
	m.entries[k] = &shardEntry{state: shardEvicted}
	atomic.AddInt64(&m.cntEvicted, 1)
}

// MarkLoaded records a shard as fully resident in RAM. Called by n.own() when
// a new ownership area is created during normal (non-restore) operation so the
// shard is immediately available without a disk round-trip.
func (m *ShardManager) MarkLoaded(k domain.Key) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if e, ok := m.entries[k]; ok {
		if e.state != shardLoaded {
			e.state = shardLoaded
			atomic.AddInt64(&m.cntLoaded, 1)
			atomic.AddInt64(&m.cntEvicted, -1)
		}
		e.lastAccess = time.Now()
		return
	}
	m.entries[k] = &shardEntry{state: shardLoaded, lastAccess: time.Now()}
	atomic.AddInt64(&m.cntLoaded, 1)
}

// Touch updates the last-access time for a loaded shard and increments the
// hit counter. Safe to call from concurrent readers (only the entry pointer
// is read under mu, the fields are updated with relaxed ordering).
func (m *ShardManager) Touch(k domain.Key) {
	m.mu.Lock()
	e, ok := m.entries[k]
	m.mu.Unlock()
	if ok && e.state == shardLoaded {
		e.lastAccess = time.Now()
		atomic.AddInt64(&m.cntHits, 1)
	}
}

// IsDirty returns true if the shard has unsaved changes.
func (m *ShardManager) IsDirty(k domain.Key) bool {
	m.mu.Lock()
	e, ok := m.entries[k]
	m.mu.Unlock()
	return ok && atomic.LoadInt32(&e.dirty) == 1
}

// MarkDirty flags a shard as needing a snapshot on the next Refresh tick.
// The clean→dirty transition increments cntDirty exactly once.
func (m *ShardManager) MarkDirty(k domain.Key) {
	m.mu.Lock()
	e, ok := m.entries[k]
	m.mu.Unlock()
	if ok && atomic.CompareAndSwapInt32(&e.dirty, 0, 1) {
		atomic.AddInt64(&m.cntDirty, 1)
	}
}

// IsLoaded returns true if the shard is currently in RAM.
func (m *ShardManager) IsLoaded(k domain.Key) bool {
	m.mu.Lock()
	e, ok := m.entries[k]
	m.mu.Unlock()
	return ok && e.state == shardLoaded
}

// EnsureLoaded guarantees the shard is in RAM before returning.
// If already loaded, it records a hit and returns immediately.
// If evicted, exactly one goroutine calls loader(); any concurrent callers
// wait for that single load (singleflight).
func (m *ShardManager) EnsureLoaded(k domain.Key, loader func() error) error {
	m.mu.Lock()
	if e, ok := m.entries[k]; ok && e.state == shardLoaded {
		e.lastAccess = time.Now()
		atomic.AddInt64(&m.cntHits, 1)
		m.mu.Unlock()
		return nil
	}
	// Check for an in-flight load.
	if ch, loading := m.inflight[k]; loading {
		m.mu.Unlock()
		<-ch
		return nil
	}
	// We are the designated loader.
	ch := make(chan struct{})
	m.inflight[k] = ch
	atomic.AddInt64(&m.cntMisses, 1)
	m.mu.Unlock()

	var loadErr error
	defer func() {
		m.mu.Lock()
		delete(m.inflight, k)
		if loadErr == nil {
			if e, ok := m.entries[k]; ok {
				e.state = shardLoaded
				e.lastAccess = time.Now()
				// freshly loaded == consistent with disk; clear dirty
				if atomic.CompareAndSwapInt32(&e.dirty, 1, 0) {
					atomic.AddInt64(&m.cntDirty, -1)
				}
			} else {
				m.entries[k] = &shardEntry{state: shardLoaded, lastAccess: time.Now()}
			}
			atomic.AddInt64(&m.cntLoaded, 1)
			atomic.AddInt64(&m.cntEvicted, -1)
			atomic.AddInt64(&m.cntLoads, 1)
		}
		m.mu.Unlock()
		close(ch)
	}()

	loadErr = loader()
	return loadErr
}

// Pin prevents a loaded shard from being evicted during a write window.
func (m *ShardManager) Pin(k domain.Key) {
	m.mu.Lock()
	e, ok := m.entries[k]
	m.mu.Unlock()
	if ok {
		atomic.AddInt32(&e.pinCount, 1)
	}
}

// Unpin releases a write-window pin.
func (m *ShardManager) Unpin(k domain.Key) {
	m.mu.Lock()
	e, ok := m.entries[k]
	m.mu.Unlock()
	if ok {
		atomic.AddInt32(&e.pinCount, -1)
	}
}

// PlanWork returns the disjoint sets of shards that the next Refresh tick
// should snapshot-only (dirty loaded shards staying in RAM) and evict
// (oldest loaded shards over the LRU budget or idle past TTL). Eviction
// candidates are excluded from toSnapshot — the eviction path snapshots
// dirty shards itself before removing them, so they would otherwise be
// written twice.
func (m *ShardManager) PlanWork(now time.Time) (toSnapshot, toEvict []domain.Key) {
	m.mu.Lock()
	defer m.mu.Unlock()

	type kv struct {
		k domain.Key
		e *shardEntry
	}
	var loaded []kv
	for k, e := range m.entries {
		if e.state == shardLoaded {
			loaded = append(loaded, kv{k, e})
		}
	}

	// Pass 1: pick eviction candidates (LRU + idle TTL) so we know which
	// keys to exclude from toSnapshot.
	evictSet := make(map[domain.Key]struct{})
	if m.maxLoaded > 0 || m.idleTTL > 0 {
		sort.Slice(loaded, func(i, j int) bool {
			return loaded[i].e.lastAccess.Before(loaded[j].e.lastAccess)
		})
		over := 0
		if m.maxLoaded > 0 {
			over = len(loaded) - m.maxLoaded
		}
		for i, kv := range loaded {
			if atomic.LoadInt32(&kv.e.pinCount) > 0 {
				continue
			}
			idleExpired := m.idleTTL > 0 && now.Sub(kv.e.lastAccess) > m.idleTTL
			overBudget := m.maxLoaded > 0 && i < over
			if idleExpired || overBudget {
				evictSet[kv.k] = struct{}{}
				toEvict = append(toEvict, kv.k)
			}
		}
	}

	// Pass 2: dirty loaded shards NOT scheduled for eviction.
	for _, kv := range loaded {
		if _, evicting := evictSet[kv.k]; evicting {
			continue
		}
		if atomic.LoadInt32(&kv.e.dirty) == 1 {
			toSnapshot = append(toSnapshot, kv.k)
		}
	}
	return
}

// MarkSnapshotted clears the dirty flag after a successful snapshot write.
// The dirty→clean transition decrements cntDirty exactly once.
func (m *ShardManager) MarkSnapshotted(k domain.Key) {
	m.mu.Lock()
	e, ok := m.entries[k]
	m.mu.Unlock()
	if ok && atomic.CompareAndSwapInt32(&e.dirty, 1, 0) {
		atomic.AddInt64(&m.cntDirty, -1)
	}
}

// MarkEvicted transitions a shard from Loaded to Evicted.
func (m *ShardManager) MarkEvicted(k domain.Key) {
	m.mu.Lock()
	e, ok := m.entries[k]
	m.mu.Unlock()
	if ok && e.state == shardLoaded {
		e.state = shardEvicted
		// Belt-and-suspenders: if a shard is evicted while still dirty
		// (race window), keep cntDirty consistent by clearing it here.
		if atomic.CompareAndSwapInt32(&e.dirty, 1, 0) {
			atomic.AddInt64(&m.cntDirty, -1)
		}
		atomic.AddInt64(&m.cntLoaded, -1)
		atomic.AddInt64(&m.cntEvicted, 1)
		atomic.AddInt64(&m.cntEvictions, 1)
	}
}

// Stats returns a point-in-time snapshot of cache counters.
func (m *ShardManager) Stats() ShardStats {
	return ShardStats{
		Loaded:    atomic.LoadInt64(&m.cntLoaded),
		Evicted:   atomic.LoadInt64(&m.cntEvicted),
		Dirty:     atomic.LoadInt64(&m.cntDirty),
		Hits:      atomic.LoadInt64(&m.cntHits),
		Misses:    atomic.LoadInt64(&m.cntMisses),
		Loads:     atomic.LoadInt64(&m.cntLoads),
		Evictions: atomic.LoadInt64(&m.cntEvictions),
	}
}
