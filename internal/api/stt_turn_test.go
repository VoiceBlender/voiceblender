package api

import (
	"sync"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/config"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/VoiceBlender/voiceblender/internal/stt"
)

func TestNewSTTProvider(t *testing.T) {
	s := newTestServer(t)
	cases := []struct {
		provider string
		want     interface{}
	}{
		{"", (*stt.ElevenLabsTranscriber)(nil)},
		{"deepgram", (*stt.DeepgramTranscriber)(nil)},
		{"deepgram_flux", (*stt.FluxTranscriber)(nil)},
		{"azure", (*stt.AzureTranscriber)(nil)},
		{"speechmatics", (*stt.SpeechmaticsTranscriber)(nil)},
	}
	for _, tc := range cases {
		got := s.newSTTProvider(tc.provider)
		if gotT, wantT := typeName(got), typeName(tc.want); gotT != wantT {
			t.Errorf("newSTTProvider(%q) = %s, want %s", tc.provider, gotT, wantT)
		}
	}
}

func typeName(v interface{}) string {
	switch v.(type) {
	case *stt.ElevenLabsTranscriber:
		return "elevenlabs"
	case *stt.DeepgramTranscriber:
		return "deepgram"
	case *stt.FluxTranscriber:
		return "flux"
	case *stt.AzureTranscriber:
		return "azure"
	case *stt.SpeechmaticsTranscriber:
		return "speechmatics"
	}
	return "unknown"
}

// Flux is a second dialect of the same vendor, so it must resolve against the
// same key rather than needing a new env var.
func TestSTTAPIKey_FluxSharesTheDeepgramKey(t *testing.T) {
	s := newTestServer(t)
	s.Config = config.Config{
		DeepgramAPIKey:     "dg",
		ElevenLabsAPIKey:   "el",
		AzureSpeechKey:     "az",
		SpeechmaticsAPIKey: "sm",
	}

	cases := []struct {
		provider string
		wantKey  string
		wantName string
	}{
		{"", "el", "elevenlabs"},
		{"deepgram", "dg", "deepgram"},
		{"deepgram_flux", "dg", "deepgram_flux"},
		{"azure", "az", "azure"},
		{"speechmatics", "sm", "speechmatics"},
	}
	for _, tc := range cases {
		key, name := s.sttAPIKey(STTRequest{Provider: tc.provider})
		if key != tc.wantKey || name != tc.wantName {
			t.Errorf("sttAPIKey(%q) = (%q, %q), want (%q, %q)", tc.provider, key, name, tc.wantKey, tc.wantName)
		}
	}

	if key, _ := s.sttAPIKey(STTRequest{Provider: "deepgram_flux", APIKey: "override"}); key != "override" {
		t.Errorf("per-request api_key = %q, want the override to win", key)
	}
}

func TestSTTOptions_MappingAndValidation(t *testing.T) {
	t.Run("pointer_fields_distinguish_zero_from_unset", func(t *testing.T) {
		zero := 0
		opts, err := sttOptions(STTRequest{Endpointing: &zero})
		if err != nil {
			t.Fatalf("sttOptions: %v", err)
		}
		if opts.Endpointing == nil || *opts.Endpointing != 0 {
			t.Fatalf("Endpointing = %v, want a pointer to 0 — 0 disables endpointing and is not the same as absent", opts.Endpointing)
		}
		if bare, _ := sttOptions(STTRequest{}); bare.Endpointing != nil {
			t.Errorf("Endpointing = %v for an absent field, want nil", bare.Endpointing)
		}
	})

	t.Run("carries_every_field", func(t *testing.T) {
		eager, eot, timeout := 0.4, 0.8, 4000
		opts, err := sttOptions(STTRequest{
			Language: "es", Partial: true, Model: "flux-general-multi",
			Keyterms: []string{"kt"}, LanguageHints: []string{"en", "es"},
			EagerEOTThreshold: &eager, EOTThreshold: &eot, EOTTimeoutMs: &timeout,
		})
		if err != nil {
			t.Fatalf("sttOptions: %v", err)
		}
		if opts.Language != "es" || !opts.Partial || opts.Model != "flux-general-multi" ||
			len(opts.Keyterms) != 1 || len(opts.LanguageHints) != 2 ||
			*opts.EagerEOTThreshold != 0.4 || *opts.EOTThreshold != 0.8 || *opts.EOTTimeoutMs != 4000 {
			t.Fatalf("options = %+v, want every field carried through", opts)
		}
	})

	t.Run("rejects_out_of_range", func(t *testing.T) {
		bad := []struct {
			name string
			req  STTRequest
		}{
			{"eager_too_low", STTRequest{EagerEOTThreshold: fptr(0.1)}},
			{"eager_too_high", STTRequest{EagerEOTThreshold: fptr(0.95)}},
			{"eot_above_one", STTRequest{EOTThreshold: fptr(1.5)}},
			{"negative_endpointing", STTRequest{Endpointing: iptr(-1)}},
			{"negative_utterance_end", STTRequest{UtteranceEndMs: iptr(-1)}},
			{"negative_eot_timeout", STTRequest{EOTTimeoutMs: iptr(-1)}},
		}
		for _, tc := range bad {
			_, err := sttOptions(tc.req)
			if err == nil {
				t.Errorf("%s: sttOptions accepted an out-of-range value", tc.name)
				continue
			}
			if ae, ok := err.(*apiError); !ok || ae.Code != 400 {
				t.Errorf("%s: error = %v, want a 400", tc.name, err)
			}
		}
	})

	// A field that means nothing to the chosen provider is ignored, so that
	// switching providers never turns a valid request into an error.
	t.Run("ignores_inapplicable_fields", func(t *testing.T) {
		if _, err := sttOptions(STTRequest{Provider: "elevenlabs", EagerEOTThreshold: fptr(0.4)}); err != nil {
			t.Errorf("sttOptions rejected a Flux field on a non-Flux provider: %v", err)
		}
	})
}

