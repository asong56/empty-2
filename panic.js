// panic.js — Dual-password auth, decoy mode, and self-destruct logic.

'use strict';

// ── Constants

const MAX_FAILED_ATTEMPTS  = 3;    // lock out + destroy after this many wrong tries
const WRONG_ATTEMPT_DELAY  = 800;  // ms delay to prevent brute-force

// In-memory attempt counter (not localStorage — trivially resettable).
let _memAttemptCount = null; // null = not yet initialised

// ── GhostAuth

export class GhostAuth {

  constructor(config) {
    this.config      = config;
    this.attempts    = this._loadAttemptCount();
    this.maxAttempts = config.maxAttempts || MAX_FAILED_ATTEMPTS;
  }

  async unlock(input) {
    const inputHash = await sha256Hex(input);

    // ── Kill Code ────────────────────────────────────────────────────────────
    if (this.config.killCodeHash && inputHash === this.config.killCodeHash) {
      return this._executeSelfDestruct('kill_code');
    }

    // ── Real Password ─────────────────────────────────────────────────────────
    if (inputHash === this.config.realPasswordHash) {
      this._resetAttemptCount();
      return { mode: 'ghost' };
    }

    // ── Decoy Password ────────────────────────────────────────────────────────
    if (this.config.decoyPasswordHash && inputHash === this.config.decoyPasswordHash) {
      this._resetAttemptCount();
      return { mode: 'decoy' };
    }

    // ── Wrong Password ────────────────────────────────────────────────────────
    this.attempts++;
    this._saveAttemptCount(this.attempts);

    // Artificial delay to slow brute-force.
    await sleep(WRONG_ATTEMPT_DELAY * Math.min(this.attempts, 5));

    if (this.attempts >= this.maxAttempts) {
      return this._executeSelfDestruct('max_attempts');
    }

    return {
      mode: 'error',
      error: `incorrect (${this.maxAttempts - this.attempts} attempts remaining)`,
    };
  }

  // ── Self-Destruct

  /**
   * Executes the self-destruct sequence:
   * 1. Capture device_id BEFORE clearing storage (Fix 6.4)
   * 2. Clear localStorage (tokens, settings, cached identifiers)
   * 3. Delete IndexedDB ghost_cache (3-day message cache)
   * 4. Delete IndexedDB ghost_sw (offline outbox)
   * 5. Unregister Service Worker
   * 6. Notify Ghost Server to revoke this device (with pre-captured device_id)
   *
   * @param {string} reason - 'kill_code' | 'max_attempts' | 'manual'
   */
  async _executeSelfDestruct(reason) {
    console.log(`[ghost/panic] self-destruct triggered: ${reason}`);

    const deviceId = this._getDeviceID();

    const steps = [
      () => this._clearLocalStorage(),
      () => this._deleteIndexedDB('ghost_cache'),
      () => this._deleteIndexedDB('ghost_sw'),
      () => this._unregisterServiceWorker(),
      () => this._revokeDeviceOnServer(reason, deviceId),
    ];

    for (const step of steps) {
      try { await step(); } catch (e) {
        console.warn('[ghost/panic] step failed (continuing):', e);
      }
    }

    return { mode: 'destroyed', reason };
  }

  _clearLocalStorage() {

    const ghostKeys = [];
    for (let i = 0; i < localStorage.length; i++) {
      const key = localStorage.key(i);
      if (key && key.startsWith('ghost.')) ghostKeys.push(key);
    }

    const noise = () => crypto.getRandomValues(new Uint8Array(64))
      .reduce((s, b) => s + b.toString(16).padStart(2, '0'), '');
    ghostKeys.forEach(k => {
      try { localStorage.setItem(k, noise()); } catch (_) {}
      localStorage.removeItem(k);
    });

    sessionStorage.clear();
    return Promise.resolve();
  }

  _deleteIndexedDB(name) {
    return new Promise((resolve) => {
      const req = indexedDB.deleteDatabase(name);
      req.onsuccess = resolve;
      req.onerror   = resolve; // Resolve even on error — keep going.
      req.onblocked = resolve;

      setTimeout(resolve, 2000);
    });
  }

