package proxy

import "sync"

// InFlightCoalescer manages concurrent requests for identical cache keys to avoid upstream dogpiling.
type InFlightCoalescer struct {
	mu      sync.Mutex
	waiters map[string][]chan struct{}
}

// NewInFlightCoalescer initializes an in-flight request coalescer.
func NewInFlightCoalescer() *InFlightCoalescer {
	return &InFlightCoalescer{
		waiters: make(map[string][]chan struct{}),
	}
}

// Start registers a key as in-flight.
// If it is the first caller for this key, it returns isFirst = true, nil.
// If another caller is already in-flight, it returns isFirst = false and a channel that closes when complete.
func (c *InFlightCoalescer) Start(key string) (bool, <-chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if chList, exists := c.waiters[key]; exists {
		ch := make(chan struct{})
		c.waiters[key] = append(chList, ch)
		return false, ch
	}

	c.waiters[key] = []chan struct{}{}
	return true, nil
}

// Done signals all waiting requests for this key and unregisters it.
func (c *InFlightCoalescer) Done(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if chList, exists := c.waiters[key]; exists {
		for _, ch := range chList {
			close(ch)
		}
		delete(c.waiters, key)
	}
}
