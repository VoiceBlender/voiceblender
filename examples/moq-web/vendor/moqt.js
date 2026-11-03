// Generated from github.com/MoQ-it/moqt v0.2.0 (ts/src) by 'make moq-web-client'. MIT License, see LICENSE. Do not edit.

// src/wire.ts
var ALPN = "moqt-22";
var SessionErrorCode = {
  NoError: 0,
  Internal: 1,
  Unauthorized: 2,
  ProtocolViolation: 3,
  InvalidRequestId: 4,
  DuplicateTrackAlias: 5,
  KeyValueFormatting: 6,
  InvalidPath: 8,
  ControlMessageTimeout: 17
};
var RequestErrorCode = {
  Internal: 0,
  Unauthorized: 1,
  Timeout: 2,
  NotSupported: 3,
  GoingAway: 6,
  DoesNotExist: 16,
  Uninterested: 32,
  Redirect: 52
};
var PublishDoneCode = {
  InternalError: 0,
  Unauthorized: 1,
  TrackEnded: 2,
  GoingAway: 4,
  TooFarBehind: 5
};
var ResetCode = {
  InternalError: 0,
  Cancelled: 1,
  DeliveryTimeout: 2,
  SessionClosed: 3
};
var SessionError = class extends Error {
  code;
  constructor(code, reason) {
    super(reason);
    this.code = code;
    this.name = "SessionError";
  }
};
var ShortBuffer = class extends Error {
  constructor() {
    super("unexpected end of data");
    this.name = "ShortBuffer";
  }
};
function protocolViolation(reason) {
  return new SessionError(SessionErrorCode.ProtocolViolation, reason);
}
function varintSize(b0) {
  return Math.clz32(~b0 & 255) - 24 + 1;
}
var encoder = new TextEncoder();
var decoder = new TextDecoder();
var Writer = class {
  buf;
  length = 0;
  constructor(capacity = 64) {
    this.buf = new Uint8Array(capacity);
  }
  ensure(n) {
    if (this.length + n <= this.buf.length) return;
    const next = new Uint8Array(Math.max(this.buf.length * 2, this.length + n));
    next.set(this.buf.subarray(0, this.length));
    this.buf = next;
  }
  u8(v) {
    this.ensure(1);
    this.buf[this.length++] = v & 255;
    return this;
  }
  u16(v) {
    this.ensure(2);
    this.buf[this.length++] = v >> 8 & 255;
    this.buf[this.length++] = v & 255;
    return this;
  }
  bytes(b) {
    this.ensure(b.length);
    this.buf.set(b, this.length);
    this.length += b.length;
    return this;
  }
  varint(v) {
    if (typeof v === "number") {
      if (!Number.isSafeInteger(v) || v < 0) throw new RangeError(`invalid varint ${v}`);
      if (v < 128) return this.u8(v);
      if (v < 16384) return this.u8(128 | v >> 8).u8(v);
      if (v < 2097152) return this.u8(192 | v >> 16).u8(v >> 8).u8(v);
      if (v < 268435456) return this.u8(224 | v >>> 24).u8(v >> 16).u8(v >> 8).u8(v);
      v = BigInt(v);
    }
    if (v < 0n || v >= 1n << 64n) throw new RangeError(`invalid varint ${v}`);
    let n = 9;
    if (v < 1n << 56n) {
      n = 1;
      while (v >= 1n << BigInt(7 * n)) n++;
    }
    if (n === 9) {
      this.u8(255);
      for (let i = 7; i >= 0; i--) this.u8(Number(v >> BigInt(8 * i) & 0xffn));
      return this;
    }
    const out = [];
    for (let i = n - 1; i >= 0; i--) out.push(Number(v >> BigInt(8 * i) & 0xffn));
    out[0] |= 65280 >> n - 1 & 255;
    for (const b of out) this.u8(b);
    return this;
  }
  lengthPrefixed(b) {
    return this.varint(b.length).bytes(b);
  }
  string(s) {
    return this.lengthPrefixed(encoder.encode(s));
  }
  finish() {
    return this.buf.slice(0, this.length);
  }
};
var Reader = class _Reader {
  offset = 0;
  buf;
  constructor(buf) {
    this.buf = buf;
  }
  get remaining() {
    return this.buf.length - this.offset;
  }
  u8() {
    if (this.offset >= this.buf.length) throw new ShortBuffer();
    return this.buf[this.offset++];
  }
  u16() {
    return this.u8() << 8 | this.u8();
  }
  bytes(n) {
    if (n > this.remaining) throw new ShortBuffer();
    const b = this.buf.subarray(this.offset, this.offset + n);
    this.offset += n;
    return b;
  }
  rest() {
    return this.bytes(this.remaining);
  }
  varint64() {
    const b0 = this.u8();
    const n = varintSize(b0);
    if (n > this.remaining + 1) throw new ShortBuffer();
    let v = n === 9 ? 0n : BigInt(b0 & 255 >> n);
    for (let i = 1; i < n; i++) v = v << 8n | BigInt(this.u8());
    return v;
  }
  /** A varint that must fit a JS number (2^53). */
  varint() {
    const b0 = this.buf[this.offset];
    if (b0 === void 0) throw new ShortBuffer();
    const n = varintSize(b0);
    if (n <= 7) {
      if (n > this.remaining) throw new ShortBuffer();
      let v2 = b0 & 255 >> n;
      for (let i = 1; i < n; i++) v2 = v2 * 256 + this.buf[this.offset + i];
      this.offset += n;
      return v2;
    }
    const v = this.varint64();
    if (v > BigInt(Number.MAX_SAFE_INTEGER)) throw protocolViolation(`varint ${v} exceeds 2^53`);
    return Number(v);
  }
  lengthPrefixed(max, what) {
    const n = this.varint();
    if (n > max) throw protocolViolation(`${what} length ${n} exceeds ${max}`);
    return this.bytes(n);
  }
  string(max, what) {
    return decoder.decode(this.lengthPrefixed(max, what));
  }
  sub(n) {
    return new _Reader(this.bytes(n));
  }
};
var MaxNamespaceFields = 32;
var MaxFullTrackNameBytes = 4096;
var MaxReasonPhraseBytes = 1024;
var MaxKeyValueBytes = 65535;
function validateNamespace(ns) {
  if (ns.length > MaxNamespaceFields) throw protocolViolation(`namespace has ${ns.length} fields`);
  let total = 0;
  for (const f of ns) {
    const n = encoder.encode(f).length;
    if (n === 0) throw protocolViolation("empty namespace field");
    total += n;
  }
  if (total > MaxFullTrackNameBytes) throw protocolViolation("namespace too long");
}
function hasPrefix(ns, prefix) {
  return prefix.length <= ns.length && prefix.every((f, i) => ns[i] === f);
}
function renderName(s) {
  let out = "";
  for (const c of encoder.encode(s)) {
    if (c >= 97 && c <= 122 || c >= 65 && c <= 90 || c >= 48 && c <= 57 || c === 95) {
      out += String.fromCharCode(c);
    } else {
      out += "." + c.toString(16).padStart(2, "0");
    }
  }
  return out;
}
function renderNamespace(ns) {
  return ns.map(renderName).join("-");
}
function renderTrack(t) {
  return renderNamespace(t.namespace) + "--" + renderName(t.name);
}
function trackKey(t) {
  return JSON.stringify([t.namespace, t.name]);
}
function writeNamespace(w, ns) {
  w.varint(ns.length);
  for (const f of ns) w.string(f);
  return w;
}
function readNamespace(r) {
  const n = r.varint();
  if (n > MaxNamespaceFields) throw protocolViolation(`namespace has ${n} fields`);
  const ns = [];
  for (let i = 0; i < n; i++) ns.push(r.string(MaxFullTrackNameBytes, "namespace field"));
  validateNamespace(ns);
  return ns;
}
function writeTrack(w, t) {
  return writeNamespace(w, t.namespace).string(t.name);
}
function readTrack(r) {
  const namespace = readNamespace(r);
  const name = r.string(MaxFullTrackNameBytes, "track name");
  return { namespace, name };
}
function writeKeyValues(w, kvs) {
  const sorted = [...kvs].sort((a, b) => a.type - b.type);
  let prev = 0;
  for (const kv of sorted) {
    w.varint(kv.type - prev);
    prev = kv.type;
    if (kv.type % 2 === 0) w.varint(kv.int ?? 0);
    else w.lengthPrefixed(kv.bytes ?? new Uint8Array());
  }
  return w;
}
function readKeyValues(r) {
  const kvs = [];
  let type = 0;
  while (r.remaining > 0) {
    type += r.varint();
    if (type % 2 === 0) kvs.push({ type, int: r.varint() });
    else kvs.push({ type, bytes: r.lengthPrefixed(MaxKeyValueBytes, "key-value") });
  }
  return kvs;
}
var AliasType = { Delete: 0, Register: 1, UseAlias: 2, UseValue: 3 };
function encodeToken(t) {
  const w = new Writer().varint(t.aliasType);
  if (t.aliasType !== AliasType.UseValue) w.varint(t.alias ?? 0);
  if (t.aliasType === AliasType.Register || t.aliasType === AliasType.UseValue) {
    w.varint(t.type ?? 0).bytes(t.value ?? new Uint8Array());
  }
  return w.finish();
}
function decodeToken(b) {
  const r = new Reader(b);
  const t = { aliasType: r.varint() };
  if (t.aliasType > AliasType.UseValue) throw new SessionError(SessionErrorCode.KeyValueFormatting, "token alias type");
  if (t.aliasType !== AliasType.UseValue) t.alias = r.varint();
  if (t.aliasType === AliasType.Register || t.aliasType === AliasType.UseValue) {
    t.type = r.varint();
    t.value = r.rest();
  } else if (r.remaining > 0) {
    throw new SessionError(SessionErrorCode.KeyValueFormatting, "trailing token bytes");
  }
  return t;
}
var Param = {
  ObjectDeliveryTimeout: 2,
  AuthorizationToken: 3,
  RendezvousTimeout: 4,
  SubgroupDeliveryTimeout: 6,
  Expires: 8,
  LargestObject: 9,
  FillTimeout: 10,
  Forward: 16,
  SubscriberPriority: 32,
  LocationFilter: 33,
  GroupOrder: 34,
  FillParameters: 35,
  SubgroupFilter: 37,
  ObjectIdFilter: 38,
  PriorityFilter: 39,
  ObjectPropertyFilter: 40,
  TrackPropertyFilter: 41,
  NewGroupRequest: 50,
  TrackNamespacePrefix: 52,
  IncludeProperties: 53
};
var FilterType = {
  None: 0,
  RelativeStart: 1,
  AbsoluteStart: 2,
  AbsoluteStartGroupEnd: 3,
  AbsoluteRange: 4,
  NextObject: 5
};
function paramEntries(p) {
  const e = [];
  const v = (type, x) => x !== void 0 && e.push([type, (w) => w.varint(x)]);
  const u8 = (type, x) => x !== void 0 && e.push([type, (w) => w.u8(x)]);
  const rf = (type, fs, prop) => fs?.forEach((f) => e.push([type, (w) => writeRangeFilter(w, f, prop)]));
  v(Param.ObjectDeliveryTimeout, p.objectDeliveryTimeout);
  p.authorizationTokens?.forEach((t) => e.push([Param.AuthorizationToken, (w) => w.lengthPrefixed(encodeToken(t))]));
  v(Param.RendezvousTimeout, p.rendezvousTimeout);
  v(Param.SubgroupDeliveryTimeout, p.subgroupDeliveryTimeout);
  v(Param.Expires, p.expires);
  if (p.largestObject) {
    const l = p.largestObject;
    e.push([Param.LargestObject, (w) => w.varint(l.group).varint(l.object)]);
  }
  v(Param.FillTimeout, p.fillTimeout);
  u8(Param.Forward, p.forward);
  u8(Param.SubscriberPriority, p.subscriberPriority);
  if (p.locationFilter) {
    const f = p.locationFilter;
    e.push([Param.LocationFilter, (w) => writeLocationFilter(w, f)]);
  }
  u8(Param.GroupOrder, p.groupOrder);
  if (p.fillParameters) {
    const inner = p.fillParameters;
    e.push([Param.FillParameters, (w) => w.lengthPrefixed(writeParamValues(new Writer(), paramEntries(inner)).finish())]);
  }
  rf(Param.SubgroupFilter, p.subgroupFilters, false);
  rf(Param.ObjectIdFilter, p.objectIdFilters, false);
  rf(Param.PriorityFilter, p.priorityFilters, false);
  rf(Param.ObjectPropertyFilter, p.objectPropertyFilters, true);
  rf(Param.TrackPropertyFilter, p.trackPropertyFilters, true);
  v(Param.NewGroupRequest, p.newGroupRequest);
  if (p.trackNamespacePrefix) {
    const ns = p.trackNamespacePrefix;
    e.push([Param.TrackNamespacePrefix, (w) => writeNamespace(w, ns)]);
  }
  u8(Param.IncludeProperties, p.includeProperties);
  return e;
}
function writeParamValues(w, entries) {
  let prev = 0;
  for (const [type, write] of entries) {
    w.varint(type - prev);
    prev = type;
    write(w);
  }
  return w;
}
function writeParams(w, p = {}) {
  const entries = paramEntries(p);
  return writeParamValues(w.varint(entries.length), entries);
}
function writeLocationFilter(w, f) {
  w.varint(f.type);
  if (f.type === FilterType.RelativeStart) w.varint(f.startGroup ?? 0);
  if (f.type >= FilterType.AbsoluteStart && f.type <= FilterType.AbsoluteRange) w.varint(f.startGroup ?? 0).varint(f.startObject ?? 0);
  if (f.type === FilterType.AbsoluteStartGroupEnd || f.type === FilterType.AbsoluteRange) w.varint(f.endGroupDelta ?? 0);
  if (f.type === FilterType.AbsoluteRange) w.varint(f.endObject ?? 0);
}
function writeRangeFilter(w, f, prop) {
  if (f.ranges.length === 0) {
    w.varint(0);
    return;
  }
  const body = new Writer().u8(f.setId);
  if (prop) body.varint(f.propertyType ?? 0);
  let prevEnd = 0;
  f.ranges.forEach((r, i) => {
    if (r.start < prevEnd) throw new RangeError("ranges must ascend");
    if (r.end === void 0 && i !== f.ranges.length - 1) throw new RangeError("only the last range may be open");
    body.varint(r.start - prevEnd);
    if (r.end !== void 0) {
      body.varint(r.end - r.start);
      prevEnd = r.end;
    }
  });
  w.lengthPrefixed(body.finish());
}
var repeatable = /* @__PURE__ */ new Set([
  Param.AuthorizationToken,
  Param.SubgroupFilter,
  Param.ObjectIdFilter,
  Param.PriorityFilter,
  Param.ObjectPropertyFilter,
  Param.TrackPropertyFilter
]);
function readParamValues(r, p, more) {
  const seen = /* @__PURE__ */ new Set();
  let type = 0;
  for (let i = 0; more(i); i++) {
    const delta = r.varint();
    type += delta;
    if (i > 0 && delta === 0 && !repeatable.has(type)) throw protocolViolation(`duplicate parameter 0x${type.toString(16)}`);
    seen.add(type);
    switch (type) {
      case Param.ObjectDeliveryTimeout:
        p.objectDeliveryTimeout = r.varint();
        break;
      case Param.AuthorizationToken:
        (p.authorizationTokens ??= []).push(decodeToken(r.lengthPrefixed(MaxKeyValueBytes, "token")));
        break;
      case Param.RendezvousTimeout:
        p.rendezvousTimeout = r.varint();
        break;
      case Param.SubgroupDeliveryTimeout:
        p.subgroupDeliveryTimeout = r.varint();
        break;
      case Param.Expires:
        p.expires = r.varint();
        break;
      case Param.LargestObject:
        p.largestObject = { group: r.varint(), object: r.varint() };
        break;
      case Param.FillTimeout:
        p.fillTimeout = r.varint();
        break;
      case Param.Forward: {
        const v = r.u8();
        if (v > 1) throw protocolViolation(`FORWARD ${v}`);
        p.forward = v;
        break;
      }
      case Param.SubscriberPriority:
        p.subscriberPriority = r.u8();
        break;
      case Param.LocationFilter:
        p.locationFilter = readLocationFilter(r);
        break;
      case Param.GroupOrder: {
        const v = r.u8();
        if (v !== 1 && v !== 2) throw protocolViolation(`GROUP_ORDER ${v}`);
        p.groupOrder = v;
        break;
      }
      case Param.FillParameters: {
        const sub = r.sub(r.varint());
        const inner = {};
        readParamValues(sub, inner, () => sub.remaining > 0);
        p.fillParameters = inner;
        break;
      }
      case Param.SubgroupFilter:
        (p.subgroupFilters ??= []).push(readRangeFilter(r, false));
        break;
      case Param.ObjectIdFilter:
        (p.objectIdFilters ??= []).push(readRangeFilter(r, false));
        break;
      case Param.PriorityFilter:
        (p.priorityFilters ??= []).push(readRangeFilter(r, false));
        break;
      case Param.ObjectPropertyFilter:
        (p.objectPropertyFilters ??= []).push(readRangeFilter(r, true));
        break;
      case Param.TrackPropertyFilter:
        (p.trackPropertyFilters ??= []).push(readRangeFilter(r, true));
        break;
      case Param.NewGroupRequest:
        p.newGroupRequest = r.varint();
        break;
      case Param.TrackNamespacePrefix:
        p.trackNamespacePrefix = readNamespace(r);
        break;
      case Param.IncludeProperties: {
        const v = r.u8();
        if (v > 1) throw protocolViolation(`INCLUDE_PROPERTIES ${v}`);
        p.includeProperties = v;
        break;
      }
      default:
        throw protocolViolation(`unknown parameter 0x${type.toString(16)}`);
    }
  }
  return seen;
}
function readParams(r, allowed, msg) {
  const n = r.varint();
  const p = {};
  for (const t of readParamValues(r, p, (i) => i < n)) {
    if (!allowed.includes(t)) throw protocolViolation(`parameter 0x${t.toString(16)} not allowed in ${msg}`);
  }
  return p;
}
function readLocationFilter(r) {
  const f = { type: r.varint() };
  switch (f.type) {
    case FilterType.None:
    case FilterType.NextObject:
      break;
    case FilterType.RelativeStart:
      f.startGroup = r.varint();
      break;
    case FilterType.AbsoluteStart:
      f.startGroup = r.varint();
      f.startObject = r.varint();
      break;
    case FilterType.AbsoluteStartGroupEnd:
      f.startGroup = r.varint();
      f.startObject = r.varint();
      f.endGroupDelta = r.varint();
      break;
    case FilterType.AbsoluteRange:
      f.startGroup = r.varint();
      f.startObject = r.varint();
      f.endGroupDelta = r.varint();
      f.endObject = r.varint();
      break;
    default:
      throw protocolViolation(`location filter type ${f.type}`);
  }
  return f;
}
function readRangeFilter(r, prop) {
  const sub = r.sub(r.varint());
  const f = { setId: 0, ranges: [] };
  if (sub.remaining === 0) return f;
  f.setId = sub.u8();
  if (prop) f.propertyType = sub.varint();
  let prevEnd = 0;
  while (sub.remaining > 0) {
    const start = prevEnd + sub.varint();
    if (sub.remaining === 0) {
      f.ranges.push({ start });
    } else {
      const end = start + sub.varint();
      f.ranges.push({ start, end });
      prevEnd = end;
    }
  }
  return f;
}
var P = Param;
var allowedParams = {
  subscribe: [
    P.ObjectDeliveryTimeout,
    P.AuthorizationToken,
    P.RendezvousTimeout,
    P.SubgroupDeliveryTimeout,
    P.Forward,
    P.SubscriberPriority,
    P.LocationFilter,
    P.GroupOrder,
    P.FillParameters,
    P.SubgroupFilter,
    P.ObjectIdFilter,
    P.PriorityFilter,
    P.ObjectPropertyFilter,
    P.NewGroupRequest,
    P.IncludeProperties
  ],
  subscribeOk: [P.Expires, P.LargestObject],
  publish: [
    P.ObjectDeliveryTimeout,
    P.AuthorizationToken,
    P.SubgroupDeliveryTimeout,
    P.Expires,
    P.LargestObject,
    P.Forward,
    P.SubscriberPriority,
    P.LocationFilter,
    P.GroupOrder
  ],
  publishStateNotify: [P.LargestObject, P.Forward, P.LocationFilter],
  fetch: [
    P.AuthorizationToken,
    P.FillTimeout,
    P.SubscriberPriority,
    P.LocationFilter,
    P.GroupOrder,
    P.SubgroupFilter,
    P.ObjectIdFilter,
    P.PriorityFilter,
    P.ObjectPropertyFilter,
    P.IncludeProperties
  ],
  fetchOk: [],
  trackStatus: [P.AuthorizationToken, P.IncludeProperties],
  namespaceRequest: [P.AuthorizationToken],
  subscribeTracks: [
    P.AuthorizationToken,
    P.Forward,
    P.GroupOrder,
    P.SubgroupFilter,
    P.ObjectIdFilter,
    P.PriorityFilter,
    P.ObjectPropertyFilter,
    P.TrackPropertyFilter,
    P.IncludeProperties
  ],
  requestOk: [P.Expires, P.LargestObject],
  requestUpdate: [
    P.ObjectDeliveryTimeout,
    P.AuthorizationToken,
    P.SubgroupDeliveryTimeout,
    P.Forward,
    P.SubscriberPriority,
    P.LocationFilter,
    P.FillParameters,
    P.SubgroupFilter,
    P.ObjectIdFilter,
    P.PriorityFilter,
    P.ObjectPropertyFilter,
    P.TrackPropertyFilter,
    P.NewGroupRequest,
    P.TrackNamespacePrefix
  ]
};
var MsgType = {
  RequestUpdate: 2,
  Subscribe: 3,
  SubscribeOk: 4,
  RequestError: 5,
  PublishNamespace: 6,
  RequestOk: 7,
  Namespace: 8,
  PublishDone: 11,
  TrackStatus: 13,
  NamespaceDone: 14,
  PublishSkipped: 15,
  Goaway: 16,
  Fetch: 22,
  FetchOk: 24,
  Publish: 29,
  PublishStateNotify: 34,
  SubscribeNamespace: 80,
  SubscribeTracks: 81,
  Setup: 12032
};
var SetupOption = {
  Path: 1,
  AuthorizationToken: 3,
  MaxAuthTokenCacheSize: 4,
  Authority: 5,
  MaxFilterRanges: 6,
  Implementation: 7,
  MaxRequestUpdates: 8
};
var StreamCountUnknown = (1n << 64n) - 1n;
var requestTypes = /* @__PURE__ */ new Set([
  MsgType.TrackStatus,
  MsgType.Subscribe,
  MsgType.Publish,
  MsgType.Fetch,
  MsgType.PublishNamespace,
  MsgType.SubscribeNamespace,
  MsgType.SubscribeTracks
]);
function isRequest(type) {
  return requestTypes.has(type);
}
function encodeMessage(m) {
  const body = new Writer();
  switch (m.type) {
    case MsgType.Setup:
      writeKeyValues(body, m.options);
      break;
    case MsgType.Goaway:
      body.string(m.newSessionUri).varint(m.timeout);
      break;
    case MsgType.RequestOk:
      writeKeyValues(writeParams(body, m.params), m.trackProperties);
      break;
    case MsgType.RequestError:
      body.varint(m.code).varint(m.retryInterval).string(m.reason);
      if (m.redirect) writeTrack(body.string(m.redirect.connectUri), m.redirect.track);
      break;
    case MsgType.RequestUpdate:
      writeParams(body.varint(m.requestId), m.params);
      break;
    case MsgType.Subscribe:
    case MsgType.Fetch:
    case MsgType.TrackStatus:
      writeParams(writeTrack(body.varint(m.requestId), m.track), m.params);
      break;
    case MsgType.SubscribeOk:
      writeKeyValues(writeParams(body.varint(m.trackAlias), m.params), m.trackProperties);
      break;
    case MsgType.Publish:
      writeKeyValues(writeParams(writeTrack(body.varint(m.requestId), m.track).varint(m.trackAlias), m.params), m.trackProperties);
      break;
    case MsgType.PublishDone:
      body.varint(m.statusCode).varint(m.streamCount).string(m.reason);
      break;
    case MsgType.PublishStateNotify:
      writeParams(body, m.params);
      break;
    case MsgType.FetchOk:
      writeKeyValues(writeParams(body.u8(m.endOfTrack ? 1 : 0).varint(m.endLocation.group).varint(m.endLocation.object), m.params), m.trackProperties);
      break;
    case MsgType.PublishNamespace:
    case MsgType.SubscribeNamespace:
    case MsgType.SubscribeTracks:
      writeParams(writeNamespace(body.varint(m.requestId), m.prefix), m.params);
      break;
    case MsgType.Namespace:
    case MsgType.NamespaceDone:
      writeNamespace(body, m.suffix);
      break;
    case MsgType.PublishSkipped:
      writeNamespace(body, m.suffix).string(m.trackName);
      break;
  }
  if (body.length > 65535) throw new RangeError("message body exceeds 65535 bytes");
  return new Writer(body.length + 4).varint(m.type).u16(body.length).bytes(body.finish()).finish();
}
function parseMessage(type, body) {
  const r = new Reader(body);
  let m;
  try {
    m = decodeBody(type, r);
  } catch (e) {
    if (e instanceof ShortBuffer) throw protocolViolation(`message 0x${type.toString(16)} body too short`);
    throw e;
  }
  if (r.remaining > 0) throw protocolViolation(`message 0x${type.toString(16)} has ${r.remaining} trailing bytes`);
  return m;
}
function decodeBody(type, r) {
  const A = allowedParams;
  switch (type) {
    case MsgType.Setup:
      return { type, options: readKeyValues(r) };
    case MsgType.Goaway:
      return { type, newSessionUri: r.string(8192, "GOAWAY URI"), timeout: r.varint() };
    case MsgType.RequestOk:
      return { type, params: readParams(r, A.requestOk, "REQUEST_OK"), trackProperties: readKeyValues(r) };
    case MsgType.RequestError: {
      const code = r.varint();
      const retryInterval = r.varint();
      const reason = r.string(MaxReasonPhraseBytes, "reason");
      if (code !== RequestErrorCode.Redirect) return { type, code, retryInterval, reason };
      const connectUri = r.string(8192, "redirect URI");
      return { type, code, retryInterval, reason, redirect: { connectUri, track: readTrack(r) } };
    }
    case MsgType.RequestUpdate:
      return { type, requestId: r.varint(), params: readParams(r, A.requestUpdate, "REQUEST_UPDATE") };
    case MsgType.Subscribe:
      return { type, requestId: r.varint(), track: readTrack(r), params: readParams(r, A.subscribe, "SUBSCRIBE") };
    case MsgType.Fetch:
      return { type, requestId: r.varint(), track: readTrack(r), params: readParams(r, A.fetch, "FETCH") };
    case MsgType.TrackStatus:
      return { type, requestId: r.varint(), track: readTrack(r), params: readParams(r, A.trackStatus, "TRACK_STATUS") };
    case MsgType.SubscribeOk:
      return { type, trackAlias: r.varint(), params: readParams(r, A.subscribeOk, "SUBSCRIBE_OK"), trackProperties: readKeyValues(r) };
    case MsgType.Publish: {
      const requestId = r.varint();
      const track = readTrack(r);
      const trackAlias = r.varint();
      return { type, requestId, track, trackAlias, params: readParams(r, A.publish, "PUBLISH"), trackProperties: readKeyValues(r) };
    }
    case MsgType.PublishDone:
      return { type, statusCode: r.varint(), streamCount: r.varint64(), reason: r.string(MaxReasonPhraseBytes, "reason") };
    case MsgType.PublishStateNotify:
      return { type, params: readParams(r, A.publishStateNotify, "PUBLISH_STATE_NOTIFY") };
    case MsgType.FetchOk: {
      const eot = r.u8();
      if (eot > 1) throw protocolViolation(`FETCH_OK end of track ${eot}`);
      const endLocation = { group: r.varint(), object: r.varint() };
      return { type, endOfTrack: eot === 1, endLocation, params: readParams(r, A.fetchOk, "FETCH_OK"), trackProperties: readKeyValues(r) };
    }
    case MsgType.PublishNamespace:
    case MsgType.SubscribeNamespace:
      return { type, requestId: r.varint(), prefix: readNamespace(r), params: readParams(r, A.namespaceRequest, "namespace request") };
    case MsgType.SubscribeTracks:
      return { type, requestId: r.varint(), prefix: readNamespace(r), params: readParams(r, A.subscribeTracks, "SUBSCRIBE_TRACKS") };
    case MsgType.Namespace:
    case MsgType.NamespaceDone:
      return { type, suffix: readNamespace(r) };
    case MsgType.PublishSkipped:
      return { type, suffix: readNamespace(r), trackName: r.string(MaxFullTrackNameBytes, "track name") };
  }
  throw protocolViolation(`unknown message type 0x${type.toString(16)}`);
}
function decodeMessage(b) {
  const r = new Reader(b);
  const type = r.varint();
  const len = r.u16();
  const body = r.bytes(len);
  if (r.remaining > 0) throw protocolViolation("bytes after message");
  return parseMessage(type, body);
}
var Prop = {
  ObjectDeliveryTimeout: 2,
  MaxCacheDuration: 4,
  SubgroupDeliveryTimeout: 6,
  Immutable: 11,
  DefaultPublisherPriority: 14,
  DefaultPublisherGroupOrder: 34,
  DynamicGroups: 48,
  PriorGroupIdGap: 60,
  PriorObjectIdGap: 62
};
function lookupProperty(props, type) {
  const direct = props.find((kv) => kv.type === type);
  if (direct) return direct;
  const imm = props.find((kv) => kv.type === Prop.Immutable);
  if (imm?.bytes) return readKeyValues(new Reader(imm.bytes)).find((kv) => kv.type === type);
  return void 0;
}
function parseTrackInfo(props) {
  const info = { publisherPriority: 128, groupOrder: 1, dynamicGroups: false };
  const prio = lookupProperty(props, Prop.DefaultPublisherPriority);
  if (prio) {
    if ((prio.int ?? 0) > 255) throw protocolViolation("publisher priority");
    info.publisherPriority = prio.int ?? 0;
  }
  const order = lookupProperty(props, Prop.DefaultPublisherGroupOrder);
  if (order) {
    if (order.int !== 1 && order.int !== 2) throw protocolViolation("group order");
    info.groupOrder = order.int;
  }
  const dyn = lookupProperty(props, Prop.DynamicGroups);
  if (dyn) {
    if ((dyn.int ?? 0) > 1) throw protocolViolation("dynamic groups");
    info.dynamicGroups = dyn.int === 1;
  }
  return info;
}
var StreamType = { Fetch: 5, Setup: 12032, Padding: 321601064 };
var DatagramPadding = 321601065;
function isSubgroupStreamType(t) {
  return t < 128 && (t & 16) !== 0;
}
var ObjectStatus = { Normal: 0, EndOfGroup: 3, EndOfTrack: 4 };
function validStatus(s) {
  return s === ObjectStatus.Normal || s === ObjectStatus.EndOfGroup || s === ObjectStatus.EndOfTrack;
}
var SubgroupIdMode = { Zero: 0, FirstObject: 1, Explicit: 2 };
function encodeSubgroupHeader(h) {
  let flags = 16 | h.idMode << 1;
  if (h.properties) flags |= 1;
  if (h.endOfGroup) flags |= 8;
  if (h.priority === void 0) flags |= 32;
  if (h.firstObject) flags |= 64;
  const w = new Writer().varint(flags).varint(h.trackAlias).varint(h.group);
  if (h.idMode === SubgroupIdMode.Explicit) w.varint(h.subgroupId);
  if (h.priority !== void 0) w.u8(h.priority);
  return w.finish();
}
function subgroupHeaderFlags(type) {
  const idMode = (type & 6) >> 1;
  if (!isSubgroupStreamType(type) || idMode === 3) throw protocolViolation(`subgroup header type 0x${type.toString(16)}`);
  return {
    idMode,
    properties: (type & 1) !== 0,
    endOfGroup: (type & 8) !== 0,
    hasPriority: (type & 32) === 0,
    firstObject: (type & 64) !== 0
  };
}
var SubgroupEncoder = class {
  prev = -1;
  header;
  constructor(header) {
    this.header = header;
  }
  encode(o, w = new Writer()) {
    if (o.id <= this.prev) throw new RangeError("object IDs must increase");
    if (!validStatus(o.status) || o.status !== ObjectStatus.Normal && (o.payload.length > 0 || o.properties?.length)) {
      throw new RangeError("non-Normal objects carry no payload or properties");
    }
    if (o.properties?.length && !this.header.properties) throw new RangeError("header does not carry properties");
    w.varint(this.prev < 0 ? o.id : o.id - this.prev - 1);
    this.prev = o.id;
    if (this.header.properties) w.lengthPrefixed(writeKeyValues(new Writer(), o.properties ?? []).finish());
    w.varint(o.payload.length);
    if (o.payload.length === 0) w.varint(o.status);
    else w.bytes(o.payload);
    return w;
  }
};
function encodeDatagram(d) {
  const statusForm = d.status !== ObjectStatus.Normal || d.payload.length === 0;
  if (d.status !== ObjectStatus.Normal && (d.payload.length > 0 || d.properties?.length || d.endOfGroup)) {
    throw new RangeError("non-Normal datagram carries no payload, properties or END_OF_GROUP");
  }
  if (d.endOfGroup && statusForm) throw new RangeError("empty datagram cannot set END_OF_GROUP");
  let flags = 0;
  if (d.properties?.length) flags |= 1;
  if (d.endOfGroup) flags |= 2;
  if (d.object === 0) flags |= 4;
  if (d.priority === void 0) flags |= 8;
  if (statusForm) flags |= 32;
  const w = new Writer().varint(flags).varint(d.trackAlias).varint(d.group);
  if (d.object !== 0) w.varint(d.object);
  if (d.priority !== void 0) w.u8(d.priority);
  if (d.properties?.length) w.lengthPrefixed(writeKeyValues(new Writer(), d.properties).finish());
  return statusForm ? w.varint(d.status).finish() : w.bytes(d.payload).finish();
}
function parseDatagram(b) {
  const r = new Reader(b);
  try {
    const flags = r.varint();
    if (flags === DatagramPadding) return void 0;
    if (flags >= 128 || (flags & ~47) !== 0 || (flags & 34) === 34) throw protocolViolation(`datagram type 0x${flags.toString(16)}`);
    const d = { trackAlias: r.varint(), group: r.varint(), object: 0, status: ObjectStatus.Normal, payload: new Uint8Array() };
    if ((flags & 2) !== 0) d.endOfGroup = true;
    if ((flags & 4) === 0) d.object = r.varint();
    if ((flags & 8) === 0) d.priority = r.u8();
    if ((flags & 1) !== 0) {
      const n = r.varint();
      if (n === 0) throw protocolViolation("empty datagram properties");
      d.properties = readKeyValues(r.sub(n));
    }
    if ((flags & 32) !== 0) {
      d.status = r.varint();
      if (!validStatus(d.status)) throw protocolViolation(`object status ${d.status}`);
      if (d.status !== ObjectStatus.Normal && d.properties) throw protocolViolation("properties on non-Normal object");
      if (r.remaining > 0) throw protocolViolation("bytes after datagram status");
    } else {
      d.payload = r.rest();
    }
    return d;
  } catch (e) {
    if (e instanceof ShortBuffer) throw protocolViolation("truncated datagram");
    throw e;
  }
}

