// Tiny browser-side MoQ session for the demo. Wraps a WebTransport
// connection, runs the draft-11 handshake, ANNOUNCEs the `mic`
// namespace, accepts the server's incoming SUBSCRIBE for it,
// SUBSCRIBEs to `mix`, and exposes events for incoming audio + a
// publisher for outbound audio.

import {
  MSG, DRAFT_VERSION,
  encodeClientSetup, decodeServerSetup,
  encodeAnnounce, decodeAnnounceOk, decodeAnnounceError,
  encodeSubscribe, decodeSubscribe, decodeSubscribeError,
  encodeSubscribeOk, decodeSubscribeOk, decodeMaxRequestID, encodeMaxRequestID,
  encodeSubgroupHeader, encodeSubgroupObject,
  readSubgroupHeader, readSubgroupObject, readSubgroupObjectNoExt,
  readVarint, Reader,
} from './moq-wire.js';

const INITIAL_MAX_REQUEST_ID = 100n;

const MIX_NAMESPACE = ['mix'];
const MIC_NAMESPACE = ['mic'];
const AUDIO_TRACK   = 'audio';

export class MoqSession extends EventTarget {
  constructor(wt) {
    super();
    this.wt = wt;
    this.controlWriter = null;
    this.controlReader = null;
    this.handshakeDone = false;
    this.nextRequestID = 0n;
    this.announceWaiters = new Map();   // requestID -> {resolve, reject}
    this.subscribeWaiters = new Map();  // requestID -> {resolve, reject}
    this.mixSubscribeID = null;
    this.micPublisher = null;           // resolves to a write fn once server SUBSCRIBEs
    this.micPublisherPromise = new Promise((res) => { this._micResolve = res; });
    this.closed = false;
    this.objectsRx = 0;
    this.objectsTx = 0;
    this.bytesRx = 0;
    this.bytesTx = 0;
  }

  // Driver: opens the bidi control stream, sends CLIENT_SETUP, waits
  // for SERVER_SETUP, then keeps the read loop running for the life
  // of the session.
  async run() {
    const stream = await this.wt.createBidirectionalStream();
    this.controlWriter = stream.writable.getWriter();
    this.controlReader = stream.readable.getReader();

    // CLIENT_SETUP
    const setupBytes = encodeClientSetup({
      versions: [DRAFT_VERSION],
      parameters: [],
    });
    await this.controlWriter.write(setupBytes);
    this.dispatchEvent(new CustomEvent('log', { detail: 'sent CLIENT_SETUP' }));

    // Read loop. We buffer arriving bytes and parse one control
    // message at a time.
    this._buf = new Uint8Array(0);
    this._readPump().catch((err) => {
      this.dispatchEvent(new CustomEvent('error', { detail: String(err) }));
    });

    // Also drain incoming unidirectional streams (subgroup data).
    this._uniPump().catch((err) => {
      this.dispatchEvent(new CustomEvent('error', { detail: 'uni pump: ' + err }));
    });

    // Wait for handshake to complete.
    await new Promise((resolve, reject) => {
      const onReady = () => { this.removeEventListener('handshake', onReady); resolve(); };
      this.addEventListener('handshake', onReady);
      setTimeout(() => reject(new Error('handshake timeout')), 5000);
    });
  }

  async announceMic() {
    const requestID = this._nextRequestID();
    const bytes = encodeAnnounce({ requestID, namespace: MIC_NAMESPACE });
    await this.controlWriter.write(bytes);
    this.dispatchEvent(new CustomEvent('log', { detail: 'sent ANNOUNCE mic ' + requestID }));
    return new Promise((resolve, reject) => {
      this.announceWaiters.set(String(requestID), { resolve, reject });
      setTimeout(() => {
        if (this.announceWaiters.delete(String(requestID))) reject(new Error('ANNOUNCE timeout'));
      }, 5000);
    });
  }

  async subscribeMix() {
    const requestID = this._nextRequestID();
    const bytes = encodeSubscribe({
      requestID,
      trackAlias: 0n,
      namespace: MIX_NAMESPACE,
      trackName: AUDIO_TRACK,
    });
    await this.controlWriter.write(bytes);
    this.mixSubscribeID = requestID;
    this.dispatchEvent(new CustomEvent('log', { detail: 'sent SUBSCRIBE mix ' + requestID }));
    return new Promise((resolve, reject) => {
      this.subscribeWaiters.set(String(requestID), { resolve, reject });
      setTimeout(() => {
        if (this.subscribeWaiters.delete(String(requestID))) reject(new Error('SUBSCRIBE timeout'));
      }, 5000);
    });
  }

