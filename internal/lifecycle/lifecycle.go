// Package lifecycle tracks the instance status (starting, ready, draining,
// drained, stopping) that drives SIP admission, the health endpoints, the
// instance.status_changed event and the drain deadline.
package lifecycle

import (
	"errors"
	"sync"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
)

type State string

const (
	Starting State = "starting"
	Ready    State = "ready"
	Draining State = "draining"
	Drained  State = "drained"
	Stopping State = "stopping"
)

// States lists every state in lifecycle order.
var States = []State{Starting, Ready, Draining, Drained, Stopping}

// Drain sources reported in Status.DrainSource.
const (
	SourceAPI    = "api"
	SourceSignal = "signal"
)

var (
	ErrStopping    = errors.New("instance is shutting down")
	ErrNotDraining = errors.New("instance is not draining")
)

type Config struct {
	// EmptyGrace is how long the leg count must stay at zero before a drain
	// counts as drained.
	EmptyGrace time.Duration
	// DefaultDrainTimeout bounds a drain that does not set its own timeout;
	// zero means no limit.
	DefaultDrainTimeout time.Duration
	// RetryAfter is the Retry-After value, in seconds, sent with 503s.
	RetryAfter int
	// RoomCount reports the current number of rooms; nil reports zero.
	RoomCount func() int
}

// DrainOptions configures StartDrain.
type DrainOptions struct {
	Source          string
	ExitWhenDrained bool
	// Timeout overrides the drain deadline; zero means no limit. Nil keeps
	// the deadline of a drain already running, or applies the default to a
	// new one.
	Timeout *time.Duration
}

// Status is a point-in-time view of the controller.
type Status struct {
	State           State
	Since           time.Time
	DrainingSince   time.Time
	DrainSource     string
	ExitWhenDrained bool
	DrainDeadline   time.Time
	Legs            int
	Rooms           int
}

type Controller struct {
	cfg Config
	bus *events.Bus

	mu              sync.Mutex
	state           State
	since           time.Time
	drainingSince   time.Time
	drainSource     string
	exitWhenDrained bool
	deadline        time.Time
	legs            int
	graceTimer      *time.Timer
	deadlineTimer   *time.Timer
	onDrainStart    func()
	onDrainCancel   func()

	shutdownOnce sync.Once
	shutdownCh   chan struct{}

	hookMu      sync.Mutex
	hooks       []func()
	hookRunning bool
}

func New(bus *events.Bus, cfg Config) *Controller {
	return &Controller{
		cfg:        cfg,
		bus:        bus,
		state:      Starting,
		since:      time.Now(),
		shutdownCh: make(chan struct{}),
	}
}

// SetDrainHooks registers functions run when a drain starts and when it is
// cancelled. They run off the caller's goroutine, one at a time, in order.
func (c *Controller) SetDrainHooks(onStart, onCancel func()) {
	c.mu.Lock()
	c.onDrainStart, c.onDrainCancel = onStart, onCancel
	c.mu.Unlock()
}

// ShutdownRequested is closed when the instance should run its ordered
// shutdown: an exit drain reached drained, or a drain deadline passed.
func (c *Controller) ShutdownRequested() <-chan struct{} { return c.shutdownCh }

func (c *Controller) RetryAfter() int { return c.cfg.RetryAfter }

func (c *Controller) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

// AcceptingSIP reports whether new SIP calls and out-of-dialog OPTIONS are
// accepted.
func (c *Controller) AcceptingSIP() bool { return c.State() == Ready }

// AcceptingNew reports whether the API may create new legs and rooms. They
// stay allowed while draining because the application may still need them
// for calls in progress.
func (c *Controller) AcceptingNew() bool {
	switch c.State() {
	case Starting, Ready, Draining:
		return true
	}
	return false
}

func (c *Controller) Snapshot() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snapshotLocked()
}

func (c *Controller) MarkReady() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == Starting {
		c.setLocked(Ready)
	}
}

func (c *Controller) StartDrain(opts DrainOptions) (Status, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch c.state {
	case Stopping:
		return c.snapshotLocked(), ErrStopping
	case Draining, Drained:
		if opts.ExitWhenDrained {
			c.exitWhenDrained = true
		}
		if opts.Timeout != nil {
			c.setDeadlineLocked(*opts.Timeout)
		}
		if c.state == Drained && c.exitWhenDrained {
			c.requestShutdown()
		}
		return c.snapshotLocked(), nil
	}

	c.drainingSince = time.Now()
	c.drainSource = opts.Source
	c.exitWhenDrained = opts.ExitWhenDrained
	timeout := c.cfg.DefaultDrainTimeout
	if opts.Timeout != nil {
		timeout = *opts.Timeout
	}
	c.setDeadlineLocked(timeout)
	c.setLocked(Draining)
	if c.legs == 0 {
		c.startGraceLocked()
	}
	c.runHook(c.onDrainStart)
	return c.snapshotLocked(), nil
}

