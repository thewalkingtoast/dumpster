(() => {
  'use strict';

  const body = document.body;
  const ID = body.dataset.id;
  const ORIGIN = body.dataset.origin;
  const CSRF = body.dataset.csrf || '';
  const GRPC = body.dataset.grpc;
  // One switch from the server decides both: gRPC is encrypted and the socket
  // example says wss when the app is served over TLS, never half of each.
  const TLS = body.dataset.tls === 'true';
  const TCP = body.dataset.tcp;
  const UDP = body.dataset.udp;

  const listEl = document.getElementById('records');
  const emptyEl = document.getElementById('empty');
  const countEl = document.getElementById('count');
  const statusEl = document.getElementById('status');
  const ttlEl = document.getElementById('ttl');
  const themeBtn = document.getElementById('theme-toggle');

  // -- theme switch ---------------------------------------------------------
  // theme.js has already set data-theme before paint; this only flips it and
  // keeps the button's label honest about what a click will do.
  const THEME_KEY = 'dumpster-theme';

  function currentTheme() {
    return document.documentElement.getAttribute('data-theme') === 'light' ? 'light' : 'dark';
  }

  function paintThemeButton() {
    if (!themeBtn) return;
    const next = currentTheme() === 'light' ? 'dark' : 'light';
    const label = 'Switch to ' + next + ' mode';
    themeBtn.setAttribute('aria-label', label);
    themeBtn.setAttribute('title', label);
    themeBtn.setAttribute('aria-pressed', currentTheme() === 'light' ? 'true' : 'false');
  }

  if (themeBtn) {
    paintThemeButton();
    themeBtn.addEventListener('click', () => {
      const next = currentTheme() === 'light' ? 'dark' : 'light';
      document.documentElement.setAttribute('data-theme', next);
      try { window.localStorage.setItem(THEME_KEY, next); } catch (e) { /* private mode */ }
      paintThemeButton();
    });
  }

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
  function tickAges(now = Date.now(), maxAgeSeconds = Infinity) {
    for (const node of document.querySelectorAll('.age[data-at]')) {
      const age = (now - new Date(node.dataset.at).getTime()) / 1000;
      if (age > maxAgeSeconds) continue;
      const next = ago(node.dataset.at, now);
      if (node.textContent !== next) node.textContent = next;
    }
  }

  // A label only changes as fast as its own precision: every second while it
  // reads "12s ago", once a minute once it reads "12m ago". Two passes, each at
  // the cadence its rows need, stop a long-lived page from rescanning every row
  // sixty times a minute to print the same text.
  //
  // The 62-second ceiling is a hair past a minute on purpose: a row crossing the
  // boundary must not be stranded on "59s ago" until the slow pass next fires.
  const YOUNG_SECONDS = 62;

  function startAgeTicker() {
    // A hidden tab has nobody to read it, so skip the work and catch up on return.
    setInterval(() => {
      if (!document.hidden) tickAges(Date.now(), YOUNG_SECONDS);
    }, 2000);
    setInterval(() => {
      if (!document.hidden) tickAges();
    }, 60000);
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
      const body = el('pre', {
        text: typeof value === 'string' ? value : JSON.stringify(value, null, 2),
      });
      wireCopyable(body, label);
      wrap.append(el('h4', { text: label }), body);
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
      sse: `curl -N '${ORIGIN}/d/${ID}/sse'`,
      grpc: `grpcurl ${TLS ? '-insecure' : '-plaintext'} -import-path examples -proto dumpster.proto \\\n  -H 'x-dumpster-id: ${ID}' \\\n  -d '{"payload":"aGVsbG8="}' ${GRPC} dumpster.Ingest/Anything`,
      ws: `websocat "${TLS ? 'wss' : 'ws'}://${location.host}/d/${ID}/ws"`,
      tcp: `printf '%s\\nhello\\n' '${ID}' | nc ${tcp.host} ${tcp.port}`,
      udp: `printf '%s|hello' '${ID}' | nc -u -w1 ${udp.host} ${udp.port}`,
    };

    for (const [key, value] of Object.entries(commands)) {
      const pre = document.querySelector('[data-cmd="' + key + '"]');
      if (pre) pre.textContent = value;
    }
  }

  // navigator.clipboard only exists in a secure context, and this app is usually
  // reached over plain http on a LAN address — precisely where a phone throws the
  // API away. The textarea trick still works there, so try it rather than leaving
  // a tap looking broken.
  function legacyCopy(text, done) {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.setAttribute('readonly', '');
    ta.style.position = 'fixed';
    ta.style.top = '-1000px';
    document.body.append(ta);
    ta.select();
    try {
      if (document.execCommand('copy')) done();
    } catch {
      /* nothing sane left to try */
    }
    ta.remove();
  }

  // One toast for the whole page: built on first use so a page nobody copies
  // from never carries it, and reused so rapid taps replace the message instead
  // of stacking a tower of them.
  let toastEl = null;
  let toastTimer = 0;
  function showToast(message) {
    if (!toastEl) {
      toastEl = el('div', { class: 'toast', role: 'status', 'aria-live': 'polite' });
      document.body.append(toastEl);
    }
    toastEl.textContent = message;
    toastEl.classList.add('show');
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => toastEl.classList.remove('show'), 1000);
  }

  function copyText(text, node) {
    if (!text) return;
    const flash = () => {
      node.classList.add('copied');
      setTimeout(() => node.classList.remove('copied'), 700);
      showToast('Copied');
    };
    if (navigator.clipboard && window.isSecureContext) {
      navigator.clipboard.writeText(text).then(flash, () => legacyCopy(text, flash));
      return;
    }
    legacyCopy(text, flash);
  }

  // Copies the block's own text: pretty-printed JSON stays pretty-printed, and a
  // raw body stays raw.
  function wireCopyable(node, label) {
    if (!node) return;
    if (label) node.title = 'click to copy ' + label;
    node.addEventListener('click', () => copyText(node.textContent, node));
  }

  function wireCopy() {
    for (const pre of document.querySelectorAll('pre.cmd')) wireCopyable(pre, '');
  }

  // The button emits; it does not listen. Opening the EventSource from the page
  // would make the dashboard its own listener and duplicate every row it renders.
  function wireSSE() {
    const button = document.getElementById('btn-sse');
    if (!button) return;
    const idle = 'Send 5 SSE events';
    let busy = false;

    button.addEventListener('click', async () => {
      if (busy) return;
      busy = true;
      button.disabled = true;
      try {
        const res = await fetch(ORIGIN + '/d/' + ID + '/sse/emit?count=5', {
          method: 'POST',
          headers: { 'X-CSRF-Token': CSRF },
        });
        const body = await res.json();
        const n = body.listeners || 0;
        button.textContent = n
          ? 'sent to ' + n + (n === 1 ? ' listener' : ' listeners')
          : 'sent — nobody listening';
      } catch {
        button.textContent = 'send failed';
      }
      setTimeout(() => {
        button.textContent = idle;
        button.disabled = false;
        busy = false;
      }, 2000);
    });
  }

  renderCommands();
  wireCopy();
  wireSSE();
  startAgeTicker();
  connect();
})();