  // Returns a function that publishes one Opus frame on the `mic` track.
  // Resolves once the server subscribes (i.e. our SubscribeHandler fired).
  async waitForMicPublisher() {
    return this.micPublisherPromise;
  }

  close() {
    if (this.closed) return;
    this.closed = true;
    try { this.wt.close(); } catch {}
  }

  // ------------- internals -------------

  _nextRequestID() {
    // Client-side request IDs start at 0 and step by 2 (parity 0).
    const id = this.nextRequestID;
    this.nextRequestID += 2n;
    return id;
  }

  async _readPump() {
    while (!this.closed) {
      const { value, done } = await this.controlReader.read();
      if (done) {
        this.dispatchEvent(new CustomEvent('log', { detail: 'control stream EOF' }));
        return;
      }
      this._appendCtrl(value);
      while (this._tryParseOneCtrl()) {/* loop */}
    }
  }

  _appendCtrl(chunk) {
    if (this._buf.byteLength === 0) {
      this._buf = chunk;
    } else {
      const combined = new Uint8Array(this._buf.byteLength + chunk.byteLength);
      combined.set(this._buf, 0);
      combined.set(chunk, this._buf.byteLength);
      this._buf = combined;
    }
  }

  // Tries to parse one full control message from _buf. Returns true if
  // it consumed one (so the caller should loop), false if it needs more
  // bytes.
  _tryParseOneCtrl() {
    if (this._buf.byteLength < 3) return false;
    const view = new DataView(this._buf.buffer, this._buf.byteOffset, this._buf.byteLength);
    let off = 0;
    let mtype, mn;
    try {
      [mtype, mn] = readVarint(view, 0);
    } catch {
      return false;
    }
    off += mn;
    if (off + 2 > this._buf.byteLength) return false;
    const len = view.getUint16(off, false);
    off += 2;
    if (off + len > this._buf.byteLength) return false;
    const payload = this._buf.subarray(off, off + len);
    this._buf = this._buf.subarray(off + len);
    this._handleControl(Number(mtype), payload);
    return true;
  }

  _handleControl(type, payload) {
    switch (type) {
      case MSG.SERVER_SETUP: {
        const m = decodeServerSetup(payload);
        this.dispatchEvent(new CustomEvent('log', {
          detail: `got SERVER_SETUP version=0x${m.selectedVersion.toString(16)}`,
        }));
        // Grant the server request-ID quota so it can issue its
        // SUBSCRIBE for our `mic` namespace. Without this we'd deadlock
        // waiting on waitForMicPublisher().
        this.controlWriter.write(encodeMaxRequestID(INITIAL_MAX_REQUEST_ID))
          .catch((err) => this.dispatchEvent(new CustomEvent('log', { detail: 'send MAX_REQUEST_ID failed: ' + err.message })));
        this.handshakeDone = true;
        this.dispatchEvent(new CustomEvent('handshake'));
        return;
      }
      case MSG.MAX_REQUEST_ID: {
        const m = decodeMaxRequestID(payload);
        this.dispatchEvent(new CustomEvent('log', { detail: 'got MAX_REQUEST_ID ' + m.requestID }));
        return;
      }
      case MSG.REQUESTS_BLOCKED:
        // Flow-control housekeeping; nothing to do for the demo.
        return;
      case MSG.ANNOUNCE_OK: {
        const m = decodeAnnounceOk(payload);
        const w = this.announceWaiters.get(String(m.requestID));
        if (w) { this.announceWaiters.delete(String(m.requestID)); w.resolve(); }
        this.dispatchEvent(new CustomEvent('log', { detail: 'got ANNOUNCE_OK ' + m.requestID }));
        return;
      }
      case MSG.ANNOUNCE_ERROR: {
        const m = decodeAnnounceError(payload);
        const w = this.announceWaiters.get(String(m.requestID));
        if (w) { this.announceWaiters.delete(String(m.requestID)); w.reject(new Error('ANNOUNCE_ERROR: ' + m.reason)); }
        this.dispatchEvent(new CustomEvent('log', { detail: 'got ANNOUNCE_ERROR ' + m.reason }));
        return;
      }
      case MSG.SUBSCRIBE: {
        // The server is subscribing to our mic track. Accept and wire
        // up a publisher the audio module can call to send frames.
        const m = decodeSubscribe(payload);
        this.dispatchEvent(new CustomEvent('log', {
          detail: `got SUBSCRIBE for ${m.namespace.join('/')}/${m.trackName} req=${m.requestID}`,
        }));
        this._handleIncomingSubscribe(m);
        return;
      }
      case MSG.SUBSCRIBE_OK: {
        const m = decodeSubscribeOk(payload);
        const w = this.subscribeWaiters.get(String(m.requestID));
        if (w) { this.subscribeWaiters.delete(String(m.requestID)); w.resolve(m); }
        this.dispatchEvent(new CustomEvent('log', { detail: 'got SUBSCRIBE_OK ' + m.requestID }));
        return;
      }
      case MSG.SUBSCRIBE_ERROR: {
        const m = decodeSubscribeError(payload);
        const w = this.subscribeWaiters.get(String(m.requestID));
        if (w) { this.subscribeWaiters.delete(String(m.requestID)); w.reject(new Error('SUBSCRIBE_ERROR: ' + m.reason)); }
        this.dispatchEvent(new CustomEvent('log', { detail: 'got SUBSCRIBE_ERROR ' + m.reason }));
        return;
      }
      case MSG.GO_AWAY: {
        this.dispatchEvent(new CustomEvent('log', { detail: 'got GO_AWAY' }));
        return;
      }
      default:
        this.dispatchEvent(new CustomEvent('log', { detail: 'got unhandled control type 0x' + type.toString(16) }));
    }
  }

