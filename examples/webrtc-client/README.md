# VoiceBlender WebRTC Client

A single-file browser client for interacting with VoiceBlender via WebRTC.

## Usage

1. Start VoiceBlender:

   ```bash
   go run ./cmd/voiceblender
   ```

2. Open `index.html` in a browser (Chrome/Edge recommended), or serve it:

   ```bash
   python3 -m http.server 3000 -d examples/webrtc-client
   ```

   Then open http://localhost:3000

3. Set the VoiceBlender server URL (default: `http://localhost:8080`) and click **Connect**.

## Features

- **WebRTC audio** -- full-duplex voice via `POST /v1/webrtc/offer` (PCMU/G.711 at 8kHz)
- **Mute/unmute** -- toggles mic locally and notifies the server
- **Room management** -- create or join a room, add your leg to it for multi-party mixing
- **DTMF keypad** -- send RFC 4733 telephone-event digits
- **Audio meters** -- real-time mic and speaker level indicators
- **Event log** -- shows connection state, API responses, and errors

## Requirements

- A modern browser with WebRTC and `getUserMedia` support
- VoiceBlender running and reachable from the browser
- Microphone access (the browser will prompt)

## Notes

- The client uses PCMU (G.711 u-law) at 8kHz, matching VoiceBlender's WebRTC codec.
- When joining a room, the mixer operates at 16kHz internally; VoiceBlender handles resampling.
- If VoiceBlender runs on a different host, ensure CORS is allowed or use a reverse proxy.
