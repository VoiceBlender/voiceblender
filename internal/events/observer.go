package events

// MetricsObserver receives webhook egress outcomes. It is declared here rather
// than in internal/metrics because that package already imports this one.
//
// Implementations must be goroutine-safe and non-blocking: OnWebhookEnqueued
// and OnWebhookDropped run inline on the publisher's goroutine.
type MetricsObserver interface {
	OnWebhookEnqueued()
	OnWebhookDropped()
	// OnWebhookDelivered reports a terminal delivery outcome: "success",
	// "exhausted", "marshal_error" or "request_error".
	OnWebhookDelivered(outcome string)
}
