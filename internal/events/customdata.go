package events

import (
	"encoding/json"
	"sync"
)

// CustomData is opaque caller-supplied JSON carried on a leg and echoed in
// every event for that leg. It is stored verbatim so number precision and key
// order survive the round trip.
type CustomData json.RawMessage

func (c CustomData) MarshalJSON() ([]byte, error) {
	if len(c) == 0 {
		return []byte("null"), nil
	}
	return c, nil
}

func (c *CustomData) UnmarshalJSON(b []byte) error {
	*c = append((*c)[0:0], b...)
	return nil
}

// IsNull reports whether the value is an explicit JSON null, which callers use
// to clear previously attached data.
func (c CustomData) IsNull() bool {
	return string(c) == "null"
}

// CustomDataRegistry holds the custom data attached to each leg. It mirrors
// WebhookRegistry's per-leg lifetime: entries are set when the leg is created
// (or answered) and cleared only after leg.disconnected has been published, so
// the final event still carries the correlation data.
type CustomDataRegistry struct {
	mu   sync.RWMutex
	legs map[string]CustomData
}

func NewCustomDataRegistry() *CustomDataRegistry {
	return &CustomDataRegistry{legs: make(map[string]CustomData)}
}

func (r *CustomDataRegistry) SetLeg(legID string, d CustomData) {
	if legID == "" {
		return
	}
	r.mu.Lock()
	r.legs[legID] = append(CustomData(nil), d...)
	r.mu.Unlock()
}

func (r *CustomDataRegistry) ClearLeg(legID string) {
	r.mu.Lock()
	delete(r.legs, legID)
	r.mu.Unlock()
}

// Leg returns the data registered for legID, or nil if none.
func (r *CustomDataRegistry) Leg(legID string) CustomData {
	if legID == "" {
		return nil
	}
	r.mu.RLock()
	d := r.legs[legID]
	r.mu.RUnlock()
	return d
}
