package sip

import (
	"fmt"
	"sync"
)

// PortAllocator manages a pool of UDP ports within a configured range.
// It is safe for concurrent use.
type PortAllocator struct {
	mu   sync.Mutex
	min  int
	max  int
	used map[int]bool
	next int // cursor for round-robin scanning
}

// NewPortAllocator creates a port allocator for the given range [min, max].
// Returns nil if min and max are both 0 (use OS-assigned ports).
func NewPortAllocator(min, max int) (*PortAllocator, error) {
	if min == 0 && max == 0 {
		return nil, nil
	}
	if min <= 0 || max <= 0 {
		return nil, fmt.Errorf("RTP_PORT_MIN and RTP_PORT_MAX must both be set (got %d, %d)", min, max)
	}
	if min >= max {
		return nil, fmt.Errorf("RTP_PORT_MIN (%d) must be less than RTP_PORT_MAX (%d)", min, max)
	}
	if max-min < 100 {
		return nil, fmt.Errorf("RTP port range must be at least 100 ports (got %d)", max-min)
	}
	return &PortAllocator{
		min:  min,
		max:  max,
		used: make(map[int]bool),
		next: min,
	}, nil
}

// Allocate returns the next available port from the pool.
func (pa *PortAllocator) Allocate() (int, error) {
	pa.mu.Lock()
	defer pa.mu.Unlock()

	total := pa.max - pa.min + 1
	if len(pa.used) >= total {
		return 0, fmt.Errorf("RTP port pool exhausted (%d ports in use)", total)
	}

	// Scan from cursor, wrapping around.
	for i := 0; i < total; i++ {
		port := pa.min + (pa.next-pa.min+i)%total
		if !pa.used[port] {
			pa.used[port] = true
			pa.next = port + 1
			if pa.next > pa.max {
				pa.next = pa.min
			}
			return port, nil
		}
	}
	return 0, fmt.Errorf("RTP port pool exhausted")
}

// Release returns a port back to the pool.
func (pa *PortAllocator) Release(port int) {
	pa.mu.Lock()
	defer pa.mu.Unlock()
	delete(pa.used, port)
}

// Range returns the configured min and max ports.
func (pa *PortAllocator) Range() (int, int) {
	return pa.min, pa.max
}
