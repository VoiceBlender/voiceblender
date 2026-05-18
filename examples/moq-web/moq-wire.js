// MoQ draft-ietf-moq-transport-11 wire codec — the minimal subset the
// browser demo needs to interop with a Go server built on
// github.com/mengelbart/moqtransport@v0.5.0.
//
// Scope:
//   - QUIC varint encode/decode (RFC 9000 §16)
//   - Control message framing: varint(type) + uint16BE(payload_len) + payload
//   - Messages used:  CLIENT_SETUP, SERVER_SETUP, ANNOUNCE, ANNOUNCE_OK,
//                     SUBSCRIBE, SUBSCRIBE_OK, MAX_REQUEST_ID
//   - Subgroup data stream header + objects (StreamType 0x0d: SID + Ext)
//
// Anything not on that list is intentionally absent.

export const MSG = Object.freeze({
  CLIENT_SETUP:     0x20,
  SERVER_SETUP:     0x21,
  GO_AWAY:          0x10,
  MAX_REQUEST_ID:   0x15,
  REQUESTS_BLOCKED: 0x1a,
  SUBSCRIBE:        0x03,
  SUBSCRIBE_OK:     0x04,
  SUBSCRIBE_ERROR:  0x05,
  SUBSCRIBE_DONE:   0x0b,
  ANNOUNCE:         0x06,
  ANNOUNCE_OK:      0x07,
  ANNOUNCE_ERROR:   0x08,
});

// Subgroup stream type used by mengelbart/moqtransport's publisher:
// subgroup ID is present in the header AND each object carries an
// extensions block.
export const STREAM_TYPE_SUBGROUP_SID_EXT = 0x0d;

export const DRAFT_VERSION = 0xff00000b; // draft-ietf-moq-transport-11

// ---------------------------------------------------------------------------
// Varint (QUIC, RFC 9000 §16)
// ---------------------------------------------------------------------------

export function varintLen(value) {
  if (value < 0n) throw new RangeError('varint: negative');
  const v = BigInt(value);
  if (v < 0x40n) return 1;
  if (v < 0x4000n) return 2;
  if (v < 0x40000000n) return 4;
  if (v < 0x4000000000000000n) return 8;
  throw new RangeError('varint: value too large');
}

export function writeVarint(view, offset, value) {
  const v = BigInt(value);
  const len = varintLen(v);
  switch (len) {
    case 1:
      view.setUint8(offset, Number(v));
      break;
    case 2:
      view.setUint16(offset, Number(v) | 0x4000, false);
      break;
    case 4:
      view.setUint32(offset, Number(v) | 0x80000000, false);
      break;
    case 8:
      view.setBigUint64(offset, v | 0xc000000000000000n, false);
      break;
  }
  return len;
}

// Reads a varint, returning [value (BigInt), bytesConsumed]. Throws on
// truncated input.
export function readVarint(view, offset) {
  if (offset >= view.byteLength) throw new RangeError('varint: truncated');
  const first = view.getUint8(offset);
  const lenBits = first >> 6;
  switch (lenBits) {
    case 0: return [BigInt(first & 0x3f), 1];
    case 1: {
      if (offset + 2 > view.byteLength) throw new RangeError('varint: truncated 2');
      return [BigInt(view.getUint16(offset, false) & 0x3fff), 2];
    }
    case 2: {
      if (offset + 4 > view.byteLength) throw new RangeError('varint: truncated 4');
      return [BigInt(view.getUint32(offset, false) & 0x3fffffff), 4];
    }
    case 3: {
      if (offset + 8 > view.byteLength) throw new RangeError('varint: truncated 8');
      return [view.getBigUint64(offset, false) & 0x3fffffffffffffffn, 8];
    }
  }
}

// ---------------------------------------------------------------------------
// Writer / Reader helpers
// ---------------------------------------------------------------------------

export class Writer {
  constructor() {
    this.chunks = [];
    this.length = 0;
  }
  _push(buf) { this.chunks.push(buf); this.length += buf.byteLength; }

