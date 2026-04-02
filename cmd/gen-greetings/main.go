// Command gen-greetings uses the ElevenLabs TTS API to generate short human
// greeting WAV files for AMD false-positive testing.
//
// Usage:
//
//	ELEVENLABS_API_KEY=sk-... go run ./cmd/gen-greetings -out tests/data/greetings/human
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	baseURL       = "https://api.elevenlabs.io/v1/text-to-speech"
	defaultModel  = "eleven_multilingual_v2"
	defaultVoice  = "JBFqnCBsd6RMkjVDRZzb" // "George" — clear male voice
	sampleRate    = 16000
	bitsPerSample = 16
	numChannels   = 1
)

// Greetings to generate. Short, natural human phone answers in multiple
// languages. The ElevenLabs eleven_multilingual_v2 model auto-detects the
// language from the text.
var greetings = []struct {
	filename string
	text     string
}{
	// English
	{"en_hello.wav", "Hello?"},
	{"en_hi_there.wav", "Hi there."},
	{"en_yeah.wav", "Yeah?"},
	{"en_this_is_john.wav", "Hi, this is John."},
	{"en_speaking.wav", "Speaking."},
	{"en_good_morning.wav", "Good morning."},
	{"en_who_is_this.wav", "Hi, who is this?"},
	{"en_hey_whats_up.wav", "Hey, what's up?"},

	// Spanish
	{"es_hola.wav", "¿Hola?"},
	{"es_digame.wav", "Dígame."},
	{"es_buenas_tardes.wav", "Buenas tardes."},
	{"es_quien_habla.wav", "¿Quién habla?"},

	// French
	{"fr_allo.wav", "Allô?"},
	{"fr_oui_bonjour.wav", "Oui, bonjour."},
	{"fr_qui_est_a_lappareil.wav", "Qui est à l'appareil?"},
	{"fr_je_vous_ecoute.wav", "Je vous écoute."},

	// German
	{"de_hallo.wav", "Hallo?"},
	{"de_ja_bitte.wav", "Ja, bitte?"},
	{"de_guten_tag.wav", "Guten Tag."},
	{"de_wer_spricht.wav", "Wer spricht da?"},

	// Polish
	{"pl_halo.wav", "Halo?"},
	{"pl_slucham.wav", "Słucham."},
	{"pl_tak_slucham.wav", "Tak, słucham?"},
	{"pl_dzien_dobry.wav", "Dzień dobry."},
	{"pl_kto_mowi.wav", "Kto mówi?"},
	{"pl_prosze.wav", "Proszę."},

	// Portuguese
	{"pt_alo.wav", "Alô?"},
	{"pt_boa_tarde.wav", "Boa tarde."},
	{"pt_quem_fala.wav", "Quem fala?"},
	{"pt_pois_nao.wav", "Pois não?"},

	// Italian
	{"it_pronto.wav", "Pronto?"},
	{"it_chi_parla.wav", "Chi parla?"},
	{"it_buongiorno.wav", "Buongiorno."},
	{"it_si_dica.wav", "Sì, dica."},

	// Japanese
	{"ja_moshi_moshi.wav", "もしもし?"},
	{"ja_hai.wav", "はい。"},
	{"ja_hai_donata.wav", "はい、どなたですか?"},

	// Hindi
	{"hi_hello.wav", "हैलो?"},
	{"hi_haanji.wav", "हाँजी, बोलिए।"},
	{"hi_kaun_bol_raha.wav", "कौन बोल रहा है?"},

	// Chinese (Mandarin)
	{"zh_wei.wav", "喂？"},
	{"zh_ni_hao.wav", "你好。"},
	{"zh_qing_wen.wav", "请问哪位？"},

	// Arabic
	{"ar_alo.wav", "ألو؟"},
	{"ar_marhaba.wav", "مرحبا."},
	{"ar_min_yatakallam.wav", "من يتكلم؟"},
}

func main() {
	outDir := flag.String("out", "tests/data/greetings/human", "output directory for WAV files")
	voice := flag.String("voice", defaultVoice, "ElevenLabs voice ID")
	model := flag.String("model", defaultModel, "ElevenLabs model ID")
	silenceSec := flag.Int("trailing-silence", 3, "seconds of trailing silence to append (simulates real call)")
	flag.Parse()

	apiKey := os.Getenv("ELEVENLABS_API_KEY")
	if apiKey == "" {
		log.Fatal("ELEVENLABS_API_KEY environment variable is required")
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		log.Fatalf("create output dir: %v", err)
	}

	client := &http.Client{Timeout: 30 * time.Second}

	var generated int
	for _, g := range greetings {
		path := filepath.Join(*outDir, g.filename)
		fmt.Printf("%-30s → %s ... ", g.text, g.filename)

		pcm, err := synthesize(client, apiKey, *voice, *model, g.text)
		if err != nil {
			fmt.Printf("ERROR: %v\n", err)
			continue
		}

		// Append trailing silence to simulate a real call where silence
		// continues after the person speaks.
		if *silenceSec > 0 {
			silenceBytes := *silenceSec * sampleRate * numChannels * (bitsPerSample / 8)
			pcm = append(pcm, make([]byte, silenceBytes)...)
		}

		if err := writeWAV(path, pcm); err != nil {
			fmt.Printf("ERROR: %v\n", err)
			continue
		}

		durationMs := len(pcm) / (sampleRate * numChannels * bitsPerSample / 8) * 1000
		fmt.Printf("OK (%d ms, %d bytes)\n", durationMs, len(pcm))
		generated++
	}

	fmt.Printf("\nGenerated %d/%d greetings in %s\n", generated, len(greetings), *outDir)
}

func synthesize(client *http.Client, apiKey, voiceID, modelID, text string) ([]byte, error) {
	url := fmt.Sprintf("%s/%s/stream?output_format=pcm_16000", baseURL, voiceID)

	body := fmt.Sprintf(`{"text":%q,"model_id":%q}`, text, modelID)
	req, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("xi-api-key", apiKey)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, errBody)
	}

	return io.ReadAll(resp.Body)
}

func writeWAV(path string, pcmData []byte) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	dataSize := uint32(len(pcmData))
	byteRate := uint32(sampleRate * numChannels * bitsPerSample / 8)
	blockAlign := uint16(numChannels * bitsPerSample / 8)

	// RIFF header
	f.Write([]byte("RIFF"))
	binary.Write(f, binary.LittleEndian, uint32(36+dataSize))
	f.Write([]byte("WAVE"))

	// fmt chunk
	f.Write([]byte("fmt "))
	binary.Write(f, binary.LittleEndian, uint32(16)) // chunk size
	binary.Write(f, binary.LittleEndian, uint16(1))  // PCM format
	binary.Write(f, binary.LittleEndian, uint16(numChannels))
	binary.Write(f, binary.LittleEndian, uint32(sampleRate))
	binary.Write(f, binary.LittleEndian, byteRate)
	binary.Write(f, binary.LittleEndian, blockAlign)
	binary.Write(f, binary.LittleEndian, uint16(bitsPerSample))

	// data chunk
	f.Write([]byte("data"))
	binary.Write(f, binary.LittleEndian, dataSize)
	_, err = f.Write(pcmData)
	return err
}
