//go:build integration

package integration

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/VoiceBlender/voiceblender/internal/amd"
	"github.com/VoiceBlender/voiceblender/internal/audiofilter/denoise"
)

// TestAMD_SNRSweep reports AMD accuracy against signal-to-noise ratio, so the
// point where detection starts failing can be located rather than guessed. It
// is the tool to re-run whenever the AMD parameters or the denoise model
// change, since both move that point.
//
// Skipped unless SWEEP_DIR names a corpus tree, which is built with:
//
//	go run ./cmd/gen-noisy-greetings -out /tmp/sweep \
//	    -snr "30,28,26,24,22,20,18,16,15,10,5,0,-5,-10"
//	SWEEP_DIR=/tmp/sweep go test -tags integration ./tests/integration/ \
//	    -run TestAMD_SNRSweep -v
//
// Measured on 46 human greetings per point, so one file is 2.2% and small
// non-monotonic wobbles between adjacent SNRs are sampling noise.
func TestAMD_SNRSweep(t *testing.T) {
	root := os.Getenv("SWEEP_DIR")
	if root == "" {
		t.Skip("set SWEEP_DIR to a corpus tree built by cmd/gen-noisy-greetings")
	}
	if err := denoise.Install(); err != nil {
		t.Fatalf("install denoise: %v", err)
	}
	t.Cleanup(func() { denoise.Shutdown() })

	params := amd.DefaultParams()
	params.TotalAnalysisTime = 10 * time.Second
	params.BeepTimeout = 10 * time.Second

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}

	type key struct {
		noise string
		snr   int
	}
	type row struct {
		clean, denoised, total int
		verdicts               map[amd.Result]int
	}
	results := map[key]row{}
	noises := map[string]bool{}
	snrs := map[int]bool{}

	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "human-") {
			continue
		}
		name := strings.TrimPrefix(e.Name(), "human-")
		if !strings.HasSuffix(name, "db") {
			continue
		}
		// Scan the SNR off the end rather than splitting on the last "-": a
		// negative SNR puts a second "-" there ("human-office--10db").
		body := strings.TrimSuffix(name, "db")
		j := len(body)
		for j > 0 && body[j-1] >= '0' && body[j-1] <= '9' {
			j--
		}
		if j > 0 && body[j-1] == '-' && j-1 > 0 && body[j-2] == '-' {
			j--
		}
		if j == len(body) || j == 0 {
			continue
		}
		noise := strings.TrimSuffix(body[:j], "-")
		snr, err := strconv.Atoi(body[j:])
		if err != nil {
			continue
		}
		noises[noise] = true
		snrs[snr] = true

		r := row{verdicts: map[amd.Result]int{}}
		files, _ := os.ReadDir(filepath.Join(root, e.Name()))
		for _, f := range files {
			if f.IsDir() || strings.ToLower(filepath.Ext(f.Name())) != ".wav" {
				continue
			}
			pcm, sr, err := decodeAudioFile(filepath.Join(root, e.Name(), f.Name()))
			if err != nil {
				continue
			}
			if sr != 16000 {
				pcm = resample(pcm, sr, 16000)
			}
			r.total++
			raw := pcmToBytes(pcm)
			got := runAMD(t, params, raw)
			r.verdicts[got]++
			if got == amd.ResultHuman {
				r.clean++
			}
			if runAMD(t, params, denoiseBytes(t, raw, 16000)) == amd.ResultHuman {
				r.denoised++
			}
		}
		results[key{noise, snr}] = r
	}

	var noiseList []string
	for n := range noises {
		noiseList = append(noiseList, n)
	}
	sort.Strings(noiseList)
	var snrList []int
	for s := range snrs {
		snrList = append(snrList, s)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(snrList)))

	for _, n := range noiseList {
		t.Logf("\n=== %s: AMD accuracy vs SNR (human greetings, all should read human) ===", n)
		t.Logf("%6s %12s %12s", "SNR", "as-is", "denoised")
		for _, s := range snrList {
			r, ok := results[key{n, s}]
			if !ok || r.total == 0 {
				continue
			}
			t.Logf("%4d dB %11.1f%% %11.1f%%   unfiltered verdicts: %s",
				s, pct(r.clean, r.total), pct(r.denoised, r.total), verdictBreakdown(r.verdicts))
		}
	}
}

// verdictBreakdown shows what AMD said when it was not "human", which is the
// operationally important part: a human classified as a machine gets hung up on.
func verdictBreakdown(v map[amd.Result]int) string {
	var keys []string
	for k := range v {
		keys = append(keys, string(k))
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, v[amd.Result(k)]))
	}
	return strings.Join(parts, " ")
}