func (c *Controller) CancelDrain() (Status, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch c.state {
	case Stopping:
		return c.snapshotLocked(), ErrStopping
	case Draining, Drained:
	default:
		return c.snapshotLocked(), ErrNotDraining
	}
	c.stopTimersLocked()
	c.drainingSince = time.Time{}
	c.drainSource = ""
	c.exitWhenDrained = false
	c.deadline = time.Time{}
	c.setLocked(Ready)
	c.runHook(c.onDrainCancel)
	return c.snapshotLocked(), nil
}

// BeginStop moves to stopping. It reports false if the instance was already
// stopping.
func (c *Controller) BeginStop() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == Stopping {
		return false
	}
	c.stopTimersLocked()
	c.setLocked(Stopping)
	return true
}

// LegCountChanged feeds the current leg count; it drives the empty grace
// timer.
func (c *Controller) LegCountChanged(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.legs = n
	switch c.state {
	case Draining:
		if n == 0 {
			c.startGraceLocked()
		} else if c.graceTimer != nil {
			c.graceTimer.Stop()
			c.graceTimer = nil
		}
	case Drained:
		// A create admitted just before drained can still land a leg.
		if n > 0 {
			c.setLocked(Draining)
		}
	}
}

func (c *Controller) startGraceLocked() {
	if c.graceTimer != nil {
		return
	}
	if c.cfg.EmptyGrace <= 0 {
		c.markDrainedLocked()
		return
	}
	var t *time.Timer
	t = time.AfterFunc(c.cfg.EmptyGrace, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.graceTimer != t {
			return
		}
		c.graceTimer = nil
		if c.state == Draining && c.legs == 0 {
			c.markDrainedLocked()
		}
	})
	c.graceTimer = t
}

func (c *Controller) markDrainedLocked() {
	c.setLocked(Drained)
	if c.exitWhenDrained {
		c.requestShutdown()
	}
}

func (c *Controller) setDeadlineLocked(timeout time.Duration) {
	if c.deadlineTimer != nil {
		c.deadlineTimer.Stop()
		c.deadlineTimer = nil
	}
	if timeout <= 0 {
		c.deadline = time.Time{}
		return
	}
	c.deadline = time.Now().Add(timeout)
	var t *time.Timer
	t = time.AfterFunc(timeout, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.deadlineTimer != t {
			return
		}
		c.deadlineTimer = nil
		if c.state == Draining || c.state == Drained {
			c.requestShutdown()
		}
	})
	c.deadlineTimer = t
}

func (c *Controller) stopTimersLocked() {
	if c.graceTimer != nil {
		c.graceTimer.Stop()
		c.graceTimer = nil
	}
	if c.deadlineTimer != nil {
		c.deadlineTimer.Stop()
		c.deadlineTimer = nil
	}
}

func (c *Controller) requestShutdown() {
	c.shutdownOnce.Do(func() { close(c.shutdownCh) })
}

func (c *Controller) setLocked(to State) {
	from := c.state
	if from == to {
		return
	}
	c.state = to
	c.since = time.Now()
	c.publishFromLocked(from, to)
}

// publishFromLocked publishes under c.mu so events leave in transition order;
// bus subscribers must not call back into the controller.
func (c *Controller) publishFromLocked(from, to State) {
	if c.bus == nil {
		return
	}
	s := c.snapshotLocked()
	d := &events.InstanceStatusChangedData{
		From:            string(from),
		To:              string(to),
		Legs:            s.Legs,
		Rooms:           s.Rooms,
		DrainSource:     s.DrainSource,
		ExitWhenDrained: s.ExitWhenDrained,
	}
	if !s.DrainDeadline.IsZero() {
		d.DrainDeadline = s.DrainDeadline.UTC().Format(time.RFC3339)
	}
	c.bus.Publish(events.InstanceStatusChanged, d)
}

func (c *Controller) snapshotLocked() Status {
	rooms := 0
	if c.cfg.RoomCount != nil {
		rooms = c.cfg.RoomCount()
	}
	return Status{
		State:           c.state,
		Since:           c.since,
		DrainingSince:   c.drainingSince,
		DrainSource:     c.drainSource,
		ExitWhenDrained: c.exitWhenDrained,
		DrainDeadline:   c.deadline,
		Legs:            c.legs,
		Rooms:           rooms,
	}
}

func (c *Controller) runHook(fn func()) {
	if fn == nil {
		return
	}
	c.hookMu.Lock()
	c.hooks = append(c.hooks, fn)
	if c.hookRunning {
		c.hookMu.Unlock()
		return
	}
	c.hookRunning = true
	c.hookMu.Unlock()
	go func() {
		for {
			c.hookMu.Lock()
			if len(c.hooks) == 0 {
				c.hookRunning = false
				c.hookMu.Unlock()
				return
			}
			next := c.hooks[0]
			c.hooks = c.hooks[1:]
			c.hookMu.Unlock()
			next()
		}
	}()
}
