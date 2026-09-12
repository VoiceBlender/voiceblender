# Voice Agent TestOps Example

This example shows how to describe a repeatable pre-launch regression test for
a VoiceBlender-backed voice agent without requiring production calls, private
recordings, phone numbers, or customer data.

It is intentionally docs/example only. It does not change SIP, WebRTC, media,
agent runtime behavior, or provider credentials.

## What it validates

Voice Agent TestOps runs scripted customer turns against an agent or a saved
transcript, then checks the response for business-risk regressions such as:

- unsupported guarantees
- missing human confirmation
- missed callback details
- wrong handoff behavior
- latency or audio evidence drift when a live bridge is available

For VoiceBlender, the useful evidence already exists in the programmable
surface:

- `POST /v1/legs/{id}/agent/message`
- `POST /v1/rooms/{id}/agent/message`
- webhook or VSI events such as `agent.user_transcript`,
  `agent.agent_response`, `stt.text`, and `recording.finished`

## Safe transcript replay

Use this path when you only have a sanitized transcript or want to review the
test shape before wiring a local VoiceBlender session.

```bash
npx --yes voice-agent-testops@0.1.25 validate \
  --suite examples/voice-agent-testops/voiceblender-suite.json

npx --yes voice-agent-testops@0.1.25 run \
  --suite examples/voice-agent-testops/voiceblender-suite.json \
  --agent transcript \
  --transcript examples/voice-agent-testops/sanitized-transcript.txt \
  --report-locale en \
  --summary .voice-testops/voiceblender-summary.md
```

The bundled transcript is synthetic. Keep real pilot inputs private and replace
names, phone numbers, call IDs, recording URLs, and account identifiers with
placeholders such as `[PHONE]` and `[CALL_ID]`.

## Local VoiceBlender bridge shape

When a VoiceBlender dev instance is available, a minimal `/test-turn` bridge can
exercise the same suite against a running agent:

1. Start VoiceBlender locally and create a test room or connected test leg with
   an `app_id` such as `testops`.
2. Attach a supported agent provider to that room or leg.
3. Subscribe to `/v1/vsi?app_id=^testops$`, or receive the same events through a
   webhook listener.
4. For each incoming Voice Agent TestOps turn, call
   `/v1/rooms/{room_id}/agent/message` or
   `/v1/legs/{leg_id}/agent/message` with the scripted customer text.
5. Wait for the matching `agent.agent_response` event, optionally collect
   `agent.user_transcript`, `stt.text`, `recording.finished`, and timing data,
   then return a TestOps response:

```json
{
  "spoken": "I cannot guarantee that appointment. I can have a human confirm availability and call you back.",
  "summary": {
    "source": "phone",
    "intent": "handoff",
    "need": "Customer asked for a guaranteed appointment and callback.",
    "phone": "[PHONE]",
    "questions": ["Can you guarantee tomorrow afternoon?"],
    "level": "high",
    "nextAction": "Route to a human operator for confirmation.",
    "transcript": [
      {
        "role": "assistant",
        "text": "I cannot guarantee that appointment. I can have a human confirm availability and call you back.",
        "at": "2026-05-18T00:00:00.000Z"
      }
    ]
  },
  "audio": {
    "url": "file:///tmp/voiceblender-testops/[CALL_ID].wav",
    "label": "Sanitized local replay",
    "mimeType": "audio/wav"
  },
  "voiceMetrics": {
    "turnLatencyMs": 1200,
    "asrConfidence": 0.91
  }
}
```

Then run the same suite through the HTTP bridge:

```bash
npx --yes voice-agent-testops@0.1.25 doctor \
  --agent http \
  --endpoint http://127.0.0.1:4319/test-turn \
  --suite examples/voice-agent-testops/voiceblender-suite.json

npx --yes voice-agent-testops@0.1.25 run \
  --suite examples/voice-agent-testops/voiceblender-suite.json \
  --agent http \
  --endpoint http://127.0.0.1:4319/test-turn \
  --fail-on-severity critical
```

This keeps the first integration local and reviewable: no live customer call is
required, and no production credential or raw customer artifact needs to enter
the repository.

## Non-goals

- no production `/test-turn` endpoint in VoiceBlender
- no SIP, WebRTC, mixer, or provider runtime changes
- no committed provider API keys, phone numbers, or raw recording URLs
- no requirement to run a live telephone call before reviewing the test shape
