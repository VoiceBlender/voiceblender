package peers

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
)

// Factory starts an endpoint. It is called at most once per process.
type Factory func() (Peer, error)

// EnvPeers selects the endpoints interop tests run against, comma separated.
const EnvPeers = "VB_INTEROP_PEERS"

// DefaultPeers is used when EnvPeers is unset.
const DefaultPeers = "freeswitch"

type entry struct {
	factory Factory
	once    sync.Once
	peer    Peer
	err     error
}

var registry struct {
	mu      sync.Mutex
	entries map[string]*entry
}

// Register makes an endpoint available under name.
func Register(name string, f Factory) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.entries == nil {
		registry.entries = map[string]*entry{}
	}
	if _, dup := registry.entries[name]; dup {
		panic("peers: duplicate registration of " + name)
	}
	registry.entries[name] = &entry{factory: f}
}

// Names lists every registered endpoint.
func Names() []string {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	names := make([]string, 0, len(registry.entries))
	for n := range registry.entries {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Selected lists the endpoints named by EnvPeers.
func Selected() []string {
	v := os.Getenv(EnvPeers)
	if strings.TrimSpace(v) == "" {
		v = DefaultPeers
	}
	var out []string
	for _, n := range strings.Split(v, ",") {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// Get returns the running endpoint called name, starting it on first use. A
// failed start is remembered, so later callers fail fast with the same error.
func Get(name string) (Peer, error) {
	registry.mu.Lock()
	e, ok := registry.entries[name]
	registry.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("peers: no endpoint %q registered (have %v)", name, Names())
	}
	e.once.Do(func() { e.peer, e.err = e.factory() })
	return e.peer, e.err
}

// ShutdownAll closes every endpoint that was started.
func ShutdownAll() {
	registry.mu.Lock()
	entries := make([]*entry, 0, len(registry.entries))
	for _, e := range registry.entries {
		entries = append(entries, e)
	}
	registry.mu.Unlock()
	for _, e := range entries {
		if e.err == nil && e.peer != nil {
			e.peer.Close()
		}
	}
}