  varint(value) {
    const len = varintLen(value);
    const buf = new Uint8Array(len);
    writeVarint(new DataView(buf.buffer), 0, value);
    this._push(buf);
    return this;
  }
  u8(b) { this._push(new Uint8Array([b])); return this; }
  u16BE(v) {
    const buf = new Uint8Array(2);
    new DataView(buf.buffer).setUint16(0, v, false);
    this._push(buf);
    return this;
  }
  bytes(arr) { this._push(arr); return this; }

  // Tuple = varint(count) + for each: varint(len) + bytes
  tuple(strs) {
    this.varint(strs.length);
    for (const s of strs) {
      const enc = new TextEncoder().encode(s);
      this.varint(enc.byteLength);
      this._push(enc);
    }
    return this;
  }

  // VarintBytes = varint(len) + bytes
  varintBytes(arr) {
    this.varint(arr.byteLength);
    this._push(arr);
    return this;
  }

  // KVPList written with element-count prefix (used by ANNOUNCE/SUBSCRIBE).
  kvpListNum(kvps) {
    this.varint(kvps.length);
    for (const kv of kvps) appendKVP(this, kv);
    return this;
  }

  // KVPList written with byte-length prefix (used by Object extensions).
  kvpListLength(kvps) {
    const inner = new Writer();
    for (const kv of kvps) appendKVP(inner, kv);
    this.varint(inner.length);
    for (const c of inner.chunks) this._push(c);
    return this;
  }

  toBytes() {
    const out = new Uint8Array(this.length);
    let off = 0;
    for (const c of this.chunks) { out.set(c, off); off += c.byteLength; }
    return out;
  }
}

function appendKVP(w, kv) {
  w.varint(kv.type);
  if (Number(kv.type) % 2 === 1) {
    const bytes = kv.bytes ?? new Uint8Array(0);
    w.varint(bytes.byteLength);
    w._push(bytes);
  } else {
    w.varint(kv.value ?? 0);
  }
}

export class Reader {
  constructor(buf) {
    this.buf = buf instanceof Uint8Array ? buf : new Uint8Array(buf);
    this.view = new DataView(this.buf.buffer, this.buf.byteOffset, this.buf.byteLength);
    this.off = 0;
  }
  remaining() { return this.buf.byteLength - this.off; }

  varint() {
    const [v, n] = readVarint(this.view, this.off);
    this.off += n;
    return v;
  }
  u8() {
    const b = this.view.getUint8(this.off);
    this.off += 1;
    return b;
  }
  u16BE() {
    const v = this.view.getUint16(this.off, false);
    this.off += 2;
    return v;
  }
  bytes(n) {
    const out = this.buf.subarray(this.off, this.off + n);
    this.off += n;
    return out;
  }
  tuple() {
    const count = Number(this.varint());
    const out = [];
    for (let i = 0; i < count; i++) {
      const len = Number(this.varint());
      out.push(new TextDecoder().decode(this.bytes(len)));
    }
    return out;
  }
  varintBytes() {
    const len = Number(this.varint());
    return this.bytes(len);
  }
  kvpListNum() {
    const count = Number(this.varint());
    const out = [];
    for (let i = 0; i < count; i++) out.push(readKVP(this));
    return out;
  }
  kvpListLength() {
    const len = Number(this.varint());
    const end = this.off + len;
    const out = [];
    while (this.off < end) out.push(readKVP(this));
    return out;
  }
}

function readKVP(r) {
  const type = r.varint();
  if (Number(type) % 2 === 1) {
    const len = Number(r.varint());
    return { type, bytes: r.bytes(len) };
  }
  return { type, value: r.varint() };
}

// ---------------------------------------------------------------------------
// Control message framing
// ---------------------------------------------------------------------------

// Wraps a payload as: varint(type) + uint16BE(length) + payload.
export function frameControlMessage(type, payload) {
  const w = new Writer();
  w.varint(type);
  w.u16BE(payload.byteLength);
  w._push(payload);
  return w.toBytes();
}

// ---------------------------------------------------------------------------
// Specific message encoders/decoders (subset)
// ---------------------------------------------------------------------------

export function encodeClientSetup({ versions, parameters = [] }) {
  const inner = new Writer();
  inner.varint(versions.length);
  for (const v of versions) inner.varint(v);
  inner.kvpListNum(parameters);
  return frameControlMessage(MSG.CLIENT_SETUP, inner.toBytes());
}

