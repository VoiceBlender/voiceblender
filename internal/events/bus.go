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

	// CustomData holds the opaque per-leg JSON that Publish stamps onto every
	// leg-scoped event. Owned by the bus so no wiring step can be missed.
	CustomData *CustomDataRegistry
}

func NewBus(instanceID string) *Bus {
	return &Bus{
		handlers:   make(map[uint64]Handler),
		instanceID: instanceID,
		CustomData: NewCustomDataRegistry(),
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

func (b *Bus) Publish(typ EventType, data EventData) {
	// The ID is stamped once here, before fan-out, so every subscriber and every
	// webhook retry of this event sees the same value.
	e := Event{
		Type:       typ,
		EventID:    uuid.NewString(),
		Timestamp:  time.Now().UTC(),
		InstanceID: b.instanceID,
		Data:       data,
	}
	// Resolved once, before fan-out, so every subscriber and every webhook
	// retry sees the same value.
	if data != nil {
		e.CustomData = b.CustomData.Leg(data.GetLegID())
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
