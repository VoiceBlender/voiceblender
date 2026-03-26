package events

import (
	"sync"
	"time"
)

type Handler func(Event)

type Bus struct {
	mu       sync.RWMutex
	handlers []Handler
}

func NewBus() *Bus {
	return &Bus{}
}

func (b *Bus) Subscribe(h Handler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handlers = append(b.handlers, h)
}

func (b *Bus) Publish(typ EventType, data map[string]interface{}) {
	e := Event{
		Type:      typ,
		Timestamp: time.Now().UTC(),
		Data:      data,
	}
	b.mu.RLock()
	handlers := make([]Handler, len(b.handlers))
	copy(handlers, b.handlers)
	b.mu.RUnlock()
	for _, h := range handlers {
		h(e)
	}
}
