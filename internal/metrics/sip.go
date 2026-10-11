package metrics

import "github.com/prometheus/client_golang/prometheus"

// sipRequestsRejected is package-level because the SIP engine has no Collector
// reference. New registers it.
var sipRequestsRejected = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "voiceblender_sip_requests_rejected_total",
	Help: "Total SIP requests dropped because their source is not in SIP_ALLOWED_IPS.",
})

// RecordSIPRejected counts one SIP request dropped by the source allowlist.
func RecordSIPRejected() {
	sipRequestsRejected.Inc()
}