  _unregisterServiceWorker() {
    if (!navigator.serviceWorker) return Promise.resolve();
    return navigator.serviceWorker.getRegistrations()
      .then(regs => Promise.all(regs.map(reg => reg.unregister())));
  }

  async _revokeDeviceOnServer(reason, deviceId) {

    try {
      await fetch(`${this.config.ghostServerURL}/api/device/revoke`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ reason, device_id: deviceId }),
        signal: AbortSignal.timeout(3000), // Don't hang.
      });
    } catch {

    }
  }

  _getDeviceID() {
    return localStorage.getItem('ghost.device_id') || 'unknown';
  }

  _loadAttemptCount() {
    if (_memAttemptCount !== null) return _memAttemptCount;
    const stored = parseInt(sessionStorage.getItem('ghost.auth.attempts') || '0', 10);
    _memAttemptCount = isNaN(stored) ? 0 : stored;
    return _memAttemptCount;
  }

  _saveAttemptCount(n) {
    _memAttemptCount = n;
    try { sessionStorage.setItem('ghost.auth.attempts', String(n)); } catch (_) {}
  }

  _resetAttemptCount() {
    _memAttemptCount = 0;
    this.attempts = 0;
    try { sessionStorage.removeItem('ghost.auth.attempts'); } catch (_) {}
    localStorage.removeItem('ghost.auth.attempts'); // clean up legacy key if present
  }
}

// ── Decoy Mode

/**
 * DecoyMode renders a convincing, fully-functional news reader.
 * It fetches real public RSS feeds so it looks genuine under scrutiny.
 * No WireGuard, no message database, no Ghost server connection.
 */
export class DecoyMode {
  constructor() {
    this.feeds = [
      'https://feeds.bbci.co.uk/news/rss.xml',
      'https://rss.nytimes.com/services/xml/rss/nyt/HomePage.xml',
      'https://www.theguardian.com/world/rss',
    ];
  }

  /**
   * Activates decoy mode — replaces the Ghost UI with a news reader.
   */
  async activate() {

    document.title = 'Briefing — News Reader';

    document.getElementById('app').innerHTML = this._buildDecoyShell();

    await this._loadFeed(this.feeds[Math.floor(Math.random() * this.feeds.length)]);
  }

  _buildDecoyShell() {
    return `<div id="decoy-app" style=" max-width: 640px; margin: 0 auto; padding: 24px 16px; font-family: Georgia, 'Times New Roman', serif; background: #faf9f7; min-height: 100vh; color: #1a1a1a; "> <header style="border-bottom: 2px solid #1a1a1a; padding-bottom: 12px; margin-bottom: 24px;"> <h1 style="font-size: 1.6rem; font-weight: 700; margin: 0; letter-spacing: -0.02em;"> Briefing </h1> <p style="font-size: 0.8rem; color: #666; margin: 4px 0 0; font-family: monospace;"> World News — ${new Date().toLocaleDateString('en-US', { weekday: 'long', year: 'numeric', month: 'long', day: 'numeric' })} </p> </header> <div id="decoy-feed" style="display: flex; flex-direction: column; gap: 28px;"> <div style="text-align: center; padding: 40px; color: #aaa; font-family: monospace; font-size: 0.8rem;"> Loading headlines… </div> </div> <footer style="margin-top: 40px; padding-top: 16px; border-top: 1px solid #ddd; font-size: 0.72rem; color: #aaa; font-family: monospace; text-align: center;"> Briefing v2.1 · RSS Aggregator · No tracking </footer> </div>`;
  }

