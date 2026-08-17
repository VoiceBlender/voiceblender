package stt

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/wsutilx"
)

// ForceEndOfUtterance is a genuine mid-stream flush, so the provider claims the
// capability that answers POST /legs/{id}/stt/finalize with 200.
var _ Finalizer = (*SpeechmaticsTranscriber)(nil)

func TestNewSpeechmatics_DefaultsToTheSaaSEndpoint(t *testing.T) {
	if got := NewSpeechmatics("", slog.Default()).url; got != smDefaultURL {
		t.Errorf("url = %q, want %q", got, smDefaultURL)
	}
	const selfHosted = "ws://asr:9000/v2"
	if got := NewSpeechmatics(selfHosted, slog.Default()).url; got != selfHosted {
		t.Errorf("url = %q, want %q", got, selfHosted)
	}
}

func TestBuildStartRecognition(t *testing.T) {
	const prefix = `{"message":"StartRecognition","audio_format":{"type":"raw","encoding":"pcm_s16le","sample_rate":16000},"transcription_config":`

	cases := []struct {
		name string
		opts Options
		want string
	}{
		{
			name: "defaults_turn_detection_on",
			opts: Options{},
			want: `{"language":"en","conversation_config":{"end_of_utterance_silence_trigger":0.6}}`,
		},
		{
			name: "language_model_and_partials",
			opts: Options{Language: "es", Model: "enhanced", Partial: true},
			want: `{"language":"es","model":"enhanced","enable_partials":true,"conversation_config":{"end_of_utterance_silence_trigger":0.6}}`,
		},
		{
			name: "keyterms_become_additional_vocab",
			opts: Options{Keyterms: []string{"VoiceBlender", "SIPREC"}},
			want: `{"language":"en","additional_vocab":["VoiceBlender","SIPREC"],"conversation_config":{"end_of_utterance_silence_trigger":0.6}}`,
		},
		{
			name: "utterance_end_ms_maps_to_seconds",
			opts: Options{UtteranceEndMs: intp(900)},
			want: `{"language":"en","conversation_config":{"end_of_utterance_silence_trigger":0.9}}`,
		},
		{
			name: "utterance_end_ms_clamped_to_the_provider_max",
			opts: Options{UtteranceEndMs: intp(5000)},
			want: `{"language":"en","conversation_config":{"end_of_utterance_silence_trigger":2}}`,
		},
		{
			// 0 is meaningful, not absent: it turns turn detection off.
			name: "utterance_end_ms_zero_disables_turn_detection",
			opts: Options{UtteranceEndMs: intp(0)},
			want: `{"language":"en"}`,
		},
		{
			name: "endpointing_maps_to_max_delay",
			opts: Options{Endpointing: intp(1500)},
			want: `{"language":"en","max_delay":1.5,"conversation_config":{"end_of_utterance_silence_trigger":0.6}}`,
		},
		{
			name: "endpointing_clamped_up_to_the_provider_min",
			opts: Options{Endpointing: intp(100)},
			want: `{"language":"en","max_delay":0.7,"conversation_config":{"end_of_utterance_silence_trigger":0.6}}`,
		},
		{
			name: "endpointing_clamped_down_to_the_provider_max",
			opts: Options{Endpointing: intp(9000)},
			want: `{"language":"en","max_delay":4,"conversation_config":{"end_of_utterance_silence_trigger":0.6}}`,
		},
		{
			// Deepgram reads 0 as "no endpointing"; Speechmatics cannot express
			// that, so it must fall back to the vendor default rather than 0.
			name: "endpointing_zero_leaves_max_delay_unset",
			opts: Options{Endpointing: intp(0)},
			want: `{"language":"en","conversation_config":{"end_of_utterance_silence_trigger":0.6}}`,
		},
		{
			name: "flux_only_fields_are_ignored",
			opts: Options{EagerEOTThreshold: f64p(0.4), EOTThreshold: f64p(0.75), EOTTimeoutMs: intp(4000), LanguageHints: []string{"en", "es"}},
			want: `{"language":"en","conversation_config":{"end_of_utterance_silence_trigger":0.6}}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildStartRecognition(tc.opts)
			if err != nil {
				t.Fatalf("buildStartRecognition: %v", err)
			}
			want := prefix + tc.want + "}"
			if string(got) != want {
				t.Errorf("buildStartRecognition()\n got %s\nwant %s", got, want)
			}
		})
	}
}

func smWordResult(word string, start, end float64) string {
	return fmt.Sprintf(`{"type":"word","start_time":%v,"end_time":%v,"alternatives":[{"content":%q,"confidence":0.95}]}`, start, end, word)
}

func smTranscriptFrame(message, transcript string, start, end float64, results ...string) string {
	var b bytes.Buffer
	fmt.Fprintf(&b, `{"message":%q,"metadata":{"transcript":%q,"start_time":%v,"end_time":%v},"results":[`, message, transcript, start, end)
	for i, r := range results {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(r)
	}
	b.WriteString("]}")
	return b.String()
}

func runSpeechmaticsRecv(t *testing.T, opts Options, frames []string) *collector {
	t.Helper()
	conn := sttScriptServer(t, frames)
	var c collector
	tr := NewSpeechmatics("", slog.Default())

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	tr.recvLoop(ctx, conn, wsutilx.NewLockedWriter(conn), c.sinks(opts), nil)
	return &c
}

// One utterance spanning two AddTranscript segments, closed by EndOfUtterance,
// followed by a second utterance — the sequence a voice agent turn-takes on.
func TestSpeechmaticsDispatch_UtteranceSequence(t *testing.T) {
	frames := []string{
		`{"message":"RecognitionStarted","id":"sess-1"}`,
		`{"message":"AudioAdded","seq_no":1}`,
		smTranscriptFrame("AddPartialTranscript", "how do", 0.0, 0.7),
		smTranscriptFrame("AddTranscript", "How do I", 0.0, 1.0,
			smWordResult("How", 0.1, 0.3), smWordResult("do", 0.3, 0.5), smWordResult("I", 0.5, 0.7)),
		smTranscriptFrame("AddTranscript", "reset it?", 1.0, 1.8,
			smWordResult("reset", 1.1, 1.4), smWordResult("it", 1.4, 1.6),
			`{"type":"punctuation","start_time":1.6,"end_time":1.6,"alternatives":[{"content":"?","confidence":1}]}`),
		`{"message":"EndOfUtterance","metadata":{"start_time":1.8,"end_time":2.4}}`,
		smTranscriptFrame("AddTranscript", "Yes.", 3.0, 3.4, smWordResult("Yes", 3.05, 3.3)),
		`{"message":"EndOfUtterance","metadata":{"start_time":3.4,"end_time":4.0},"forced":true}`,
	}

	assertTurns := func(t *testing.T, c *collector) {
		t.Helper()
		if got := c.turnEvents(); !equalStrings(got, []string{TurnEndOfTurn, TurnEndOfTurn}) {
			t.Fatalf("turn events = %v, want two %s", got, TurnEndOfTurn)
		}

		first := c.turns[0]
		if first.TurnIndex != 0 {
			t.Errorf("first turn index = %d, want 0", first.TurnIndex)
		}
		// The segments are joined, not overwritten: a turn is the whole
		// utterance, not just its last AddTranscript.
		if first.Transcript != "How do I reset it?" {
			t.Errorf("first turn transcript = %q, want %q", first.Transcript, "How do I reset it?")
		}
		if first.AudioWindowStart != 0.0 || first.AudioWindowEnd != 2.4 {
			t.Errorf("first turn window = [%v, %v], want [0, 2.4]", first.AudioWindowStart, first.AudioWindowEnd)
		}
		wantWords := []string{"How", "do", "I", "reset", "it"}
		gotWords := make([]string, len(first.Words))
		for i, w := range first.Words {
			gotWords[i] = w.Word
		}
		// Punctuation results carry no timing worth reporting and must not
		// arrive as words.
		if !equalStrings(gotWords, wantWords) {
			t.Errorf("first turn words = %v, want %v", gotWords, wantWords)
		}
		if len(first.Words) > 0 && (first.Words[0].Start != 0.1 || first.Words[0].End != 0.3 || first.Words[0].Confidence != 0.95) {
			t.Errorf("first word = %+v, want start 0.1 end 0.3 confidence 0.95", first.Words[0])
		}

		second := c.turns[1]
		if second.TurnIndex != 1 {
			t.Errorf("second turn index = %d, want 1", second.TurnIndex)
		}
		// The accumulator resets, so the second turn does not inherit the first.
		if second.Transcript != "Yes." {
			t.Errorf("second turn transcript = %q, want %q", second.Transcript, "Yes.")
		}
		if second.AudioWindowStart != 3.0 || second.AudioWindowEnd != 4.0 {
			t.Errorf("second turn window = [%v, %v], want [3, 4]", second.AudioWindowStart, second.AudioWindowEnd)
		}
	}

	t.Run("with_partials", func(t *testing.T) {
		c := runSpeechmaticsRecv(t, Options{Partial: true}, frames)

		want := []TranscriptEvent{
			{Text: "how do"},
			{Text: "How do I", IsFinal: true},
			{Text: "reset it?", IsFinal: true},
			{Text: "Yes.", IsFinal: true},
		}
		if len(c.transcripts) != len(want) {
			t.Fatalf("got %d transcripts, want %d: %+v", len(c.transcripts), len(want), c.transcripts)
		}
		for i, ev := range c.transcripts {
			if ev != want[i] {
				t.Errorf("transcript[%d] = %+v, want %+v", i, ev, want[i])
			}
			// EndOfUtterance is what says the speaker stopped, and it arrives
			// after the final — so no final may claim speech_final.
			if ev.SpeechFinal {
				t.Errorf("transcript[%d] (%q) claims speech_final", i, ev.Text)
			}
		}
		assertTurns(t, c)
	})

	t.Run("without_partials", func(t *testing.T) {
		c := runSpeechmaticsRecv(t, Options{}, frames)

		if len(c.transcripts) != 3 {
			t.Fatalf("got %d transcripts, want 3 finals and no partial: %+v", len(c.transcripts), c.transcripts)
		}
		for i, ev := range c.transcripts {
			if !ev.IsFinal {
				t.Errorf("transcript[%d] (%q) is not final; partials must be suppressed", i, ev.Text)
			}
		}
		// Turn detection is independent of partials: an agent that does not
		// want interim text still needs the turn boundary.
		assertTurns(t, c)
	})
}

// A forced end of utterance with nothing buffered still closes the turn, so a
// caller waiting on stt.turn after /stt/finalize is not left hanging.
func TestSpeechmaticsDispatch_EmptyUtteranceStillEmitsTurn(t *testing.T) {
	c := runSpeechmaticsRecv(t, Options{}, []string{
		`{"message":"EndOfUtterance","metadata":{"start_time":1.0,"end_time":1.2},"forced":true}`,
	})
	if got := c.turnEvents(); !equalStrings(got, []string{TurnEndOfTurn}) {
		t.Fatalf("turn events = %v, want one %s", got, TurnEndOfTurn)
	}
	if c.turns[0].Transcript != "" {
		t.Errorf("turn transcript = %q, want empty", c.turns[0].Transcript)
	}
}

// Speechmatics closes the socket itself on a fatal error, so recvLoop must keep
// reading rather than tear down a session that is still delivering transcripts.
func TestSpeechmaticsDispatch_ErrorFrameDoesNotEndTheLoop(t *testing.T) {
	c := runSpeechmaticsRecv(t, Options{}, []string{
		`{"message":"Warning","type":"duration_limit_exceeded","reason":"nearly there"}`,
		`{"message":"Error","type":"job_error","reason":"transient","code":500}`,
		smTranscriptFrame("AddTranscript", "still here", 0.0, 0.5),
	})
	if len(c.transcripts) != 1 || c.transcripts[0].Text != "still here" {
		t.Fatalf("transcripts = %+v, want the frame after the error", c.transcripts)
	}
}

// EndOfTranscript is the server's acknowledgement of EndOfStream; there is
// nothing further to read, so the loop returns.
func TestSpeechmaticsDispatch_EndOfTranscriptReturns(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		runSpeechmaticsRecv(t, Options{}, []string{`{"message":"EndOfTranscript"}`})
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("recvLoop did not return on EndOfTranscript")
	}
}

func TestSpeechmaticsFinalize_SendsExactFrameAndKeepsSocketOpen(t *testing.T) {
	conn, textCh, binaryCh := dgEchoServer(t)

	tr := NewSpeechmatics("", slog.Default())
	lw := wsutilx.NewLockedWriter(conn)
	tr.setWriter(lw)

	if err := tr.Finalize(context.Background()); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	if got := string(dgRecv(t, textCh, "force end of utterance")); got != `{"message":"ForceEndOfUtterance"}` {
		t.Errorf("finalize frame = %s, want the ForceEndOfUtterance message", got)
	}

	// The session survives the flush — that is the whole difference from stop.
	if err := lw.WriteBinary([]byte{0x01, 0x02}); err != nil {
		t.Fatalf("write after finalize: %v", err)
	}
	if got := dgRecv(t, binaryCh, "audio"); !bytes.Equal(got, []byte{0x01, 0x02}) {
		t.Errorf("audio after finalize = %v, want the frame that was sent", got)
	}
}

func TestSpeechmaticsFinalize_ErrorsWithoutASession(t *testing.T) {
	tr := NewSpeechmatics("", slog.Default())
	if err := tr.Finalize(context.Background()); err == nil {
		t.Fatal("Finalize before Start returned nil, want error")
	}

	conn, _, _ := dgEchoServer(t)
	tr.setWriter(wsutilx.NewLockedWriter(conn))
	tr.clearWriter()
	if err := tr.Finalize(context.Background()); err == nil {
		t.Fatal("Finalize after the session ended returned nil, want error")
	}
}

// EndOfStream tells the server how much audio to expect, so last_seq_no has to
// match the frames actually written or the tail is never transcribed.
func TestSpeechmaticsSendLoop_EndOfStreamCarriesTheFrameCount(t *testing.T) {
	t.Run("on_reader_close", func(t *testing.T) {
		conn, textCh, binaryCh := dgEchoServer(t)
		src := &chunkedReader{data: bytes.Repeat([]byte{0xAB}, smFrameBytes*3), chunk: smFrameBytes}

		tr := NewSpeechmatics("", slog.Default())
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		tr.sendLoop(ctx, src, wsutilx.NewLockedWriter(conn))

		for i := 0; i < 3; i++ {
			if f := recvFrame(t, binaryCh); len(f) != smFrameBytes {
				t.Fatalf("frame %d is %d bytes, want %d", i, len(f), smFrameBytes)
			}
		}
		assertEndOfStream(t, dgRecv(t, textCh, "end of stream"), 3)
	})

	t.Run("on_context_cancel", func(t *testing.T) {
		conn, textCh, _ := dgEchoServer(t)

		tr := NewSpeechmatics("", slog.Default())
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		tr.sendLoop(ctx, &chunkedReader{data: bytes.Repeat([]byte{0xAB}, smFrameBytes), chunk: smFrameBytes}, wsutilx.NewLockedWriter(conn))

		assertEndOfStream(t, dgRecv(t, textCh, "end of stream"), 0)
	})
}

func assertEndOfStream(t *testing.T, frame []byte, wantSeq int) {
	t.Helper()
	var got struct {
		Message   string `json:"message"`
		LastSeqNo int    `json:"last_seq_no"`
	}
	if err := json.Unmarshal(frame, &got); err != nil {
		t.Fatalf("parse %s: %v", frame, err)
	}
	if got.Message != "EndOfStream" {
		t.Errorf("message = %q, want EndOfStream", got.Message)
	}
	if got.LastSeqNo != wantSeq {
		t.Errorf("last_seq_no = %d, want %d", got.LastSeqNo, wantSeq)
	}
}

func TestSpeechmaticsSTT_FinalTranscriptTextOnlyAtDebug(t *testing.T) {
	assertCanaryOnlyAtDebug(t,
		captureSpeechmaticsRecv(t, Options{}, smTranscriptFrame("AddTranscript", piiCanary, 0, 1)),
		"speechmatics stt final transcript")
}

func TestSpeechmaticsSTT_PartialTranscriptTextOnlyAtDebug(t *testing.T) {
	// partial must be true: recvLoop skips the branch entirely when it is
	// false, which would make both assertions vacuous.
	assertCanaryOnlyAtDebug(t,
		captureSpeechmaticsRecv(t, Options{Partial: true}, smTranscriptFrame("AddPartialTranscript", piiCanary, 0, 1)),
		"speechmatics stt partial transcript")
}

func captureSpeechmaticsRecv(t *testing.T, opts Options, frame string) []capturedRecord {
	t.Helper()
	conn := sttScriptServer(t, []string{frame})
	log, captured := newCapturingLogger()

	var c collector
	tr := NewSpeechmatics("", log)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	tr.recvLoop(ctx, conn, wsutilx.NewLockedWriter(conn), c.sinks(opts), nil)

	if len(c.transcripts) != 1 || c.transcripts[0].Text != piiCanary {
		t.Fatalf("callback did not receive the transcript (got %+v) — the branch under test never ran, so the log assertions would be vacuous", c.transcripts)
	}
	return captured()
}
