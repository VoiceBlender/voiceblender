#!/usr/bin/env python3
"""
VoiceBlender call handler example.

Listens for incoming SIP calls via webhook, answers them,
and adds all calls to a single shared room so callers can talk to each other.
Optionally plays an audio file into the room.

With --early-media, the script enables early media (SIP 183) before answering,
plays an announcement to the caller, and then answers the call.

Usage:
    python3 call_handler.py                                    # shared room, no audio
    python3 call_handler.py --audio-url https://example.com/greeting.wav
    python3 call_handler.py --direct-leg --audio-url URL       # bypass mixer
    python3 call_handler.py --early-media --audio-url URL      # play before answer

Environment variables:
    VOICEBLENDER_URL   Base URL of VoiceBlender API (default: http://localhost:8090)
    WEBHOOK_PORT       Port for the local webhook listener (default: 5000)
"""

import argparse
import json
import logging
import sys
import threading
import time
import uuid
from http.server import HTTPServer, BaseHTTPRequestHandler

import requests

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s %(levelname)s %(message)s",
)
log = logging.getLogger("call_handler")


class Config:
    def __init__(self, args):
        self.base_url = args.base_url.rstrip("/")
        self.webhook_port = args.webhook_port
        self.audio_url = args.audio_url
        self.room_prefix = args.room_prefix
        self.direct_leg = args.direct_leg
        self.early_media = args.early_media
        self.early_media_delay = args.early_media_delay
        self.record = args.record
        # Shared room: created once, all calls join it
        self.shared_room_id = None
        self.shared_room_lock = threading.Lock()


def api(cfg, method, path, body=None):
    """Make an API request to VoiceBlender and return the JSON response."""
    url = f"{cfg.base_url}/v1{path}"
    resp = requests.request(method, url, json=body, timeout=10)
    log.debug("%s %s -> %d", method, url, resp.status_code)
    if resp.status_code >= 400:
        log.error("API error: %s %s -> %d %s", method, path, resp.status_code, resp.text)
    return resp.status_code, resp.json()


def get_or_create_room(cfg):
    """Get the shared room, creating it on first call."""
    with cfg.shared_room_lock:
        if cfg.shared_room_id is not None:
            return cfg.shared_room_id

        room_id = f"{cfg.room_prefix}-{uuid.uuid4().hex[:8]}"
        status, resp = api(cfg, "POST", "/rooms", {"id": room_id})
        if status != 201:
            log.error("Failed to create room %s: %s", room_id, resp)
            return None
        log.info("Created shared room %s", room_id)
        cfg.shared_room_id = room_id

        # Optionally start room recording
        if cfg.record:
            status, resp = api(cfg, "POST", f"/rooms/{room_id}/record")
            if status == 200:
                log.info("Room recording started: %s", resp.get("file", "?"))
            else:
                log.warning("Failed to start room recording: %s", resp)

        return room_id


def wait_for_state(cfg, leg_id, target_states, timeout=5, interval=0.1):
    """Poll GET /v1/legs/{id} until state is one of target_states or timeout."""
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        status, resp = api(cfg, "GET", f"/legs/{leg_id}")
        if status != 200:
            return False
        if resp.get("state") in target_states:
            return True
        time.sleep(interval)
    return False


def handle_ringing(cfg, event_data):
    """Answer the call and add the leg to the shared room."""
    leg_id = event_data.get("leg_id")
    caller = event_data.get("from", "unknown")
    callee = event_data.get("to") or event_data.get("uri", "unknown")
    sip_headers = event_data.get("sip_headers", {})
    log.info("Incoming call: %s -> %s (leg %s)", caller, callee, leg_id)
    if sip_headers:
        for name, value in sip_headers.items():
            log.info("  SIP header: %s: %s", name, value)

    # --- Early media: play audio or tone before answering ---
    if cfg.early_media:
        status, resp = api(cfg, "POST", f"/legs/{leg_id}/early-media")
        if status != 200:
            log.error("Failed to enable early media on leg %s: %s", leg_id, resp)
            return
        log.info("Early media enabled on leg %s (SIP 183 sent)", leg_id)

        # Play announcement or ringback tone while still ringing
        if cfg.audio_url:
            play_body = {"url": cfg.audio_url, "mime_type": "audio/wav"}
        else:
            play_body = {"tone": "us_ringback"}
        status, resp = api(cfg, "POST", f"/legs/{leg_id}/play", play_body)
        if status != 200:
            log.error("Failed to play early media on leg %s: %s", leg_id, resp)
        else:
            log.info("Playing early media on leg %s: %s", leg_id, play_body)

        # Wait before answering so the caller hears the announcement
        delay = cfg.early_media_delay
        log.info("Waiting %.1fs before answering leg %s...", delay, leg_id)
        time.sleep(delay)

    # 1. Answer the call
    status, resp = api(cfg, "POST", f"/legs/{leg_id}/answer")
    if status != 200:
        log.error("Failed to answer leg %s: %s", leg_id, resp)
        return
    log.info("Answering leg %s, waiting for connected state...", leg_id)

    # Poll until the leg reaches "connected" state (SIP 200 OK + media setup)
    if not wait_for_state(cfg, leg_id, ("connected",)):
        log.error("Leg %s did not reach connected state", leg_id)
        return
    log.info("Leg %s connected", leg_id)

    if cfg.direct_leg:
        # --- Direct leg playback (bypasses mixer/room entirely) ---
        log.info("Playing audio DIRECTLY to leg %s (no room/mixer)", leg_id)
        status, resp = api(cfg, "POST", f"/legs/{leg_id}/play", {
            "url": cfg.audio_url,
            "mime_type": "audio/wav",
        })
        if status != 200:
            log.error("Failed to play audio to leg %s: %s", leg_id, resp)
        else:
            log.info("Playing audio to leg %s: %s", leg_id, cfg.audio_url)
        return

    # --- Shared room (all calls join the same room) ---

    # 2. Get or create the shared room
    room_id = get_or_create_room(cfg)
    if room_id is None:
        return

    # 3. Add the leg to the room
    status, resp = api(cfg, "POST", f"/rooms/{room_id}/legs", {"leg_id": leg_id})
    if status != 200:
        log.error("Failed to add leg %s to room %s: %s", leg_id, room_id, resp)
        return
    log.info("Added leg %s to room %s", leg_id, room_id)

    # 4. Optionally play audio into the room
    if cfg.audio_url:
        status, resp = api(cfg, "POST", f"/rooms/{room_id}/play", {
            "url": cfg.audio_url,
            "mime_type": "audio/wav",
        })
        if status != 200:
            log.error("Failed to play audio in room %s: %s", room_id, resp)
        else:
            log.info("Playing audio in room %s: %s", room_id, cfg.audio_url)


