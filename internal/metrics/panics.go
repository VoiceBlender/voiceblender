package metrics

import "github.com/prometheus/client_golang/prometheus"

// recoveredPanics is package-level because the packages that recover — mixer,
// room — are leaves with no Collector reference. New registers it.
var recoveredPanics = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "voiceblender_recovered_panics_total",
	Help: "Total panics recovered and contained, by component and recovery site.",
}, []string{"component", "site"})

// RecordPanic counts one panic that was recovered rather than crashing the
// process. site names the recovering function or loop.
func RecordPanic(component, site string) {
	recoveredPanics.WithLabelValues(component, site).Inc()
}
