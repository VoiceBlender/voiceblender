package api

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/csiwek/VoiceBlender/internal/agent"
	"github.com/csiwek/VoiceBlender/internal/events"
	"github.com/csiwek/VoiceBlender/internal/leg"
	"github.com/csiwek/VoiceBlender/internal/mixer"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// streamBuffer accepts variable-sized writes and provides paced reads.
// ElevenLabs TTS delivers audio in bursts (faster than real-time), but
// the mixer's readLoop drains its Reader as fast as possible into a tiny
// 3-slot incoming channel, dropping overflow. The pacing here ensures
// the readLoop gets at most one 640-byte frame per 20ms — matching the
// mixer's tick rate — so no frames are dropped.
type streamBuffer struct {
	mu       sync.Mutex
	cond     *sync.Cond
	buf      []byte
	closed   bool
	lastRead time.Time
	pace     time.Duration
}

func newStreamBuffer() *streamBuffer {
	sb := &streamBuffer{pace: time.Duration(mixer.Ptime) * time.Millisecond}
	sb.cond = sync.NewCond(&sb.mu)
	return sb
}

func (sb *streamBuffer) Write(p []byte) (int, error) {
	sb.mu.Lock()
	if sb.closed {
		sb.mu.Unlock()
		return len(p), nil
	}
	sb.buf = append(sb.buf, p...)
	sb.cond.Signal()
	sb.mu.Unlock()
	return len(p), nil
}

func (sb *streamBuffer) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	// Pace: wait at least one frame interval between reads so the mixer's
	// readLoop doesn't flood its tiny incoming channel.
	if !sb.lastRead.IsZero() {
		wait := sb.pace - time.Since(sb.lastRead)
		if wait > 0 {
			time.Sleep(wait)
		}
	}

	sb.mu.Lock()
	for len(sb.buf) < len(p) && !sb.closed {
		sb.cond.Wait()
	}
	if len(sb.buf) == 0 && sb.closed {
		sb.mu.Unlock()
		return 0, io.EOF
	}
	n := copy(p, sb.buf)
	// Compact: shift remaining data to front to avoid unbounded growth.
	remaining := copy(sb.buf, sb.buf[n:])
	sb.buf = sb.buf[:remaining]
	sb.mu.Unlock()

	sb.lastRead = time.Now()
	return n, nil
}

func (sb *streamBuffer) Close() {
	sb.mu.Lock()
	sb.closed = true
	sb.cond.Broadcast()
	sb.mu.Unlock()
}

type agentInfo struct {
	session  *agent.Session
	sourceID string         // mixer playback source / participant ID
	pipes    []*pipeWriter  // pipes to close on cleanup
	speakBuf *streamBuffer  // paced speak buffer (closed before RemoveParticipant)
	roomID   string         // for leg agents: which room (if any)
	cancel   context.CancelFunc // for room agents: dedicated context
}

var (
	legAgents = struct {
		sync.Mutex
		m map[string]*agentInfo
	}{m: make(map[string]*agentInfo)}

	roomAgents = struct {
		sync.Mutex
		m map[string]*agentInfo
	}{m: make(map[string]*agentInfo)}
)

type agentRequest struct {
	AgentID          string            `json:"agent_id"`
	FirstMessage     string            `json:"first_message,omitempty"`
	Language         string            `json:"language,omitempty"`
	DynamicVariables map[string]string `json:"dynamic_variables,omitempty"`
	APIKey           string            `json:"api_key,omitempty"`
}