func iptr(v int) *int         { return &v }
func fptr(v float64) *float64 { return &v }

type sttEventSink struct {
	mu    sync.Mutex
	texts []*events.STTTextData
	turns []*events.STTTurnData
}

func (k *sttEventSink) subscribe(s *Server) {
	s.Bus.Subscribe(func(e events.Event) {
		k.mu.Lock()
		defer k.mu.Unlock()
		switch d := e.Data.(type) {
		case *events.STTTextData:
			k.texts = append(k.texts, d)
		case *events.STTTurnData:
			k.turns = append(k.turns, d)
		}
	})
}

func (k *sttEventSink) snapshot() ([]*events.STTTextData, []*events.STTTurnData) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return append([]*events.STTTextData(nil), k.texts...), append([]*events.STTTurnData(nil), k.turns...)
}

func TestAttachSTTSinks_PublishesTextAndTurn(t *testing.T) {
	s := newTestServer(t)
	var sink sttEventSink
	sink.subscribe(s)

	opts := s.attachSTTSinks(stt.Options{}, events.LegRoomScope{LegID: "leg-1", RoomID: "room-1", AppID: "app-1"}, "")
	opts.OnTranscript(stt.TranscriptEvent{Text: "done", IsFinal: true, SpeechFinal: true})
	opts.OnTurn(stt.TurnEvent{
		Event:            stt.TurnEndOfTurn,
		TurnIndex:        2,
		Transcript:       "done",
		AudioWindowStart: 0.25,
		AudioWindowEnd:   1.5,
		Words:            []stt.TurnWord{{Word: "done", Confidence: 0.9, Start: 0.25, End: 0.5}},
		Languages:        []string{"en"},
	})

	texts, turns := sink.snapshot()
	if len(texts) != 1 || !texts[0].IsFinal || !texts[0].SpeechFinal || texts[0].LegID != "leg-1" {
		t.Fatalf("stt.text = %+v, want one final speech_final event scoped to leg-1", texts)
	}
	if len(turns) != 1 {
		t.Fatalf("stt.turn = %+v, want 1", turns)
	}
	tv := turns[0]
	if tv.Event != stt.TurnEndOfTurn || tv.TurnIndex != 2 || tv.RoomID != "room-1" || tv.AppID != "app-1" {
		t.Errorf("turn = %+v, want end_of_turn #2 scoped to room-1/app-1", tv)
	}
	// Deepgram reports seconds; every duration on this API is milliseconds.
	if tv.AudioWindowStartMs != 250 || tv.AudioWindowEndMs != 1500 {
		t.Errorf("audio window = %d..%dms, want 250..1500", tv.AudioWindowStartMs, tv.AudioWindowEndMs)
	}
	if len(tv.Words) != 1 || tv.Words[0].StartMs != 250 || tv.Words[0].EndMs != 500 {
		t.Errorf("words = %+v, want one word spanning 250..500ms", tv.Words)
	}
}

// A room shares one Options template across its legs. Binding the callbacks in
// place instead of on a copy would route every leg's events to whichever leg
// started last.
func TestAttachSTTSinks_LegsDoNotCrossWire(t *testing.T) {
	s := newTestServer(t)
	var sink sttEventSink
	sink.subscribe(s)

	shared := stt.Options{Partial: true}
	first := s.attachSTTSinks(shared, events.LegRoomScope{LegID: "leg-a", RoomID: "room-1"}, "")
	second := s.attachSTTSinks(shared, events.LegRoomScope{LegID: "leg-b", RoomID: "room-1"}, "")

	if shared.OnTurn != nil || shared.OnTranscript != nil {
		t.Fatal("attachSTTSinks mutated the shared template; every leg in the room would share one sink")
	}

	first.OnTurn(stt.TurnEvent{Event: stt.TurnStartOfTurn})
	second.OnTurn(stt.TurnEvent{Event: stt.TurnEndOfTurn})

	_, turns := sink.snapshot()
	if len(turns) != 2 {
		t.Fatalf("stt.turn count = %d, want 2", len(turns))
	}
	if turns[0].LegID != "leg-a" || turns[1].LegID != "leg-b" {
		t.Fatalf("turn leg ids = %q, %q; want leg-a then leg-b", turns[0].LegID, turns[1].LegID)
	}
}

func TestSTTStartRejectsOutOfRangeThreshold(t *testing.T) {
	s := newTestServer(t)
	s.Config.DeepgramAPIKey = "dg"
	s.LegMgr.Add(&apiMockLeg{id: "leg-1", createdAt: time.Now()})

	_, err := s.doStartSTTLeg("leg-1", STTRequest{Provider: "deepgram_flux", EagerEOTThreshold: fptr(0.1)})
	if err == nil {
		t.Fatal("doStartSTTLeg accepted eager_eot_threshold=0.1")
	}
	if ae, ok := err.(*apiError); !ok || ae.Code != 400 {
		t.Fatalf("error = %v, want a 400", err)
	}
}
