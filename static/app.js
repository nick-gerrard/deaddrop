(function () {
  var toastEl = document.getElementById('toast'), toastTimer;
  function clearToast() {
    clearTimeout(toastTimer);
    if (toastEl) toastEl.classList.remove('show');
  }
  function toast(msg) {
    if (!toastEl) return;
    toastEl.textContent = msg;
    toastEl.classList.add('show');
    clearTimeout(toastTimer);
    toastTimer = setTimeout(clearToast, 3500);
  }
  function isStateReq(e) {
    return (((e.detail && e.detail.pathInfo) || {}).requestPath || '').indexOf('/state') >= 0;
  }

  // ---------- sound & mute ----------
  var MUTE_KEY = 'dd_mute';
  var muted = false;
  try { muted = localStorage.getItem(MUTE_KEY) === '1'; } catch (_) {}
  var audioCtx = null;

  function updateMuteBtn() {
    var btn = document.getElementById('mute-btn');
    if (!btn) return;
    btn.setAttribute('aria-pressed', muted ? 'true' : 'false');
    btn.textContent = muted ? 'Muted' : 'Sound on';
  }
  updateMuteBtn();

  function getAudio() {
    if (muted) return null;
    var Ctx = window.AudioContext || window.webkitAudioContext;
    if (!Ctx) return null;
    if (!audioCtx) {
      try { audioCtx = new Ctx(); } catch (_) { return null; }
    }
    if (audioCtx.state === 'suspended') {
      try { audioCtx.resume(); } catch (_) {}
    }
    return audioCtx;
  }

  function playBlip() {
    var ctx = getAudio();
    if (!ctx) return;
    try {
      var now = ctx.currentTime;
      var osc = ctx.createOscillator();
      var gain = ctx.createGain();
      osc.type = 'sine';
      osc.frequency.setValueAtTime(580, now);
      osc.frequency.setValueAtTime(820, now + 0.06);
      gain.gain.setValueAtTime(0.001, now);
      gain.gain.exponentialRampToValueAtTime(0.11, now + 0.015);
      gain.gain.exponentialRampToValueAtTime(0.001, now + 0.16);
      osc.connect(gain);
      gain.connect(ctx.destination);
      osc.start(now);
      osc.stop(now + 0.17);
    } catch (_) {}
  }

  function playStamp() {
    var ctx = getAudio();
    if (!ctx) return;
    try {
      var now = ctx.currentTime;
      var osc = ctx.createOscillator();
      var gain = ctx.createGain();
      osc.type = 'triangle';
      osc.frequency.setValueAtTime(220, now);
      osc.frequency.exponentialRampToValueAtTime(90, now + 0.08);
      gain.gain.setValueAtTime(0.14, now);
      gain.gain.exponentialRampToValueAtTime(0.001, now + 0.09);
      osc.connect(gain);
      gain.connect(ctx.destination);
      osc.start(now);
      osc.stop(now + 0.1);
    } catch (_) {}
  }

  function playThud() {
    var ctx = getAudio();
    if (!ctx) return;
    try {
      var now = ctx.currentTime;
      var osc = ctx.createOscillator();
      var gain = ctx.createGain();
      osc.type = 'sawtooth';
      osc.frequency.setValueAtTime(135, now);
      osc.frequency.exponentialRampToValueAtTime(42, now + 0.28);
      gain.gain.setValueAtTime(0.18, now);
      gain.gain.exponentialRampToValueAtTime(0.001, now + 0.3);
      osc.connect(gain);
      gain.connect(ctx.destination);
      osc.start(now);
      osc.stop(now + 0.32);
    } catch (_) {}
  }

  // ---------- title flash ----------
  var baseTitle = document.title;
  var flashTimer = null;
  var flashOn = false;

  function stopTitleFlash() {
    if (flashTimer) {
      clearInterval(flashTimer);
      flashTimer = null;
    }
    flashOn = false;
    document.title = baseTitle;
  }

  function startTitleFlash() {
    if (document.hasFocus()) return;
    if (flashTimer) return;
    flashOn = true;
    document.title = '▶ Your move — Dead Drop';
    flashTimer = setInterval(function () {
      flashOn = !flashOn;
      document.title = flashOn ? '▶ Your move — Dead Drop' : baseTitle;
    }, 1000);
  }

  window.addEventListener('focus', stopTitleFlash);

  // ---------- turn countdown ----------
  var clockOffset = 0;
  var expiredDeadline = 0;

  function syncClockOffset() {
    var pill = document.querySelector('.timer-pill[data-now]');
    if (pill) {
      var srvNow = Number(pill.dataset.now);
      if (srvNow > 0) clockOffset = srvNow - Date.now();
    }
  }

  function tickTimer() {
    var pill = document.querySelector('.timer-pill[data-deadline]');
    if (!pill) return;
    var dl = Number(pill.dataset.deadline);
    if (!dl) return;
    var remMs = dl - (Date.now() + clockOffset);
    var secs = Math.max(0, Math.ceil(remMs / 1000));
    var m = Math.floor(secs / 60);
    var s = secs % 60;
    pill.textContent = m + ':' + (s < 10 ? '0' : '') + s;
    pill.classList.toggle('low', secs <= 10);
    if (remMs <= 0 && expiredDeadline !== dl) {
      expiredDeadline = dl;
      var g = document.getElementById('game');
      if (g && window.htmx) htmx.trigger(g, 'refresh');
    }
  }

  syncClockOffset();
  setInterval(tickTimer, 250);

  // ---------- card confirm (arm / disarm) ----------
  function disarmAll() {
    document.querySelectorAll('.card.armed').forEach(function (c) { c.classList.remove('armed'); });
  }

  // ---------- htmx lifecycle ----------
  var b = document.body;
  var prev = null;

  b.addEventListener('htmx:beforeRequest', function (e) {
    if (e.detail.elt && e.detail.elt.id === 'game') {
      if (document.hidden) {
        e.preventDefault();
        return;
      }
      var ae = document.activeElement;
      if (ae && ae.closest && ae.closest('#settings-panel')) {
        e.preventDefault();
      }
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
    var g = document.getElementById('game');
    var sp = document.getElementById('settings-panel');
    var armedEl = document.querySelector('.card.armed');
    prev = {
      revealed: {},
      yourTurn: g ? g.dataset.yourTurn : '0',
      turnPhase: g ? g.dataset.turnPhase : '',
      hadClueInput: !!document.getElementById('clue-word'),
      settingsOpen: !!(sp && sp.open),
      armedIdx: armedEl ? armedEl.dataset.i : null
    };
    document.querySelectorAll('.card.revealed').forEach(function (c) { prev.revealed[c.dataset.i] = 1; });
  });

  b.addEventListener('htmx:afterSwap', function () {
    if (!prev) return;
    var snap = prev;
    prev = null;

    var newlyRevealed = 0;
    var burnedRevealed = false;
    document.querySelectorAll('.card.revealed').forEach(function (c) {
      if (!snap.revealed[c.dataset.i]) {
        c.classList.add('fresh');
        newlyRevealed++;
        if (c.classList.contains('c-burned')) burnedRevealed = true;
      }
    });

    if (!document.querySelector('.overlay')) delete document.documentElement.dataset.ovx;

    // Sync series score in header
    var g = document.getElementById('game');
    if (g) {
      var ew = document.querySelector('#series-score [data-wins="ember"]');
      var tw = document.querySelector('#series-score [data-wins="tide"]');
      if (ew && g.dataset.emberWins != null) ew.textContent = g.dataset.emberWins;
      if (tw && g.dataset.tideWins != null) tw.textContent = g.dataset.tideWins;
    }

    // Restore settings panel open state
    if (snap.settingsOpen) {
      var sp = document.getElementById('settings-panel');
      if (sp) sp.open = true;
    }

    // Restore armed card if still clickable and wasn't cleared
    if (snap.armedIdx != null) {
      var card = document.querySelector('.card.clickable[data-i="' + snap.armedIdx + '"]');
      if (card) card.classList.add('armed');
    }

    // Autofocus clue input when it newly appears
    var clueInput = document.getElementById('clue-word');
    if (clueInput && !snap.hadClueInput) {
      var ae = document.activeElement;
      if (!ae || (ae.tagName !== 'INPUT' && ae.tagName !== 'TEXTAREA' && ae.tagName !== 'SELECT')) {
        clueInput.focus();
      }
    }

    // Sounds & attention cues
    if (burnedRevealed) {
      playThud();
    } else if (newlyRevealed > 0) {
      playStamp();
    }

    if (g) {
      var nowYourTurn = g.dataset.yourTurn === '1';
      var phaseChanged = g.dataset.turnPhase !== snap.turnPhase;
      if (nowYourTurn && (snap.yourTurn !== '1' || phaseChanged)) {
        playBlip();
        startTitleFlash();
      } else if (!nowYourTurn) {
        stopTitleFlash();
      }
    }

    syncClockOffset();
    tickTimer();
  });

  document.addEventListener('visibilitychange', function () {
    if (!document.hidden) {
      var g = document.getElementById('game');
      if (g && window.htmx) htmx.trigger(g, 'refresh');
    }
  });

  document.addEventListener('change', function (e) {
    if (e.target && e.target.id === 'set-pack') {
      var wrap = document.getElementById('custom-wrap');
      if (wrap) wrap.classList.toggle('hidden', e.target.value !== 'custom');
    }
  });

  document.addEventListener('keydown', function (e) {
    if (e.key === 'Escape') disarmAll();
  });

  document.addEventListener('click', function (e) {
    var mute = e.target.closest('#mute-btn');
    if (mute) {
      muted = !muted;
      try { localStorage.setItem(MUTE_KEY, muted ? '1' : '0'); } catch (_) {}
      updateMuteBtn();
      if (!muted) playBlip();
      return;
    }

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
      return;
    }

    // Clicking Reveal or Mark clears armed state so afterSwap doesn't re-arm it.
    if (e.target.closest('.ca-reveal') || e.target.closest('.ca-mark')) {
      disarmAll();
      return;
    }

    var face = e.target.closest('[data-select-card]');
    if (face) {
      var card = face.closest('.card.clickable');
      if (card) {
        var wasArmed = card.classList.contains('armed');
        disarmAll();
        if (!wasArmed) card.classList.add('armed');
      }
      return;
    }

    if (!e.target.closest('.card.armed')) {
      disarmAll();
    }
  });
})();
