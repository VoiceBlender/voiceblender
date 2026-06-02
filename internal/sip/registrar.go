package sip

import (
	"context"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/emiago/sipgo/sip"
)

// Binding represents one Contact registered under an AOR.
type Binding struct {
	AOR            string
	Contact        string
	Socket         string
	Transport      string
	UserAgent      string
	CallID         string
	AppID          string
	CreatedAt      time.Time
	LastRefresh    time.Time
	ExpiresAt      time.Time
	GrantedExpires int // seconds granted at bind time (constant for the binding)
}

// RegistrarConfig holds tunables for the registrar.
type RegistrarConfig struct {
	DefaultExpiresSeconds int
	MaxExpiresSeconds     int
	SweepInterval         time.Duration
	AllowMultipleContacts bool
}

func (c RegistrarConfig) withDefaults() RegistrarConfig {
	if c.DefaultExpiresSeconds <= 0 {
		c.DefaultExpiresSeconds = 3600
	}
	if c.MaxExpiresSeconds <= 0 {
		c.MaxExpiresSeconds = 7200
	}
	if c.SweepInterval <= 0 {
		c.SweepInterval = time.Second
	}
	return c
}

// Registrar is the in-memory AOR registry. Safe for concurrent use.
type Registrar struct {
	cfg RegistrarConfig
	bus *events.Bus
	log *slog.Logger

	mu   sync.RWMutex
	aors map[string][]*Binding // canonical AOR -> bindings, sorted by LastRefresh desc
}

// NewRegistrar constructs a registrar. bus may be nil (events are dropped).
func NewRegistrar(bus *events.Bus, log *slog.Logger, cfg RegistrarConfig) *Registrar {
	if log == nil {
		log = slog.Default()
	}
	return &Registrar{
		cfg:  cfg.withDefaults(),
		bus:  bus,
		log:  log,
		aors: make(map[string][]*Binding),
	}
}

// Config returns the effective configuration (with defaults applied).
func (r *Registrar) Config() RegistrarConfig { return r.cfg }

// Start launches the expiry sweeper. Returns immediately; the sweeper runs
// until ctx is cancelled.
func (r *Registrar) Start(ctx context.Context) {
	go func() {
		t := time.NewTicker(r.cfg.SweepInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				r.sweep(now)
			}
		}
	}()
}

// ClampExpires clamps a requested expiry to [60, MaxExpiresSeconds]. A
// non-positive input is treated as the default.
func (r *Registrar) ClampExpires(requested int) int {
	if requested <= 0 {
		requested = r.cfg.DefaultExpiresSeconds
	}
	if requested < 60 {
		return 60
	}
	if requested > r.cfg.MaxExpiresSeconds {
		return r.cfg.MaxExpiresSeconds
	}
	return requested
}

// Bind inserts a new binding or refreshes an existing one (same AOR +
// Contact identity). When AllowMultipleContacts is false, any pre-existing
// bindings for the AOR are removed first (one event each, reason "replaced").
// Emits sip.registration_active.
func (r *Registrar) Bind(b Binding) {
	if b.AOR == "" || b.Contact == "" {
		return
	}
	now := time.Now()
	if b.CreatedAt.IsZero() {
		b.CreatedAt = now
	}
	b.LastRefresh = now

	var replaced []*Binding

	r.mu.Lock()
	list := r.aors[b.AOR]
	contactID := canonicalContactID(b.Contact)

	if !r.cfg.AllowMultipleContacts {
		for _, existing := range list {
			if canonicalContactID(existing.Contact) != contactID {
				replaced = append(replaced, existing)
			}
		}
		if len(replaced) > 0 {
			list = list[:0]
		}
	}

	var found bool
	for _, existing := range list {
		if canonicalContactID(existing.Contact) == contactID {
			existing.Contact = b.Contact
			existing.Socket = b.Socket
			existing.Transport = b.Transport
			existing.UserAgent = b.UserAgent
			existing.CallID = b.CallID
			if b.AppID != "" {
				existing.AppID = b.AppID
			}
			existing.LastRefresh = b.LastRefresh
			existing.ExpiresAt = b.ExpiresAt
			found = true
			b = *existing
			break
		}
	}
	if !found {
		nb := b
		list = append(list, &nb)
	}
	sort.SliceStable(list, func(i, j int) bool {
		return list[i].LastRefresh.After(list[j].LastRefresh)
	})
	r.aors[b.AOR] = list
	r.mu.Unlock()

	for _, p := range replaced {
		r.publishExpired(*p, "replaced")
	}
	r.publishActive(b)
}

