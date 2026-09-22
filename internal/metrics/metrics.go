package metrics

import (
	"net/http"
	"sync"
	"sync/atomic"

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

	activeLegs prometheus.Gauge

	// filtersUnavailable counts filters dropped from a leg's requested chain
	// because their backing resource was not running. A non-zero rate means
	// calls are being processed with less than was asked for.
	filtersUnavailable *prometheus.CounterVec

	// denoiseStats is the pool's own view, read at scrape time so there is no
	// per-leg bookkeeping to drift out of step with reality.
	denoiseStats atomic.Value // func() (streams, instances int)
	activeRooms  prometheus.Gauge

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

	// webhookEnqueued counts events accepted onto the webhook delivery queue.
	// Denominator for the drop ratio alongside webhookDropped.
	webhookEnqueued prometheus.Counter

	// webhookDropped counts events discarded because the delivery queue was full.
	webhookDropped prometheus.Counter

	// webhookDeliveries counts terminal delivery outcomes.
	// Labels: outcome ("success"|"exhausted"|"marshal_error"|"request_error").
	webhookDeliveries *prometheus.CounterVec

	// vsiEventsDropped counts events discarded because a VSI WebSocket client's
	// send buffer was full.
	vsiEventsDropped prometheus.Counter

	registry *prometheus.Registry
}

var _ events.MetricsObserver = (*Collector)(nil)

var durationBuckets = []float64{5, 15, 30, 60, 120, 300, 600, 1800, 3600}

// SetDenoiseStatsSource supplies the denoise pool's live counts. Without it the
// gauges read zero, which is correct for a build with no kernel installed.
func (c *Collector) SetDenoiseStatsSource(fn func() (streams, instances int)) {
	if fn != nil {
		c.denoiseStats.Store(fn)
	}
}

func (c *Collector) denoise() (int, int) {
	if fn, ok := c.denoiseStats.Load().(func() (int, int)); ok && fn != nil {
		return fn()
	}
	return 0, 0
}

// FilterUnavailable records a filter dropped from a requested chain.
func (c *Collector) FilterUnavailable(name string) {
	if c == nil {
		return
	}
	c.filtersUnavailable.WithLabelValues(name).Inc()
}

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

		filtersUnavailable: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "voiceblender_audio_filters_unavailable_total",
			Help: "Total filters dropped from a leg's requested chain because the filter was unavailable.",
		}, []string{"filter"}),

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

		webhookEnqueued: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "voiceblender_webhook_enqueued_total",
			Help: "Total events accepted onto the webhook delivery queue.",
		}),

		webhookDropped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "voiceblender_webhook_dropped_total",
			Help: "Total events dropped because the webhook delivery queue was full.",
		}),

		webhookDeliveries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "voiceblender_webhook_deliveries_total",
			Help: "Total terminal webhook delivery outcomes.",
		}, []string{"outcome"}),

		vsiEventsDropped: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "voiceblender_vsi_events_dropped_total",
			Help: "Total events dropped because a VSI WebSocket client's buffer was full.",
		}),

		registry: reg,
	}

	reg.MustRegister(
		// Standard Go runtime and process metrics. NewGoCollector exposes
		// `go_goroutines` (live goroutine count) which is the canonical
		// signal for goroutine-leak regressions — alert on persistent
		// growth after sessions end. No need for a separate gauge.
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		// VoiceBlender metrics.
		c.activeLegs,
		c.filtersUnavailable,
		c.activeRooms,
		c.legsTotal,
		c.disconnectReasons,
		c.callDurationSeconds,
		c.callTotalDurationSeconds,
		c.webhookEnqueued,
		c.webhookDropped,
		c.webhookDeliveries,
		c.vsiEventsDropped,
		recoveredPanics,
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "voiceblender_audio_denoise_streams",
			Help: "Legs currently running the denoise filter.",
		}, func() float64 { n, _ := c.denoise(); return float64(n) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "voiceblender_audio_denoise_instances",
			Help: "Pooled WebAssembly instances backing the denoise filter.",
		}, func() float64 { _, n := c.denoise(); return float64(n) }),
	)

	_ = bus.Subscribe(c.handle)
	return c
}

func (c *Collector) OnWebhookEnqueued() { c.webhookEnqueued.Inc() }

func (c *Collector) OnWebhookDropped() { c.webhookDropped.Inc() }

func (c *Collector) OnWebhookDelivered(outcome string) {
	c.webhookDeliveries.WithLabelValues(outcome).Inc()
}

// ObserveVSIDropped stays off events.MetricsObserver because internal/api can
// call the concrete collector directly.
func (c *Collector) ObserveVSIDropped() { c.vsiEventsDropped.Inc() }

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