func (s *Server) agentLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req agentRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.AgentID == "" {
		writeError(w, http.StatusBadRequest, "agent_id is required")
		return
	}

	apiKey := req.APIKey
	if apiKey == "" {
		apiKey = s.Config.ElevenLabsAPIKey
	}
	if apiKey == "" {
		writeError(w, http.StatusServiceUnavailable, "no ElevenLabs API key provided")
		return
	}

	l, ok := s.LegMgr.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "leg not found")
		return
	}
	if l.State() != leg.StateConnected {
		writeError(w, http.StatusConflict, "leg not connected")
		return
	}

	legAgents.Lock()
	if _, exists := legAgents.m[id]; exists {
		legAgents.Unlock()
		writeError(w, http.StatusConflict, "agent already attached to this leg")
		return
	}
	legAgents.Unlock()

	opts := agent.Options{
		AgentID:          req.AgentID,
		Language:         req.Language,
		FirstMessage:     req.FirstMessage,
		DynamicVariables: req.DynamicVariables,
	}

	bus := s.Bus
	session := agent.New(s.Log)
	info := &agentInfo{session: session}

	var audioIn interface{ Read([]byte) (int, error) }
	var audioOut interface{ Write([]byte) (int, error) }

	if roomID := l.RoomID(); roomID != "" {
		// Leg is in a room — use tap + playback source.
		rm, rmOK := s.RoomMgr.Get(roomID)
		if !rmOK {
			writeError(w, http.StatusConflict, "room not found")
			return
		}

		tapPR, tapPW := createPipe()
		rm.Mixer().SetParticipantTap(id, tapPW)

		sourceID := "agent-" + uuid.New().String()[:8]
		sb := newStreamBuffer()
		rm.Mixer().AddPlaybackSource(sourceID, sb)

		audioIn = tapPR
		audioOut = sb
		info.sourceID = sourceID
		info.pipes = []*pipeWriter{tapPW}
		info.speakBuf = sb
		info.roomID = roomID
	} else {
		// Standalone leg — read/write audio directly with resampling.
		ar := l.AudioReader()
		aw := l.AudioWriter()
		if ar == nil || aw == nil {
			writeError(w, http.StatusConflict, "leg has no audio reader/writer")
			return
		}
		audioIn = mixer.NewResampleReader(ar, l.SampleRate(), mixer.SampleRate)

		// ElevenLabs sends TTS audio in bursts (faster than real-time).
		// The sipWriter blocks when outFrames (capacity 5) is full, stalling
		// the agent's recvLoop. Use a streamBuffer to absorb bursts and a
		// drain goroutine to feed fixed-size frames to the sipWriter at pace.
		sb := newStreamBuffer()
		audioOut = mixer.NewResampleWriter(sb, mixer.SampleRate, l.SampleRate())
		info.speakBuf = sb

		frameSize := l.SampleRate() / 50 * 2 // 20ms PCM frame at leg's native rate
		go func() {
			buf := make([]byte, frameSize)
			for {
				n, err := sb.Read(buf)
				if err != nil || n == 0 {
					return
				}
				if _, err := aw.Write(buf[:n]); err != nil {
					return
				}
			}
		}()
	}

	legAgents.Lock()
	legAgents.m[id] = info
	legAgents.Unlock()

	cb := agent.Callbacks{
		OnConnected: func(conversationID string) {
			bus.Publish(events.AgentConnected, map[string]interface{}{
				"leg_id":          id,
				"conversation_id": conversationID,
			})
		},
		OnDisconnected: func() {
			bus.Publish(events.AgentDisconnected, map[string]interface{}{
				"leg_id": id,
			})
		},
		OnUserTranscript: func(text string) {
			bus.Publish(events.AgentUserTranscript, map[string]interface{}{
				"leg_id": id,
				"text":   text,
			})
		},
		OnAgentResponse: func(text string) {
			bus.Publish(events.AgentAgentResponse, map[string]interface{}{
				"leg_id": id,
				"text":   text,
			})
		},
	}

	go func() {
		err := session.Start(l.Context(), audioIn, audioOut, apiKey, opts, cb)
		s.Log.Info("agent session exited", "leg_id", id, "error", err)
		s.cleanupLegAgent(id)
	}()

	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "agent_started", "leg_id": id})
}

func (s *Server) stopAgentLeg(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	legAgents.Lock()
	_, ok := legAgents.m[id]
	legAgents.Unlock()

	if !ok {
		writeError(w, http.StatusNotFound, "no agent attached to this leg")
		return
	}

	s.cleanupLegAgent(id)
	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "agent_stopped"})
}

func (s *Server) cleanupLegAgent(legID string) {
	legAgents.Lock()
	info, ok := legAgents.m[legID]
	if ok {
		delete(legAgents.m, legID)
	}
	legAgents.Unlock()

	if !ok {
		return
	}

	info.session.Stop()

	// Close speakBuf first to unblock the mixer's readLoop (which may
	// be blocked in streamBuffer.Read) before removing the participant.
	if info.speakBuf != nil {
		info.speakBuf.Close()
	}

	// Clear mixer tap and remove playback source if leg was in a room.
	if info.roomID != "" {
		if rm, rmOK := s.RoomMgr.Get(info.roomID); rmOK {
			mix := rm.Mixer()
			mix.ClearParticipantTap(legID)
			if info.sourceID != "" {
				mix.RemoveParticipant(info.sourceID)
			}
		}
	}

	// Close pipes to unblock goroutines.
	for _, pw := range info.pipes {
		pw.Close()
	}
}

