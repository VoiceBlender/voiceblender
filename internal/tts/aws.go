package tts

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/polly"
	"github.com/aws/aws-sdk-go-v2/service/polly/types"
)

// AWS implements Provider using Amazon Polly.
type AWS struct {
	region string
	log    *slog.Logger
}

// AWSConfig holds optional AWS credentials for per-request overrides.
type AWSConfig struct {
	Region    string
	AccessKey string
	SecretKey string
}

// NewAWS creates an AWS Polly TTS provider.
func NewAWS(region string, log *slog.Logger) *AWS {
	if region == "" {
		region = "us-east-1"
	}
	return &AWS{region: region, log: log}
}

func (a *AWS) Synthesize(ctx context.Context, text string, opts Options) (*Result, error) {
	client, err := a.buildClient(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("polly: build client: %w", err)
	}

	voiceID := opts.Voice
	if voiceID == "" {
		voiceID = "Joanna"
	}

	engine := types.EngineNeural
	if opts.ModelID != "" {
		switch opts.ModelID {
		case "standard":
			engine = types.EngineStandard
		case "neural":
			engine = types.EngineNeural
		case "long-form":
			engine = types.EngineLongForm
		case "generative":
			engine = types.EngineGenerative
		}
	}

	a.log.Debug("polly synthesize", "voice", voiceID, "engine", engine, "text_len", len(text))

	output, err := client.SynthesizeSpeech(ctx, &polly.SynthesizeSpeechInput{
		Text:         aws.String(text),
		VoiceId:      types.VoiceId(voiceID),
		Engine:       engine,
		OutputFormat: types.OutputFormatPcm,
		SampleRate:   aws.String("16000"),
	})
	if err != nil {
		return nil, fmt.Errorf("polly: synthesize: %w", err)
	}

	return &Result{
		Audio:    output.AudioStream,
		MimeType: "audio/pcm;rate=16000",
	}, nil
}

func (a *AWS) buildClient(ctx context.Context, opts Options) (*polly.Client, error) {
	// If per-request credentials are provided via APIKey (format: "accessKey:secretKey"),
	// use them. Otherwise fall back to default AWS credential chain.
	var cfgOpts []func(*config.LoadOptions) error
	cfgOpts = append(cfgOpts, config.WithRegion(a.region))

	if opts.APIKey != "" {
		accessKey, secretKey, ok := parseAWSCredentials(opts.APIKey)
		if !ok {
			return nil, fmt.Errorf("polly: invalid api_key format, expected 'ACCESS_KEY:SECRET_KEY'")
		}
		cfgOpts = append(cfgOpts, config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
		))
	}

	cfg, err := config.LoadDefaultConfig(ctx, cfgOpts...)
	if err != nil {
		return nil, err
	}

	return polly.NewFromConfig(cfg), nil
}

// parseAWSCredentials splits "accessKey:secretKey" into its parts.
func parseAWSCredentials(apiKey string) (accessKey, secretKey string, ok bool) {
	for i := 0; i < len(apiKey); i++ {
		if apiKey[i] == ':' {
			if i > 0 && i < len(apiKey)-1 {
				return apiKey[:i], apiKey[i+1:], true
			}
			return "", "", false
		}
	}
	return "", "", false
}

// audioStreamCloser wraps the Polly audio stream to implement io.ReadCloser.
type audioStreamCloser struct {
	io.ReadCloser
}