export function decodeServerSetup(payload) {
  const r = new Reader(payload);
  const selectedVersion = r.varint();
  const parameters = r.kvpListNum();
  return { selectedVersion, parameters };
}

export function encodeAnnounce({ requestID, namespace, parameters = [] }) {
  const inner = new Writer();
  inner.varint(requestID);
  inner.tuple(namespace);
  inner.kvpListNum(parameters);
  return frameControlMessage(MSG.ANNOUNCE, inner.toBytes());
}

export function decodeAnnounceOk(payload) {
  return { requestID: new Reader(payload).varint() };
}

export function decodeAnnounceError(payload) {
  const r = new Reader(payload);
  return {
    requestID: r.varint(),
    errorCode: r.varint(),
    reason: new TextDecoder().decode(r.varintBytes()),
  };
}

// FilterType values, draft-11 §6.4.
export const FILTER_LATEST_OBJECT     = 0x02;
export const FILTER_NEXT_GROUP_START  = 0x01;
export const FILTER_ABSOLUTE_START    = 0x03;
export const FILTER_ABSOLUTE_RANGE    = 0x04;

// Encodes SUBSCRIBE with the simplest possible options:
//   - SubscriberPriority = 128
//   - GroupOrder = Ascending (1)
//   - Forward = 1
//   - FilterType = LatestObject (no start/end location)
//   - No extra parameters
export function encodeSubscribe({ requestID, trackAlias, namespace, trackName }) {
  const inner = new Writer();
  inner.varint(requestID);
  inner.varint(trackAlias);
  inner.tuple(namespace);
  inner.varintBytes(new TextEncoder().encode(trackName));
  inner.u8(128);   // subscriber priority
  inner.u8(1);     // group order: Ascending
  inner.u8(1);     // forward: yes
  inner.varint(FILTER_LATEST_OBJECT);
  inner.kvpListNum([]);
  return frameControlMessage(MSG.SUBSCRIBE, inner.toBytes());
}

export function decodeSubscribe(payload) {
  const r = new Reader(payload);
  const requestID = r.varint();
  const trackAlias = r.varint();
  const namespace = r.tuple();
  const trackName = new TextDecoder().decode(r.varintBytes());
  const subscriberPriority = r.u8();
  const groupOrder = r.u8();
  const forward = r.u8();
  const filterType = r.varint();
  // We don't bother with StartLocation / EndGroup for the demo; if the
  // server uses an absolute filter we'd miss those bytes, but its
  // outgoing SUBSCRIBE for our mic uses LatestObject.
  const parameters = r.kvpListNum();
  return { requestID, trackAlias, namespace, trackName, subscriberPriority,
           groupOrder, forward, filterType, parameters };
}

// SubscribeOk written with no LargestLocation, no parameters.
export function encodeSubscribeOk({ requestID, expiresMs = 0, groupOrder = 1 }) {
  const inner = new Writer();
  inner.varint(requestID);
  inner.varint(expiresMs);
  inner.u8(groupOrder);
  inner.u8(0);          // ContentExists = false
  inner.kvpListNum([]); // no parameters
  return frameControlMessage(MSG.SUBSCRIBE_OK, inner.toBytes());
}

export function decodeSubscribeOk(payload) {
  const r = new Reader(payload);
  const requestID = r.varint();
  const expiresMs = r.varint();
  const groupOrder = r.u8();
  const contentExists = r.u8() === 1;
  let largest = null;
  if (contentExists) {
    largest = { group: r.varint(), object: r.varint() };
  }
  const parameters = r.kvpListNum();
  return { requestID, expiresMs, groupOrder, contentExists, largest, parameters };
}

export function decodeSubscribeError(payload) {
  const r = new Reader(payload);
  return {
    requestID: r.varint(),
    errorCode: r.varint(),
    reason: new TextDecoder().decode(r.varintBytes()),
    trackAlias: r.remaining() > 0 ? r.varint() : null,
  };
}

export function decodeMaxRequestID(payload) {
  return { requestID: new Reader(payload).varint() };
}

export function encodeMaxRequestID(maxRequestID) {
  const inner = new Writer();
  inner.varint(maxRequestID);
  return frameControlMessage(MSG.MAX_REQUEST_ID, inner.toBytes());
}

