package tts

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// Azure implements Provider using the Azure Cognitive Speech Services REST API.
type Azure struct {
	apiKey string
	region string
	client *http.Client
	log    *slog.Logger
}

// NewAzure creates an Azure TTS provider.
func NewAzure(apiKey, region string, log *slog.Logger) *Azure {
	return &Azure{
		apiKey: apiKey,
		region: region,
		client: &http.Client{},
		log:    log,
	}
}

func (a *Azure) Synthesize(ctx context.Context, text string, opts Options) (*Result, error) {
	apiKey := opts.APIKey
	if apiKey == "" {
		apiKey = a.apiKey
	}
	if apiKey == "" {
		return nil, fmt.Errorf("azure: no API key provided")
	}

	voice := opts.Voice
	if voice == "" {
		voice = "en-US-JennyNeural"
	}

	lang := opts.Language
	if lang == "" {
		lang = extractAzureLang(voice)
	}

	ssml := buildSSML(lang, voice, text)

	url := fmt.Sprintf("https://%s.tts.speech.microsoft.com/cognitiveservices/v1", a.region)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(ssml))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/ssml+xml")
	req.Header.Set("X-Microsoft-OutputFormat", "raw-16khz-16bit-mono-pcm")
	req.Header.Set("Ocp-Apim-Subscription-Key", apiKey)
	req.Header.Set("User-Agent", "voiceblender")

	a.log.Debug("azure synthesize request",
		"url", url,
		"region", a.region,
		"voice", voice,
		"lang", lang,
		"text_len", len(text),
		"ssml_len", len(ssml),
		"ssml", ssml,
	)

	resp, err := a.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("azure request: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		a.log.Error("azure synthesize failed",
			"status", resp.StatusCode,
			"url", url,
			"region", a.region,
			"voice", voice,
			"lang", lang,
			"body", string(errBody),
			"content_type", resp.Header.Get("Content-Type"),
			"ms_error_code", resp.Header.Get("X-Microsoft-Error-Code"),
			"ms_error_msg", resp.Header.Get("X-Microsoft-Error-Message"),
			"ms_tts_error", resp.Header.Get("X-Microsoft-Tts-Error"),
			"ssml", ssml,
		)
		return nil, fmt.Errorf("azure: status %d: body=%q ms-error-code=%q ms-error-message=%q",
			resp.StatusCode,
			string(errBody),
			resp.Header.Get("X-Microsoft-Error-Code"),
			resp.Header.Get("X-Microsoft-Error-Message"),
		)
	}

	return &Result{
		Audio:    resp.Body,
		MimeType: "audio/pcm;rate=16000",
	}, nil
}

// extractAzureLang extracts the language code from an Azure voice name.
// e.g. "en-US-JennyNeural" -> "en-US", "pl-PL-MarekNeural" -> "pl-PL".
func extractAzureLang(voice string) string {
	parts := strings.SplitN(voice, "-", 3)
	if len(parts) >= 2 {
		return parts[0] + "-" + parts[1]
	}
	return "en-US"
}

// buildSSML constructs an SSML document for Azure TTS.
func buildSSML(lang, voice, text string) string {
	var buf bytes.Buffer
	buf.WriteString(`<speak version='1.0' xmlns='http://www.w3.org/2001/10/synthesis' xml:lang='`)
	xmlEscape(&buf, lang)
	buf.WriteString(`'><voice name='`)
	xmlEscape(&buf, voice)
	buf.WriteString(`'>`)
	xmlEscape(&buf, text)
	buf.WriteString(`</voice></speak>`)
	return buf.String()
}

// xmlEscape writes s to buf with XML special characters escaped.
func xmlEscape(buf *bytes.Buffer, s string) {
	for _, r := range s {
		switch r {
		case '&':
			buf.WriteString("&amp;")
		case '<':
			buf.WriteString("&lt;")
		case '>':
			buf.WriteString("&gt;")
		case '"':
			buf.WriteString("&quot;")
		case '\'':
			buf.WriteString("&apos;")
		default:
			buf.WriteRune(r)
		}
	}
}
