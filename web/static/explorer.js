/* FundTrack chart explorer: pick series, choose a date range, drag to highlight a period.
   Dependency-free SVG. Data: window.__EXPLORER__ (see chartdata.go). */
(function () {
  'use strict';
  var D = window.__EXPLORER__;
  var host = document.getElementById('xp-chart');
  if (!D || !host) return;
  var NS = 'http://www.w3.org/2000/svg';
  var DAY = 864e5;
  var PALETTE = ['#2f6fed', '#e8590c', '#2b9a66', '#9c36b5', '#d6336c', '#f08c00', '#0c8599', '#5c7cfa', '#74b816', '#868e96'];
  var T = D.t, N = T.length;
  var items = D.items;
  var TOTAL = '__total';

  function $(id) { return document.getElementById(id); }
  function el(name, attrs, parent) {
    var e = document.createElementNS(NS, name);
    for (var k in attrs) e.setAttribute(k, attrs[k]);
    if (parent) parent.appendChild(e);
    return e;
  }
  function iso(t) { return new Date(t).toISOString().slice(0, 10); }
  function parseISO(s) { var t = Date.parse(s + 'T00:00:00Z'); return isNaN(t) ? null : t; }
  function fdate(t, long) {
    return new Date(t).toLocaleDateString('en-AU', long ? { day: 'numeric', month: 'short', year: 'numeric', timeZone: 'UTC' } : { day: 'numeric', month: 'short', timeZone: 'UTC' });
  }
  function fmonth(t) { return new Date(t).toLocaleDateString('en-AU', { month: 'short', year: '2-digit', timeZone: 'UTC' }); }
  function money(v, dp) { return (v < 0 ? '-$' : '$') + Math.abs(v).toLocaleString('en-AU', { minimumFractionDigits: dp || 0, maximumFractionDigits: dp || 0 }); }
  function axisMoney(v) {
    var a = Math.abs(v), s = v < 0 ? '-' : '';
    if (a >= 1e6) return s + '$' + (a / 1e6).toFixed(a >= 1e7 ? 0 : 1) + 'M';
    if (a >= 1e3) return s + '$' + (a / 1e3).toFixed(a >= 1e4 ? 0 : 1) + 'k';
    return s + '$' + Math.round(a);
  }
  function safe(fn) { try { return fn(); } catch (e) { return null; } }

  // ---- state ----
  var st = { metric: 'value', sel: {}, combine: false, inv: false, from: T[0], to: T[N - 1], hl: null };
  var saved = safe(function () { return JSON.parse(localStorage.getItem('ft.xp') || 'null'); });
  items.forEach(function (it, i) { it.color = PALETTE[i % PALETTE.length]; it.dash = i >= PALETTE.length ? '4 3' : ''; it.key = it.sym; });
  if (saved && saved.sel) {
    st.sel = saved.sel; st.metric = saved.metric || 'value'; st.combine = !!saved.combine; st.inv = !!saved.inv;
  } else {
    st.sel[TOTAL] = true; st.inv = true; // new visitors see money invested alongside value
  }
  var preset = saved && saved.preset !== undefined ? saved.preset : 'all';
  function persist() {
    safe(function () { localStorage.setItem('ft.xp', JSON.stringify({ sel: st.sel, metric: st.metric, combine: st.combine, inv: st.inv, preset: preset })); });
  }

  // ---- ranges ----
  var tmin = T[0], tmax = T[N - 1];
  var PRESETS = [['1M', 1], ['3M', 3], ['6M', 6], ['YTD', 'ytd'], ['FY', 'fy'], ['1Y', 12], ['3Y', 36], ['5Y', 60], ['All', 'all']];
  function presetFrom(p) {
    var e = new Date(tmax), y = e.getUTCFullYear(), m = e.getUTCMonth();
    if (p === 'all') return tmin;
    if (p === 'ytd') return Date.UTC(y, 0, 1);
    if (p === 'fy') return Date.UTC(m >= 6 ? y : y - 1, 6, 1); // Australian financial year starts 1 July
    return Date.UTC(y, m - p, e.getUTCDate());
  }
  function applyPreset(p) {
    preset = p; st.from = Math.max(tmin, presetFrom(p)); st.to = tmax; st.hl = null; sync();
  }
  function buildPresets() {
    var box = $('xp-range'); box.innerHTML = '';
    PRESETS.forEach(function (p) {
      var b = document.createElement('button'); b.type = 'button'; b.textContent = p[0]; b.setAttribute('data-p', p[1]);
      b.onclick = function () { applyPreset(p[1]); };
      box.appendChild(b);
    });
  }
  function markPreset() {
    Array.prototype.forEach.call($('xp-range').children, function (b) { b.classList.toggle('on', String(preset) === b.getAttribute('data-p')); });
  }

  // ---- data access ----
  function lo(t) { var a = 0, b = N - 1; while (a < b) { var m = (a + b) >> 1; if (T[m] < t) a = m + 1; else b = m; } return a; }
  function nearest(t) { var i = lo(t); if (i > 0 && t - T[i - 1] < T[i] - t) i--; return i; }

  function lines() {
    var out = [], i0 = lo(st.from), i1 = lo(st.to);
    if (T[i1] > st.to && i1 > 0) i1--;
    if (i1 < i0) i1 = i0;
    var chosen = items.filter(function (it) { return st.sel[it.key]; });
    var wantTotal = st.sel[TOTAL] && st.metric !== 'index';
    function valueOf(s, i) {
      var v = s.value[i];
      if (v === null) return null;
      if (st.metric === 'gain') return s.invested[i] === null ? null : v - s.invested[i];
      return v;
    }
    function mk(name, color, dash, getter, cls, invGetter) {
      var ys = new Array(N), iv = invGetter ? new Array(N) : null;
      for (var i = 0; i < N; i++) { ys[i] = getter(i); if (iv) iv[i] = invGetter(i); }
      return { name: name, color: color, dash: dash, y: ys, inv: iv, cls: cls || '' };
    }
    function sumInv(parts) {
      return function (i) {
        var s = 0, any = false;
        parts.forEach(function (p) { var v = p.invested[i]; if (v !== null) { s += v; any = true; } });
        return any ? s : null;
      };
    }
    // a dashed "money invested" line in the same colour as the line it belongs to
    function invLine(ln) {
      if (!st.inv || st.metric !== 'value' || !ln.inv) return null;
      var iv = ln.inv;
      return { name: 'Invested: ' + ln.name, color: ln.color, dash: '5 4', y: iv, inv: null, cls: 'inv' };
    }
    function push(ln) { out.push(ln); var x = invLine(ln); if (x) out.push(x); }
    if (st.metric === 'index') {
      chosen.forEach(function (it) {
        var base = null;
        for (var i = i0; i <= i1; i++) if (it.price[i] !== null) { base = it.price[i]; break; }
        if (!base) return;
        out.push(mk(it.name, it.color, it.dash, function (i) { return it.price[i] === null ? null : it.price[i] / base * 100; }));
      });
    } else if (st.combine && chosen.length + (wantTotal ? 1 : 0) > 0) {
      var parts = chosen.slice(); if (wantTotal) parts = [D.total];
      var nm = wantTotal ? 'Total portfolio' : chosen.length === 1 ? chosen[0].name : 'Selected (' + chosen.length + ')';
      push(mk(nm, 'var(--accent)', '', function (i) {
        var s = 0, any = false;
        parts.forEach(function (p) { var v = valueOf(p, i); if (v !== null) { s += v; any = true; } });
        return any ? s : null;
      }, 'total', sumInv(parts)));
    } else {
      if (wantTotal) push(mk('Total portfolio', 'var(--accent)', '', function (i) { return valueOf(D.total, i); }, 'total', function (i) { return D.total.invested[i]; }));
      chosen.forEach(function (it) { push(mk(it.name, it.color, it.dash, function (i) { return valueOf(it, i); }, '', function (i) { return it.invested[i]; })); });
    }
    return { lines: out, i0: i0, i1: i1 };
  }

  // ---- drawing ----
  var view = null; // geometry of the last draw, used by the pointer handlers
  function niceTicks(min, max, n) {
    var span = (max - min) || 1, step0 = span / n, mag = Math.pow(10, Math.floor(Math.log10(step0))), m = step0 / mag;
    var step = (m < 1.5 ? 1 : m < 3 ? 2 : m < 7 ? 5 : 10) * mag, out = [];
    for (var v = Math.ceil(min / step) * step; v <= max + step * 1e-6; v += step) out.push(v);
    return out;
  }
  function fmtY(v) { return st.metric === 'index' ? v.toFixed(0) : axisMoney(v); }
  function fmtFull(v) { return st.metric === 'index' ? v.toFixed(1) : money(v, 0); }

  function draw() {
    host.innerHTML = '';
    var L = lines(), i0 = L.i0, i1 = L.i1;
    var w = Math.max(host.clientWidth, 220), h = w < 480 ? 260 : w < 760 ? 320 : 380;
    if (!L.lines.length) { host.innerHTML = '<p class="muted xp-empty">Pick at least one investment below.</p>'; view = null; return; }
    var ymin = Infinity, ymax = -Infinity;
    L.lines.forEach(function (ln) { for (var i = i0; i <= i1; i++) { var v = ln.y[i]; if (v !== null) { if (v < ymin) ymin = v; if (v > ymax) ymax = v; } } });
    if (ymin === Infinity) { host.innerHTML = '<p class="muted xp-empty">No data in this date range.</p>'; view = null; return; }
    var pad = (ymax - ymin) * 0.07 || Math.abs(ymax) * 0.05 || 1; ymin -= pad; ymax += pad;
    if (st.metric === 'value' && ymin > 0 && ymin < (ymax - ymin) * 0.5) ymin = 0;
    var xmin = T[i0], xmax = T[i1] > xmin ? T[i1] : xmin + DAY;
    var ticks = niceTicks(ymin, ymax, h < 300 ? 4 : 6), ml = 0;
    ticks.forEach(function (v) { ml = Math.max(ml, fmtY(v).length); });
    var m = { l: Math.max(40, ml * 7 + 12), r: 12, t: 10, b: 26 }, iw = w - m.l - m.r, ih = h - m.t - m.b;
    function X(t) { return m.l + iw * (t - xmin) / (xmax - xmin); }
    function Y(v) { return m.t + ih * (1 - (v - ymin) / (ymax - ymin)); }
    function invX(px) { return xmin + (px - m.l) / iw * (xmax - xmin); }

    var svg = el('svg', { width: w, height: h, viewBox: '0 0 ' + w + ' ' + h, role: 'img', 'aria-label': 'Interactive value chart' }, host);
    svg.setAttribute('class', 'ch');
    ticks.forEach(function (v) {
      el('line', { 'class': 'ch-grid', x1: m.l, x2: w - m.r, y1: Y(v), y2: Y(v) }, svg);
      el('text', { 'class': 'ch-axis', x: m.l - 6, y: Y(v) + 4, 'text-anchor': 'end' }, svg).textContent = fmtY(v);
    });
    if (ymin < 0 && ymax > 0) el('line', { 'class': 'ch-zero', x1: m.l, x2: w - m.r, y1: Y(0), y2: Y(0) }, svg);
    var nx = Math.max(2, Math.min(7, Math.floor(w / 100))), span = xmax - xmin;
    for (var k = 0; k < nx; k++) {
      var t = xmin + span * k / (nx - 1);
      el('text', { 'class': 'ch-axis', x: X(t), y: h - 7, 'text-anchor': k === 0 ? 'start' : k === nx - 1 ? 'end' : 'middle' }, svg).textContent = span > 500 * DAY ? fmonth(t) : fdate(t, span > 150 * DAY);
    }

    // highlight band (behind the lines)
    var hlRect = null;
    if (st.hl) {
      var a = Math.max(xmin, Math.min(st.hl.a, st.hl.b)), b = Math.min(xmax, Math.max(st.hl.a, st.hl.b));
      if (b > a) hlRect = el('rect', { 'class': 'xp-hlrect', x: X(a), y: m.t, width: Math.max(1, X(b) - X(a)), height: ih }, svg);
    }
    L.lines.forEach(function (ln) {
      var d = '', pen = false;
      for (var i = i0; i <= i1; i++) {
        var v = ln.y[i];
        if (v === null) { pen = false; continue; }
        d += (pen ? 'L' : 'M') + X(T[i]).toFixed(1) + ' ' + Y(v).toFixed(1); pen = true;
      }
      if (!d) return;
      var p = el('path', { 'class': 'xp-line' + (ln.cls ? ' xp-' + ln.cls : ''), d: d, stroke: ln.color }, svg);
      if (ln.dash) p.setAttribute('stroke-dasharray', ln.dash);
    });

    var cross = el('line', { 'class': 'ch-cross', y1: m.t, y2: h - m.b, visibility: 'hidden' }, svg);
    var dragRect = el('rect', { 'class': 'xp-drag', y: m.t, height: ih, visibility: 'hidden' }, svg);
    var overlay = el('rect', { x: m.l, y: m.t, width: iw, height: ih, fill: 'transparent' }, svg);
    overlay.style.touchAction = 'pan-y';
    overlay.style.cursor = 'crosshair';
    var tip = document.createElement('div'); tip.className = 'ch-tip'; tip.hidden = true; host.appendChild(tip);

    var legend = document.createElement('div'); legend.className = 'ch-legend';
    L.lines.forEach(function (ln) {
      var s = document.createElement('span'); s.innerHTML = '<i class="sw" style="background:' + ln.color + '"></i>'; s.appendChild(document.createTextNode(ln.name)); legend.appendChild(s);
    });
    host.appendChild(legend);

    function px(ev) { var r = svg.getBoundingClientRect(); return Math.min(Math.max(ev.clientX - r.left, m.l), w - m.r); }
    function showTip(ev) {
      var x = px(ev), i = Math.min(i1, Math.max(i0, nearest(invX(x))));
      var rows = L.lines.map(function (ln) { return ln.y[i] === null ? null : '<div><span><i class="sw" style="background:' + ln.color + '"></i>' + esc(ln.name) + '</span><em>' + fmtFull(ln.y[i]) + '</em></div>'; }).filter(Boolean);
      if (!rows.length) { tip.hidden = true; return; }
      tip.innerHTML = '<b>' + fdate(T[i], true) + '</b>' + rows.join('');
      tip.hidden = false;
      var xx = X(T[i]), tw = tip.offsetWidth, left = xx + 12;
      if (left + tw > w - 4) left = xx - tw - 12;
      tip.style.left = Math.max(4, left) + 'px'; tip.style.top = (m.t + 4) + 'px';
      cross.setAttribute('x1', xx); cross.setAttribute('x2', xx); cross.setAttribute('visibility', 'visible');
    }
    function hideTip() { tip.hidden = true; cross.setAttribute('visibility', 'hidden'); }

    var drag = null;
    overlay.addEventListener('pointerdown', function (ev) {
      drag = { x0: px(ev), moved: false }; overlay.setPointerCapture(ev.pointerId); showTip(ev);
    });
    overlay.addEventListener('pointermove', function (ev) {
      var x = px(ev);
      if (drag) {
        if (Math.abs(x - drag.x0) > 5) drag.moved = true;
        if (drag.moved) {
          tip.hidden = true; cross.setAttribute('visibility', 'hidden');
          dragRect.setAttribute('x', Math.min(drag.x0, x)); dragRect.setAttribute('width', Math.abs(x - drag.x0)); dragRect.setAttribute('visibility', 'visible');
          return;
        }
      }
      showTip(ev);
    });
    overlay.addEventListener('pointerup', function (ev) {
      if (!drag) return;
      var x = px(ev), d = drag; drag = null;
      dragRect.setAttribute('visibility', 'hidden');
      if (d.moved) {
        var a = invX(Math.min(d.x0, x)), b = invX(Math.max(d.x0, x));
        st.hl = { a: T[nearest(a)], b: T[nearest(b)] };
        if (st.hl.b <= st.hl.a) st.hl = null;
        render();
      } else if (st.hl) { st.hl = null; render(); }
    });
    overlay.addEventListener('pointercancel', function () { drag = null; dragRect.setAttribute('visibility', 'hidden'); });
    overlay.addEventListener('pointerleave', function (e) { if (e.pointerType === 'mouse' && !drag) hideTip(); });
    overlay.addEventListener('dblclick', function () { if (st.hl) { st.hl = null; render(); } });
    view = { lines: L.lines, i0: i0, i1: i1 };
  }

  function esc(s) { return String(s).replace(/[&<>"]/g, function (c) { return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]; }); }

  // ---- highlight bar + stats ----
  function stats() {
    var tbl = $('xp-stats'), bar = $('xp-hl');
    if (!view) { tbl.innerHTML = ''; bar.hidden = true; return; }
    var a, b, label;
    if (st.hl) { a = nearest(st.hl.a); b = nearest(st.hl.b); label = 'Highlighted period'; }
    else { a = view.i0; b = view.i1; label = 'Visible period'; }
    bar.hidden = !st.hl;
    if (st.hl) {
      bar.innerHTML = '<span><b>Highlighted:</b> ' + fdate(T[a], true) + ' → ' + fdate(T[b], true) + ' (' + Math.round((T[b] - T[a]) / DAY) + ' days)</span>' +
        '<span class="xp-hl-act"><button type="button" class="btn" id="xp-zoom">Zoom in</button><button type="button" class="btn" id="xp-clear">Clear</button></span>';
      $('xp-zoom').onclick = function () { st.from = T[a]; st.to = T[b]; preset = null; st.hl = null; sync(); };
      $('xp-clear').onclick = function () { st.hl = null; render(); };
    }
    var head = '<thead><tr><th>' + label + ': ' + fdate(T[a], true) + ' – ' + fdate(T[b], true) + '</th><th class="r">Start</th><th class="r">End</th><th class="r">Change</th><th class="r">%</th><th class="r" title="Money put in, net of sales, at the end of the period">Invested</th></tr></thead>';
    var rows = view.lines.filter(function (l) { return l.cls !== 'inv'; }).map(function (l) {
      var s = null, e = null, i, ivEnd = null;
      if (l.inv) for (i = b; i >= a; i--) if (l.inv[i] !== null) { ivEnd = l.inv[i]; break; }
      for (i = a; i <= b; i++) if (l.y[i] !== null) { s = l.y[i]; break; }
      for (i = b; i >= a; i--) if (l.y[i] !== null) { e = l.y[i]; break; }
      if (s === null || e === null) return '<tr><td>' + dot(l) + esc(l.name) + '</td><td class="r" colspan="5">no data</td></tr>';
      var ch = e - s, pct = Math.abs(s) > 1e-9 ? ch / Math.abs(s) * 100 : null, cls = ch > 0 ? 'pos' : ch < 0 ? 'neg' : '';
      var idx = st.metric === 'index';
      return '<tr><td>' + dot(l) + esc(l.name) + '</td><td class="r">' + fmtFull(s) + '</td><td class="r">' + fmtFull(e) + '</td>' +
        '<td class="r ' + cls + '">' + (idx ? (ch >= 0 ? '+' : '') + ch.toFixed(1) + ' pts' : (ch >= 0 ? '+' : '-') + money(Math.abs(ch), 0)) + '</td>' +
        '<td class="r ' + cls + '">' + (pct === null ? '—' : (pct >= 0 ? '+' : '') + pct.toFixed(1) + '%') + '</td>' +
        '<td class="r">' + (ivEnd === null ? '—' : money(ivEnd, 0)) + '</td></tr>';
    }).join('');
    tbl.innerHTML = head + '<tbody>' + rows + '</tbody>';
    var note = { value: 'Value changes include money you added or took out. Use Gain / loss or Price index to compare how investments actually performed.', gain: 'Gain / loss is value minus the money you put in (net of sales); it excludes distributions paid out as cash.', index: 'Price index starts each investment at 100 on the first day of the range, so lines are directly comparable regardless of how much you hold.' }[st.metric];
    $('xp-note').textContent = note;
    if (window.FT && FT.labelTables) FT.labelTables();
  }
  function dot(l) { return '<i class="sw" style="background:' + l.color + '"></i> '; }

  // ---- picker ----
  function buildList() {
    var box = $('xp-list'); box.innerHTML = '';
    function row(key, name, color, sub) {
      var lb = document.createElement('label'); lb.className = 'xp-item'; lb.setAttribute('data-k', key);
      var cb = document.createElement('input'); cb.type = 'checkbox'; cb.checked = !!st.sel[key];
      cb.onchange = function () { if (cb.checked) st.sel[key] = true; else delete st.sel[key]; render(); };
      lb.appendChild(cb);
      lb.insertAdjacentHTML('beforeend', '<i class="sw" style="background:' + color + '"></i><span class="xp-nm">' + esc(name) + '</span><span class="muted s">' + esc(sub) + '</span>');
      box.appendChild(lb);
    }
    row(TOTAL, 'Total portfolio', 'var(--accent)', money(D.total.latest, 0));
    items.forEach(function (it) { row(it.key, it.sym, it.color, it.latest > 0 ? money(it.latest, 0) : 'sold'); box.lastChild.title = it.name; });
  }
  function syncList() {
    Array.prototype.forEach.call($('xp-list').children, function (lb) {
      var k = lb.getAttribute('data-k'); lb.firstChild.checked = !!st.sel[k];
      lb.classList.toggle('dis', k === TOTAL && st.metric === 'index');
    });
  }

  // ---- wiring ----
  function sync() { // controls <-> state, then redraw
    $('xp-from').value = iso(st.from); $('xp-to').value = iso(st.to);
    $('xp-from').min = $('xp-to').min = iso(tmin); $('xp-from').max = $('xp-to').max = iso(tmax);
    render();
  }
  function render() {
    Array.prototype.forEach.call($('xp-metric').children, function (b) { b.classList.toggle('on', b.getAttribute('data-m') === st.metric); });
    $('xp-combine').checked = st.combine; $('xp-inv').checked = st.inv;
    $('xp-inv-wrap').hidden = st.metric !== 'value';
    markPreset(); syncList(); draw(); stats(); persist();
  }
  Array.prototype.forEach.call($('xp-metric').children, function (b) {
    b.onclick = function () { st.metric = b.getAttribute('data-m'); st.hl = null; render(); };
  });
  $('xp-combine').onchange = function () { st.combine = this.checked; render(); };
  $('xp-inv').onchange = function () { st.inv = this.checked; render(); };
  $('xp-all').onclick = function () { items.forEach(function (it) { st.sel[it.key] = true; }); render(); };
  $('xp-none').onclick = function () { st.sel = {}; render(); };
  $('xp-top').onclick = function () { st.sel = {}; items.slice(0, 5).forEach(function (it) { st.sel[it.key] = true; }); render(); };
  function dateInput() {
    var f = parseISO($('xp-from').value), t = parseISO($('xp-to').value);
    if (f === null || t === null) return;
    if (f > t) { var x = f; f = t; t = x; }
    st.from = Math.max(tmin, f); st.to = Math.min(tmax, t); preset = null; st.hl = null; render();
  }
  $('xp-from').onchange = $('xp-to').onchange = dateInput;

  // keep only keys that still exist (data may have changed since the choice was saved)
  var valid = {}; valid[TOTAL] = true; items.forEach(function (it) { valid[it.key] = true; });
  Object.keys(st.sel).forEach(function (k) { if (!valid[k]) delete st.sel[k]; });
  if (!Object.keys(st.sel).length) st.sel[TOTAL] = true;

  buildPresets(); buildList();
  if (preset === null || preset === undefined) preset = 'all';
  applyPreset(preset);
  var rafId, lastW = host.clientWidth;
  if (window.ResizeObserver) new ResizeObserver(function () {
    if (host.clientWidth === lastW) return; lastW = host.clientWidth;
    cancelAnimationFrame(rafId); rafId = requestAnimationFrame(function () { draw(); });
  }).observe(host);
})();