  async _loadFeed(feedURL) {

    const feedEl = document.getElementById('decoy-feed');
    try {
      const res = await fetch(
        '/api/decoy/feed?source=' + encodeURIComponent(feedURL),
        { signal: AbortSignal.timeout(6000) }
      );
      if (!res.ok) throw new Error('server unavailable');
      const xml = await res.text();
      const doc = new DOMParser().parseFromString(xml, 'text/xml');
      const items = Array.from(doc.querySelectorAll('item')).slice(0, 12);
      if (!feedEl || items.length === 0) throw new Error('no items');
      feedEl.innerHTML = items.map(item => {
        const title = item.querySelector('title') ? item.querySelector('title').textContent : '';
        const desc = item.querySelector('description') ? item.querySelector('description').textContent : '';
        const pub = item.querySelector('pubDate') ? item.querySelector('pubDate').textContent : '';
        return '<article>' +
          '<h2 style="font-size:1.05rem;margin:0 0 6px;line-height:1.35;">' + escapeHtml(title) + '</h2>' +
          '<p style="font-size:.82rem;color:#555;margin:0 0 6px;line-height:1.5;">' + escapeHtml(stripHTML(desc).slice(0, 200)) + '…</p>' +
          '<span style="font-size:.7rem;color:#aaa;font-family:monospace;">' + (pub ? new Date(pub).toLocaleString() : '') + '</span>' +
          '</article>';
      }).join('<hr style="border:none;border-top:1px solid #e8e8e8;margin:0;">');
    } catch (_) {
      if (!feedEl) return;
      var headlines = [
        { t: 'Global Markets Mixed Amid Rate Decision Uncertainty', a: '34m' },
        { t: 'Climate Summit Delegates Near Agreement on Emissions', a: '1h' },
        { t: 'Tech Earnings Season Opens With Cautious Outlook', a: '2h' },
        { t: 'Scientists Report Progress on Fusion Energy Milestone', a: '3h' },
        { t: 'Infrastructure Bill Advances With Bipartisan Support', a: '4h' },
        { t: 'Central Bank Holds Rates, Signals Data-Dependent Path', a: '5h' },
      ];
      feedEl.innerHTML = headlines.map(function(h) {
        return '<article><h2 style="font-size:1.05rem;margin:0 0 6px;">' + escapeHtml(h.t) + '</h2>' +
          '<span style="font-size:.7rem;color:#aaa;font-family:monospace;">' + h.a + ' ago</span></article>';
      }).join('<hr style="border:none;border-top:1px solid #e8e8e8;margin:0;">');
    }
  }
}

// ── Login Screen

/**
 * renderLoginScreen renders the PIN/passphrase entry screen.
 * Looks like a generic "Locked" screen — no Ghost branding.
 *
 * @param {Function} onUnlock - called with the raw input when user submits
 */
export function renderLoginScreen(onUnlock) {
  const existing = document.getElementById('ghost-lock-screen');
  if (existing) existing.remove();

  const screen = document.createElement('div');
  screen.id = 'ghost-lock-screen';
  screen.style.cssText = `
    position: fixed;
    inset: 0;
    background: #080808;
    display: flex;
    flex-direction: column;
    align-items: center;
    justify-content: center;
    gap: 24px;
    z-index: 9999;
    font-family: 'IBM Plex Mono', monospace;
  `;

  screen.innerHTML = `
  <div style="
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 20px;
  width: 100%;
  max-width: 280px;
  padding: 0 24px;
  ">
  <div style="
  font-size: 1.6rem;
  color: rgba(125,255,168,0.3);
  letter-spacing: 0.1em;
  user-select: none;
  " aria-hidden="true">◈</div>
  <div id="lock-dots" style="
  display: flex;
  gap: 14px;
  height: 12px;
  align-items: center;
  ">
  <!-- filled by JS -->
  </div>
  <input
  id="lock-input"
  type="password"
  inputmode="numeric"
  maxlength="12"
  autocomplete="off"
  autocorrect="off"
  spellcheck="false"
  placeholder="·  ·  ·  ·"
  aria-label="Enter PIN or passphrase"
  style="
  width: 100%;
  text-align: center;
  background: transparent;
  border: none;
  border-bottom: 1px solid #2a2a2a;
  color: #d8d8d8;
  font-family: 'IBM Plex Mono', monospace;
  font-size: 1.4rem;
  letter-spacing: 0.3em;
  padding: 8px 0;
  outline: none;
  caret-color: rgba(125,255,168,0.7);
  "
  />
  <div id="lock-error" style="
  font-size: 0.7rem;
  color: #ff4e4e;
  letter-spacing: 0.05em;
  min-height: 18px;
  text-align: center;
  " aria-live="polite"></div>
  </div>
  `;
  document.body.appendChild(screen);

  const input  = screen.querySelector('#lock-input');
  const errorEl = screen.querySelector('#lock-error');
  const dotsEl  = screen.querySelector('#lock-dots');

  function updateDots(len) {
    dotsEl.innerHTML = Array.from({ length: Math.max(len, 4) }, (_, i) => `
      <div style="
        width: 8px; height: 8px; border-radius: 50%;
        background: ${i < len ? 'rgba(125,255,168,0.8)' : '#2a2a2a'};
        transition: background 0.1s ease;
      "></div>
    `).join('');
  }

  updateDots(0);

  input.addEventListener('input', () => updateDots(input.value.length));

  input.addEventListener('keydown', async e => {
    if (e.key === 'Enter' && input.value.trim()) {
      const value = input.value.trim();
      input.value = '';
      updateDots(0);
      errorEl.textContent = '';

      input.style.opacity = '0.4';
      setTimeout(() => { input.style.opacity = '1'; }, 200);

      await onUnlock(value, errorEl);
    }
  });

  input.focus();
}

