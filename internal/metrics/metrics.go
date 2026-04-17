package metrics

import (
	"net/http"
	"sync"

	"github.com/VoiceBlender/voiceblender/internal/events"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Collector tracks VoiceBlender-specific Prometheus metrics and exposes a
// handler for the /metrics endpoint.
type Collector struct {
	mu      sync.Mutex
	legType map[string]string // leg_id → "sip_inbound" | "sip_outbound"

	activeLegs  prometheus.Gauge
	activeRooms prometheus.Gauge

	// legsTotal counts every leg lifecycle transition.
	// Labels: type ("sip_inbound"|"sip_outbound"|"unknown"), state ("ringing"|"connected"|"disconnected").
	legsTotal *prometheus.CounterVec

	// disconnectReasons counts legs by disconnect reason.
	// Labels: type, reason (e.g. "remote_bye", "api_hangup", "rtp_timeout", …).
	disconnectReasons *prometheus.CounterVec

	// callDurationSeconds observes the answered (talking) duration for each
	// call that was connected. rate(sum)/rate(count) gives ACD.
	// Labels: type, reason.
	callDurationSeconds *prometheus.HistogramVec

	// callTotalDurationSeconds observes total leg lifetime (ringing + talking).
	// Labels: type, reason.
	callTotalDurationSeconds *prometheus.HistogramVec

	registry *prometheus.Registry
}

var durationBuckets = []float64{5, 15, 30, 60, 120, 300, 600, 1800, 3600}

// New creates a Collector, registers all metrics, subscribes to the bus, and
// returns the ready-to-use collector.
func New(bus *events.Bus) *Collector {
	reg := prometheus.NewRegistry()

	c := &Collector{
		legType: make(map[string]string),

		activeLegs: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "voiceblender_active_legs",
			Help: "Number of legs currently in any state (ringing, early_media, connected, held).",
		}),

		activeRooms: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "voiceblender_active_rooms",
			Help: "Number of rooms currently open.",
		}),

		legsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "voiceblender_legs_total",
			Help: "Total leg lifecycle transitions.",
		}, []string{"type", "state"}),

		disconnectReasons: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "voiceblender_disconnect_reasons_total",
			Help: "Total disconnected legs by type and reason.",
		}, []string{"type", "reason"}),

		callDurationSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "voiceblender_call_duration_seconds",
			Help:    "Answered call duration in seconds. Use rate(sum)/rate(count) for ACD.",
			Buckets: durationBuckets,
		}, []string{"type"}),

		callTotalDurationSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "voiceblender_call_total_duration_seconds",
			Help:    "Total leg lifetime (ringing + talking) in seconds.",
			Buckets: durationBuckets,
		}, []string{"type"}),

		registry: reg,
	}

	reg.MustRegister(
		// Standard Go runtime and process metrics.
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		// VoiceBlender metrics.
		c.activeLegs,
		c.activeRooms,
		c.legsTotal,
		c.disconnectReasons,
		c.callDurationSeconds,
		c.callTotalDurationSeconds,
	)

	_ = bus.Subscribe(c.handle)
	return c
}

// Handler returns an http.Handler that serves the Prometheus metrics page.
func (c *Collector) Handler() http.Handler {
	return promhttp.HandlerFor(c.registry, promhttp.HandlerOpts{})
}

func (c *Collector) handle(e events.Event) {
	switch e.Type {
	case events.LegRinging:
		d := e.Data.(*events.LegRingingData)
		// Inbound ringing events have "from"/"to"; outbound have "uri".
		legType := "sip_inbound"
		if d.URI != "" {
			legType = "sip_outbound"
		}
		c.mu.Lock()
		if d.LegID != "" {
			c.legType[d.LegID] = legType
		}
		c.mu.Unlock()
		c.activeLegs.Inc()
		c.legsTotal.WithLabelValues(legType, "ringing").Inc()

	case events.LegConnected:
		d := e.Data.(*events.LegConnectedData)
		legType := d.LegType
		if legType == "" {
			legType = "unknown"
		}
		// Update the stored type (outbound type is now known with certainty).
		c.mu.Lock()
		if d.LegID != "" {
			c.legType[d.LegID] = legType
		}
		c.mu.Unlock()
		c.legsTotal.WithLabelValues(legType, "connected").Inc()

	case events.LegDisconnected:
		d := e.Data.(*events.LegDisconnectedData)
		reason := d.CDR.Reason
		if reason == "" {
			reason = "unknown"
		}
		durationTotal := d.CDR.DurationTotal
		durationAnswered := d.CDR.DurationAnswered

		c.mu.Lock()
		legType := c.legType[d.LegID]
		if d.LegID != "" {
			delete(c.legType, d.LegID)
		}
		c.mu.Unlock()

		if legType == "" {
			legType = "unknown"
		}

		c.activeLegs.Dec()
		c.legsTotal.WithLabelValues(legType, "disconnected").Inc()
		c.disconnectReasons.WithLabelValues(legType, reason).Inc()

		if durationTotal > 0 {
			c.callTotalDurationSeconds.WithLabelValues(legType).Observe(durationTotal)
		}
		if durationAnswered > 0 {
			c.callDurationSeconds.WithLabelValues(legType).Observe(durationAnswered)
		}

	case events.RoomCreated:
		c.activeRooms.Inc()

	case events.RoomDeleted:
		c.activeRooms.Dec()
	}
}
