package api

import (
	"log/slog"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/leg"
)

// A leg delete takes the leg out of the manager before its recording stops, so
// the stop must take the app from the leg its caller holds.
func TestStopLegRecording_FinishedCarriesAppID(t *testing.T) {
	const appID = "rec-app"

	for _, tc := range []struct {
		name      string
		siprec    bool
		inManager bool
	}{
		{"leg delete", false, false},
		{"leg delete, SIPREC session", true, false},
		{"API stop", false, true},
		{"API stop, SIPREC session", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			l := leg.NewWebSocketOutboundPendingLeg(16000, false, slog.Default())
			l.SetAppID(appID)
			legID := l.ID()

			if tc.siprec {
				s.siprecMu.Lock()
				s.siprecRecordings[legID] = &siprecRecording{
					sampleRate: 16000,
					startTime:  time.Now(),
					dir:        t.TempDir(),
					log:        s.Log,
				}
				s.siprecMu.Unlock()
			} else {
				rec, _ := discardedRecorder(t, t.TempDir())
				legRecorders.Lock()
				legRecorders.m[legID] = rec
				legRecorders.Unlock()
				t.Cleanup(func() {
					legRecorders.Lock()
					delete(legRecorders.m, legID)
					legRecorders.Unlock()
				})
			}

			var got *events.RecordingFinishedData
			unsub := s.Bus.Subscribe(func(e events.Event) {
				if d, ok := e.Data.(*events.RecordingFinishedData); ok && e.Type == events.RecordingFinished {
					got = d
				}
			})
			defer unsub()

			if tc.inManager {
				s.LegMgr.Add(l)
				if _, err := s.doStopRecordLeg(legID); err != nil {
					t.Fatalf("doStopRecordLeg: %v", err)
				}
			} else if _, ok := s.stopLegRecording(legID, l); !ok {
				t.Fatal("stopLegRecording reported no recording, though one was in progress")
			}

			if got == nil {
				t.Fatal("no recording.finished was published, so this test proves nothing")
			}
			if got.LegID != legID {
				t.Errorf("recording.finished leg_id = %q, want %q", got.LegID, legID)
			}
			if got.AppID != appID {
				t.Errorf("recording.finished app_id = %q, want %q", got.AppID, appID)
			}
		})
	}
}
