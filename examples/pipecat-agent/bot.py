#!/usr/bin/env python3
"""
Pipecat voice bot for VoiceBlender.

Runs a WebSocket server that accepts protobuf-encoded 16kHz PCM audio,
processes it through Deepgram STT -> OpenAI LLM -> ElevenLabs TTS,
and streams synthesized audio back.

Usage:
    export DEEPGRAM_API_KEY=...
    export OPENAI_API_KEY=...
    export ELEVENLABS_API_KEY=...
    python3 bot.py

Environment variables:
    PIPECAT_HOST       Bind address (default: 0.0.0.0)
    PIPECAT_PORT       Bind port (default: 8765)
    DEEPGRAM_API_KEY   Deepgram STT API key
    OPENAI_API_KEY     OpenAI LLM API key
    ELEVENLABS_API_KEY ElevenLabs TTS API key
    SYSTEM_PROMPT      Custom system prompt for the LLM
"""

import asyncio
import os
import sys

from loguru import logger

from pipecat.audio.vad.silero import SileroVADAnalyzer
from pipecat.pipeline.pipeline import Pipeline
from pipecat.pipeline.runner import PipelineRunner
from pipecat.pipeline.task import PipelineParams, PipelineTask
from pipecat.processors.aggregators.llm_response_universal import (
    LLMContextAggregatorPair,
    LLMUserAggregatorParams,
)
from pipecat.processors.aggregators.llm_context import LLMContext
from pipecat.serializers.protobuf import ProtobufFrameSerializer
from pipecat.services.deepgram.stt import DeepgramSTTService
from pipecat.services.elevenlabs.tts import ElevenLabsTTSService
from pipecat.services.openai.llm import OpenAILLMService
from pipecat.transports.websocket.server import (
    WebsocketServerParams,
    WebsocketServerTransport,
)

DEFAULT_SYSTEM_PROMPT = (
    "You are a friendly and helpful voice assistant connected through a phone call. "
    "Keep your responses brief and conversational — one or two sentences is ideal. "
    "Your output will be converted to speech, so avoid markdown, bullet points, "
    "code blocks, and special characters. Speak naturally as if you're having a "
    "phone conversation."
)


async def main():
    host = os.getenv("PIPECAT_HOST", "0.0.0.0")
    port = int(os.getenv("PIPECAT_PORT", "8765"))
    system_prompt = os.getenv("SYSTEM_PROMPT", DEFAULT_SYSTEM_PROMPT)

    # Validate API keys.
    for key in ("DEEPGRAM_API_KEY", "OPENAI_API_KEY", "ELEVENLABS_API_KEY"):
        if not os.getenv(key):
            logger.error(f"Missing required environment variable: {key}")
            sys.exit(1)

    # --- Transport ---
    # VoiceBlender sends/receives 16kHz 16-bit mono PCM via protobuf frames.
    transport = WebsocketServerTransport(
        host=host,
        port=port,
        params=WebsocketServerParams(
            serializer=ProtobufFrameSerializer(),
            audio_in_enabled=True,
            audio_out_enabled=True,
            audio_in_sample_rate=16000,
            audio_out_sample_rate=16000,
            audio_in_channels=1,
            audio_out_channels=1,
            add_wav_header=False,
            session_timeout=300,
        ),
    )

    # --- STT (Deepgram) ---
    stt = DeepgramSTTService(api_key=os.getenv("DEEPGRAM_API_KEY"))

    # --- LLM (OpenAI) ---
    llm = OpenAILLMService(
        api_key=os.getenv("OPENAI_API_KEY"),
        model="gpt-4o-mini",
    )

    # --- TTS (ElevenLabs) ---
    tts = ElevenLabsTTSService(
        api_key=os.getenv("ELEVENLABS_API_KEY"),
        voice_id="pNInz6obpgDQGcFmaJgB",  # "Adam"
    )

    # --- Conversation context ---
    context = LLMContext(
        messages=[
            {"role": "system", "content": system_prompt},
        ]
    )
    user_aggregator, assistant_aggregator = LLMContextAggregatorPair(
        context,
        user_params=LLMUserAggregatorParams(
            vad_analyzer=SileroVADAnalyzer(),
        ),
    )

    # --- Pipeline ---
    # Audio in -> STT -> context aggregator -> LLM -> TTS -> Audio out
    pipeline = Pipeline(
        [
            transport.input(),
            stt,
            user_aggregator,
            llm,
            tts,
            transport.output(),
            assistant_aggregator,
        ]
    )

    task = PipelineTask(
        pipeline,
        params=PipelineParams(
            enable_metrics=True,
            enable_usage_metrics=True,
        ),
    )

    # --- Event handlers ---
    @transport.event_handler("on_client_connected")
    async def on_client_connected(transport, client):
        logger.info(f"Client connected: {client.remote_address}")

    @transport.event_handler("on_client_disconnected")
    async def on_client_disconnected(transport, client):
        logger.info(f"Client disconnected: {client.remote_address}")
        await task.cancel()

    @transport.event_handler("on_session_timeout")
    async def on_session_timeout(transport, client):
        logger.warning(f"Session timeout: {client.remote_address}")
        await task.cancel()

    # --- Run ---
    logger.info(f"Pipecat bot listening on ws://{host}:{port}")
    runner = PipelineRunner()
    await runner.run(task)


if __name__ == "__main__":
    asyncio.run(main())