def make_webhook_handler(cfg):
    """Create an HTTP request handler class that processes VoiceBlender webhooks."""

    class WebhookHandler(BaseHTTPRequestHandler):
        def do_POST(self):
            content_length = int(self.headers.get("Content-Length", 0))
            body = self.rfile.read(content_length)
            self.send_response(200)
            self.end_headers()

            try:
                event = json.loads(body)
            except json.JSONDecodeError:
                log.error("Invalid JSON in webhook: %s", body)
                return

            event_type = event.get("type", "")
            event_data = event.get("data", {})
            log.info("Webhook event: %s %s", event_type, json.dumps(event_data))

            if event_type == "leg.ringing":
                threading.Thread(
                    target=handle_ringing,
                    args=(cfg, event_data),
                    daemon=True,
                ).start()

        def log_message(self, format, *args):
            # Suppress default HTTP server logging
            pass

    return WebhookHandler


def register_webhook(cfg):
    """Register our local webhook listener with VoiceBlender."""
    import socket
    local_ip = socket.gethostbyname(socket.gethostname())
    webhook_url = f"http://{local_ip}:{cfg.webhook_port}/webhook"

    status, resp = api(cfg, "POST", "/webhooks", {"url": webhook_url})
    if status == 201:
        log.info("Registered webhook: %s (id: %s)", webhook_url, resp.get("id"))
        return resp.get("id")
    else:
        log.error("Failed to register webhook: %s", resp)
        sys.exit(1)


def main():
    parser = argparse.ArgumentParser(description="VoiceBlender call handler example")
    parser.add_argument(
        "--base-url",
        default="http://localhost:8090",
        help="VoiceBlender API base URL (default: http://localhost:8090)",
    )
    parser.add_argument(
        "--webhook-port",
        type=int,
        default=5000,
        help="Local port for webhook listener (default: 5000)",
    )
    parser.add_argument(
        "--audio-url",
        default=None,
        help="URL of a WAV file to play into the room (optional)",
    )
    parser.add_argument(
        "--room-prefix",
        default="room",
        help="Prefix for auto-generated room IDs (default: room)",
    )
    parser.add_argument(
        "--direct-leg",
        action="store_true",
        help="Play audio directly to leg (bypasses room/mixer for debugging)",
    )
    parser.add_argument(
        "--early-media",
        action="store_true",
        help="Enable early media (SIP 183) and play --audio-url before answering",
    )
    parser.add_argument(
        "--early-media-delay",
        type=float,
        default=3.0,
        help="Seconds to wait after starting early media playback before answering (default: 3.0)",
    )
    parser.add_argument(
        "--record",
        action="store_true",
        help="Start room recording when playing (for debugging)",
    )
    parser.add_argument(
        "--verbose", "-v",
        action="store_true",
        help="Enable debug logging",
    )
    args = parser.parse_args()

    if args.verbose:
        logging.getLogger().setLevel(logging.DEBUG)

    cfg = Config(args)

    # Register webhook with VoiceBlender
    webhook_id = register_webhook(cfg)

    # Start webhook HTTP server
    server = HTTPServer(("0.0.0.0", cfg.webhook_port), make_webhook_handler(cfg))
    log.info("Webhook listener started on port %d", cfg.webhook_port)
    log.info("Waiting for incoming calls...")

    try:
        server.serve_forever()
    except KeyboardInterrupt:
        log.info("Shutting down...")
        server.shutdown()
        # Clean up webhook registration
        api(cfg, "DELETE", f"/webhooks/{webhook_id}")
        log.info("Webhook unregistered, goodbye.")


if __name__ == "__main__":
    main()
