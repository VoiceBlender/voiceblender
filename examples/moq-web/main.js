import { MoqSession } from './moq-session.js';
import { MicCapture, Speaker } from './audio.js';

const $ = (id) => document.getElementById(id);
const logEl = $('log');
const statusEl = $('status');

function setStatus(text, cls = '') {
  statusEl.textContent = text;
  statusEl.className = 'status ' + cls;
}

function log(msg) {
  const ts = new Date().toLocaleTimeString();
  logEl.textContent += `[${ts}] ${msg}\n`;
  logEl.scrollTop = logEl.scrollHeight;
}

let session = null;
let mic = null;
let speaker = null;
let statsTimer = null;
let lastStats = { rx: 0, tx: 0, rxBytes: 0, txBytes: 0 };

function updateStats() {
  if (!session) {
    $('stats').textContent = 'rx: 0 obj · 0 B/s   |   tx: 0 obj · 0 B/s';
    return;
  }
  const drx = session.objectsRx - lastStats.rx;
  const dtx = session.objectsTx - lastStats.tx;
  const dRxB = session.bytesRx - lastStats.rxBytes;
  const dTxB = session.bytesTx - lastStats.txBytes;
  lastStats = { rx: session.objectsRx, tx: session.objectsTx, rxBytes: session.bytesRx, txBytes: session.bytesTx };
  $('stats').textContent =
    `rx: ${session.objectsRx} obj (${drx}/s · ${dRxB} B/s)   |   ` +
    `tx: ${session.objectsTx} obj (${dtx}/s · ${dTxB} B/s)`;
}

async function connect() {
  $('connect').disabled = true;
  setStatus('connecting…');
  logEl.textContent = '';

  const endpoint = $('endpoint').value.trim();
  const roomID = $('room').value.trim();
  const certHashHex = $('certhash').value.trim().replace(/\s+/g, '');
  const url = endpoint + (endpoint.includes('?') ? '&' : '?') + 'room_id=' + encodeURIComponent(roomID);

  try {
    log('opening WebTransport: ' + url);
    const opts = {};
    if (certHashHex) {
      if (certHashHex.length !== 64 || !/^[0-9a-fA-F]+$/.test(certHashHex)) {
        throw new Error('cert hash must be 64 hex chars (SHA-256)');
      }
      const bytes = new Uint8Array(32);
      for (let i = 0; i < 32; i++) bytes[i] = parseInt(certHashHex.slice(i * 2, i * 2 + 2), 16);
      opts.serverCertificateHashes = [{ algorithm: 'sha-256', value: bytes }];
      log('using serverCertificateHashes for self-signed cert');
    }
    const wt = new WebTransport(url, opts);
    await wt.ready;
    log('WebTransport ready');

    session = new MoqSession(wt);
    session.addEventListener('log', (e) => log(e.detail));
    session.addEventListener('error', (e) => log('ERR ' + e.detail));

    await session.run();
    log('MoQ handshake done');

    await session.announceMic();
    log('mic announced');

    speaker = new Speaker({ onLog: log });
    await speaker.start();
    session.addEventListener('object', (e) => speaker.feed(e.detail));

    const subOk = await session.subscribeMix();
    log('mix subscribed: ' + JSON.stringify({
      expires: String(subOk.expiresMs),
      groupOrder: subOk.groupOrder,
    }));

    const publish = await session.waitForMicPublisher();
    log('server subscribed to mic — starting capture');
    mic = new MicCapture({ publish, onLog: log });
    await mic.start();

    setStatus('connected', 'ok');
    $('disconnect').disabled = false;
    lastStats = { rx: 0, tx: 0, rxBytes: 0, txBytes: 0 };
    statsTimer = setInterval(updateStats, 1000);
  } catch (err) {
    log('FATAL ' + err.message);
    setStatus('failed', 'err');
    await disconnect();
    $('connect').disabled = false;
  }
}

async function disconnect() {
  if (statsTimer) { clearInterval(statsTimer); statsTimer = null; }
  if (mic) { mic.stop(); mic = null; }
  if (speaker) { speaker.stop(); speaker = null; }
  if (session) { session.close(); session = null; }
  updateStats();
  setStatus('idle');
  $('connect').disabled = false;
  $('disconnect').disabled = true;
}

$('connect').addEventListener('click', connect);
$('disconnect').addEventListener('click', disconnect);

// Feature checks — fail fast with a clear message.
if (!('WebTransport' in window)) {
  log('Browser does not support WebTransport. Use Chrome 97+ / Edge 97+ over HTTPS.');
  setStatus('unsupported', 'err');
  $('connect').disabled = true;
}
if (!('AudioEncoder' in window) || !('AudioDecoder' in window)) {
  log('Browser does not support WebCodecs (AudioEncoder/Decoder).');
  setStatus('unsupported', 'err');
  $('connect').disabled = true;
}
