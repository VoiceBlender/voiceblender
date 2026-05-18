// Mic capture -> Opus encode (WebCodecs) -> caller-supplied publish fn.
// Incoming Opus frames -> WebCodecs decode -> AudioContext playback.
//
// Encoder/decoder run at 48 kHz mono with 20 ms frames so the bytes
// match what gopus on the server side wants.

const SAMPLE_RATE = 48000;
const FRAME_MS = 20;
const FRAME_SAMPLES = SAMPLE_RATE * FRAME_MS / 1000; // 960

export class MicCapture {
  constructor({ publish, onLog = () => {} }) {
    this.publish = publish;
    this.onLog = onLog;
    this.stream = null;
    this.encoder = null;
    this.ctx = null;
    this.scriptNode = null;
    this.source = null;
    this.frameBuf = new Float32Array(0);
  }

  async start() {
    this.stream = await navigator.mediaDevices.getUserMedia({
      audio: { echoCancellation: true, noiseSuppression: true, channelCount: 1 },
    });
    this.ctx = new AudioContext({ sampleRate: SAMPLE_RATE });
    if (this.ctx.state === 'suspended') await this.ctx.resume();
    this.source = this.ctx.createMediaStreamSource(this.stream);

    this.encoder = new AudioEncoder({
      output: (chunk) => {
        const buf = new Uint8Array(chunk.byteLength);
        chunk.copyTo(buf);
        this.publish(buf).catch((err) => this.onLog('publish failed: ' + err.message));
      },
      error: (e) => this.onLog('encoder error: ' + e.message),
    });
    this.encoder.configure({
      codec: 'opus',
      sampleRate: SAMPLE_RATE,
      numberOfChannels: 1,
      bitrate: 24000,
      opus: { frameDuration: 20000 },
    });

    // Use ScriptProcessor (deprecated but widely supported) instead of
    // AudioWorklet to keep the demo single-file. 1024-sample chunks at
    // 48 kHz = ~21 ms — we re-frame to exact 960 samples below.
    this.scriptNode = this.ctx.createScriptProcessor(1024, 1, 1);
    this.scriptNode.onaudioprocess = (e) => this._onSamples(e.inputBuffer.getChannelData(0));
    this.source.connect(this.scriptNode);
    // ScriptProcessorNode only fires onaudioprocess while connected to
    // a destination. Route through a zero-gain node so the mic doesn't
    // echo to the speakers but the graph still ticks.
    this.muteGain = this.ctx.createGain();
    this.muteGain.gain.value = 0;
    this.scriptNode.connect(this.muteGain);
    this.muteGain.connect(this.ctx.destination);
  }

  _onSamples(chunk) {
    const combined = new Float32Array(this.frameBuf.length + chunk.length);
    combined.set(this.frameBuf, 0);
    combined.set(chunk, this.frameBuf.length);
    let off = 0;
    const now = performance.now() * 1000;
    while (combined.length - off >= FRAME_SAMPLES) {
      const slice = combined.subarray(off, off + FRAME_SAMPLES);
      const data = new AudioData({
        format: 'f32',
        sampleRate: SAMPLE_RATE,
        numberOfFrames: FRAME_SAMPLES,
        numberOfChannels: 1,
        timestamp: now + off * 1_000_000 / SAMPLE_RATE,
        data: slice,
      });
      this.encoder.encode(data);
      data.close();
      off += FRAME_SAMPLES;
    }
    this.frameBuf = combined.subarray(off);
  }

  stop() {
    try { this.scriptNode?.disconnect(); } catch {}
    try { this.muteGain?.disconnect(); } catch {}
    try { this.source?.disconnect(); } catch {}
    try { this.encoder?.close(); } catch {}
    try { this.ctx?.close(); } catch {}
    if (this.stream) for (const t of this.stream.getTracks()) t.stop();
  }
}

export class Speaker {
  constructor({ onLog = () => {} } = {}) {
    this.onLog = onLog;
    this.ctx = null;
    this.decoder = null;
    this.playhead = 0; // ctx-time of next scheduled buffer
    this.started = false;
  }

  async start() {
    this.ctx = new AudioContext({ sampleRate: SAMPLE_RATE });
    if (this.ctx.state === 'suspended') await this.ctx.resume();
    this.decoder = new AudioDecoder({
      output: (frame) => this._onFrame(frame),
      error: (e) => this.onLog('decoder error: ' + e.message),
    });
    this.decoder.configure({
      codec: 'opus',
      sampleRate: SAMPLE_RATE,
      numberOfChannels: 1,
    });
    this.started = true;
  }

  // Feed one Opus-encoded packet for decoding.
  feed(opusPacket) {
    if (!this.started) return;
    const chunk = new EncodedAudioChunk({
      type: 'key',
      timestamp: performance.now() * 1000,
      data: opusPacket,
    });
    try { this.decoder.decode(chunk); } catch (e) { this.onLog('decode failed: ' + e.message); }
  }

  _onFrame(frame) {
    const samples = frame.numberOfFrames;
    const channels = frame.numberOfChannels;
    const buf = this.ctx.createBuffer(1, samples, SAMPLE_RATE);
    const tmp = new Float32Array(samples);
    if (frame.format === 'f32' || frame.format === 'f32-planar') {
      frame.copyTo(tmp, { planeIndex: 0 });
    } else {
      // Convert int16 -> float32 manually
      const i16 = new Int16Array(samples * channels);
      frame.copyTo(i16, { planeIndex: 0 });
      for (let i = 0; i < samples; i++) tmp[i] = i16[i * channels] / 32768;
    }
    buf.copyToChannel(tmp, 0);
    frame.close();

    const src = this.ctx.createBufferSource();
    src.buffer = buf;
    src.connect(this.ctx.destination);
    const now = this.ctx.currentTime;
    if (this.playhead < now + 0.02) this.playhead = now + 0.05;
    src.start(this.playhead);
    this.playhead += samples / SAMPLE_RATE;
  }

  stop() {
    try { this.decoder?.close(); } catch {}
    try { this.ctx?.close(); } catch {}
    this.started = false;
  }
}
