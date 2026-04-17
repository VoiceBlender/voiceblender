package events

import (
	"sync"
	"time"
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

func (b *Bus) Publish(typ EventType, data EventData) {
	e := Event{
		Type:       typ,
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
