package exact

import (
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/liltok/liltok/internal/cache"
)

// MemoryLRU provides a thread-safe in-memory L1 cache.
type MemoryLRU struct {
	cache *lru.Cache[string, *cache.CacheEntry]
}

// NewMemoryLRU creates a new L1 LRU cache with the specified capacity.
func NewMemoryLRU(capacity int) (*MemoryLRU, error) {
	if capacity <= 0 {
		capacity = 10000
	}
	c, err := lru.New[string, *cache.CacheEntry](capacity)
	if err != nil {
		return nil, err
	}
	return &MemoryLRU{cache: c}, nil
}

// Get retrieves an entry from L1 memory, verifying that it has not expired according to its TTL.
func (m *MemoryLRU) Get(hash string) (*cache.CacheEntry, bool) {
	entry, ok := m.cache.Get(hash)
	if !ok {
		return nil, false
	}

	// Check TTL expiration
	if entry.TTLSeconds > 0 {
		expirationTime := entry.CreatedAt.Add(time.Duration(entry.TTLSeconds) * time.Second)
		if time.Now().After(expirationTime) {
			m.cache.Remove(hash)
			return nil, false
		}
	}

	return entry, true
}

// Set stores or updates an entry in L1 memory.
func (m *MemoryLRU) Set(entry *cache.CacheEntry) {
	m.cache.Add(entry.Hash, entry)
}

// Remove evicts an entry from L1 memory.
func (m *MemoryLRU) Remove(hash string) {
	m.cache.Remove(hash)
}

// Purge flushes all entries from L1 memory.
func (m *MemoryLRU) Purge() {
	m.cache.Purge()
}

// Len returns the current number of active entries in L1 memory.
func (m *MemoryLRU) Len() int {
	return m.cache.Len()
}
