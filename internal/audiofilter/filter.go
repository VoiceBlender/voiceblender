// Package audiofilter provides an ordered chain of per-leg audio processing
// stages. The chain owns sample-rate conversion and block framing so that
// stages see only whole frames at one rate, and so audio is resampled once
// regardless of how many stages run.
package audiofilter

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// MaxChainLength bounds a configured chain. Filters run inside a leg's read
// goroutine against a real-time budget, so an unbounded chain is a denial of
// service on that leg's audio.
const MaxChainLength = 4

// Stage transforms one frame of mono s16 PCM in place, at the chain's working
// rate. A Stage belongs to exactly one chain (one leg) and is driven by that
// leg's read goroutine only, so it needs no internal locking.
type Stage interface {
	Process(frame []int16)
	Close() error
}

// Params are a filter's optional tuning values, keyed by name.
type Params map[string]float64

// Get returns the named parameter, or def when it is absent.
func (p Params) Get(name string, def float64) float64 {
	if v, ok := p[name]; ok {
		return v
	}
	return def
}

// Spec is one configured filter: a registered name plus its parameters. Specs
// come from a request body or an env var, never from user-supplied code.
type Spec struct {
	Type   string `json:"type"`
	Params Params `json:"params,omitempty"`
}

// Descriptor is a filter's registration. The rate and frame requirements must
// be known before any Stage exists, because the chain resolves its working
// rate from them and only then constructs stages against that rate.
type Descriptor struct {
	// RequiredRate is the sample rate the filter must run at, or 0 for any.
	RequiredRate int
	// FrameSamples is the exact frame length the filter needs, or 0 for any.
	// A filter whose frame length follows the rate leaves this zero and has its
	// stage implement FrameSizer instead.
	FrameSamples int
	// Corrective marks a filter that cleans audio rather than changing its
	// character, so detection paths can take it and leave the effects.
	Corrective bool
	// Unique rejects a chain that names this filter more than once. Set it for
	// filters holding a costly per-stream resource, where a duplicate silently
	// doubles the cost.
	Unique bool
	// ValidateParams rejects bad parameters at the API boundary. Without it a
	// bad value is only caught when the chain is built, which is after the
	// request has been accepted. nil means the filter takes no parameters.
	ValidateParams func(p Params) error
	// Available reports whether the filter can currently be built. A filter
	// backed by a resource that may fail to initialise stays registered and
	// reports false, so a chain naming it is dropped to passthrough rather than
	// failing call setup. nil means always available.
	Available func() bool
	// New builds a stage at the chain's resolved working rate. Any per-stream
	// resource is acquired here and released by the stage's Close.
	New func(rate int, p Params) (Stage, error)
}

// FrameSizer is implemented by a stage whose frame length is only known once
// it has been built at the chain's working rate. The chain asks the stage
// rather than the descriptor, so there is no second copy of the rule.
type FrameSizer interface {
	FrameSamples() int
}

var registry = map[string]Descriptor{}

// Register adds a filter under name. It panics on a duplicate or an incomplete
// descriptor: both are programmer errors discoverable at startup.
func Register(name string, d Descriptor) {
	name = strings.ToLower(name)
	if name == "" || d.New == nil {
		panic("audiofilter: Register requires a name and a New func")
	}
	if _, dup := registry[name]; dup {
		panic("audiofilter: duplicate filter " + name)
	}
	registry[name] = d
}

// Unregister removes a filter. It exists for tests; production code registers
// once at startup and never removes.
func Unregister(name string) { delete(registry, strings.ToLower(name)) }

// Names lists the registered filters in sorted order, for validation messages
// and generated documentation.
func Names() []string {
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func lookup(name string) (Descriptor, bool) {
	d, ok := registry[strings.ToLower(name)]
	return d, ok
}

// Validate checks a chain without building it, so a bad request fails at the
// API boundary rather than when the leg's first audio frame arrives.
func Validate(specs []Spec) error {
	if len(specs) > MaxChainLength {
		return fmt.Errorf("filter chain has %d entries, maximum is %d", len(specs), MaxChainLength)
	}
	seen := map[string]bool{}
	for _, s := range specs {
		name := strings.ToLower(s.Type)
		d, ok := lookup(name)
		if !ok {
			return fmt.Errorf("unknown filter %q (known: %s)", s.Type, strings.Join(Names(), ", "))
		}
		if d.Unique && seen[name] {
			return fmt.Errorf("filter %q may appear only once in a chain", name)
		}
		if d.ValidateParams != nil {
			if err := d.ValidateParams(s.Params); err != nil {
				return fmt.Errorf("filter %q: %w", name, err)
			}
		}
		seen[name] = true
	}
	return nil
}

// Corrective returns only the filters that clean audio, dropping effects.
// Detection paths want this: answering-machine detection and voice activity
// detection benefit from denoise, but running them through `robotic` or
// `pitch` would wreck the very signal they are scoring.
func Corrective(specs []Spec) []Spec {
	var out []Spec
	for _, s := range specs {
		if d, ok := lookup(s.Type); ok && d.Corrective {
			out = append(out, s)
		}
	}
	return out
}

// Resolve drops filters that are registered but currently unavailable,
// returning the chain that will actually run. Callers report the result rather
// than what was requested, so an operator sees the real processing instead of
// silent degradation. Unknown filters are left in place for Validate to reject.
func Resolve(specs []Spec) []Spec {
	var out []Spec
	for _, s := range specs {
		d, ok := lookup(s.Type)
		if ok && d.Available != nil && !d.Available() {
			continue
		}
		out = append(out, s)
	}
	return out
}

// Parse reads an ordered filter list of the form
// "bandpass:low_hz=300:high_hz=3400,denoise". Used for the server-default env
// var; the API takes the structured form instead.
func Parse(s string) ([]Spec, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var out []Spec
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		parts := strings.Split(item, ":")
		spec := Spec{Type: strings.ToLower(strings.TrimSpace(parts[0]))}
		for _, kv := range parts[1:] {
			k, v, found := strings.Cut(kv, "=")
			if !found {
				return nil, fmt.Errorf("filter %q: parameter %q is not key=value", spec.Type, kv)
			}
			f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err != nil {
				return nil, fmt.Errorf("filter %q: parameter %q: %v", spec.Type, k, err)
			}
			if spec.Params == nil {
				spec.Params = Params{}
			}
			spec.Params[strings.TrimSpace(k)] = f
		}
		out = append(out, spec)
	}
	if err := Validate(out); err != nil {
		return nil, err
	}
	return out, nil
}

// Format renders a chain in the same syntax Parse accepts, for logs and for
// echoing a resolved chain back to an operator.
func Format(specs []Spec) string {
	var b strings.Builder
	for i, s := range specs {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strings.ToLower(s.Type))
		keys := make([]string, 0, len(s.Params))
		for k := range s.Params {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteByte(':')
			b.WriteString(k)
			b.WriteByte('=')
			b.WriteString(strconv.FormatFloat(s.Params[k], 'g', -1, 64))
		}
	}
	return b.String()
}