// ── Frontend HTML Washer

// This frontend washer is a secondary display-layer cleaner.

/**
 * Frontend content washer — strips remaining HTML from server-delivered body.
 * The Go backend washer handles the heavy lifting; this is a safety net.
 */
export function frontendWashBody(rawBody) {
  if (!rawBody) return '';
  // Strip any residual HTML tags that escaped the Go washer.
  return rawBody
    .replace(/<script[^>]*>[\s\S]*?<\/script>/gi, '')
    .replace(/<[^>]+>/g, '')
    .replace(/&amp;/g, '&')
    .replace(/&lt;/g, '<')
    .replace(/&gt;/g, '>')
    .replace(/&quot;/g, '"')
    .replace(/&#39;/g, "'")
    .replace(/&nbsp;/g, ' ')
    .trim();
}

// ── Utilities

async function sha256Hex(input) {
  const encoded = new TextEncoder().encode(input);
  const hashBuffer = await crypto.subtle.digest('SHA-256', encoded);
  const hashArray  = Array.from(new Uint8Array(hashBuffer));
  return hashArray.map(b => b.toString(16).padStart(2, '0')).join('');
}

function sleep(ms) {
  return new Promise(resolve => setTimeout(resolve, ms));
}

function escapeHtml(str) {
  return String(str)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
}

function stripHTML(str) {
  return str.replace(/<[^>]+>/g, '');
}

// ── Auth flow export

export async function initAuth(hashes, serverURL, onGhostMode) {
  const auth = new GhostAuth({
    realPasswordHash:  hashes.real,
    decoyPasswordHash: hashes.decoy,
    killCodeHash:      hashes.kill,
    ghostServerURL:    serverURL,
  });

  renderLoginScreen(async (input, errorEl) => {
    const result = await auth.unlock(input);

    switch (result.mode) {
      case 'ghost':
        // Remove lock screen, boot Ghost UI.
        document.getElementById('ghost-lock-screen')?.remove();
        document.getElementById('app').style.display = '';
        onGhostMode();
        break;

      case 'decoy':
        // Activate decoy news reader.
        document.getElementById('ghost-lock-screen')?.remove();
        const decoy = new DecoyMode();
        await decoy.activate();
        break;

      case 'destroyed':
        // Show a generic "reset" screen that reveals nothing.
        document.getElementById('ghost-lock-screen')?.remove();
        document.body.innerHTML = `
          <div style="
            display:flex;align-items:center;justify-content:center;
            height:100vh;background:#080808;
            font-family:monospace;font-size:0.8rem;color:#333;
          ">device reset</div>
        `;
        break;

      case 'error':
        if (errorEl) errorEl.textContent = result.error || 'incorrect';
        // Shake animation.
        const lockEl = document.getElementById('ghost-lock-screen');
        if (lockEl) {
          lockEl.style.animation = 'none';
          lockEl.offsetHeight; // force reflow
          lockEl.style.animation = 'shake 0.3s ease';
        }
        break;
    }
  });
}
