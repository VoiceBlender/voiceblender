//go:build integration

package integration

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/amd"
	"github.com/VoiceBlender/voiceblender/internal/audiofilter"
	"github.com/VoiceBlender/voiceblender/internal/audiofilter/denoise"
)

// TestAMD_NoisyAccuracy_Denoised answers the question the clean corpus cannot:
// how much of AMD's accuracy does denoise recover on noisy audio? Each file is
// pushed through the same push-mode chain the live AMD feed uses, then scored
// by the same analyzer, so the only difference from TestAMD_NoisyAccuracy is
// the filtering.
//
// Reports rather than asserts, like the other accuracy tests.
func TestAMD_NoisyAccuracy_Denoised(t *testing.T) {
	if err := denoise.Install(); err != nil {
		t.Skipf("denoise unavailable: %v", err)
	}
	t.Cleanup(func() { denoise.Shutdown() })

	params := amd.DefaultParams()
	params.TotalAnalysisTime = 10 * time.Second
	params.BeepTimeout = 10 * time.Second

	type row struct {
		corpus                 string
		clean, denoised, total int
	}
	var rows []row
	var any bool

	for _, name := range noisyHumanCorpora {
		dir := greetingsDir + "/" + name
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatalf("read dir %s: %v", dir, err)
		}
		any = true

		r := row{corpus: name}
		for _, e := range entries {
			if e.IsDir() || strings.ToLower(filepath.Ext(e.Name())) != ".wav" {
				continue
			}
			pcm, sr, err := decodeAudioFile(filepath.Join(dir, e.Name()))
			if err != nil {
				continue
			}
			if sr != 16000 {
				pcm = resample(pcm, sr, 16000)
			}
			r.total++

			if runAMD(t, params, pcmToBytes(pcm)) == amd.ResultHuman {
				r.clean++
			}
			if runAMD(t, params, denoiseBytes(t, pcmToBytes(pcm), 16000)) == amd.ResultHuman {
				r.denoised++
			}
		}
		rows = append(rows, r)
	}
	if !any {
		t.Skip("noisy corpora not found — run 'make gen-noisy-greetings' first")
	}

	t.Log("\n=== AMD on noisy human greetings: unfiltered vs denoised ===")
	t.Logf("%-26s %10s %10s", "corpus", "as-is", "denoised")
	var tc, td, tt int
	for _, r := range rows {
		t.Logf("%-26s %9.1f%% %9.1f%%", r.corpus,
			pct(r.clean, r.total), pct(r.denoised, r.total))
		tc, td, tt = tc+r.clean, td+r.denoised, tt+r.total
	}
	t.Logf("%-26s %9.1f%% %9.1f%%   (%d files)", "ALL", pct(tc, tt), pct(td, tt), tt)
}

func pct(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(n) / float64(total) * 100
}

func runAMD(t *testing.T, params amd.Params, pcm []byte) amd.Result {
	t.Helper()
	return amd.New(params).Run(context.Background(), bytes.NewReader(pcm)).Result
}

// denoiseBytes pushes audio through the same chain the live AMD feed builds.
func denoiseBytes(t *testing.T, pcm []byte, rate int) []byte {
	t.Helper()
	var out bytes.Buffer
	w, err := audiofilter.NewWriter(&out, rate, rate, []audiofilter.Spec{{Type: "denoise"}})
	if err != nil {
		t.Fatalf("build denoise chain: %v", err)
	}
	frame := rate / 50 * 2 // 20 ms
	for off := 0; off < len(pcm); off += frame {
		end := off + frame
		if end > len(pcm) {
			end = len(pcm)
		}
		if _, err := w.Write(pcm[off:end]); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	w.Close()
	return out.Bytes()
}