// ---------------------------------------------------------------------------
// Subgroup data streams
// ---------------------------------------------------------------------------

// Writes the StreamTypeSubgroupSIDExt (0x0d) header to a stream writer.
export function encodeSubgroupHeader({ trackAlias, groupID, subgroupID = 0, priority = 0 }) {
  const w = new Writer();
  w.varint(STREAM_TYPE_SUBGROUP_SID_EXT);
  w.varint(trackAlias);
  w.varint(groupID);
  w.varint(subgroupID);
  w.u8(priority);
  return w.toBytes();
}

// One Object body inside a subgroup stream. Includes the empty
// extensions block required by SID+Ext stream type 0x0d.
export function encodeSubgroupObject({ objectID, payload }) {
  const w = new Writer();
  w.varint(objectID);
  w.kvpListLength([]);          // zero-length extension list
  w.varint(payload.byteLength);
  if (payload.byteLength > 0) w._push(payload);
  return w.toBytes();
}

// Parses one Object from the *body* of a subgroup stream (after the
// header has already been consumed). Pulls bytes from `pull(n)` (async
// function returning a Uint8Array of exactly n bytes) so the caller can
// drive it off a WebTransport ReadableStream.
export async function readSubgroupObject(pull) {
  // ObjectID
  const idVarint = await readVarintFromPull(pull);
  // Extensions (byte-length-prefixed)
  const extLen = await readVarintFromPull(pull);
  if (extLen > 0n) await pull(Number(extLen)); // discard contents
  // Payload length
  const payloadLen = await readVarintFromPull(pull);
  let payload = new Uint8Array(0);
  let objectStatus = 0;
  if (payloadLen === 0n) {
    objectStatus = Number(await readVarintFromPull(pull));
  } else {
    payload = await pull(Number(payloadLen));
  }
  return { objectID: idVarint, payload, objectStatus };
}

// Parses the stream-header at the start of an incoming subgroup uni-stream.
export async function readSubgroupHeader(pull) {
  const streamType = Number(await readVarintFromPull(pull));
  if (streamType < 0x08 || streamType > 0x0d) {
    throw new Error(`unexpected stream type 0x${streamType.toString(16)}`);
  }
  const hasSubgroupID = streamType === 0x0c || streamType === 0x0d;
  const trackAlias = await readVarintFromPull(pull);
  const groupID    = await readVarintFromPull(pull);
  let subgroupID = 0n;
  if (hasSubgroupID) subgroupID = await readVarintFromPull(pull);
  const priorityBuf = await pull(1);
  const priority = priorityBuf[0];
  // For our purposes we always read ext blocks on each Object — stream
  // types 0x08, 0x0a, 0x0c (no ext) and 0x09, 0x0b, 0x0d (ext) differ
  // only in whether the extensions length varint is present.
  const hasExtensions = (streamType & 0x01) === 1;
  return { streamType, trackAlias, groupID, subgroupID, priority, hasExtensions };
}

async function readVarintFromPull(pull) {
  const first = await pull(1);
  const lenBits = first[0] >> 6;
  if (lenBits === 0) return BigInt(first[0] & 0x3f);
  const more = lenBits === 1 ? 1 : lenBits === 2 ? 3 : 7;
  const rest = await pull(more);
  const all = new Uint8Array(1 + more);
  all.set(first, 0); all.set(rest, 1);
  const view = new DataView(all.buffer);
  switch (lenBits) {
    case 1: return BigInt(view.getUint16(0, false) & 0x3fff);
    case 2: return BigInt(view.getUint32(0, false) & 0x3fffffff);
    case 3: return view.getBigUint64(0, false) & 0x3fffffffffffffffn;
  }
}

// Variant of readSubgroupObject for streams whose header indicated
// hasExtensions=false. Skips the extensions-length varint.
export async function readSubgroupObjectNoExt(pull) {
  const objectID = await readVarintFromPull(pull);
  const payloadLen = await readVarintFromPull(pull);
  let payload = new Uint8Array(0);
  let objectStatus = 0;
  if (payloadLen === 0n) {
    objectStatus = Number(await readVarintFromPull(pull));
  } else {
    payload = await pull(Number(payloadLen));
  }
  return { objectID, payload, objectStatus };
}
