// wavdump downloads a WAV file and decodes it using the same pipeline
// as VoiceBlender's playback, writing the raw PCM output to a file.
// This lets you verify the decoded audio independently of diago/RTP.
//
// Usage:
//
//	go run ./cmd/wavdump -url https://example.com/file.wav -out decoded.raw -rate 8000
//	aplay -r 8000 -f S16_LE -c 1 decoded.raw    # play 8kHz output
//	aplay -r 16000 -f S16_LE -c 1 decoded.raw    # play 16kHz output
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/csiwek/VoiceBlender/internal/playback"
)

func main() {
	url := flag.String("url", "", "URL of WAV file to decode")
	outFile := flag.String("out", "decoded.raw", "Output raw PCM file")
	rate := flag.Int("rate", 8000, "Target sample rate (8000 or 16000)")
	flag.Parse()

	if *url == "" {
		fmt.Fprintln(os.Stderr, "Usage: wavdump -url <WAV_URL> [-out decoded.raw] [-rate 8000]")
		os.Exit(1)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	p := playback.NewPlayer(log)

	f, err := os.Create(*outFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create output: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	ctx := context.Background()
	if *rate == 16000 {
		err = p.Play(ctx, f, *url, "audio/wav", 1)
	} else {
		err = p.PlayAt8kHz(ctx, f, *url, "audio/wav", 1)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "playback error: %v\n", err)
		os.Exit(1)
	}

	info, _ := f.Stat()
	fmt.Fprintf(os.Stderr, "Written %d bytes to %s\n", info.Size(), *outFile)
	fmt.Fprintf(os.Stderr, "Play with: aplay -r %d -f S16_LE -c 1 %s\n", *rate, *outFile)
}
