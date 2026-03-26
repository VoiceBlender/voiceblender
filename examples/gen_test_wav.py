#!/usr/bin/env python3
"""
Generate test WAV files for debugging VoiceBlender playback.

Usage:
    python3 gen_test_wav.py                    # default: 440Hz sine, PCM, 8kHz, mono
    python3 gen_test_wav.py --freq 1000        # 1kHz tone
    python3 gen_test_wav.py --format ulaw      # mu-law encoded
    python3 gen_test_wav.py --channels 2       # stereo
    python3 gen_test_wav.py --rate 16000       # 16kHz sample rate
    python3 gen_test_wav.py --duration 5       # 5 seconds
"""

import argparse
import audioop
import math
import struct
import sys
import wave


def generate_sine(freq, sample_rate, duration, amplitude=0.8):
    """Generate a sine wave as 16-bit PCM samples."""
    num_samples = int(sample_rate * duration)
    samples = []
    for i in range(num_samples):
        t = i / sample_rate
        val = int(amplitude * 32767 * math.sin(2 * math.pi * freq * t))
        samples.append(max(-32768, min(32767, val)))
    return struct.pack(f"<{len(samples)}h", *samples)


def main():
    parser = argparse.ArgumentParser(description="Generate test WAV files")
    parser.add_argument("--freq", type=float, default=440.0, help="Tone frequency in Hz (default: 440)")
    parser.add_argument("--rate", type=int, default=8000, help="Sample rate (default: 8000)")
    parser.add_argument("--duration", type=float, default=3.0, help="Duration in seconds (default: 3)")
    parser.add_argument("--channels", type=int, default=1, choices=[1, 2], help="Channels (default: 1)")
    parser.add_argument("--format", choices=["pcm", "ulaw", "alaw"], default="pcm", help="Audio format (default: pcm)")
    parser.add_argument("--output", "-o", default=None, help="Output filename (auto-generated if not set)")
    args = parser.parse_args()

    pcm_data = generate_sine(args.freq, args.rate, args.duration)

    if args.channels == 2:
        # Duplicate mono to stereo (same audio in both channels)
        stereo = b""
        for i in range(0, len(pcm_data), 2):
            sample = pcm_data[i:i+2]
            stereo += sample + sample
        pcm_data = stereo

    output = args.output
    if output is None:
        output = f"test_{args.format}_{args.rate}hz_{args.channels}ch_{int(args.freq)}hz_tone.wav"

    fmt_map = {"pcm": 1, "ulaw": 7, "alaw": 6}
    fmt_code = fmt_map[args.format]

    if args.format == "pcm":
        # Standard PCM WAV via wave module
        with wave.open(output, "wb") as wf:
            wf.setnchannels(args.channels)
            wf.setsampwidth(2)  # 16-bit
            wf.setframerate(args.rate)
            wf.writeframes(pcm_data)
    else:
        # For ulaw/alaw, we need to encode and write a custom WAV header
        if args.format == "ulaw":
            encoded = audioop.lin2ulaw(pcm_data, 2)
        else:
            encoded = audioop.lin2alaw(pcm_data, 2)

        # If stereo, encoded data has 1 byte per channel per frame
        bits_per_sample = 8
        block_align = args.channels * bits_per_sample // 8
        byte_rate = args.rate * block_align
        data_size = len(encoded)

        with open(output, "wb") as f:
            # RIFF header
            f.write(b"RIFF")
            f.write(struct.pack("<I", 36 + data_size))
            f.write(b"WAVE")

            # fmt chunk (18 bytes for non-PCM to include cbSize)
            f.write(b"fmt ")
            f.write(struct.pack("<I", 18))  # chunk size
            f.write(struct.pack("<H", fmt_code))  # format
            f.write(struct.pack("<H", args.channels))
            f.write(struct.pack("<I", args.rate))
            f.write(struct.pack("<I", byte_rate))
            f.write(struct.pack("<H", block_align))
            f.write(struct.pack("<H", bits_per_sample))
            f.write(struct.pack("<H", 0))  # cbSize

            # fact chunk (required for non-PCM)
            num_samples = len(encoded) // args.channels
            f.write(b"fact")
            f.write(struct.pack("<I", 4))
            f.write(struct.pack("<I", num_samples))

            # data chunk
            f.write(b"data")
            f.write(struct.pack("<I", data_size))
            f.write(encoded)

    print(f"Generated: {output}")
    print(f"  Format:     {args.format} (code {fmt_code})")
    print(f"  Rate:       {args.rate} Hz")
    print(f"  Channels:   {args.channels}")
    print(f"  Duration:   {args.duration}s")
    print(f"  Frequency:  {args.freq} Hz")
    print(f"  File size:  {len(open(output, 'rb').read())} bytes")


if __name__ == "__main__":
    main()
