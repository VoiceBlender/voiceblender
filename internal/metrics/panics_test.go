package metrics

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/events"
)

func scrape(t *testing.T, c *Collector) string {
	t.Helper()
	rec := httptest.NewRecorder()
	c.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

func TestRecordPanic_Exported(t *testing.T) {
	c := New(events.NewBus("test"))

	RecordPanic("mixer", "readLoop")
	RecordPanic("mixer", "readLoop")
	RecordPanic("room", "panicTeardown")

	body := scrape(t, c)
	for _, want := range []string{
		`voiceblender_recovered_panics_total{component="mixer",site="readLoop"} 2`,
		`voiceblender_recovered_panics_total{component="room",site="panicTeardown"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
}

// The counter is a package var, so a second Collector must be able to register
// it too — api and integration tests build more than one.
func TestRecordPanic_SurvivesSecondCollector(t *testing.T) {
	_ = New(events.NewBus("test"))
	c := New(events.NewBus("test"))

	RecordPanic("mixer", "mixTick")

	if !strings.Contains(scrape(t, c), `site="mixTick"`) {
		t.Error("second collector does not export the panic counter")
	}
}
