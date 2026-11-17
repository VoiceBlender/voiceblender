package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
	"github.com/VoiceBlender/voiceblender/internal/lifecycle"
	"github.com/VoiceBlender/voiceblender/internal/wsutilx"
	"github.com/gobwas/ws"
)

// reasonServerShutdown is the disconnect reason for legs ended by the
// ordered shutdown.
const reasonServerShutdown = "server_shutdown"

// inflight counts teardowns in progress. Unlike sync.WaitGroup it allows new
// work to start while a waiter is blocked.
type inflight struct {
	mu   sync.Mutex
	n    int
	idle chan struct{}
}

func newInflight() *inflight {
	f := &inflight{idle: make(chan struct{})}
	close(f.idle)
	return f
}

func (f *inflight) add() {
	f.mu.Lock()
	if f.n == 0 {
		f.idle = make(chan struct{})
	}
	f.n++
	f.mu.Unlock()
}

func (f *inflight) done() {
	f.mu.Lock()
	f.n--
	if f.n == 0 {
		close(f.idle)
	}
	f.mu.Unlock()
}

func (f *inflight) wait(ctx context.Context) error {
	for {
		f.mu.Lock()
		if f.n == 0 {
			f.mu.Unlock()
			return nil
		}
		idle := f.idle
		f.mu.Unlock()
		select {
		case <-idle:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// teardownLeg runs the full leg teardown and publishes leg.disconnected,
// tracked so the ordered shutdown can wait for it.
func (s *Server) teardownLeg(l leg.Leg, reason string) {
	s.teardowns.add()
	defer s.teardowns.done()
	s.cleanupLeg(l)
	s.publishDisconnect(l, reason)
}

// endLeg ends a leg through the normal teardown path and returns once it is
// done. It does nothing for a leg another path is already ending.
func (s *Server) endLeg(l leg.Leg, reason string) {
	if _, ok := s.LegMgr.Remove(l.ID()); !ok {
		return
	}
	s.teardowns.add()
	defer s.teardowns.done()
	if sl, ok := l.(*leg.SIPLeg); ok && sl.Type() == leg.TypeSIPInbound {
		if st := sl.State(); st == leg.StateRinging || st == leg.StateEarlyMedia {
			sl.SetDisconnectReason(reason)
			if err := sl.Reject(context.Background(), http.StatusServiceUnavailable, "Service Unavailable"); err != nil {
				s.Log.Debug("shutdown reject", "leg_id", l.ID(), "error", err)
			}
			s.cleanupLeg(l)
			s.publishDisconnect(l, reason)
			return
		}
	}
	if err := l.Hangup(context.Background()); err != nil {
		s.Log.Debug("shutdown hangup", "leg_id", l.ID(), "error", err)
	}
	s.cleanupLeg(l)
	s.publishDisconnect(l, reason)
}

// admitNew refuses new legs and rooms once the instance is drained or
// stopping.
func (s *Server) admitNew() error {
	if s.Lifecycle == nil || s.Lifecycle.AcceptingNew() {
		return nil
	}
	return &apiError{
		Code:       http.StatusServiceUnavailable,
		Message:    fmt.Sprintf("instance is %s and not accepting new legs or rooms", s.Lifecycle.State()),
		RetryAfter: s.Lifecycle.RetryAfter(),
	}
}

// Shutdown runs the ordered shutdown: every leg ends through the normal
// teardown, room recordings are finalised, BYEs and trunk unregisters go out
// while the SIP sockets are still open, and queued webhooks and VSI events are
// flushed. It returns when done or when ctx ends; the caller then closes the
// listeners.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.Lifecycle != nil {
		s.Lifecycle.BeginStop()
	}
	var errs []error
	step := func(name string, err error) {
		if err != nil {
			s.Log.Warn("shutdown step did not finish", "step", name, "error", err)
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}

	legs := s.LegMgr.List()
	var sipLegs []*leg.SIPLeg
	var wg sync.WaitGroup
	for _, l := range legs {
		if sl, ok := l.(*leg.SIPLeg); ok {
			sipLegs = append(sipLegs, sl)
		}
		wg.Add(1)
		go func(l leg.Leg) {
			defer wg.Done()
			s.endLeg(l, reasonServerShutdown)
		}(l)
	}
	step("end legs", waitGroup(ctx, &wg))
	step("leg teardowns", s.teardowns.wait(ctx))

	for _, rm := range s.RoomMgr.List() {
		s.cleanupRoomAgent(rm.ID)
		s.finalizeRoomRecording(rm.ID, rm.AppID, "shutdown")
	}

	for _, sl := range sipLegs {
		select {
		case <-sl.ByeDone():
		case <-ctx.Done():
		}
	}
	step("BYEs", ctx.Err())

	if s.SIPEngine != nil {
		s.SIPEngine.Trunks().Shutdown(ctx)
	}
	if s.Webhooks != nil {
		step("webhooks", s.Webhooks.Flush(ctx))
	}
	step("VSI", s.closeVSIConns(ctx))
	return errors.Join(errs...)
}

func waitGroup(ctx context.Context, wg *sync.WaitGroup) error {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// vsiConn is one live VSI connection, tracked so shutdown can flush it.
type vsiConn struct {
	conn net.Conn
	lw   *wsutilx.LockedWriter
	ch   chan events.Event
}

func (c *vsiConn) pending() int { return len(c.ch) }

func (s *Server) addVSIConn(c *vsiConn) {
	s.vsiMu.Lock()
	s.vsiConns[c] = struct{}{}
	s.vsiMu.Unlock()
}

func (s *Server) removeVSIConn(c *vsiConn) {
	s.vsiMu.Lock()
	delete(s.vsiConns, c)
	s.vsiMu.Unlock()
}

// closeVSIConns waits for each VSI client's queued events to be written, then
// closes it with 1001 Going Away.
func (s *Server) closeVSIConns(ctx context.Context) error {
	s.vsiMu.Lock()
	conns := make([]*vsiConn, 0, len(s.vsiConns))
	for c := range s.vsiConns {
		conns = append(conns, c)
	}
	s.vsiMu.Unlock()

	var err error
	for _, c := range conns {
		for c.pending() > 0 && ctx.Err() == nil {
			select {
			case <-ctx.Done():
			case <-time.After(10 * time.Millisecond):
			}
		}
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		_ = c.lw.WriteControl(ws.OpClose, ws.NewCloseFrameBody(ws.StatusGoingAway, "server shutting down"))
		_ = c.conn.Close()
	}
	return err
}

// --- REST: drain and health ---

// StartDrainRequest is the optional body of POST /v1/drain.
type StartDrainRequest struct {
	TimeoutSeconds *int `json:"timeout_seconds,omitempty"`
}

// InstanceStatus is the body of the drain and health endpoints.
type InstanceStatus struct {
	Status          string `json:"status"`
	Version         string `json:"version,omitempty"`
	UptimeSeconds   int64  `json:"uptime_seconds"`
	DrainingSince   string `json:"draining_since,omitempty"`
	DrainSource     string `json:"drain_source,omitempty"`
	ExitWhenDrained bool   `json:"exit_when_drained"`
	DrainDeadline   string `json:"drain_deadline,omitempty"`
	Legs            int    `json:"legs"`
	Rooms           int    `json:"rooms"`
}

func (s *Server) instanceStatus() InstanceStatus {
	st := s.Lifecycle.Snapshot()
	out := InstanceStatus{
		Status:          string(st.State),
		Version:         s.Version,
		UptimeSeconds:   int64(time.Since(s.startedAt).Seconds()),
		DrainSource:     st.DrainSource,
		ExitWhenDrained: st.ExitWhenDrained,
		Legs:            st.Legs,
		Rooms:           st.Rooms,
	}
	if !st.DrainingSince.IsZero() {
		out.DrainingSince = st.DrainingSince.UTC().Format(time.RFC3339)
	}
	if !st.DrainDeadline.IsZero() {
		out.DrainDeadline = st.DrainDeadline.UTC().Format(time.RFC3339)
	}
	return out
}

func (s *Server) doStartDrain(req StartDrainRequest) (InstanceStatus, error) {
	opts := lifecycle.DrainOptions{Source: lifecycle.SourceAPI}
	if req.TimeoutSeconds != nil {
		if *req.TimeoutSeconds < 0 {
			return InstanceStatus{}, newAPIError(http.StatusBadRequest, "timeout_seconds must be 0 or more")
		}
		d := time.Duration(*req.TimeoutSeconds) * time.Second
		opts.Timeout = &d
	}
	if _, err := s.Lifecycle.StartDrain(opts); err != nil {
		return s.instanceStatus(), newAPIError(http.StatusConflict, "%v", err)
	}
	return s.instanceStatus(), nil
}

func (s *Server) doCancelDrain() (InstanceStatus, error) {
	if _, err := s.Lifecycle.CancelDrain(); err != nil {
		return s.instanceStatus(), newAPIError(http.StatusConflict, "%v", err)
	}
	return s.instanceStatus(), nil
}

func (s *Server) startDrain(w http.ResponseWriter, r *http.Request) {
	var req StartDrainRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
			return
		}
	}
	st, err := s.doStartDrain(req)
	if err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, st)
}

func (s *Server) cancelDrain(w http.ResponseWriter, r *http.Request) {
	st, err := s.doCancelDrain()
	if err != nil {
		handleAPIError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) healthLive(w http.ResponseWriter, r *http.Request) {
	st := s.instanceStatus()
	code := http.StatusOK
	if st.Status == string(lifecycle.Stopping) {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, st)
}

func (s *Server) healthReady(w http.ResponseWriter, r *http.Request) {
	st := s.instanceStatus()
	code := http.StatusOK
	if st.Status != string(lifecycle.Ready) {
		code = http.StatusServiceUnavailable
		if ra := s.Lifecycle.RetryAfter(); ra > 0 && st.Status != string(lifecycle.Starting) {
			w.Header().Set("Retry-After", strconv.Itoa(ra))
		}
	}
	writeJSON(w, code, st)
}
