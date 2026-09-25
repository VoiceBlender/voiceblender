package sip

import (
	"context"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/emiago/sipgo/sip"
)

// TrunkManager is the concurrent registry of all SIP trunks (sip_register
// today; ip_ip etc. in the future). Lookups are type-agnostic and indexed by
// id, canonical AOR, and upstream peer socket.
type TrunkManager struct {
	mu       sync.RWMutex
	byID     map[string]Trunk
	byAOR    map[string]Trunk
	bySocket map[string]Trunk
}

func NewTrunkManager() *TrunkManager {
	return &TrunkManager{
		byID:     map[string]Trunk{},
		byAOR:    map[string]Trunk{},
		bySocket: map[string]Trunk{},
	}
}

// Add inserts a trunk, indexing it by id, AOR (when non-empty), and peer
// socket (when known).
func (m *TrunkManager) Add(t Trunk) {
	if t == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byID[t.ID()] = t
	if aor := t.AOR(); aor != "" {
		m.byAOR[aor] = t
	}
	host, port, _ := t.PeerSocket()
	if k := socketKey(host, port); k != "" {
		m.bySocket[k] = t
	}
}

// Get returns the trunk with the given id, or nil.
func (m *TrunkManager) Get(id string) Trunk {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.byID[id]
}

// Remove removes a trunk by id (does not Stop it). Returns the removed
// trunk, or nil if absent.
func (m *TrunkManager) Remove(id string) Trunk {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.byID[id]
	if !ok {
		return nil
	}
	delete(m.byID, id)
	if aor := t.AOR(); aor != "" {
		if cur, ok := m.byAOR[aor]; ok && cur.ID() == t.ID() {
			delete(m.byAOR, aor)
		}
	}
	host, port, _ := t.PeerSocket()
	if k := socketKey(host, port); k != "" {
		if cur, ok := m.bySocket[k]; ok && cur.ID() == t.ID() {
			delete(m.bySocket, k)
		}
	}
	return t
}

// List returns every registered trunk in indeterminate order.
func (m *TrunkManager) List() []Trunk {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Trunk, 0, len(m.byID))
	for _, t := range m.byID {
		out = append(out, t)
	}
	return out
}

// LookupByFromAOR returns the trunk whose canonical AOR matches the given
// URI (after canonicalisation), or nil. The input may be a raw SIP URI
// string ("sip:alice@host") or already-canonical form.
func (m *TrunkManager) LookupByFromAOR(aor string) Trunk {
	if aor == "" {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.byAOR[aor]
}

// LookupByAORUser returns a trunk whose AOR user-part matches the given
// user string. Falls back when the caller only supplied a bare user on
// `from` (e.g. POST /v1/legs with `from: "alice"`) — the engine's
// publicHost won't match the trunk's upstream domain. Returns nil if no
// match; if multiple trunks have the same user, returns one arbitrarily.
func (m *TrunkManager) LookupByAORUser(user string) Trunk {
	if user == "" {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for aor, t := range m.byAOR {
		if aorUser(aor) == user {
			return t
		}
	}
	return nil
}

// aorUser extracts the user-part of a canonical "sip:user@host" string.
func aorUser(aor string) string {
	if i := strings.Index(aor, ":"); i >= 0 {
		rest := aor[i+1:]
		if j := strings.Index(rest, "@"); j >= 0 {
			return rest[:j]
		}
	}
	return ""
}

// LookupByPeerSocket returns the trunk whose upstream peer matches the given
// transport address, or nil. Matches first on full host:port, then on host
// alone (for cases where the peer's ephemeral source port differs from the
// configured registrar port).
func (m *TrunkManager) LookupByPeerSocket(host string, port int) Trunk {
	if host == "" {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if t, ok := m.bySocket[host+":"+strconv.Itoa(port)]; ok {
		return t
	}
	// Host-only fallback: source port may have been ephemeral.
	for k, t := range m.bySocket {
		h, _, err := net.SplitHostPort(k)
		if err == nil && strings.EqualFold(h, host) {
			return t
		}
	}
	return nil
}

// LookupInbound resolves the trunk that delivered an inbound INVITE. Trunks
// whose peer socket matches host:port (or host alone, for ephemeral source
// ports) are narrowed by the Request-URI user against each trunk's Contact
// user, then by the To URI against each trunk's AOR. unique is false when
// more than one trunk survives; the returned trunk is then arbitrary.
func (m *TrunkManager) LookupInbound(host string, port int, requestUser string, to sip.Uri) (t Trunk, unique bool) {
	if host == "" {
		return nil, false
	}
	m.mu.RLock()
	var exact, hostOnly []Trunk
	for _, cand := range m.byID {
		h, p, _ := cand.PeerSocket()
		if h == "" || !strings.EqualFold(h, host) {
			continue
		}
		if p == port {
			exact = append(exact, cand)
		} else {
			hostOnly = append(hostOnly, cand)
		}
	}
	m.mu.RUnlock()

	cands := exact
	if len(cands) == 0 {
		cands = hostOnly
	}
	if len(cands) == 0 {
		return nil, false
	}
	if requestUser != "" {
		cands = narrowTrunks(cands, func(c Trunk) bool { return c.ContactUser() == requestUser })
	}
	if aor := CanonicalizeAOR(to); aor != "" {
		cands = narrowTrunks(cands, func(c Trunk) bool { return c.AOR() == aor })
	}
	if to.User != "" {
		cands = narrowTrunks(cands, func(c Trunk) bool { return aorUser(c.AOR()) == to.User })
	}
	return cands[0], len(cands) == 1
}

// narrowTrunks keeps the trunks matching keep, or returns cands unchanged when
// none match so a weaker signal never discards every candidate.
func narrowTrunks(cands []Trunk, keep func(Trunk) bool) []Trunk {
	if len(cands) < 2 {
		return cands
	}
	var out []Trunk
	for _, c := range cands {
		if keep(c) {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return cands
	}
	return out
}

// RefreshIndex re-indexes the trunk under its current AOR and peer socket.
// Call this after a trunk's PeerSocket() value becomes known (e.g. after the
// first successful REGISTER reveals the registrar's actual transport addr).
func (m *TrunkManager) RefreshIndex(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.byID[id]
	if !ok {
		return
	}
	// Strip any stale entries pointing at this id, then re-add fresh.
	for k, cur := range m.byAOR {
		if cur.ID() == id {
			delete(m.byAOR, k)
		}
	}
	for k, cur := range m.bySocket {
		if cur.ID() == id {
			delete(m.bySocket, k)
		}
	}
	if aor := t.AOR(); aor != "" {
		m.byAOR[aor] = t
	}
	host, port, _ := t.PeerSocket()
	if k := socketKey(host, port); k != "" {
		m.bySocket[k] = t
	}
}

// Shutdown stops every trunk in parallel, honouring ctx. After return the
// manager is empty.
func (m *TrunkManager) Shutdown(ctx context.Context) {
	trunks := m.List()
	var wg sync.WaitGroup
	for _, t := range trunks {
		wg.Add(1)
		go func(t Trunk) {
			defer wg.Done()
			_ = t.Stop(ctx)
		}(t)
	}
	wg.Wait()
	m.mu.Lock()
	m.byID = map[string]Trunk{}
	m.byAOR = map[string]Trunk{}
	m.bySocket = map[string]Trunk{}
	m.mu.Unlock()
}

// socketKey returns "host:port" for indexing, or "" when host/port is unset.
func socketKey(host string, port int) string {
	if host == "" || port == 0 {
		return ""
	}
	return host + ":" + strconv.Itoa(port)
}
