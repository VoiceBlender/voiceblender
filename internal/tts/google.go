package tts

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"

	texttospeech "cloud.google.com/go/texttospeech/apiv1"
	"cloud.google.com/go/texttospeech/apiv1/texttospeechpb"
	"google.golang.org/api/option"
)

// Google implements Provider using Google Cloud Text-to-Speech.
type Google struct {
	log *slog.Logger
}

// NewGoogle creates a Google Cloud TTS provider.
func NewGoogle(log *slog.Logger) *Google {
	return &Google{log: log}
}

func (g *Google) Synthesize(ctx context.Context, text string, opts Options) (*Result, error) {
	var clientOpts []option.ClientOption
	if opts.APIKey != "" {
		clientOpts = append(clientOpts, option.WithAPIKey(opts.APIKey))
	}

	client, err := texttospeech.NewClient(ctx, clientOpts...)
	if err != nil {
		return nil, fmt.Errorf("google tts: create client: %w", err)
	}
	defer client.Close()

	voice := opts.Voice
	if voice == "" {
		voice = "en-US-Neural2-F"
	}

	// Use explicit language if provided; otherwise extract from voice name
	// (e.g. "en-US-Neural2-F" -> "en-US"). Gemini TTS voices like "Achernar"
	// don't embed a language code, so the caller must set Language explicitly.
	langCode := opts.Language
	if langCode == "" {
		langCode = extractLangCode(voice)
	}

	g.log.Debug("google tts synthesize", "voice", voice, "lang", langCode, "model", opts.ModelID, "text_len", len(text))

	voiceParams := &texttospeechpb.VoiceSelectionParams{
		LanguageCode: langCode,
		Name:         voice,
	}
	if opts.ModelID != "" {
		voiceParams.ModelName = opts.ModelID
	}

	input := &texttospeechpb.SynthesisInput{
		InputSource: &texttospeechpb.SynthesisInput_Text{
			Text: text,
		},
	}
	if opts.Prompt != "" {
		input.Prompt = &opts.Prompt
	}

	req := &texttospeechpb.SynthesizeSpeechRequest{
		Input: input,
		Voice: voiceParams,
		AudioConfig: &texttospeechpb.AudioConfig{
			AudioEncoding:   texttospeechpb.AudioEncoding_LINEAR16,
			SampleRateHertz: 16000,
		},
	}

	resp, err := client.SynthesizeSpeech(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("google tts: synthesize: %w", err)
	}

	audio := resp.AudioContent
	// LINEAR16 includes a 44-byte WAV header; strip it for raw PCM.
	if len(audio) > 44 {
		audio = audio[44:]
	}

	return &Result{
		Audio:    io.NopCloser(bytes.NewReader(audio)),
		MimeType: "audio/pcm;rate=16000",
	}, nil
}

// extractLangCode extracts "en-US" from voice names like "en-US-Neural2-F".
// Falls back to "en-US" if the format is unrecognized.
func extractLangCode(voice string) string {
	// Voice names follow the pattern: {lang}-{region}-{type}-{variant}
	// e.g. "en-US-Neural2-F", "de-DE-Standard-A", "ja-JP-WaveNet-B"
	// We need the first two segments: "en-US", "de-DE", "ja-JP".
	dashes := 0
	for i, c := range voice {
		if c == '-' {
			dashes++
			if dashes == 2 {
				return voice[:i]
			}
		}
	}
	return "en-US"
}
