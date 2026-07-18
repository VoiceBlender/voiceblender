package events

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

type Handler func(Event)

type Bus struct {
	mu         sync.RWMutex
	handlers   map[uint64]Handler
	nextID     uint64
	instanceID string
}

func NewBus(instanceID string) *Bus {
	return &Bus{
		handlers:   make(map[uint64]Handler),
		instanceID: instanceID,
	}
}

// Subscribe registers h and returns an unsubscribe function that removes it.
func (b *Bus) Subscribe(h Handler) func() {
	b.mu.Lock()
	id := b.nextID
	b.nextID++
	b.handlers[id] = h
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		delete(b.handlers, id)
		b.mu.Unlock()
	}
}

// Publish builds the event envelope and invokes every subscribed handler
// synchronously on the caller's goroutine, so handlers must not block.
//
// The event is stamped with a fresh EventID here, before the handler snapshot
// is taken and before any sink retries. That ordering is what makes the id
// identical across all subscribers and constant across a webhook's delivery
// attempts: it is assigned once per event, never per fan-out and never per
// attempt.
func (b *Bus) Publish(typ EventType, data EventData) {
	e := Event{
		Type:       typ,
		EventID:    uuid.NewString(),
		Timestamp:  time.Now().UTC(),
		InstanceID: b.instanceID,
		Data:       data,
	}
	b.mu.RLock()
	handlers := make([]Handler, 0, len(b.handlers))
	for _, h := range b.handlers {
		handlers = append(handlers, h)
	}
	b.mu.RUnlock()
	for _, h := range handlers {
		h(e)
	}
}
