# Background noise recordings

Source: **https://pixabay.com/**

These are ambient recordings downloaded from Pixabay and used as the noise bed
for the answering-machine-detection fixtures. They are covered by the
[Pixabay Content License](https://pixabay.com/service/license-summary/), which
permits free use for commercial and non-commercial purposes and does not
require attribution. The credit here is courtesy, not obligation. The license
does restrict redistributing the files as-is as a standalone product (for
example re-uploading them to another stock site) — using them as test input, as
this repository does, is not that. Check the linked summary rather than relying
on this paragraph if it matters to you.

The per-file uploader and track URL were not recorded at download time; if you
need exact provenance for a specific file, re-download it and note the URL here.

| File | Rate | Channels | Duration | Size |
|------|------|----------|----------|------|
| `busy-city.mp3` | 48 kHz | stereo | 57 s | 1.7 MB |
| `office.mp3` | 44.1 kHz | stereo | 29 s | 0.9 MB |

## How they are used

`make gen-noisy-greetings` mixes these with the clean human greetings in
`tests/data/greetings/human` to build the noisy AMD corpora. The generator
(`cmd/gen-noisy-greetings`) decodes each file, averages the stereo pair to mono,
resamples to 16 kHz, and draws a segment from the middle of the recording for
every greeting — skipping a margin at each end, because recordings often open
and close quietly, and redrawing when a segment lands in a quiet patch.

Nothing here needs to be in any particular format: the generator accepts `.wav`
and `.mp3` at any rate and converts on load. Adding a file to this directory
adds two more corpora (one per SNR in `-snr`), named after the file. Keep the
recordings long enough that segments do not all overlap — a minute is
comfortable for the 46 greetings, half a minute is workable.

Deliberately *not* speech: the point is background noise that a denoiser can be
asked to remove. A recording of people talking would be a different test, and a
much harder one, since separating one talker from another is not what RNNoise
does.
