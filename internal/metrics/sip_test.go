package metrics

import (
	"strconv"
	"strings"
	"testing"

	"github.com/VoiceBlender/voiceblender/internal/events"
)

// The counter is a package var shared by every test in the binary, so tests
// compare readings instead of expecting an absolute value.
func sipRejectedValue(t *testing.T, c *Collector) float64 {
	t.Helper()
	const name = "voiceblender_sip_requests_rejected_total "
	for _, line := range strings.Split(scrape(t, c), "\n") {
		if strings.HasPrefix(line, name) {
			v, err := strconv.ParseFloat(strings.TrimPrefix(line, name), 64)
			if err != nil {
				t.Fatalf("parse %q: %v", line, err)
			}
			return v
		}
	}
	t.Fatalf("%snot exported", name)
	return 0
}

func TestRecordSIPRejected_Exported(t *testing.T) {
	c := New(events.NewBus("test"))
	before := sipRejectedValue(t, c)

	RecordSIPRejected()
	RecordSIPRejected()

	if got := sipRejectedValue(t, c); got != before+2 {
		t.Errorf("counter = %g, want %g", got, before+2)
	}
}

func TestRecordSIPRejected_SurvivesSecondCollector(t *testing.T) {
	_ = New(events.NewBus("test"))
	c := New(events.NewBus("test"))
	before := sipRejectedValue(t, c)

	RecordSIPRejected()

	if got := sipRejectedValue(t, c); got != before+1 {
		t.Errorf("counter = %g, want %g", got, before+1)
	}
}
