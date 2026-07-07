package api

import (
	"os"
	"regexp"
	"testing"
)

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
		// SIP trunks (outbound registrations)
		"create_sip_trunk", "list_sip_trunks", "get_sip_trunk", "delete_sip_trunk",
		// SIP registrations
		"delete_sip_registration",
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

// TestVSIMetadata_EveryCommandHasDispatchCase guards against advertising a
// command in the metadata (and therefore in asyncapi.yaml) without a matching
// handler in the WS dispatcher — the dispatcher falls through to "unknown
// command" and clients get a runtime error for a command the spec promises.
func TestVSIMetadata_EveryCommandHasDispatchCase(t *testing.T) {
	src, err := os.ReadFile("ws_commands.go")
	if err != nil {
		t.Fatalf("read ws_commands.go: %v", err)
	}
	handled := make(map[string]bool)
	for _, m := range regexp.MustCompile(`case "([a-z_]+)":`).FindAllStringSubmatch(string(src), -1) {
		handled[m[1]] = true
	}
	for _, cmd := range VSICommandsMetadata() {
		if !handled[cmd.Name] {
			t.Errorf("VSI command %q is in VSICommandsMetadata but has no dispatch case in ws_commands.go", cmd.Name)
		}
	}
}

// restOnlyRoutes are REST endpoints with no VSI equivalent by design: protocol
// upgrades (the connection itself is the transport) and infra endpoints.
var restOnlyRoutes = map[string]bool{
	"Get /metrics":        true, // Prometheus scrape
	"Get /legs/websocket": true, // inbound WebSocket media upgrade (wsLeg)
	"Connect /legs/moq":   true, // WebTransport/HTTP3 upgrade (moqLeg)
	"Get /rooms/{id}/ws":  true, // inbound WebSocket room upgrade (wsRoom)
	"Get /vsi":            true, // the VSI WebSocket endpoint itself
}