// src/stream.ts
var MaxObjectPayload = 16 << 20;
var MaxObjectProperties = 256 << 10;
var StreamReader = class {
  reader;
  buf = new Uint8Array(0);
  off = 0;
  eof = false;
  ended = false;
  /** @internal Called once the stream has ended: FIN read, reset, or cancelled. */
  onEnd;
  constructor(stream) {
    this.reader = stream.getReader();
  }
  end() {
    if (this.ended) return;
    this.ended = true;
    this.onEnd?.();
  }
  get buffered() {
    return this.buf.length - this.off;
  }
  /** Ensures n bytes are buffered; false if the stream ends first. */
  async fill(n) {
    while (this.buffered < n) {
      if (this.eof) return false;
      let r;
      try {
        r = await this.reader.read();
      } catch (e) {
        this.end();
        throw e;
      }
      const { value, done } = r;
      if (done) {
        this.eof = true;
        this.end();
        return this.buffered >= n;
      }
      if (this.off > 0 || this.buf.length === 0) {
        const rest = this.buf.subarray(this.off);
        const next = new Uint8Array(rest.length + value.length);
        next.set(rest);
        next.set(value, rest.length);
        this.buf = next;
        this.off = 0;
      } else {
        const next = new Uint8Array(this.buf.length + value.length);
        next.set(this.buf);
        next.set(value, this.buf.length);
        this.buf = next;
      }
    }
    return true;
  }
  /** True when the stream ended cleanly with nothing left to read. */
  async done() {
    return !await this.fill(1);
  }
  /** Reads n bytes; throws if the stream ends first. */
  async exact(n) {
    if (!await this.fill(n)) throw new EndOfStream();
    const b = this.buf.slice(this.off, this.off + n);
    this.off += n;
    return b;
  }
  async u8() {
    return (await this.exact(1))[0];
  }
  async varint() {
    if (!await this.fill(1)) throw new EndOfStream();
    const n = varintSize(this.buf[this.off]);
    return new Reader(await this.exact(n)).varint();
  }
  /** Reads one control message. */
  async message() {
    const type = await this.varint();
    return this.messageBody(type);
  }
  /** Reads the length and body of a message whose type was already read. */
  async messageBody(type) {
    const len = new Reader(await this.exact(2)).u16();
    return parseMessage(type, await this.exact(len));
  }
  /** STOP_SENDING / cancel. */
  async cancel(reason) {
    this.end();
    await this.reader.cancel(reason).catch(() => {
    });
  }
};
var EndOfStream = class extends Error {
  constructor() {
    super("stream ended");
    this.name = "EndOfStream";
  }
};
async function readSubgroupHeader(r, type) {
  const f = subgroupHeaderFlags(type);
  const h = {
    trackAlias: await r.varint(),
    group: await r.varint(),
    idMode: f.idMode,
    subgroupId: 0,
    properties: f.properties,
    endOfGroup: f.endOfGroup,
    firstObject: f.firstObject
  };
  if (h.idMode === 2) h.subgroupId = await r.varint();
  if (f.hasPriority) h.priority = await r.u8();
  return h;
}
var SubgroupDecoder = class {
  prev = -1;
  r;
  header;
  constructor(r, header) {
    this.r = r;
    this.header = header;
  }
  /** The next Object, or undefined at the clean end of the subgroup. */
  async next() {
    if (await this.r.done()) return void 0;
    try {
      const delta = await this.r.varint();
      const id = this.prev < 0 ? delta : this.prev + delta + 1;
      this.prev = id;
      let properties;
      if (this.header.properties) {
        const n = await this.r.varint();
        if (n > MaxObjectProperties) throw protocolViolation(`object properties of ${n} bytes`);
        properties = readKeyValues(new Reader(await this.r.exact(n)));
      }
      const len = await this.r.varint();
      if (len === 0) {
        const status = await this.r.varint();
        if (status !== ObjectStatus.Normal && status !== ObjectStatus.EndOfGroup && status !== ObjectStatus.EndOfTrack) {
          throw protocolViolation(`object status ${status}`);
        }
        if (status !== ObjectStatus.Normal && properties?.length) throw protocolViolation("properties on non-Normal object");
        return { id, properties, status, payload: new Uint8Array() };
      }
      if (len > MaxObjectPayload) throw protocolViolation(`object payload of ${len} bytes`);
      return { id, properties, status: ObjectStatus.Normal, payload: await this.r.exact(len) };
    } catch (e) {
      if (e instanceof EndOfStream) throw protocolViolation("stream ended inside an object");
      throw e;
    }
  }
};
var FetchRange = { Object: 0, NonExistent: 140, Unknown: 268, TimedOut: 524 };
var FetchDecoder = class {
  constructor(r, descending = false) {
    this.r = r;
    this.descending = descending;
  }
  r;
  descending;
  started = false;
  haveObject = false;
  group = 0;
  obj = 0;
  subgroup = 0;
  priority = 0;
  /** The next entry, or undefined at the clean end of the stream. */
  async next() {
    if (await this.r.done()) return void 0;
    try {
      return await this.entry();
    } catch (e) {
      if (e instanceof EndOfStream) throw protocolViolation("stream ended inside an object");
      throw e;
    }
  }
  async entry() {
    const r = this.r;
    const flags = await r.varint();
    if (flags >= 128) {
      if (flags !== FetchRange.NonExistent && flags !== FetchRange.Unknown && flags !== FetchRange.TimedOut) {
        throw protocolViolation(`fetch serialization flags 0x${flags.toString(16)}`);
      }
      const group2 = await r.varint();
      const object2 = await r.varint();
      this.started = true;
      this.group = group2;
      this.obj = object2;
      return { range: flags, group: group2, object: object2, datagram: false, subgroupId: 0, priority: 0, payload: new Uint8Array() };
    }
    if (!this.started && (flags & 12) !== 12) throw protocolViolation("first fetch entry must carry group and object");
    let group = this.group;
    let object;
    if (flags & 8) {
      const delta = await r.varint();
      if (!this.started) group = delta;
      else if (this.descending) {
        if (delta >= this.group) throw protocolViolation("fetch group underflow");
        group = this.group - delta - 1;
      } else group = this.group + delta + 1;
    }
    const datagram = (flags & 64) !== 0;
    let subgroupId = 0;
    if (!datagram) {
      switch (flags & 3) {
        case 1:
        case 2:
          if (!this.haveObject) throw protocolViolation("fetch entry references a prior subgroup that does not exist");
          subgroupId = this.subgroup + (flags & 3) - 1;
          break;
        case 3:
          subgroupId = await r.varint();
          break;
      }
    }
    if (flags & 4) {
      const delta = await r.varint();
      object = !this.started || flags & 8 ? delta : this.obj + delta;
    } else object = this.obj + 1;
    this.started = true;
    this.group = group;
    this.obj = object;
    let priority;
    if (flags & 16) priority = await r.u8();
    else {
      if (!this.haveObject) throw protocolViolation("fetch entry references a prior priority that does not exist");
      priority = this.priority;
    }
    let properties;
    if (flags & 32) {
      const n = await r.varint();
      if (n > MaxObjectProperties) throw protocolViolation(`object properties of ${n} bytes`);
      properties = readKeyValues(new Reader(await r.exact(n)));
    }
    const len = await r.varint();
    if (len > MaxObjectPayload) throw protocolViolation(`object payload of ${len} bytes`);
    const payload = len > 0 ? await r.exact(len) : new Uint8Array();
    this.haveObject = true;
    this.priority = priority;
    if (!datagram) this.subgroup = subgroupId;
    return { range: FetchRange.Object, group, object, datagram, subgroupId, priority, properties, payload };
  }
};

