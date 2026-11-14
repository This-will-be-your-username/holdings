/* Keeps the dashboard's prices current while a market is open: every 30s ask the server whether a market is open and,
   if so (or once more right after it closes), re-fetch the page and swap in the live parts. */
(function () {
  'use strict';
  if (!document.getElementById('live-chip')) return;
  var wasOpen = document.getElementById('live-chip').getAttribute('data-open') === '1';
  var busy = false;

  function swap(doc, id) {
    var a = document.getElementById(id), b = doc.getElementById(id);
    if (a && b) { a.innerHTML = b.innerHTML; }
  }

  function refresh() {
    if (busy || document.hidden) return;
    busy = true;
    fetch('/api/live', { cache: 'no-store' }).then(function (r) { return r.json(); }).then(function (s) {
      var need = s.open || wasOpen;
      wasOpen = !!s.open;
      if (!need) return;
      return fetch(location.pathname + location.search, { cache: 'no-store' }).then(function (r) { return r.text(); }).then(function (html) {
        var doc = new DOMParser().parseFromString(html, 'text/html');
        ['live-head', 'live-stats', 'live-holdings'].forEach(function (id) { swap(doc, id); });
        if (window.FT && window.FT.labelTables) window.FT.labelTables();
      });
    }).catch(function () { /* offline or server restarting: try again next tick */ })
      .then(function () { busy = false; });
  }

  setInterval(refresh, 30000);
  document.addEventListener('visibilitychange', function () { if (!document.hidden) refresh(); });
})();