func (s *Server) agentRoom(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	var req agentRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.AgentID == "" {
		writeError(w, http.StatusBadRequest, "agent_id is required")
		return
	}

	apiKey := req.APIKey
	if apiKey == "" {
		apiKey = s.Config.ElevenLabsAPIKey
	}
	if apiKey == "" {
		writeError(w, http.StatusServiceUnavailable, "no ElevenLabs API key provided")
		return
	}

	rm, ok := s.RoomMgr.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "room not found")
		return
	}

	roomAgents.Lock()
	if _, exists := roomAgents.m[id]; exists {
		roomAgents.Unlock()
		writeError(w, http.StatusConflict, "agent already attached to this room")
		return
	}
	roomAgents.Unlock()

	opts := agent.Options{
		AgentID:          req.AgentID,
		Language:         req.Language,
		FirstMessage:     req.FirstMessage,
		DynamicVariables: req.DynamicVariables,
	}

	sourceID := "agent-" + uuid.New().String()[:8]

	// speak buffer: agent writes PCM → paced streamBuffer → mixer reads
	sb := newStreamBuffer()
	// listen pipe: mixer writes mixed-minus-self → agent reads it
	listenPR, listenPW := createPipe()

	rm.Mixer().AddParticipant(sourceID, sb, listenPW)

	ctx, cancel := context.WithCancel(context.Background())

	session := agent.New(s.Log)
	info := &agentInfo{
		session:  session,
		sourceID: sourceID,
		pipes:    []*pipeWriter{listenPW},
		speakBuf: sb,
		cancel:   cancel,
	}

	roomAgents.Lock()
	roomAgents.m[id] = info
	roomAgents.Unlock()

	bus := s.Bus
	cb := agent.Callbacks{
		OnConnected: func(conversationID string) {
			bus.Publish(events.AgentConnected, map[string]interface{}{
				"room_id":         id,
				"conversation_id": conversationID,
			})
		},
		OnDisconnected: func() {
			bus.Publish(events.AgentDisconnected, map[string]interface{}{
				"room_id": id,
			})
		},
		OnUserTranscript: func(text string) {
			bus.Publish(events.AgentUserTranscript, map[string]interface{}{
				"room_id": id,
				"text":    text,
			})
		},
		OnAgentResponse: func(text string) {
			bus.Publish(events.AgentAgentResponse, map[string]interface{}{
				"room_id": id,
				"text":    text,
			})
		},
	}

	go func() {
		err := session.Start(ctx, listenPR, sb, apiKey, opts, cb)
		s.Log.Info("agent room session exited", "room_id", id, "error", err)
		s.cleanupRoomAgent(id)
	}()

	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "agent_started", "room_id": id})
}

func (s *Server) stopAgentRoom(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	roomAgents.Lock()
	_, ok := roomAgents.m[id]
	roomAgents.Unlock()

	if !ok {
		writeError(w, http.StatusNotFound, "no agent attached to this room")
		return
	}

	s.cleanupRoomAgent(id)
	writeJSON(w, http.StatusOK, map[string]interface{}{"status": "agent_stopped"})
}

// stopRoomAgentIfEmpty cleans up the room's agent when no leg participants
// remain. Called after a leg is removed from a room.
func (s *Server) stopRoomAgentIfEmpty(roomID string) {
	rm, ok := s.RoomMgr.Get(roomID)
	if !ok || rm.ParticipantCount() > 0 {
		return
	}
	s.cleanupRoomAgent(roomID)
}

func (s *Server) cleanupRoomAgent(roomID string) {
	roomAgents.Lock()
	info, ok := roomAgents.m[roomID]
	if ok {
		delete(roomAgents.m, roomID)
	}
	roomAgents.Unlock()

	if !ok {
		return
	}

	// Cancel dedicated context first, then stop session.
	if info.cancel != nil {
		info.cancel()
	}
	info.session.Stop()

	// Close speakBuf first to unblock the mixer's readLoop (which may
	// be blocked in streamBuffer.Read) before removing the participant.
	if info.speakBuf != nil {
		info.speakBuf.Close()
	}

	// Close pipes to unblock goroutines.
	for _, pw := range info.pipes {
		pw.Close()
	}

	// Remove agent from mixer (signals done, stops readLoop/writeLoop).
	if rm, rmOK := s.RoomMgr.Get(roomID); rmOK {
		rm.Mixer().RemoveParticipant(info.sourceID)
	}
}
