//go:build race

package sip

// raceEnabled reports whether the test binary was built with -race. Used to
// skip tests that would otherwise trip a data race living inside a third-party
// dependency (not in VoiceBlender code) that we cannot synchronize.
const raceEnabled = true
