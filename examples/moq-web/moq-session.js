// Browser-side MoQ session for the demo, built on the moqt client
// (draft-ietf-moq-transport-22, vendor/moqt.js). Publishes the `mic`
// namespace, accepts the server's SUBSCRIBE for it, subscribes to `mix`,
// and exposes events for incoming audio plus a publisher for outbound audio.

import { ObjectStatus, RequestErrorCode, Session } from './vendor/moqt.js';

const MIX_TRACK = { namespace: ['mix'], name: 'audio' };
const MIC_TRACK = { namespace: ['mic'], name: 'audio' };

export class MoqSession extends EventTarget {
  // serverCertificateHashes: optional array of SHA-256 Uint8Arrays for a
  // self-signed dev certificate.
  constructor({ url, serverCertificateHashes }) {
    super();
    this.url = url;
    this.serverCertificateHashes = serverCertificateHashes;
    this.session = null;
    this.micPublisherPromise = new Promise((res) => { this._micResolve = res; });
    this.closed = false;
    this.objectsRx = 0;
    this.objectsTx = 0;
    this.bytesRx = 0;
    this.bytesTx = 0;
  }

  // Opens WebTransport (protocol moqt-22) and exchanges SETUP.
  async run() {
    this.session = await Session.connect({
      url: this.url,
      serverCertificateHashes: this.serverCertificateHashes,
      handler: {
        onPublishNamespace: (req) => {
          this._log('server published namespace ' + req.prefix.join('/'));
          req.accept().catch((err) => this._error('accept namespace: ' + err.message));
        },
        onSubscribe: (req) => this._handleSubscribe(req),
      },
    });
    this.session.closed.then((err) => {
      if (!this.closed) this._error('session closed: ' + err.message);
    });
    this._log('SETUP done, protocol ' + (this.session.wt.protocol || '(not negotiated)'));
  }

  async announceMic() {
    await this.session.publishNamespace(MIC_TRACK.namespace);
    this._log('PUBLISH_NAMESPACE mic accepted');
  }

  async subscribeMix() {
    const sub = await this.session.subscribe(MIX_TRACK);
    this._log('SUBSCRIBE mix ok, track alias ' + sub.trackAlias);
    this._readMix(sub).catch((err) => this._error('mix: ' + err.message));
    return sub;
  }

  // Returns a function that publishes one Opus frame on the `mic` track.
  // Resolves once the server subscribes.
  async waitForMicPublisher() {
    return this.micPublisherPromise;
  }

  close() {
    if (this.closed) return;
    this.closed = true;
    this.session?.close();
  }

  // ------------- internals -------------

  // Each mix Group arrives on its own subgroup stream; read them
  // concurrently so one slow stream never holds up the next frame.
  async _readMix(sub) {
    for await (const group of sub) {
      (async () => {
        for await (const obj of group) {
          if (obj.status !== ObjectStatus.Normal || obj.payload.byteLength === 0) continue;
          this.objectsRx++;
          this.bytesRx += obj.payload.byteLength;
          this.dispatchEvent(new CustomEvent('object', { detail: obj.payload }));
        }
      })().catch(() => {});
    }
  }

  async _handleSubscribe(req) {
    const t = req.track;
    if (t.name !== MIC_TRACK.name || t.namespace.join('/') !== MIC_TRACK.namespace.join('/')) {
      await req.reject(RequestErrorCode.DoesNotExist, 'unknown track');
      return;
    }
    const writer = await req.accept();
    this._log('server subscribed to mic');

    // One Opus frame per Group, matching what the server sends on `mix`.
    let group = 0;
    const publish = async (opusPayload) => {
      if (this.closed || writer.signal.aborted) return;
      const sg = writer.openSubgroup(group++, 0, { endOfGroup: true, firstObject: true });
      await sg.writeObject({ id: 0, status: ObjectStatus.Normal, payload: opusPayload });
      await sg.close();
      this.objectsTx++;
      this.bytesTx += opusPayload.byteLength;
    };
    this._micResolve(publish);
  }

  _log(msg) {
    this.dispatchEvent(new CustomEvent('log', { detail: msg }));
  }

  _error(msg) {
    this.dispatchEvent(new CustomEvent('error', { detail: msg }));
  }
}
