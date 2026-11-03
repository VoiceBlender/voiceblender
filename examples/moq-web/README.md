# VoiceBlender MoQ — browser demo

A minimal MoQ (Media over QUIC) client that talks to VoiceBlender's
`CONNECT /v1/legs/moq` endpoint from a browser via WebTransport, built on
the [`moqt`](https://github.com/MoQ-it/moqt) TypeScript client. Bidirectional Opus audio: mic captured and published
on the `mic` namespace, room mix subscribed on the `mix` namespace.

## Status: experimental / PoC

- Speaks IETF **draft-ietf-moq-transport-22** (WebTransport protocol
  `moqt-22`), the same draft as the server (`MoQ-it/moqt`).
- Tested with Chrome. Other browsers need WebTransport plus WebCodecs
  Opus encode/decode; they have not been tried with this page.
- Files served from `file://` cannot use WebTransport (Chrome requires
  a secure context). Serve them over HTTP/HTTPS (instructions below).

## Files

| File | What it does |
| ---- | ------------ |
| `vendor/moqt.js` | The `moqt` browser client (draft-22), bundled from its TypeScript sources. Generated — rebuild with `make moq-web-client`, which also refreshes `vendor/LICENSE` (MIT). |
| `moq-session.js` | Drives one MoQ session: SETUP, PUBLISH_NAMESPACE mic, SUBSCRIBE mix, accept the server's SUBSCRIBE for our mic. |
| `audio.js`       | `MicCapture` (mic → WebCodecs Opus → publish) and `Speaker` (incoming Opus → WebCodecs decode → AudioContext). |
| `main.js`        | Wires the UI to the session + audio modules. |
| `index.html`     | Two inputs, two buttons, a log pane. |

## Prerequisites

### 1. Generate a dev cert

The cert **must be ECDSA P-256** with a validity window **≤ 14 days** for
Chrome's WebTransport `serverCertificateHashes` path. RSA certs are
rejected even though they're otherwise valid TLS.

```bash
mkdir -p certs && cd certs
openssl ecparam -name prime256v1 -genkey -noout -out moq-dev.key
chmod 600 moq-dev.key
openssl req -new -x509 -key moq-dev.key -out moq-dev.crt -days 14 \
  -subj "/CN=localhost" \
  -addext "subjectAltName=DNS:localhost,IP:127.0.0.1"
cd ..
```

Then compute the cert's SHA-256 DER fingerprint (hex form) — the
demo page needs it to bypass trust-store checks:

```bash
openssl x509 -in certs/moq-dev.crt -outform DER \
  | openssl dgst -sha256 -hex | awk '{print $2}'
```

Paste that hex string into the demo's "Server cert SHA-256 (hex)"
field. The default field value matches whatever cert is currently in
`certs/` at the time this README was written; regenerate ⇒ re-paste.

### 2. Start VoiceBlender with MoQ enabled

```bash
MOQ_ENABLED=true \
MOQ_LISTEN_ADDR=:8443 \
MOQ_TLS_CERT_FILE=$PWD/certs/moq-dev.crt \
MOQ_TLS_KEY_FILE=$PWD/certs/moq-dev.key \
go run ./cmd/voiceblender
```

### 3. Serve the static files

WebTransport requires the page itself to load over `http://localhost`
or `https://` (a "secure context"). The simplest setup:

```bash
cd examples/moq-web
python3 -m http.server 8000
# then open http://localhost:8000/
```

If you use a different port or host, that's fine — the server has
`CheckOrigin` set to permissive for the MoQ listener.

## Using the demo

1. Open `http://localhost:8000/` (or wherever you served the files).
2. Confirm the endpoint URL — it defaults to
   `https://localhost:8443/v1/legs/moq`. Adjust if your
   `MOQ_LISTEN_ADDR` is different.
3. Confirm the cert SHA-256 hash matches your current cert (see step
   1 above to recompute).
4. Pick a room ID. Any other VoiceBlender leg in the same room will
   exchange audio with you (try connecting a WebSocket leg or playing
   TTS into the room via `POST /v1/rooms/{id}/tts`).
5. Click **Connect**. Grant mic permission. You should see:
   - `SETUP done, protocol moqt-22`
   - `server published namespace mix`
   - `PUBLISH_NAMESPACE mic accepted` and `server subscribed to mic`
   - `SUBSCRIBE mix ok, track alias 0`
   - `server subscribed to mic — starting capture`, after which the
     rx/tx counters climb at about 50 objects/s each.
6. **Disconnect** stops the mic, decoder, and the WebTransport session,
   which triggers `leg.disconnected` on the server side.

## Hearing real audio

A single MoQ leg in an otherwise-empty room receives mixed-minus-self
silence (Opus DTX frames). To verify audio actually flows, drive
content into the room from a second source:

```bash
# Play a tone into the room.
curl -XPOST 'http://localhost:8080/v1/rooms/moq-demo/play' \
  -H content-type:application/json \
  -d '{"url":"file:///path/to/test.wav"}'

# Or run text-to-speech into the room (needs ELEVENLABS_API_KEY etc.):
curl -XPOST 'http://localhost:8080/v1/rooms/moq-demo/tts' \
  -H content-type:application/json \
  -d '{"text":"hello from voiceblender"}'
```

If you connect a second WebSocket leg to the same room, you should
hear that leg's audio through the browser, and that leg should hear
your mic.

## Troubleshooting

- **"WebTransport not supported"** — the browser lacks WebTransport;
  use a current Chrome or Edge.
- **"WebCodecs not supported"** — newer browser needed, or you're not on
  a secure context (HTTPS or localhost).
- **"Opening handshake failed" / `net::ERR_QUIC_PROTOCOL_ERROR`** —
  cert hash mismatch. Re-run the openssl one-liner from step 1 to
  recompute the hash and paste it into the cert-hash field. Also
  verify the cert is **ECDSA P-256** (RSA certs are rejected by
  Chrome's WebTransport, even with a correct hash).
- **`unknown track`** (REQUEST_ERROR, DOES_NOT_EXIST) — the server
  didn't recognise the namespace/track name. The demo hard-codes `mix`/`audio` (downlink)
  and `mic`/`audio` (uplink); make sure you're running the matching
  server version with `internal/moqmedia/transport.go` from this repo.
- **Handshake succeeds, no objects** — try the curl-into-room
  instructions above to push real audio through the mixer. An empty
  room just produces silence frames (still valid, just inaudible).
- **Connect button disabled** — the page reports the missing feature
  (WebTransport or WebCodecs) in the log pane.
- **`server negotiated …, not moqt-22`** — the endpoint is not a
  draft-22 MoQ server, e.g. an older VoiceBlender build.

## Known sharp edges

- The audio module uses the deprecated `ScriptProcessorNode`. Suitable
  for a PoC; for production migrate to `AudioWorklet`.
- The decoder's clock-drift handling is naive (~50 ms playhead
  cushion). You'll hear pops if the connection stalls; this is a
  PoC-grade decoder, not a jitter buffer.
- Multi-stream subgroup framing is not coalesced: every published Opus
  frame opens a fresh unidirectional stream. Cheap on QUIC but wasteful
  vs. a sustained group.