// routeToVSICommand maps every actionable REST route ("<Method> <path>") to the
// VSI command that mirrors it. Kept explicit (rather than derived) so it reads
// as a parity contract.
var routeToVSICommand = map[string]string{
	"Post /legs":                                      "create_leg",
	"Get /legs":                                       "list_legs",
	"Get /legs/{id}":                                  "get_leg",
	"Post /legs/{id}/answer":                          "answer_leg",
	"Post /legs/{id}/early-media":                     "leg_early_media",
	"Post /legs/{id}/ring":                            "leg_ring",
	"Post /legs/{id}/challenge":                       "challenge_leg",
	"Post /legs/{id}/mute":                            "mute_leg",
	"Delete /legs/{id}/mute":                          "unmute_leg",
	"Post /legs/{id}/deaf":                            "deaf_leg",
	"Delete /legs/{id}/deaf":                          "undeaf_leg",
	"Post /legs/{id}/hold":                            "hold_leg",
	"Delete /legs/{id}/hold":                          "unhold_leg",
	"Post /legs/{id}/transfer":                        "leg_transfer",
	"Delete /legs/{id}":                               "delete_leg",
	"Post /legs/{id}/dtmf":                            "send_leg_dtmf",
	"Post /legs/{id}/dtmf/accept":                     "accept_leg_dtmf",
	"Post /legs/{id}/dtmf/reject":                     "reject_leg_dtmf",
	"Post /legs/{id}/rtt":                             "send_leg_rtt",
	"Post /legs/{id}/rtt/accept":                      "accept_leg_rtt",
	"Post /legs/{id}/rtt/reject":                      "reject_leg_rtt",
	"Post /legs/{id}/play":                            "leg_play_start",
	"Delete /legs/{id}/play/{playbackID}":             "leg_play_stop",
	"Patch /legs/{id}/play/{playbackID}":              "leg_play_volume",
	"Post /legs/{id}/tts":                             "leg_tts",
	"Post /legs/{id}/record":                          "leg_record_start",
	"Delete /legs/{id}/record":                        "leg_record_stop",
	"Post /legs/{id}/record/pause":                    "leg_record_pause",
	"Post /legs/{id}/record/resume":                   "leg_record_resume",
	"Post /legs/{id}/stt":                             "leg_stt_start",
	"Delete /legs/{id}/stt":                           "leg_stt_stop",
	"Post /legs/{id}/agent/elevenlabs":                "leg_agent_elevenlabs",
	"Post /legs/{id}/agent/vapi":                      "leg_agent_vapi",
	"Post /legs/{id}/agent/pipecat":                   "leg_agent_pipecat",
	"Post /legs/{id}/agent/deepgram":                  "leg_agent_deepgram",
	"Post /legs/{id}/agent/message":                   "leg_agent_message",
	"Delete /legs/{id}/agent":                         "leg_agent_stop",
	"Post /legs/{id}/amd":                             "leg_amd_start",
	"Post /legs/{id}/ice-candidates":                  "webrtc_add_candidate",
	"Get /legs/{id}/ice-candidates":                   "webrtc_get_candidates",
	"Patch /legs/{id}/role":                           "set_leg_role",
	"Post /rooms":                                     "create_room",
	"Get /rooms":                                      "list_rooms",
	"Get /rooms/{id}":                                 "get_room",
	"Delete /rooms/{id}":                              "delete_room",
	"Post /rooms/{id}/legs":                           "add_leg_to_room",
	"Delete /rooms/{id}/legs/{legID}":                 "remove_leg_from_room",
	"Post /rooms/{id}/bridges":                        "bridge_create",
	"Get /rooms/{id}/bridges":                         "bridge_list",
	"Get /rooms/{id}/bridges/{bridgeID}":              "bridge_get",
	"Patch /rooms/{id}/bridges/{bridgeID}":            "bridge_update",
	"Delete /rooms/{id}/bridges/{bridgeID}":           "bridge_delete",
	"Get /rooms/{id}/routing":                         "room_routing_get",
	"Put /rooms/{id}/routing":                         "room_routing_set",
	"Patch /rooms/{id}/routing":                       "room_routing_update",
	"Post /rooms/{id}/play":                           "room_play_start",
	"Delete /rooms/{id}/play/{playbackID}":            "room_play_stop",
	"Patch /rooms/{id}/play/{playbackID}":             "room_play_volume",
	"Post /rooms/{id}/tts":                            "room_tts",
	"Post /rooms/{id}/record":                         "room_record_start",
	"Delete /rooms/{id}/record":                       "room_record_stop",
	"Post /rooms/{id}/record/pause":                   "room_record_pause",
	"Post /rooms/{id}/record/resume":                  "room_record_resume",
	"Post /rooms/{id}/stt":                            "room_stt_start",
	"Delete /rooms/{id}/stt":                          "room_stt_stop",
	"Post /rooms/{id}/agent/elevenlabs":               "room_agent_elevenlabs",
	"Post /rooms/{id}/agent/vapi":                     "room_agent_vapi",
	"Post /rooms/{id}/agent/pipecat":                  "room_agent_pipecat",
	"Post /rooms/{id}/agent/deepgram":                 "room_agent_deepgram",
	"Post /rooms/{id}/agent/message":                  "room_agent_message",
	"Delete /rooms/{id}/agent":                        "room_agent_stop",
	"Get /sip/registrations":                          "list_sip_registrations",
	"Delete /sip/registrations/{aor}":                 "delete_sip_registration",
	"Post /sip/registrations/attempts/{id}/challenge": "challenge_registration",
	"Post /sip/registrations/attempts/{id}/accept":    "accept_registration",
	"Post /sip/registrations/attempts/{id}/reject":    "reject_registration",
	"Post /sip/trunks":                                "create_sip_trunk",
	"Get /sip/trunks":                                 "list_sip_trunks",
	"Get /sip/trunks/{id}":                            "get_sip_trunk",
	"Delete /sip/trunks/{id}":                         "delete_sip_trunk",
	"Post /webrtc/offer":                              "webrtc_offer",
}

// TestVSIRESTParity fails when a REST route registered in server.go is neither
// mapped to a VSI command nor explicitly marked REST-only. This forces the
// author of a new REST endpoint to make a deliberate choice — add the mirroring
// VSI command (and map it here) or allowlist it — rather than silently leaving
// the WebSocket surface behind.
func TestVSIRESTParity(t *testing.T) {
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("read server.go: %v", err)
	}
	registered := make(map[string]bool)
	for _, cmd := range VSICommandsMetadata() {
		registered[cmd.Name] = true
	}
	routeRe := regexp.MustCompile(`r\.(Get|Post|Put|Patch|Delete|Connect)\("([^"]+)"`)
	for _, m := range routeRe.FindAllStringSubmatch(string(src), -1) {
		route := m[1] + " " + m[2]
		if restOnlyRoutes[route] {
			continue
		}
		cmd, ok := routeToVSICommand[route]
		if !ok {
			t.Errorf("REST route %q has no VSI command mapping and is not marked REST-only "+
				"(add it to routeToVSICommand or restOnlyRoutes)", route)
			continue
		}
		if !registered[cmd] {
			t.Errorf("REST route %q maps to VSI command %q which is not in VSICommandsMetadata", route, cmd)
		}
	}
}
