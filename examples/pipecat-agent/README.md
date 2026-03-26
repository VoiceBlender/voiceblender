# Pipecat Agent Example

This example demonstrates how to use VoiceBlender's Pipecat agent provider to
connect a SIP call to a self-hosted [Pipecat](https://pipecat.ai) voice bot.

## Architecture

```
Caller (SIP phone)
    |
    v
VoiceBlender (SIP + audio mixing)
    |
    v  WebSocket (protobuf-encoded 16kHz PCM)
    |
Pipecat Bot (STT -> LLM -> TTS)
    |  uses Deepgram, OpenAI, ElevenLabs
```

1. A SIP call arrives at VoiceBlender
2. The call handler webhook answers the call
3. The handler attaches a Pipecat agent via `POST /v1/legs/{id}/agent`
   with `provider: "pipecat"` and `agent_id: "ws://localhost:8765"`
4. VoiceBlender streams the caller's audio to the Pipecat bot over WebSocket
5. The bot transcribes speech, generates an LLM response, synthesizes audio,
   and streams it back
6. VoiceBlender plays the bot's audio to the caller

## Prerequisites

```bash
# Python 3.10+
pip install "pipecat-ai[websocket,deepgram,openai,elevenlabs,silero]"
```

## Environment Variables

| Variable | Required | Description |
|----------|----------|-------------|
| `DEEPGRAM_API_KEY` | yes | Deepgram STT API key |
| `OPENAI_API_KEY` | yes | OpenAI LLM API key |
| `ELEVENLABS_API_KEY` | yes | ElevenLabs TTS API key |
| `VOICEBLENDER_URL` | no | VoiceBlender API URL (default: `http://localhost:8080`) |
| `WEBHOOK_PORT` | no | Webhook listener port (default: `5000`) |
| `PIPECAT_HOST` | no | Pipecat bot bind address (default: `0.0.0.0`) |
| `PIPECAT_PORT` | no | Pipecat bot port (default: `8765`) |

## Running

**Terminal 1** — Start VoiceBlender:
```bash
export WEBHOOK_URL=http://localhost:5000/webhook
./voiceblender
```

**Terminal 2** — Start the Pipecat bot:
```bash
export DEEPGRAM_API_KEY=...
export OPENAI_API_KEY=...
export ELEVENLABS_API_KEY=...
python3 bot.py
```

**Terminal 3** — Start the call handler:
```bash
python3 call_handler.py
```

**Terminal 4** — Make a test call:
```bash
# Using a SIP softphone pointed at VoiceBlender's SIP port
# Or use the VoiceBlender API to originate a call
curl -X POST http://localhost:8080/v1/legs \
  -H 'Content-Type: application/json' \
  -d '{"type": "sip", "uri": "sip:test@your-phone:5060"}'
```

## Files

- `bot.py` — Pipecat voice bot (STT + LLM + TTS pipeline)
- `call_handler.py` — Webhook handler that answers calls and attaches the Pipecat agent