// UnbindAll removes every binding under the AOR. One event per binding.
// Returns the number of removed bindings.
func (r *Registrar) UnbindAll(aor, reason string) int {
	r.mu.Lock()
	list := r.aors[aor]
	delete(r.aors, aor)
	r.mu.Unlock()
	for _, b := range list {
		r.publishExpired(*b, reason)
	}
	return len(list)
}

// UnbindContact removes a single contact under an AOR. Returns true if a
// binding was removed.
func (r *Registrar) UnbindContact(aor, contactURI, reason string) bool {
	id := canonicalContactID(contactURI)

	r.mu.Lock()
	list := r.aors[aor]
	var removed *Binding
	out := list[:0]
	for _, b := range list {
		if canonicalContactID(b.Contact) == id {
			removed = b
			continue
		}
		out = append(out, b)
	}
	if len(out) == 0 {
		delete(r.aors, aor)
	} else {
		r.aors[aor] = out
	}
	r.mu.Unlock()

	if removed != nil {
		r.publishExpired(*removed, reason)
		return true
	}
	return false
}

// Lookup returns the most-recently-refreshed binding for an AOR. The second
// return value is false when no binding exists.
func (r *Registrar) Lookup(aor string) (Binding, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	list := r.aors[aor]
	if len(list) == 0 {
		return Binding{}, false
	}
	return *list[0], true
}

// LookupAll returns every binding for an AOR, sorted most-recently-refreshed
// first.
func (r *Registrar) LookupAll(aor string) []Binding {
	r.mu.RLock()
	defer r.mu.RUnlock()
	list := r.aors[aor]
	out := make([]Binding, len(list))
	for i, b := range list {
		out[i] = *b
	}
	return out
}

// List returns every binding across all AORs. Order is not specified.
func (r *Registrar) List() []Binding {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Binding, 0)
	for _, list := range r.aors {
		for _, b := range list {
			out = append(out, *b)
		}
	}
	return out
}

func (r *Registrar) sweep(now time.Time) {
	var expired []Binding
	r.mu.Lock()
	for aor, list := range r.aors {
		kept := list[:0]
		for _, b := range list {
			if !b.ExpiresAt.IsZero() && b.ExpiresAt.Before(now) {
				expired = append(expired, *b)
				continue
			}
			kept = append(kept, b)
		}
		if len(kept) == 0 {
			delete(r.aors, aor)
		} else {
			r.aors[aor] = kept
		}
	}
	r.mu.Unlock()
	for _, b := range expired {
		r.publishExpired(b, "ttl")
	}
}

func (r *Registrar) publishActive(b Binding) {
	if r.bus == nil {
		return
	}
	r.bus.Publish(events.SIPRegistrationActive, &events.SIPRegistrationActiveData{
		SIPRegistrationScope:  events.SIPRegistrationScope{AppID: b.AppID},
		AOR:                   b.AOR,
		Contact:               b.Contact,
		Socket:                b.Socket,
		Transport:             b.Transport,
		UserAgent:             b.UserAgent,
		CallID:                b.CallID,
		GrantedExpiresSeconds: b.GrantedExpires,
		ExpiresAt:             b.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

func (r *Registrar) publishExpired(b Binding, reason string) {
	if r.bus == nil {
		return
	}
	r.bus.Publish(events.SIPRegistrationExpired, &events.SIPRegistrationExpiredData{
		SIPRegistrationScope: events.SIPRegistrationScope{AppID: b.AppID},
		AOR:                  b.AOR,
		Contact:              b.Contact,
		Socket:               b.Socket,
		Reason:               reason,
	})
}

// CanonicalizeAOR returns the registry key for an AOR URI. The transformation
// is: lower-case scheme, strip user-info parameters, lower-case host, keep
// port only when explicit, drop URI params. Returns "" for malformed input.
func CanonicalizeAOR(u sip.Uri) string {
	if u.Host == "" {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "" {
		scheme = "sip"
	}
	host := strings.ToLower(u.Host)
	out := scheme + ":"
	if u.User != "" {
		out += u.User + "@"
	}
	out += host
	if u.Port > 0 {
		out += ":" + strconv.Itoa(u.Port)
	}
	return out
}

// canonicalContactID returns a comparable identity for a Contact URI within
// an AOR. URI params (e.g. transport=tcp, gr=) are ignored for identity but
// the original Contact string is preserved on the binding.
func canonicalContactID(contact string) string {
	var u sip.Uri
	if err := sip.ParseUri(contact, &u); err != nil {
		return strings.ToLower(strings.TrimSpace(contact))
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "" {
		scheme = "sip"
	}
	host := strings.ToLower(u.Host)
	out := scheme + ":"
	if u.User != "" {
		out += u.User + "@"
	}
	out += host
	if u.Port > 0 {
		out += ":" + strconv.Itoa(u.Port)
	}
	return out
}