  async _handleIncomingSubscribe(m) {
    const trackAlias = m.trackAlias;
    // Reply SUBSCRIBE_OK.
    const okBytes = encodeSubscribeOk({ requestID: m.requestID });
    await this.controlWriter.write(okBytes);

    // Build a publisher closure. Each call opens a fresh unidirectional
    // stream containing one subgroup with one object — matches what the
    // server does for the mix track and keeps state minimal.
    let groupID = 0n;
    const publish = async (opusPayload) => {
      if (this.closed) return;
      const uni = await this.wt.createUnidirectionalStream();
      const writer = uni.getWriter();
      const header = encodeSubgroupHeader({ trackAlias, groupID, subgroupID: 0n, priority: 0 });
      const body = encodeSubgroupObject({ objectID: 0n, payload: opusPayload });
      const both = new Uint8Array(header.byteLength + body.byteLength);
      both.set(header, 0);
      both.set(body, header.byteLength);
      await writer.write(both);
      await writer.close();
      groupID += 1n;
      this.objectsTx++;
      this.bytesTx += opusPayload.byteLength;
    };
    this._micResolve(publish);
  }

  async _uniPump() {
    const reader = this.wt.incomingUnidirectionalStreams.getReader();
    while (!this.closed) {
      const { value: stream, done } = await reader.read();
      if (done) return;
      this._readSubgroupStream(stream).catch((err) => {
        this.dispatchEvent(new CustomEvent('log', { detail: 'subgroup read error: ' + err.message }));
      });
    }
  }

  async _readSubgroupStream(stream) {
    const r = stream.getReader();
    let buf = new Uint8Array(0);
    let eof = false;

    const ensure = async (n) => {
      while (buf.byteLength < n && !eof) {
        const { value, done } = await r.read();
        if (done) { eof = true; break; }
        const combined = new Uint8Array(buf.byteLength + value.byteLength);
        combined.set(buf, 0);
        combined.set(value, buf.byteLength);
        buf = combined;
      }
      if (buf.byteLength < n) throw new Error('subgroup stream truncated');
      const out = buf.subarray(0, n);
      buf = buf.subarray(n);
      return out;
    };

    const header = await readSubgroupHeader(ensure);
    const readOne = header.hasExtensions ? readSubgroupObject : readSubgroupObjectNoExt;

    while (true) {
      // If buffer empty AND stream is at EOF, we're done.
      if (buf.byteLength === 0 && eof) return;
      try {
        const obj = await readOne(ensure);
        if (obj.payload.byteLength > 0) {
          this.objectsRx++;
          this.bytesRx += obj.payload.byteLength;
          this.dispatchEvent(new CustomEvent('object', { detail: obj.payload }));
        }
      } catch (err) {
        if (eof && buf.byteLength === 0) return;
        throw err;
      }
    }
  }
}
