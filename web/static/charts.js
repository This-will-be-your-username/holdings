/* FundTrack charts: dependency-free, responsive SVG charts with touch/mouse tooltips.
   Config comes from outlook.go / outlookpage.go (ChartCfg). Re-draws at the real
   container width, so text stays legible on phones. */
(function () {
  'use strict';
  var NS = 'http://www.w3.org/2000/svg';

  function el(name, attrs, parent) {
    var e = document.createElementNS(NS, name);
    for (var k in attrs) e.setAttribute(k, attrs[k]);
    if (parent) parent.appendChild(e);
    return e;
  }

  var fmts = {
    money: {
      axis: function (v) {
        var a = Math.abs(v);
        if (a >= 1e6) return '$' + (v / 1e6).toFixed(a >= 1e7 ? 0 : 1) + 'M';
        if (a >= 1e3) return '$' + (v / 1e3).toFixed(a >= 1e4 ? 0 : 1) + 'k';
        return '$' + Math.round(v);
      },
      full: function (v) { return '$' + Math.round(v).toLocaleString('en-AU'); }
    },
    num: {
      axis: function (v) { return v >= 1000 ? Math.round(v).toLocaleString('en-AU') : v.toFixed(v >= 100 ? 0 : 1); },
      full: function (v) { return v.toLocaleString('en-AU', { maximumFractionDigits: 2 }); }
    },
    fx: {
      axis: function (v) { return v.toFixed(3); },
      full: function (v) { return v.toFixed(4); }
    }
  };

  function niceTicks(min, max, n) {
    var span = (max - min) || 1, step0 = span / n;
    var mag = Math.pow(10, Math.floor(Math.log10(step0))), m = step0 / mag;
    var step = (m < 1.5 ? 1 : m < 3 ? 2 : m < 7 ? 5 : 10) * mag, out = [];
    for (var v = Math.ceil(min / step) * step; v <= max + step * 1e-6; v += step) out.push(v);
    return out;
  }

  function fmtDate(t, long) {
    return new Date(t).toLocaleDateString('en-AU', long
      ? { day: 'numeric', month: 'short', year: 'numeric', timeZone: 'UTC' }
      : { month: 'short', year: '2-digit', timeZone: 'UTC' });
  }

  // value of a sorted [{t,v}] list at time t (linear interpolation, clamped)
  function at(pts, t) {
    if (!pts || !pts.length) return null;
    if (t < pts[0].t || t > pts[pts.length - 1].t) return null;
    var lo = 0, hi = pts.length - 1;
    while (hi - lo > 1) { var mid = (lo + hi) >> 1; if (pts[mid].t <= t) lo = mid; else hi = mid; }
    var a = pts[lo], b = pts[hi];
    if (b.t === a.t) return a.v;
    return a.v + (b.v - a.v) * (t - a.t) / (b.t - a.t);
  }

  function fanSeries(fan, key) {
    var out = [];
    for (var i = 0; i < fan.t.length; i++) out.push({ t: fan.t[i], v: fan[key][i] });
    return out;
  }

  function pathOf(pts, X, Y) {
    var d = '';
    for (var i = 0; i < pts.length; i++) d += (i ? 'L' : 'M') + X(pts[i].t).toFixed(1) + ' ' + Y(pts[i].v).toFixed(1);
    return d;
  }

  function draw(host) {
    var cfg = (window.__CHARTS__ || {})[host.getAttribute('data-chart')];
    host.innerHTML = '';
    if (!cfg || !cfg.hist || cfg.hist.length < 2) return;
    var mini = !!cfg.mini, fmt = fmts[cfg.fmt] || fmts.money;
    var w = Math.max(host.clientWidth, 180);
    var h = mini ? 56 : (w < 480 ? 230 : w < 760 ? 280 : 340);
    var fan = cfg.fan ? {
      t: cfg.fan.t, p10: fanSeries(cfg.fan, 'p10'), p25: fanSeries(cfg.fan, 'p25'), p50: fanSeries(cfg.fan, 'p50'),
      p75: fanSeries(cfg.fan, 'p75'), p90: fanSeries(cfg.fan, 'p90')
    } : null;

    // domains
    var xmin = cfg.hist[0].t, xmax = cfg.hist[cfg.hist.length - 1].t;
    var ymin = Infinity, ymax = -Infinity;
    function scan(list, skipFan) {
      for (var i = 0; i < list.length; i++) {
        var v = list[i].v; if (v < ymin) ymin = v; if (v > ymax) ymax = v;
      }
    }
    scan(cfg.hist);
    if (!mini) {
      (cfg.lines || []).forEach(function (l) { scan(l.pts); if (l.pts.length) xmax = Math.max(xmax, l.pts[l.pts.length - 1].t); });
      if (fan) { scan(fan.p10); scan(fan.p90); xmax = Math.max(xmax, fan.t[fan.t.length - 1]); }
    }
    var pad = (ymax - ymin) * (mini ? 0.1 : 0.06) || ymax * 0.05 || 1;
    ymin -= pad; ymax += pad;
    if (!mini && ymin > 0 && ymin < (ymax - ymin) * 0.9) ymin = Math.max(0, ymin); // keep zero baseline tidy for small ranges

    var ticksY = mini ? [] : niceTicks(ymin, ymax, h < 260 ? 3 : 5);
    var maxLbl = 0; ticksY.forEach(function (v) { maxLbl = Math.max(maxLbl, fmt.axis(v).length); });
    var m = mini ? { l: 2, r: 2, t: 4, b: 4 } : { l: Math.max(34, maxLbl * 7 + 12), r: 10, t: 10, b: 24 };
    var iw = w - m.l - m.r, ih = h - m.t - m.b;
    function X(t) { return m.l + iw * (t - xmin) / (xmax - xmin); }
    function Y(v) { return m.t + ih * (1 - (v - ymin) / (ymax - ymin)); }

    var svg = el('svg', { width: w, height: h, viewBox: '0 0 ' + w + ' ' + h, role: 'img', 'aria-label': cfg.name + ' chart' }, host);
    svg.setAttribute('class', 'ch' + (mini ? ' mini' : ''));

    if (!mini) {
      ticksY.forEach(function (v) {
        el('line', { 'class': 'ch-grid', x1: m.l, x2: w - m.r, y1: Y(v), y2: Y(v) }, svg);
        var tx = el('text', { 'class': 'ch-axis', x: m.l - 6, y: Y(v) + 4, 'text-anchor': 'end' }, svg);
        tx.textContent = fmt.axis(v);
      });
      var nx = Math.max(2, Math.min(6, Math.floor(w / 95)));
      for (var i = 0; i < nx; i++) {
        var t = xmin + (xmax - xmin) * i / (nx - 1);
        var anchor = i === 0 ? 'start' : i === nx - 1 ? 'end' : 'middle';
        var lx = el('text', { 'class': 'ch-axis', x: X(t), y: h - 6, 'text-anchor': anchor }, svg);
        lx.textContent = fmtDate(t);
      }
    }

    // forecast fan (behind lines)
    if (fan && !mini) {
      var band = function (lo, hi, cls) {
        var d = pathOf(hi, X, Y);
        for (var i = lo.length - 1; i >= 0; i--) d += 'L' + X(lo[i].t).toFixed(1) + ' ' + Y(lo[i].v).toFixed(1);
        el('path', { 'class': cls, d: d + 'Z' }, svg);
      };
      band(fan.p10, fan.p90, 'ch-band1');
      band(fan.p25, fan.p75, 'ch-band2');
    }
    if (!mini && cfg.now) { // "today" divider
      var nxp = X(cfg.now);
      if (fan) {
        el('line', { 'class': 'ch-now', x1: nxp, x2: nxp, y1: m.t, y2: h - m.b }, svg);
        var tl = el('text', { 'class': 'ch-axis', x: nxp - 4, y: m.t + 10, 'text-anchor': 'end' }, svg);
        tl.textContent = 'Past';
        var tr = el('text', { 'class': 'ch-axis', x: nxp + 4, y: m.t + 10 }, svg);
        tr.textContent = 'Forecast';
      }
    }
    if (!mini) (cfg.lines || []).forEach(function (l) {
      if (l.pts.length > 1) el('path', { 'class': 'ch-line ch-' + l.cls, d: pathOf(l.pts, X, Y) }, svg);
    });
    if (fan && !mini) el('path', { 'class': 'ch-line ch-med', d: pathOf(fan.p50, X, Y) }, svg);
    if (mini) {
      var last = cfg.hist[cfg.hist.length - 1], first = cfg.hist[0];
      el('path', { 'class': 'ch-line ch-hist ' + ((last.v >= first.v) !== !!cfg.invert ? 'up' : 'down'), d: pathOf(cfg.hist, X, Y) }, svg);
    } else {
      el('path', { 'class': 'ch-line ch-hist', d: pathOf(cfg.hist, X, Y) }, svg);
    }

    // legend
    if (!mini) {
      var items = [[cfg.histName || 'Actual', 'hist']];
      (cfg.lines || []).forEach(function (l) { items.push([l.name, l.cls]); });
      if (fan) items.push(['Median forecast', 'med'], ['50% range', 'band2'], ['80% range', 'band1']);
      var lg = document.createElement('div'); lg.className = 'ch-legend';
      items.forEach(function (it) {
        var s = document.createElement('span'); s.innerHTML = '<i class="sw sw-' + it[1] + '"></i>';
        s.appendChild(document.createTextNode(it[0])); lg.appendChild(s);
      });
      host.appendChild(lg);
    }

    // tooltip + crosshair
    var tip = document.createElement('div'); tip.className = 'ch-tip'; tip.hidden = true; host.appendChild(tip);
    var cross = el('line', { 'class': 'ch-cross', y1: m.t, y2: h - m.b, visibility: 'hidden' }, svg);
    var overlay = el('rect', { x: m.l, y: m.t, width: iw, height: ih, fill: 'transparent' }, svg);
    overlay.style.touchAction = 'pan-y';

    function show(ev) {
      var r = svg.getBoundingClientRect();
      var px = Math.min(Math.max(ev.clientX - r.left, m.l), w - m.r);
      var t = xmin + (px - m.l) / iw * (xmax - xmin);
      var rows = [], hv = at(cfg.hist, t);
      var head = fmtDate(t, true);
      if (hv != null) rows.push([cfg.histName || 'Actual', fmt.full(hv)]);
      if (!mini) {
        (cfg.lines || []).forEach(function (l) {
          var v = at(l.pts, t); if (v != null) rows.push([l.name, fmt.full(v)]);
        });
        if (fan && t > cfg.now) {
          var a = at(fan.p10, t), b = at(fan.p90, t), c = at(fan.p50, t), d = at(fan.p25, t), e = at(fan.p75, t);
          if (c != null) {
            rows.push(['Median', fmt.full(c)]);
            rows.push(['50% range', fmt.full(d) + ' – ' + fmt.full(e)]);
            rows.push(['80% range', fmt.full(a) + ' – ' + fmt.full(b)]);
          }
        }
      }
      if (!rows.length) { hide(); return; }
      tip.innerHTML = '<b>' + head + '</b>' + rows.map(function (r) { return '<div><span>' + r[0] + '</span><em>' + r[1] + '</em></div>'; }).join('');
      tip.hidden = false;
      var tw = tip.offsetWidth, left = px + 12;
      if (left + tw > w - 4) left = px - tw - 12;
      tip.style.left = Math.max(4, left) + 'px';
      tip.style.top = (mini ? -4 : m.t + 4) + 'px';
      cross.setAttribute('x1', px); cross.setAttribute('x2', px); cross.setAttribute('visibility', 'visible');
    }
    function hide() { tip.hidden = true; cross.setAttribute('visibility', 'hidden'); }
    overlay.addEventListener('pointermove', show);
    overlay.addEventListener('pointerdown', show);
    overlay.addEventListener('pointerleave', function (e) { if (e.pointerType === 'mouse') hide(); });
    document.addEventListener('pointerdown', function (e) { if (!host.contains(e.target)) hide(); }, { passive: true });
  }

  var hosts = [];
  function init() {
    hosts = Array.prototype.slice.call(document.querySelectorAll('[data-chart]'));
    hosts.forEach(function (h) {
      draw(h);
      if (window.ResizeObserver) {
        var last = h.clientWidth, raf;
        new ResizeObserver(function () {
          if (h.clientWidth === last) return; // ignore height-only changes from our own drawing
          last = h.clientWidth; cancelAnimationFrame(raf); raf = requestAnimationFrame(function () { draw(h); });
        }).observe(h);
      }
    });
  }

  // Tables turn into stacked cards on narrow screens: label every cell from its column header.
  function labelTables() {
    Array.prototype.forEach.call(document.querySelectorAll('table'), function (t) {
      if (!t.tHead || !t.tHead.rows.length) return;
      var heads = Array.prototype.map.call(t.tHead.rows[0].cells, function (c) { return c.textContent.trim(); });
      t.classList.add('stack');
      Array.prototype.forEach.call(t.tBodies, function (b) {
        Array.prototype.forEach.call(b.rows, function (r) {
          if (r.cells.length === 1 && r.cells[0].colSpan > 1) { r.classList.add('note-row'); return; }
          var ci = 0; // column position, accounting for colspan
          Array.prototype.forEach.call(r.cells, function (c, i) {
            var pos = ci; ci += c.colSpan || 1;
            if (c.colSpan > 1 || !heads[pos]) return;
            c.setAttribute('data-label', heads[pos]);
            if (i > 0 && c.childNodes.length) { // keep value + sub-line together as one flex item when stacked
              var v = document.createElement('span'); v.className = 'v';
              while (c.firstChild) v.appendChild(c.firstChild);
              c.appendChild(v);
            }
          });
        });
      });
    });
  }

  function start() { labelTables(); init(); }
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', start); else start();
})();
