(() => {
  'use strict';

  const body = document.body;
  const ID = body.dataset.id;
  const ORIGIN = body.dataset.origin;
  const GRPC = body.dataset.grpc;
  const TCP = body.dataset.tcp;
  const UDP = body.dataset.udp;

  const listEl = document.getElementById('records');
  const emptyEl = document.getElementById('empty');
  const countEl = document.getElementById('count');
  const statusEl = document.getElementById('status');
  const ttlEl = document.getElementById('ttl');

  /** seq -> { li, isOpen } */
  const rows = new Map();

  // -- DOM helpers: build nodes with textContent, never raw markup ----------
  function el(tag, props, ...kids) {
    const node = document.createElement(tag);
    if (props) {
      for (const [key, value] of Object.entries(props)) {
        if (value === null || value === undefined) continue;
        if (key === 'text') node.textContent = value;
        else if (key === 'class') node.className = value;
        else if (key.startsWith('on')) node.addEventListener(key.slice(2), value);
        else node.setAttribute(key, value);
      }
    }
    for (const kid of kids) if (kid !== null && kid !== undefined) node.append(kid);
    return node;
  }

  // -- formatting ------------------------------------------------------------
  const pad = (n, width = 2) => String(n).padStart(width, '0');

  // HH:MM:SS.mmm in the browser's zone. Milliseconds matter on a page ordered by
  // arrival: two requests in the same second are common, and without them the
  // newest-first list looks arbitrarily sorted.
  //
  // A row from another day carries a date prefix. Dumpsters live three hours, so
  // records can predate midnight, and "23:58:01" yesterday reads identically to
  // "23:58:01" today.
  const fmtTime = (iso, now = new Date()) => {
    const d = new Date(iso);
    const clock = `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}.${pad(d.getMilliseconds(), 3)}`;
    const sameDay = d.getFullYear() === now.getFullYear() && d.getMonth() === now.getMonth() && d.getDate() === now.getDate();
    if (sameDay) return clock;
    return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${clock}`;
  };

  function ago(iso, now = Date.now()) {
    const seconds = Math.max(0, Math.floor((now - new Date(iso).getTime()) / 1000));
    if (seconds < 5) return 'now';
    if (seconds < 60) return seconds + 's ago';
    const minutes = Math.floor(seconds / 60);
    if (minutes < 60) return minutes + 'm ago';
    return Math.floor(minutes / 60) + 'h ago';
  }

  // Rows are built once and then mutated by socket updates, so the age cannot be
  // baked into the row text. Only the age is rewritten on the tick: replacing
  // whole rows every second would fight the live socket and collapse any row the
  // reader has open.
  //
  // The write is skipped when the string is unchanged, which is most ticks for
  // most rows — touching textContent costs a layout pass even when the value is
  // identical.
  function tickAges(now = Date.now()) {
    for (const node of document.querySelectorAll('.age[data-at]')) {
      const next = ago(node.dataset.at, now);
      if (node.textContent !== next) node.textContent = next;
    }
  }

  function startAgeTicker() {
    // A hidden tab has nobody to read it: skip the work, then catch up on return
    // so the first ages after switching back are not a second stale.
    setInterval(() => {
      if (!document.hidden) tickAges();
    }, 1000);
    document.addEventListener('visibilitychange', () => {
      if (!document.hidden) tickAges();
    });
  }

  function bytes(n) {
    if (!n) return '0 B';
    if (n < 1024) return n + ' B';
    if (n < 1048576) return (n / 1024).toFixed(1) + ' kB';
    return (n / 1048576).toFixed(1) + ' MB';
  }

  function summary(rec) {
    const req = rec.req || {};
    switch (rec.kind) {
      case 'http':
        return (req.method || '?') + ' ' + (req.subpath || '/') + (req.query ? '?' + req.query : '');
      case 'grpc':
        return req.path || 'gRPC call';
      case 'sse':
        return 'event stream' + (req.query ? '?' + req.query : '');
      case 'ws':
        return 'websocket session';
      case 'tcp':
        return 'tcp session' + (rec.meta && rec.meta.local ? ' · ' + rec.meta.local : '');
      case 'udp':
        return 'udp datagram';
      default:
        return rec.kind || '?';
    }
  }

  function sizeOf(rec) {
    if (rec.req && rec.req.body_bytes) return rec.req.body_bytes;
    if (rec.events && rec.events.length) {
      return rec.events.reduce((sum, ev) => sum + (ev.bytes || 0), 0);
    }
    return 0;
  }

  // -- detail pane ----------------------------------------------------------
  function detailNode(rec) {
    const wrap = el('div', { class: 'detail' });

    function block(label, value) {
      if (value === null || value === undefined || value === '') return;
      if (typeof value === 'object' && !Object.keys(value).length) return;
      wrap.append(
        el('h4', { text: label }),
        el('pre', { text: typeof value === 'string' ? value : JSON.stringify(value, null, 2) })
      );
    }

    // The row shows only "3s ago", so the exact instant lives here: expand the
    // row when you need to know precisely when something landed.
    block('received', fmtTime(rec.received_at));

    const req = rec.req || {};
    block('request', {
      protocol: req.protocol,
      method: req.method,
      path: req.path,
      subpath: req.subpath,
      query: req.query,
      host: req.host,
      http_version: req.http_version,
      content_length: req.content_length,
      body_bytes: req.body_bytes,
      truncated: req.truncated || undefined,
    });
    block('headers', req.headers);
    block('body (raw)', req.body_text);
    block('body (base64)', req.body_base64);
    if (req.decoded) {
      block('decoded (' + req.decoded.format + ')',
        req.decoded.error ? 'decode error: ' + req.decoded.error : req.decoded.value);
    }
    block('remote', rec.remote);
    block('timing (µs)', rec.timing);
    block('meta', rec.meta);
    if (rec.close_note) block('closed', rec.close_note);

    if (rec.events && rec.events.length) {
      const dropped = rec.dropped_events ? ', ' + rec.dropped_events + ' dropped' : '';
      wrap.append(el('h4', { text: 'events (' + rec.events.length + dropped + ')' }));
      const ul = el('ul', { class: 'events' });
      for (const ev of rec.events.slice(0, 200)) {
        ul.append(el('li', { class: 'ev ' + (ev.dir === 'out' ? 'out' : 'in') },
          el('span', { class: 'ev-at', text: fmtTime(ev.at) }),
          el('span', { class: 'ev-type', text: ev.type || '' }),
          el('span', {
            class: 'ev-body',
            text: ev.text !== undefined && ev.text !== ''
              ? ev.text
              : (ev.base64 ? '[binary ' + bytes(ev.bytes) + ']' : ''),
          })));
      }
      wrap.append(ul);
    }
    return wrap;
  }

  // -- rows -----------------------------------------------------------------
  function makeRow(rec) {
    const head = el('button', { class: 'row', type: 'button' },
      el('span', { class: 'badge k-' + rec.kind, text: String(rec.kind).toUpperCase() }),
      el('span', { class: 'sum', text: summary(rec) }),
      el('span', { class: 'when' },
        bytes(sizeOf(rec)) + ' · ' + ((rec.remote && rec.remote.ip) || '?') + ' · ',
        el('span', { class: 'age', 'data-at': rec.received_at, text: ago(rec.received_at) })));
    const li = el('li', {
      class: 'rec' + (rec.closed ? ' is-closed' : ''),
      'data-seq': String(rec.seq),
    }, head);

    let open = false;
    head.addEventListener('click', () => {
      open = !open;
      li.classList.toggle('open', open);
      li.replaceChildren(head);
      if (open) li.append(detailNode(rec));
    });
    return { li, isOpen: () => open };
  }

  function renderCount() {
    countEl.textContent = String(rows.size);
    emptyEl.hidden = rows.size > 0;
  }

  function loadSnapshot(records) {
    listEl.replaceChildren();
    rows.clear();
    for (const rec of records) {
      const fresh = makeRow(rec);
      rows.set(rec.seq, fresh);
      listEl.append(fresh.li);
    }
    renderCount();
  }

  function upsert(rec) {
    const existing = rows.get(rec.seq);
    if (existing) {
      const wasOpen = existing.isOpen();
      const fresh = makeRow(rec);
      existing.li.replaceWith(fresh.li);
      if (wasOpen) fresh.li.querySelector('.row').click();
      rows.set(rec.seq, fresh);
      return;
    }
    const fresh = makeRow(rec);
    rows.set(rec.seq, fresh);
    const first = listEl.firstElementChild;
    if (first && Number(first.dataset.seq) < rec.seq) listEl.prepend(fresh.li);
    else listEl.append(fresh.li);
  }

  // -- socket ---------------------------------------------------------------
  function setStatus(text) { statusEl.textContent = text; }

  function onMessage(raw) {
    let msg;
    try { msg = JSON.parse(raw); } catch { return; }
    switch (msg.type) {
      case 'snapshot':
        loadSnapshot(msg.records || []);
        break;
      case 'new':
      case 'update':
        if (msg.record) { upsert(msg.record); renderCount(); }
        break;
      case 'expired':
        setStatus('expired');
        document.querySelector('main').prepend(el('div', {
          class: 'banner',
          text: 'This dumpster has been emptied. Rent a new one to keep dumping.',
        }));
        break;
      default:
        break;
    }
  }

  let retry = 0;
  function connect() {
    const scheme = location.protocol === 'https:' ? 'wss' : 'ws';
    const socket = new WebSocket(scheme + '://' + location.host + '/d/' + ID + '/_live');
    socket.addEventListener('open', () => { retry = 0; setStatus('live'); });
    socket.addEventListener('message', (event) => onMessage(event.data));
    socket.addEventListener('close', () => {
      setStatus('reconnecting…');
      retry = Math.min(retry + 1, 6);
      setTimeout(connect, 250 * retry);
    });
    socket.addEventListener('error', () => socket.close());
  }

  // -- countdown ------------------------------------------------------------
  const expires = new Date(body.dataset.expires);
  function tick() {
    const ms = expires - Date.now();
    if (ms <= 0) { ttlEl.textContent = 'expired'; return; }
    const minutes = Math.floor(ms / 60000);
    const hours = Math.floor(minutes / 60);
    ttlEl.textContent = hours > 0 ? hours + 'h ' + (minutes % 60) + 'm' : minutes + 'm';
  }
  tick();
  setInterval(tick, 30000);

  // -- "how to send" panel --------------------------------------------------
  function hostPort(addr) {
    const i = String(addr).lastIndexOf(':');
    if (i < 0) return { host: addr, port: '' };
    return { host: addr.slice(0, i), port: addr.slice(i + 1) };
  }

  function renderCommands() {
    const originHost = hostPort(ORIGIN.replace(/^https?:\/\//, ''));
    const tcp = hostPort(TCP);
    const udp = hostPort(UDP);

    const commands = {
      http: `curl -sS -X POST '${ORIGIN}/d/${ID}/anything' \\\n  -H 'Content-Type: application/json' \\\n  -d '{"hello":"dumpster"}'`,
      sse: `curl -N '${ORIGIN}/d/${ID}/sse?count=5&interval_ms=1000'`,
      grpc: `grpcurl -plaintext -import-path examples -proto dumpster.proto \\\n  -H 'x-dumpster-id: ${ID}' \\\n  -d '{"payload":"aGVsbG8="}' ${GRPC} dumpster.Ingest/Anything`,
      ws: `websocat "ws://${originHost.host}:${originHost.port}/d/${ID}/ws"`,
      tcp: `printf '%s\\nhello\\n' '${ID}' | nc ${tcp.host} ${tcp.port}`,
      udp: `printf '%s|hello' '${ID}' | nc -u -w1 ${udp.host} ${udp.port}`,
    };

    for (const [key, value] of Object.entries(commands)) {
      const pre = document.querySelector('[data-cmd="' + key + '"]');
      if (pre) pre.textContent = value;
    }
  }

  function wireCopy() {
    for (const pre of document.querySelectorAll('pre.cmd')) {
      pre.title = 'click to copy';
      pre.addEventListener('click', async () => {
        try {
          await navigator.clipboard.writeText(pre.textContent);
          pre.classList.add('copied');
          setTimeout(() => pre.classList.remove('copied'), 600);
        } catch {
          // clipboard needs a secure context; the text is selectable regardless
        }
      });
    }
  }

  function wireSSE() {
    const button = document.getElementById('btn-sse');
    if (!button) return;
    let source = null;
    button.addEventListener('click', () => {
      if (source) {
        source.close();
        source = null;
        button.textContent = 'Open SSE stream';
        return;
      }
      source = new EventSource(ORIGIN + '/d/' + ID + '/sse?count=5&interval_ms=1000');
      button.textContent = 'Stop SSE';
      source.addEventListener('error', () => {
        // The stream ends after `count` events and EventSource would reconnect
        // forever, minting a new dump row every second. Close it instead.
        source.close();
        source = null;
        button.textContent = 'Open SSE stream';
      });
    });
  }

  renderCommands();
  wireCopy();
  wireSSE();
  startAgeTicker();
  connect();
})();
