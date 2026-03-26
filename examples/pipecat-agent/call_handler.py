#!/usr/bin/env python3
"""
VoiceBlender call handler for Pipecat agent.

Listens for incoming SIP calls via webhook, answers them, and attaches
a Pipecat agent so the caller can talk to the AI bot.

Usage:
    python3 call_handler.py
    python3 call_handler.py --pipecat-url ws://remote-bot:8765

Environment variables:
    VOICEBLENDER_URL   Base URL of VoiceBlender API (default: http://localhost:8080)
    WEBHOOK_PORT       Port for the local webhook listener (default: 5000)
"""

import argparse
import json
import logging
import sys
import threading
import time
from http.server import HTTPServer, BaseHTTPRequestHandler
from urllib.request import Request, urlopen
from urllib.error import URLError

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
log = logging.getLogger("pipecat-handler")

# ---------------------------------------------------------------------------
# Config
# ---------------------------------------------------------------------------

import os

VOICEBLENDER_URL = os.environ.get("VOICEBLENDER_URL", "http://localhost:8080")
WEBHOOK_PORT = int(os.environ.get("WEBHOOK_PORT", "5000"))


def api(method, path, body=None):
    """Make an HTTP request to VoiceBlender."""
    url = f"{VOICEBLENDER_URL}{path}"
    data = json.dumps(body).encode() if body else None
    req = Request(url, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    try:
        with urlopen(req, timeout=10) as resp:
            return resp.status, json.loads(resp.read())
    except URLError as e:
        log.error("API request failed: %s %s -> %s", method, url, e)
        return 0, {}


# ---------------------------------------------------------------------------
# Call handling logic
# ---------------------------------------------------------------------------

def handle_ringing(event_data, pipecat_url):
    """Answer an incoming call and attach the Pipecat agent."""
    leg_id = event_data.get("leg_id")
    caller = event_data.get("from", "unknown")
    callee = event_data.get("to", "unknown")
    log.info("Incoming call: %s -> %s (leg %s)", caller, callee, leg_id)

    # Answer the call.
    status, resp = api("POST", f"/v1/legs/{leg_id}/answer")
    if status != 200:
        log.error("Failed to answer leg %s: %s", leg_id, resp)
        return

    # Wait for the leg to be fully connected.
    for _ in range(20):
        time.sleep(0.25)
        st, leg = api("GET", f"/v1/legs/{leg_id}")
        if st == 200 and leg.get("state") == "connected":
            break
    else:
        log.error("Leg %s did not reach connected state", leg_id)
        return

    # Attach the Pipecat agent.
    status, resp = api("POST", f"/v1/legs/{leg_id}/agent", {
        "agent_id": pipecat_url,
        "provider": "pipecat",
    })
    if status == 200:
        log.info("Pipecat agent attached to leg %s", leg_id)
    else:
        log.error("Failed to attach agent to leg %s: %s", leg_id, resp)


def handle_disconnected(event_data):
    """Log call disconnection."""
    leg_id = event_data.get("leg_id")
    reason = event_data.get("reason", "unknown")
    duration = event_data.get("duration_answered", 0)
    log.info("Call ended: leg %s, reason=%s, duration=%.1fs", leg_id, reason, duration)


# ---------------------------------------------------------------------------
# Webhook HTTP server
# ---------------------------------------------------------------------------

class WebhookHandler(BaseHTTPRequestHandler):
    pipecat_url = "ws://localhost:8765"

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(length)) if length else {}

        event_type = body.get("type", "")
        event_data = body.get("data", {})

        self.send_response(200)
        self.end_headers()
        self.wfile.write(b'{"ok": true}')

        if event_type == "leg.ringing":
            # Handle in a thread so the webhook response is not delayed.
            threading.Thread(
                target=handle_ringing,
                args=(event_data, self.pipecat_url),
                daemon=True,
            ).start()
        elif event_type == "leg.disconnected":
            handle_disconnected(event_data)
        elif event_type == "agent.connected":
            log.info("Agent connected: conversation_id=%s", event_data.get("conversation_id"))
        elif event_type == "agent.disconnected":
            log.info("Agent disconnected: leg %s", event_data.get("leg_id"))
        elif event_type == "agent.user_transcript":
            log.info("User said: %s", event_data.get("text"))
        elif event_type == "agent.agent_response":
            log.info("Bot said: %s", event_data.get("text"))

    def log_message(self, format, *args):
        pass  # Suppress default HTTP logging.


def main():
    parser = argparse.ArgumentParser(description="VoiceBlender Pipecat call handler")
    parser.add_argument(
        "--pipecat-url",
        default=os.environ.get("PIPECAT_URL", "ws://localhost:8765"),
        help="WebSocket URL of the Pipecat bot (default: ws://localhost:8765)",
    )
    args = parser.parse_args()

    WebhookHandler.pipecat_url = args.pipecat_url

    # Register webhook with VoiceBlender.
    webhook_url = f"http://localhost:{WEBHOOK_PORT}/webhook"
    status, resp = api("POST", "/v1/webhooks", {"url": webhook_url})
    if status == 201:
        log.info("Webhook registered: %s (id: %s)", webhook_url, resp.get("id"))
    else:
        log.warning("Webhook registration returned %d: %s", status, resp)

    server = HTTPServer(("0.0.0.0", WEBHOOK_PORT), WebhookHandler)
    log.info("Webhook listener on port %d, Pipecat URL: %s", WEBHOOK_PORT, args.pipecat_url)
    log.info("Waiting for incoming calls...")

    try:
        server.serve_forever()
    except KeyboardInterrupt:
        log.info("Shutting down")
        server.shutdown()


if __name__ == "__main__":
    main()
