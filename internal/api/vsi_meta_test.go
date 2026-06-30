package api

import "testing"

// TestVSIMetadata_AllNewCommandsRegistered guards REST/VSI parity. Every
// REST endpoint that has a VSI equivalent must appear in VSICommandsMetadata,
// otherwise asyncapi-gen will produce an incomplete spec.
func TestVSIMetadata_AllNewCommandsRegistered(t *testing.T) {
	expected := []string{
		// Leg control
		"leg_ring", "leg_early_media", "leg_amd_start", "leg_transfer",
		// Recording
		"leg_record_start", "leg_record_stop", "leg_record_pause", "leg_record_resume",
		"room_record_start", "room_record_stop", "room_record_pause", "room_record_resume",
		// Playback
		"leg_play_start", "leg_play_stop", "leg_play_volume",
		"room_play_start", "room_play_stop", "room_play_volume",
		// STT
		"leg_stt_start", "leg_stt_stop", "room_stt_start", "room_stt_stop",
		// TTS
		"leg_tts", "room_tts",
		// Agent
		"leg_agent_elevenlabs", "leg_agent_vapi", "leg_agent_pipecat", "leg_agent_deepgram",
		"leg_agent_message", "leg_agent_stop",
		"room_agent_elevenlabs", "room_agent_vapi", "room_agent_pipecat", "room_agent_deepgram",
		"room_agent_message", "room_agent_stop",
		// Inbound auth (digest challenge)
		"challenge_leg", "challenge_registration", "accept_registration", "reject_registration",
	}
	registered := make(map[string]VSICommandMeta)
	for _, cmd := range VSICommandsMetadata() {
		registered[cmd.Name] = cmd
	}
	for _, name := range expected {
		cmd, ok := registered[name]
		if !ok {
			t.Errorf("VSI command %q missing from VSICommandsMetadata", name)
			continue
		}
		if cmd.Summary == "" {
			t.Errorf("VSI command %q has empty Summary", name)
		}
	}
}
