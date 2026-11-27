(function () {
  var toastEl = document.getElementById('toast'), timer;
  function clearToast() {
    clearTimeout(timer);
    if (toastEl) toastEl.classList.remove('show');
  }
  function toast(msg) {
    if (!toastEl) return;
    toastEl.textContent = msg;
    toastEl.classList.add('show');
    clearTimeout(timer);
    timer = setTimeout(clearToast, 3500);
  }
  function isStateReq(e) {
    return (((e.detail && e.detail.pathInfo) || {}).requestPath || '').indexOf('/state') >= 0;
  }

  var b = document.body, prev = null;
  b.addEventListener('htmx:beforeRequest', function (e) {
    if (document.hidden && e.detail.elt && e.detail.elt.id === 'game') {
      e.preventDefault();
    }
  });
  b.addEventListener('htmx:responseError', function (e) {
    if (isStateReq(e)) return;
    toast((e.detail.xhr && e.detail.xhr.responseText) || 'Request failed');
  });
  b.addEventListener('htmx:afterRequest', function (e) {
    if (e.detail.successful && !isStateReq(e)) clearToast();
  });
  b.addEventListener('htmx:beforeSwap', function () {
    prev = {};
    document.querySelectorAll('.card.revealed').forEach(function (c) { prev[c.dataset.i] = 1; });
  });
  b.addEventListener('htmx:afterSwap', function () {
    if (!prev) return;
    document.querySelectorAll('.card.revealed').forEach(function (c) {
      if (!prev[c.dataset.i]) c.classList.add('fresh');
    });
    prev = null;
    if (!document.querySelector('.overlay')) delete document.documentElement.dataset.ovx;
  });

  document.addEventListener('visibilitychange', function () {
    if (!document.hidden) {
      var g = document.getElementById('game');
      if (g && window.htmx) htmx.trigger(g, 'refresh');
    }
  });

  document.addEventListener('click', function (e) {
    var dismiss = e.target.closest('[data-dismiss-overlay]');
    if (dismiss) {
      document.documentElement.dataset.ovx = '1';
      return;
    }
    var copy = e.target.closest('#copy-link');
    if (copy) {
      var url = location.origin + copy.dataset.path;
      if (navigator.clipboard && window.isSecureContext) {
        navigator.clipboard.writeText(url).then(function () { toast('Invite link copied'); }, function () { prompt('Copy this link:', url); });
      } else {
        prompt('Copy this link:', url);
      }
    }
  });
})();