// src/session.ts
var Implementation = "moqt-ts/0.1 (draft-22)";
var encoder2 = new TextEncoder();
var RequestError = class extends Error {
  code;
  retryInterval;
  redirect;
  constructor(code, reason, retryInterval = 0, redirect) {
    super(reason || `request rejected (0x${code.toString(16)})`);
    this.name = "RequestError";
    this.code = code;
    this.retryInterval = retryInterval;
    this.redirect = redirect;
  }
};
var Queue = class {
  items = [];
  waiters = [];
  closed = false;
  push(v) {
    if (this.closed) return false;
    const w = this.waiters.shift();
    if (w) w(v);
    else this.items.push(v);
    return true;
  }
  get length() {
    return this.items.length;
  }
  close() {
    this.closed = true;
    for (const w of this.waiters) w(void 0);
    this.waiters = [];
  }
  next() {
    if (this.items.length > 0) return Promise.resolve(this.items.shift());
    if (this.closed) return Promise.resolve(void 0);
    return new Promise((r) => this.waiters.push(r));
  }
};
function streamError(code) {
  const WTE = globalThis.WebTransportError;
  try {
    if (WTE) return new WTE("moqt", { streamErrorCode: code });
  } catch {
  }
  return new Error(`moqt stream error ${code}`);
}
var RequestStream = class {
  constructor(session, stream) {
    this.session = session;
    this.reader = new StreamReader(stream.readable);
    this.writer = stream.writable.getWriter();
  }
  session;
  reader;
  writer;
  async send(m) {
    await this.writer.write(encodeMessage(m));
  }
  /** Next message; undefined on a clean FIN. Decoding errors close the session. */
  async recv() {
    try {
      if (await this.reader.done()) return void 0;
      return await this.reader.message();
    } catch (e) {
      if (e instanceof SessionError) this.session.fail(e);
      if (e instanceof EndOfStream) {
        const err = protocolViolation("request stream ended inside a message");
        this.session.fail(err);
        throw err;
      }
      throw e;
    }
  }
  /** FIN: nothing more to send. */
  finish() {
    this.writer.close().catch(() => {
    });
  }
  /** Cancel the request in both directions (Section 6.4.2.3). */
  cancel(code = ResetCode.Cancelled) {
    this.writer.abort(streamError(code)).catch(() => {
    });
    this.reader.cancel(streamError(code));
  }
  async reject(code, reason) {
    await this.send({ type: MsgType.RequestError, code, retryInterval: 0, reason }).catch(() => {
    });
    this.finish();
  }
};
var Session = class _Session {
  wt;
  handler;
  peerSetup = [];
  /** Resolves with the peer's GOAWAY. */
  goaway;
  /** Resolves when the session ends. */
  closed;
  ctrl;
  nextRequestId = 0;
  nextTrackAlias = 0;
  peerIds = /* @__PURE__ */ new Set();
  subs = /* @__PURE__ */ new Map();
  fetches = /* @__PURE__ */ new Map();
  aliasWaiters = /* @__PURE__ */ new Map();
  gotControl = false;
  resolveGoaway;
  resolveClosed;
  resolveSetup;
  rejectSetup;
  ended = false;
  datagramWriter;
  aliasWaitMs;
  constructor(wt, opts) {
    this.wt = wt;
    this.handler = opts.handler ?? {};
    this.aliasWaitMs = opts.aliasWaitMs ?? 2e3;
    this.goaway = new Promise((r) => this.resolveGoaway = r);
    this.closed = new Promise((r) => this.resolveClosed = r);
  }
  /** Connects, exchanges SETUP and returns the established session. */
  static async connect(opts) {
    const wtOpts = { protocols: [ALPN] };
    if (opts.serverCertificateHashes) {
      wtOpts.serverCertificateHashes = opts.serverCertificateHashes.map((value) => ({ algorithm: "sha-256", value }));
    }
    if (typeof WebTransport === "undefined") throw new Error("this browser does not support WebTransport");
    const wt = new WebTransport(opts.url, wtOpts);
    let timer;
    const expired = new Promise((_, reject) => {
      timer = setTimeout(() => reject(new Error(`timed out opening a WebTransport session to ${opts.url}`)), opts.readyTimeoutMs ?? 1e4);
    });
    try {
      await Promise.race([wt.ready, expired]);
    } catch (e) {
      try {
        wt.close();
      } catch {
      }
      throw e;
    } finally {
      clearTimeout(timer);
    }
    const negotiated = wt.protocol;
    if (negotiated !== void 0 && negotiated !== "" && negotiated !== ALPN) {
      wt.close({ closeCode: SessionErrorCode.ProtocolViolation, reason: "protocol" });
      throw new Error(`server negotiated ${negotiated}, not ${ALPN}`);
    }
    const s = new _Session(wt, opts);
    await s.start(opts);
    return s;
  }
  async start(opts) {
    const setupDone = new Promise((resolve, reject) => {
      this.resolveSetup = resolve;
      this.rejectSetup = reject;
    });
    this.wt.closed.then(
      (info) => this.terminate(new Error(`session closed (${info.closeCode}): ${info.reason}`)),
      (e) => this.terminate(e instanceof Error ? e : new Error(String(e)))
    );
    const timeout = setTimeout(() => {
      this.close(SessionErrorCode.ControlMessageTimeout, "no SETUP");
      this.rejectSetup(new Error("timed out waiting for SETUP"));
    }, opts.setupTimeoutMs ?? 1e4);
    void this.acceptUniStreams();
    void this.acceptRequests();
    void this.receiveDatagrams();
    try {
      await Promise.all([this.sendSetup(opts), setupDone]);
    } finally {
      clearTimeout(timeout);
    }
  }
  async sendSetup(opts) {
    this.ctrl = (await this.wt.createUnidirectionalStream()).getWriter();
    const options = [{ type: SetupOption.Implementation, bytes: encoder2.encode(Implementation) }, ...opts.setupOptions ?? []];
    if (opts.authorizationToken) {
      const token = encodeToken({ aliasType: AliasType.UseValue, type: opts.authorizationTokenType ?? 0, value: opts.authorizationToken });
      options.push({ type: SetupOption.AuthorizationToken, bytes: token });
    }
    await this.ctrl.write(encodeMessage({ type: MsgType.Setup, options }));
  }
  close(code = SessionErrorCode.NoError, reason = "") {
    if (!this.ended) {
      try {
        this.wt.close({ closeCode: code, reason });
      } catch {
      }
    }
    this.terminate(new SessionError(code, reason || "closed"));
  }
  /** Closes the session because of err (Section 12). */
  fail(err) {
    if (this.ended) return;
    const se = err instanceof SessionError ? err : new SessionError(SessionErrorCode.Internal, String(err));
    console.warn("moqt: closing session", se.code, se.message);
    this.close(se.code, se.message);
  }
  terminate(err) {
    if (this.ended) return;
    this.ended = true;
    this.rejectSetup?.(err);
    for (const sub of this.subs.values()) sub.finish(void 0, err);
    for (const f of this.fetches.values()) f.finish(err);
    this.resolveClosed(err);
  }
  takeRequestId() {
    const id = this.nextRequestId;
    this.nextRequestId += 2;
    return id;
  }
  async acceptUniStreams() {
    const reader = this.wt.incomingUnidirectionalStreams.getReader();
    for (; ; ) {
      const { value, done } = await reader.read().catch(() => ({ value: void 0, done: true }));
      if (done || !value) return;
      void this.handleUniStream(value).catch((e) => {
        if (e instanceof SessionError) this.fail(e);
      });
    }
  }
  openStreams = /* @__PURE__ */ new Map();
  /**
   * Diagnostics: incoming unidirectional streams that have not ended yet
   * (FIN read, reset or cancelled), by what they carry: a track's name,
   * "fetch", "padding", "control" or "untyped".
   */
  openIncomingStreams() {
    return Object.fromEntries(this.openStreams);
  }
  countStream(label, delta) {
    const n = (this.openStreams.get(label) ?? 0) + delta;
    if (n > 0) this.openStreams.set(label, n);
    else this.openStreams.delete(label);
  }
  // Counts r as open under label until it ends; relabelling moves it.
  countOpen(r, label) {
    let current = label;
    this.countStream(current, 1);
    r.onEnd = () => this.countStream(current, -1);
    return (next) => {
      this.countStream(current, -1);
      current = next;
      this.countStream(current, 1);
    };
  }
  async handleUniStream(stream) {
    const r = new StreamReader(stream);
    const relabel = this.countOpen(r, "untyped");
    let type;
    try {
      type = await r.varint();
    } catch {
      return;
    }
    if (type === StreamType.Setup) {
      relabel("control");
      return this.readControl(r);
    }
    if (isSubgroupStreamType(type)) return this.handleSubgroup(r, type, relabel);
    if (type === StreamType.Padding) {
      relabel("padding");
      while (!await r.done()) await r.exact(1);
      return;
    }
    if (type === StreamType.Fetch) {
      relabel("fetch");
      const f = this.fetches.get(await r.varint());
      if (f) f.deliver(r);
      else await r.cancel(streamError(ResetCode.Cancelled));
      return;
    }
    this.fail(protocolViolation(`unknown stream type 0x${type.toString(16)}`));
  }
  async readControl(r) {
    if (this.gotControl) return this.fail(protocolViolation("second control stream"));
    this.gotControl = true;
    const setup = await r.messageBody(MsgType.Setup);
    if (setup.type !== MsgType.Setup) return;
    this.peerSetup = setup.options;
    this.resolveSetup();
    for (; ; ) {
      let m;
      try {
        m = await r.message();
      } catch (e) {
        if (e instanceof SessionError) return this.fail(e);
        if (e instanceof EndOfStream) return this.fail(protocolViolation("control stream closed"));
        return;
      }
      if (m.type !== MsgType.Goaway) return this.fail(protocolViolation(`message 0x${m.type.toString(16)} on control stream`));
      this.resolveGoaway({ newSessionUri: m.newSessionUri, timeout: m.timeout });
    }
  }
  async handleSubgroup(r, type, relabel) {
    const header = await readSubgroupHeader(r, type);
    relabel(`alias ${header.trackAlias}`);
    const sub = await this.waitSubscription(header.trackAlias);
    if (sub) relabel([...sub.track.namespace, sub.track.name].join("/"));
    if (!sub) {
      await r.cancel(streamError(ResetCode.Cancelled));
      return;
    }
    sub.deliverSubgroup(new IncomingSubgroup(this, header, r));
  }
  waitSubscription(alias) {
    const sub = this.subs.get(alias);
    if (sub) return Promise.resolve(sub);
    return new Promise((resolve) => {
      const list = this.aliasWaiters.get(alias) ?? [];
      const timer = setTimeout(() => resolve(this.subs.get(alias)), this.aliasWaitMs);
      list.push(() => {
        clearTimeout(timer);
        resolve(this.subs.get(alias));
      });
      this.aliasWaiters.set(alias, list);
    });
  }
  /** @internal */
  registerAlias(alias, sub) {
    if (this.subs.has(alias)) throw new SessionError(SessionErrorCode.DuplicateTrackAlias, `track alias ${alias}`);
    this.subs.set(alias, sub);
    for (const w of this.aliasWaiters.get(alias) ?? []) w();
    this.aliasWaiters.delete(alias);
  }
  /** @internal Forget a finished subscription after late Objects stop arriving. */
  releaseAlias(alias, sub) {
    setTimeout(() => {
      if (this.subs.get(alias) === sub) this.subs.delete(alias);
    }, 3e4);
  }
  async receiveDatagrams() {
    const reader = this.wt.datagrams.readable.getReader();
    for (; ; ) {
      const { value, done } = await reader.read().catch(() => ({ value: void 0, done: true }));
      if (done || !value) return;
      let d;
      try {
        d = parseDatagram(value);
      } catch (e) {
        return this.fail(e);
      }
      if (d) this.subs.get(d.trackAlias)?.deliverDatagram(d);
    }
  }
  /** @internal */
  async sendDatagram(b) {
    this.datagramWriter ??= this.wt.datagrams.writable.getWriter();
    await this.datagramWriter.write(b);
  }
  async acceptRequests() {
    const reader = this.wt.incomingBidirectionalStreams.getReader();
    for (; ; ) {
      const { value, done } = await reader.read().catch(() => ({ value: void 0, done: true }));
      if (done || !value) return;
      void this.handleRequest(new RequestStream(this, value)).catch(() => {
      });
    }
  }
  async handleRequest(rs) {
    const m = await rs.recv();
    if (!m) return;
    if (!isRequest(m.type)) return this.fail(protocolViolation(`request stream starts with 0x${m.type.toString(16)}`));
    const id = m.requestId;
    if (!this.checkPeerRequestId(id)) return;
    switch (m.type) {
      case MsgType.Subscribe:
        if (this.handler.onSubscribe) return this.handler.onSubscribe(new SubscribeRequest(this, rs, m.requestId, m.track, m.params));
        break;
      case MsgType.PublishNamespace:
        if (this.handler.onPublishNamespace) return this.handler.onPublishNamespace(new PublishNamespaceRequest(rs, m.prefix, m.params));
        break;
      case MsgType.SubscribeNamespace:
        if (this.handler.onSubscribeNamespace) return this.handler.onSubscribeNamespace(new SubscribeNamespaceRequest(rs, m.prefix, m.params));
        break;
    }
    await rs.reject(RequestErrorCode.NotSupported, "not supported");
  }
  /** @internal Section 6.4.2.1: server IDs are odd and never repeat. */
  checkPeerRequestId(id) {
    if (id % 2 !== 1 || this.peerIds.has(id)) {
      this.fail(new SessionError(SessionErrorCode.InvalidRequestId, `request ID ${id}`));
      return false;
    }
    this.peerIds.add(id);
    return true;
  }
  async openRequest(m) {
    const rs = new RequestStream(this, await this.wt.createBidirectionalStream());
    await rs.send(m(this.takeRequestId()));
    const resp = await rs.recv();
    if (!resp) throw new Error("request stream closed without a response");
    if (resp.type === MsgType.RequestError) {
      rs.finish();
      throw new RequestError(resp.code, resp.reason, resp.retryInterval, resp.redirect);
    }
    return [rs, resp];
  }
  /** Subscribes to a track and waits for SUBSCRIBE_OK. */
  async subscribe(track, params = {}) {
    const [rs, ok] = await this.openRequest((requestId) => ({ type: MsgType.Subscribe, requestId, track, params }));
    if (ok.type !== MsgType.SubscribeOk) {
      const err = protocolViolation(`unexpected 0x${ok.type.toString(16)} answering SUBSCRIBE`);
      this.fail(err);
      throw err;
    }
    const sub = new Subscription(this, rs, track, ok.trackAlias, parseTrackInfo(ok.trackProperties), ok.trackProperties, ok.params.largestObject);
    try {
      this.registerAlias(ok.trackAlias, sub);
    } catch (e) {
      this.fail(e);
      throw e;
    }
    void sub.readResponses();
    return sub;
  }
  /** Requests already-published Objects (typically by a LOCATION_FILTER) and waits for FETCH_OK. */
  async fetch(track, params = {}) {
    let f;
    try {
      const [rs, ok] = await this.openRequest((requestId) => {
        f = new Fetch(this, track, requestId);
        this.fetches.set(requestId, f);
        return { type: MsgType.Fetch, requestId, track, params };
      });
      if (ok.type !== MsgType.FetchOk) {
        const err = protocolViolation(`unexpected 0x${ok.type.toString(16)} answering FETCH`);
        this.fail(err);
        throw err;
      }
      const info = parseTrackInfo(ok.trackProperties);
      f.accepted(rs, ok.endOfTrack, ok.endLocation, ok.trackProperties, (params.groupOrder ?? info.groupOrder) === 2);
      return f;
    } catch (e) {
      f?.finish(e instanceof Error ? e : new Error(String(e)));
      throw e;
    }
  }
  /** @internal */
  forgetFetch(id, f) {
    if (this.fetches.get(id) === f) this.fetches.delete(id);
  }
  /** Advertises that this endpoint publishes tracks under prefix. */
  async publishNamespace(prefix, params = {}) {
    validateNamespace(prefix);
    const [rs, ok] = await this.openRequest((requestId) => ({ type: MsgType.PublishNamespace, requestId, prefix, params }));
    if (ok.type !== MsgType.RequestOk) {
      const err = protocolViolation("unexpected PUBLISH_NAMESPACE response");
      this.fail(err);
      throw err;
    }
    return new PublishedNamespace(rs, prefix);
  }
  /** Asks for the namespaces under prefix and their changes. */
  async subscribeNamespace(prefix, params = {}) {
    validateNamespace(prefix);
    const [rs, ok] = await this.openRequest((requestId) => ({ type: MsgType.SubscribeNamespace, requestId, prefix, params }));
    if (ok.type !== MsgType.RequestOk) {
      const err = protocolViolation("unexpected SUBSCRIBE_NAMESPACE response");
      this.fail(err);
      throw err;
    }
    const ns = new NamespaceSubscription(this, rs, prefix);
    void ns.read();
    return ns;
  }
  /** @internal */
  allocTrackAlias() {
    return this.nextTrackAlias++;
  }
  /** @internal */
  async takeRequestIdForUpdate() {
    return this.takeRequestId();
  }
};
var IncomingSubgroup = class {
  constructor(session, header, reader) {
    this.session = session;
    this.reader = reader;
    this.header = header;
    this.decoder = new SubgroupDecoder(reader, header);
  }
  session;
  reader;
  header;
  decoder;
  firstId;
  /** Subgroup ID; with FirstObject mode, known after the first Object. */
  get subgroupId() {
    if (this.header.idMode === SubgroupIdMode.Zero) return 0;
    if (this.header.idMode === SubgroupIdMode.Explicit) return this.header.subgroupId;
    return this.firstId;
  }
  /** The next Object, or undefined when the subgroup is complete. */
  async next() {
    try {
      const o = await this.decoder.next();
      if (o && this.firstId === void 0) this.firstId = o.id;
      return o;
    } catch (e) {
      if (e instanceof SessionError) this.session.fail(e);
      throw e;
    }
  }
  async *[Symbol.asyncIterator]() {
    for (let o = await this.next(); o; o = await this.next()) yield o;
  }
  /** STOP_SENDING: no longer interested in this subgroup. */
  cancel() {
    void this.reader.cancel(streamError(ResetCode.Cancelled));
  }
};
var Fetch = class {
  // null: complete
  constructor(session, track, requestId) {
    this.session = session;
    this.track = track;
    this.requestId = requestId;
  }
  session;
  track;
  requestId;
  endOfTrack = false;
  /** The largest Location the fetch covers. */
  endLocation = { group: 0, object: 0 };
  properties = [];
  rs;
  descending = false;
  reader;
  decoder;
  streamWaiter;
  ended;
  /** @internal */
  accepted(rs, endOfTrack, endLocation, properties, descending) {
    this.rs = rs;
    this.endOfTrack = endOfTrack;
    this.endLocation = endLocation;
    this.properties = properties;
    this.descending = descending;
    void (async () => {
      try {
        while (await rs.recv()) ;
      } catch {
      }
    })();
  }
  /** @internal */
  deliver(r) {
    if (this.reader || this.ended !== void 0) {
      void r.cancel(streamError(ResetCode.Cancelled));
      return;
    }
    this.reader = r;
    this.streamWaiter?.(r);
  }
  /** @internal */
  finish(err) {
    if (this.ended !== void 0) return;
    this.ended = err;
    this.streamWaiter?.(void 0);
    this.session.forgetFetch(this.requestId, this);
  }
  /** The next Object or End of Range marker, or undefined once the fetch is complete. */
  async next() {
    if (this.ended === null) return void 0;
    if (this.ended) throw this.ended;
    if (!this.reader) {
      await new Promise((r) => this.streamWaiter = r);
      if (this.ended === null) return void 0;
      if (this.ended) throw this.ended;
    }
    this.decoder ??= new FetchDecoder(this.reader, this.descending);
    let o;
    try {
      o = await this.decoder.next();
    } catch (e) {
      if (e instanceof SessionError) this.session.fail(e);
      this.finish(e instanceof Error ? e : new Error(String(e)));
      throw this.ended;
    }
    if (!o) {
      this.rs?.finish();
      this.finish(null);
    }
    return o;
  }
  async *[Symbol.asyncIterator]() {
    for (let o = await this.next(); o; o = await this.next()) yield o;
  }
  /** Abandons the fetch; does nothing once it is complete. */
  cancel() {
    if (this.ended === null) return;
    this.finish(new Error("fetch cancelled"));
    this.rs?.cancel();
    void this.reader?.cancel(streamError(ResetCode.Cancelled));
  }
};
var lateStreamWaitMs = 2e3;
var Subscription = class {
  constructor(session, rs, track, trackAlias, info, properties, largestObject) {
    this.session = session;
    this.rs = rs;
    this.track = track;
    this.trackAlias = trackAlias;
    this.properties = properties;
    this.largestObject = largestObject;
    this.info = info;
    this.done = new Promise((r) => this.resolveDone = r);
  }
  session;
  rs;
  track;
  trackAlias;
  properties;
  largestObject;
  info;
  subgroups = new Queue();
  datagrams = new Queue();
  updateWaiters = [];
  streamsSeen = 0;
  streamArrived = () => {
  };
  ended = false;
  unsubscribed = false;
  /** PUBLISH_DONE, or the error that ended the subscription. */
  done;
  resolveDone;
  /** Called with PUBLISH_STATE_NOTIFY parameters. */
  onStateNotify;
  /** @internal */
  async readResponses() {
    for (; ; ) {
      let m;
      try {
        m = await this.rs.recv();
      } catch (e) {
        return this.finish(void 0, e instanceof Error ? e : new Error(String(e)));
      }
      if (!m) return this.finish(void 0, new Error("subscription stream closed without PUBLISH_DONE"));
      switch (m.type) {
        case MsgType.PublishDone:
          this.rs.finish();
          await this.awaitStreams(m.streamCount);
          return this.finish({ statusCode: m.statusCode, streamCount: m.streamCount, reason: m.reason });
        case MsgType.PublishStateNotify:
          this.onStateNotify?.(m.params);
          break;
        case MsgType.RequestOk:
        case MsgType.RequestError: {
          const w = this.updateWaiters.shift();
          if (!w) return this.session.fail(protocolViolation("response without a pending REQUEST_UPDATE"));
          w(m);
          break;
        }
        default:
          return this.session.fail(protocolViolation(`unexpected 0x${m.type.toString(16)} in subscription`));
      }
    }
  }
  // PUBLISH_DONE can overtake data streams; wait for its Stream Count.
  async awaitStreams(count) {
    const deadline = Date.now() + lateStreamWaitMs;
    while (count === StreamCountUnknown || BigInt(this.streamsSeen) < count) {
      const left = deadline - Date.now();
      if (left <= 0) return;
      await new Promise((resolve) => {
        const t = setTimeout(resolve, left);
        this.streamArrived = () => {
          clearTimeout(t);
          resolve();
        };
      });
      if (count === StreamCountUnknown) continue;
    }
  }
  /** @internal */
  finish(result, err) {
    if (this.ended) return;
    this.ended = true;
    this.subgroups.close();
    this.datagrams.close();
    this.resolveDone(result ?? err ?? new Error("ended"));
    this.session.releaseAlias(this.trackAlias, this);
  }
  /** @internal */
  deliverSubgroup(g) {
    this.streamsSeen++;
    this.streamArrived();
    if (this.unsubscribed || this.subgroups.length >= 64 || !this.subgroups.push(g)) g.cancel();
  }
  /** @internal */
  deliverDatagram(d) {
    if (this.datagrams.length < 256) this.datagrams.push(d);
  }
  /** The next subgroup stream; undefined once the subscription has ended. */
  nextSubgroup() {
    return this.subgroups.next();
  }
  async *[Symbol.asyncIterator]() {
    for (let g = await this.nextSubgroup(); g; g = await this.nextSubgroup()) yield g;
  }
  /** The next Object sent as a datagram. */
  nextDatagram() {
    return this.datagrams.next();
  }
  /** REQUEST_UPDATE, e.g. { newGroupRequest: 0 } for a keyframe. */
  async update(params) {
    const reply = new Promise((r) => this.updateWaiters.push(r));
    await this.rs.send({ type: MsgType.RequestUpdate, requestId: await this.session.takeRequestIdForUpdate(), params });
    const m = await reply;
    if (m.type === MsgType.RequestError) throw new RequestError(m.code, m.reason);
  }
  unsubscribe() {
    this.unsubscribed = true;
    this.finish(void 0, new Error("unsubscribed"));
    this.rs.cancel();
  }
};
var SubscribeRequest = class {
  constructor(session, rs, requestId, track, params) {
    this.session = session;
    this.rs = rs;
    this.requestId = requestId;
    this.track = track;
    this.params = params;
  }
  session;
  rs;
  requestId;
  track;
  params;
  reject(code, reason = "") {
    return this.rs.reject(code, reason);
  }
  /** Answers SUBSCRIBE_OK and returns the writer for the track. */
  async accept(opts = {}) {
    const properties = opts.properties ?? [];
    const info = parseTrackInfo(properties);
    const alias = this.session.allocTrackAlias();
    await this.rs.send({ type: MsgType.SubscribeOk, trackAlias: alias, params: { largestObject: opts.largestObject, expires: opts.expires }, trackProperties: properties });
    const w = new TrackWriter(this.session, this.rs, this.track, alias, info, this.params);
    void w.readRequests();
    return w;
  }
};
var TrackWriter = class {
  constructor(session, rs, track, trackAlias, info, params) {
    this.session = session;
    this.rs = rs;
    this.track = track;
    this.trackAlias = trackAlias;
    this.info = info;
    this.apply(params);
  }
  session;
  rs;
  track;
  trackAlias;
  info;
  streams = 0n;
  closed = false;
  abort = new AbortController();
  subscriberPriority;
  forwarding = true;
  /** Called with each REQUEST_UPDATE's parameters (already acknowledged). */
  onUpdate;
  /** Aborted when the subscriber cancels or the session ends. */
  get signal() {
    return this.abort.signal;
  }
  apply(p) {
    if (p.subscriberPriority !== void 0) this.subscriberPriority = p.subscriberPriority;
    if (p.forward !== void 0) this.forwarding = p.forward === 1;
  }
  /** @internal */
  async readRequests() {
    for (; ; ) {
      let m;
      try {
        m = await this.rs.recv();
      } catch {
        this.abort.abort(new Error("subscriber cancelled"));
        return;
      }
      if (!m) return;
      if (m.type !== MsgType.RequestUpdate) return this.session.fail(protocolViolation("unexpected message from subscriber"));
      if (!this.session.checkPeerRequestId(m.requestId)) return;
      this.apply(m.params);
      this.onUpdate?.(m.params);
      await this.rs.send({ type: MsgType.RequestOk, params: {}, trackProperties: [] }).catch(() => {
      });
    }
  }
  /** @internal Lower MOQT priority sends first; sendOrder is the reverse. */
  sendOrder(publisherPriority) {
    return 255 - (this.subscriberPriority ?? publisherPriority);
  }
  /** @internal */
  countStream() {
    this.streams++;
  }
  /** Starts a subgroup; its stream opens with the first Object. */
  openSubgroup(group, subgroupId, opts = {}) {
    const header = {
      trackAlias: this.trackAlias,
      group,
      idMode: subgroupId === 0 ? SubgroupIdMode.Zero : SubgroupIdMode.Explicit,
      subgroupId,
      priority: opts.priority,
      properties: opts.properties ?? false,
      endOfGroup: opts.endOfGroup ?? false,
      firstObject: opts.firstObject ?? false
    };
    return new OutgoingSubgroup(this.session, this, header, opts.priority ?? this.info.publisherPriority);
  }
  async writeDatagram(d) {
    await this.session.sendDatagram(encodeDatagram({ ...d, trackAlias: this.trackAlias }));
  }
  /** PUBLISH_DONE; call after every subgroup has been closed or reset. */
  async close(statusCode = PublishDoneCode.TrackEnded, reason = "") {
    if (this.closed) return;
    this.closed = true;
    await this.rs.send({ type: MsgType.PublishDone, statusCode, streamCount: this.streams, reason }).catch(() => {
    });
    this.rs.finish();
    this.abort.abort(new Error("publish done"));
  }
};
var OutgoingSubgroup = class {
  constructor(session, track, header, priority) {
    this.session = session;
    this.track = track;
    this.header = header;
    this.priority = priority;
    this.encoder = new SubgroupEncoder(header);
  }
  session;
  track;
  header;
  priority;
  writer;
  encoder;
  async writeObject(o) {
    const w = new Writer();
    if (!this.writer) {
      const stream = await this.session.wt.createUnidirectionalStream({ sendOrder: this.track.sendOrder(this.priority) });
      this.writer = stream.getWriter();
      this.track.countStream();
      w.bytes(encodeSubgroupHeader(this.header));
    }
    this.encoder.encode(o, w);
    await this.writer.write(w.finish());
  }
  /** FIN after the last Object. A subgroup with no Objects sends nothing. */
  async close() {
    await this.writer?.close().catch(() => {
    });
  }
  /** Abandon the subgroup, e.g. on a delivery timeout. */
  reset(code = ResetCode.Cancelled) {
    this.writer?.abort(streamError(code)).catch(() => {
    });
  }
};
var PublishedNamespace = class {
  constructor(rs, prefix) {
    this.rs = rs;
    this.prefix = prefix;
  }
  rs;
  prefix;
  withdraw() {
    this.rs.cancel();
  }
};
var PublishNamespaceRequest = class {
  constructor(rs, prefix, params) {
    this.rs = rs;
    this.prefix = prefix;
    this.params = params;
  }
  rs;
  prefix;
  params;
  reject(code, reason = "") {
    return this.rs.reject(code, reason);
  }
  async accept() {
    await this.rs.send({ type: MsgType.RequestOk, params: {}, trackProperties: [] });
  }
};
var NamespaceSubscription = class {
  constructor(session, rs, prefix) {
    this.session = session;
    this.rs = rs;
    this.prefix = prefix;
  }
  session;
  rs;
  prefix;
  events = new Queue();
  /** @internal */
  async read() {
    const active = /* @__PURE__ */ new Map();
    try {
      for (; ; ) {
        const m = await this.rs.recv();
        if (!m) break;
        if (m.type === MsgType.Namespace) {
          const k = JSON.stringify(m.suffix);
          if (active.has(k)) continue;
          active.set(k, m.suffix);
          this.events.push({ suffix: m.suffix, active: true });
        } else if (m.type === MsgType.NamespaceDone) {
          const k = JSON.stringify(m.suffix);
          if (!active.delete(k)) return this.session.fail(protocolViolation("NAMESPACE_DONE before NAMESPACE"));
          this.events.push({ suffix: m.suffix, active: false });
        } else {
          return this.session.fail(protocolViolation("unexpected message on SUBSCRIBE_NAMESPACE stream"));
        }
      }
    } catch {
    } finally {
      for (const suffix of active.values()) this.events.push({ suffix, active: false });
      this.events.close();
    }
  }
  next() {
    return this.events.next();
  }
  async *[Symbol.asyncIterator]() {
    for (let e = await this.next(); e; e = await this.next()) yield e;
  }
  cancel() {
    this.rs.cancel();
  }
};
var SubscribeNamespaceRequest = class {
  constructor(rs, prefix, params) {
    this.rs = rs;
    this.prefix = prefix;
    this.params = params;
  }
  rs;
  prefix;
  params;
  reject(code, reason = "") {
    return this.rs.reject(code, reason);
  }
  async accept() {
    await this.rs.send({ type: MsgType.RequestOk, params: {}, trackProperties: [] });
    const active = /* @__PURE__ */ new Set();
    return {
      announce: async (suffix) => {
        if (active.has(JSON.stringify(suffix))) return;
        active.add(JSON.stringify(suffix));
        await this.rs.send({ type: MsgType.Namespace, suffix });
      },
      withdraw: async (suffix) => {
        if (!active.delete(JSON.stringify(suffix))) throw new Error("namespace was not announced");
        await this.rs.send({ type: MsgType.NamespaceDone, suffix });
      }
    };
  }
};
export {
  ALPN,
  AliasType,
  DatagramPadding,
  EndOfStream,
  Fetch,
  FetchDecoder,
  FetchRange,
  FilterType,
  Implementation,
  IncomingSubgroup,
  MaxFullTrackNameBytes,
  MaxKeyValueBytes,
  MaxNamespaceFields,
  MaxObjectPayload,
  MaxObjectProperties,
  MaxReasonPhraseBytes,
  MsgType,
  NamespaceSubscription,
  ObjectStatus,
  OutgoingSubgroup,
  Param,
  Prop,
  PublishDoneCode,
  PublishNamespaceRequest,
  PublishedNamespace,
  Reader,
  RequestError,
  RequestErrorCode,
  ResetCode,
  Session,
  SessionError,
  SessionErrorCode,
  SetupOption,
  ShortBuffer,
  StreamCountUnknown,
  StreamReader,
  StreamType,
  SubgroupDecoder,
  SubgroupEncoder,
  SubgroupIdMode,
  SubscribeNamespaceRequest,
  SubscribeRequest,
  Subscription,
  TrackWriter,
  Writer,
  decodeMessage,
  decodeToken,
  encodeDatagram,
  encodeMessage,
  encodeSubgroupHeader,
  encodeToken,
  hasPrefix,
  isRequest,
  isSubgroupStreamType,
  lookupProperty,
  parseDatagram,
  parseMessage,
  parseTrackInfo,
  protocolViolation,
  readKeyValues,
  readNamespace,
  readSubgroupHeader,
  renderName,
  renderNamespace,
  renderTrack,
  subgroupHeaderFlags,
  trackKey,
  validateNamespace,
  varintSize,
  writeKeyValues,
  writeNamespace,
  writeParams
};
