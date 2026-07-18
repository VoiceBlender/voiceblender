package events

// MetricsObserver receives notifications about webhook egress outcomes so a
// metrics backend can turn them into time-series. It lives in this package
// (rather than in internal/metrics) because the metrics package already
// imports events; defining it there would create an import cycle.
//
// Implementations MUST be goroutine-safe and non-blocking. Methods are called
// inline on the bus dispatch path — which runs handlers synchronously on the
// publisher's goroutine, potentially a media-plane goroutine — and on the
// webhook worker goroutines. Any blocking work (I/O, an unbuffered send, a
// contended lock) inside an implementation adds latency to, or stalls, those
// paths. A Prometheus counter increment is a suitable amount of work; a
// network call is not.
//
// A nil MetricsObserver is valid: every call site nil-guards, so a registry
// with no observer wired behaves exactly as it did before.
type MetricsObserver interface {
	// OnWebhookEnqueued reports that an event was accepted onto the webhook
	// delivery queue.
	OnWebhookEnqueued(webhookID, eventType string)

	// OnWebhookDropped reports that an event was discarded because the
	// webhook delivery queue was full.
	OnWebhookDropped(webhookID, eventType string)

	// OnWebhookDelivered reports the terminal outcome of a delivery attempt
	// sequence. outcome is one of "success", "exhausted", "marshal_error",
	// or "request_error". Exactly one call is made per job that reaches a
	// terminal exit, so the label set is closed and its cardinality fixed.
	OnWebhookDelivered(webhookID, eventType, outcome string)
}
