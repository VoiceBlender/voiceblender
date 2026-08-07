package events

import "log/slog"

// LogEvent writes one line per bus event. Event payloads carry caller speech,
// DTMF digits and other subscriber-visible content, so the envelope goes out
// at info and the payload only at debug.
func LogEvent(log *slog.Logger, e Event) {
	log.Info("event", "type", string(e.Type), "event_id", e.EventID)
	log.Debug("event payload", "type", string(e.Type), "event_id", e.EventID, "data", e.Data)
}
