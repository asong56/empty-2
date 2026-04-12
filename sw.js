/**
 * TardiTalk — Service Worker (sw.js)
 *
 * Strategy:
 *   - Static shell (index.html, style.css, app.js): Cache-First
 *   - API GET requests (/api/threads, /api/contacts): Stale-While-Revalidate
 *   - API POST requests (/api/messages/send): Network-First + offline queue
 *   - WebSocket: NOT intercepted (SW doesn't proxy WS)
 *   - Media (/media/*): Cache-First with 24h TTL
 *
 * Optimization 6 fix: cache key is seeded from /api/version so that
 * upgrading the server automatically busts stale caches.
 * Falls back to a date-based key if the version endpoint is unavailable.
 */

'use strict';

async function getVersionedCacheKey(prefix) {
  try {
    const res = await fetch('/api/version', { cache: 'no-store' });
    if (res.ok) {
      const { version } = await res.json();
      if (version && version !== 'dev') return `tarditalk-${version}-${prefix}`;
    }
  } catch(_) {}
    return `tarditalk-dev-${Math.floor(Date.now() / 86400000)}-${prefix}`;
}

let SHELL_CACHE  = 'tarditalk-shell';
let API_CACHE    = 'tarditalk-api';
let MEDIA_CACHE  = 'tarditalk-media';
const OUTBOX_STORE = 'tardi_outbox';

const SHELL_ASSETS = [
  '/',
  '/index.html',
  '/style.css',
  '/app.js',
];

// ── Install

self.addEventListener('install', event => {
  event.waitUntil(
    (async () => {
      SHELL_CACHE  = await getVersionedCacheKey('shell');
      API_CACHE    = await getVersionedCacheKey('api');
      MEDIA_CACHE  = await getVersionedCacheKey('media');
      const cache  = await caches.open(SHELL_CACHE);
      await cache.addAll(SHELL_ASSETS);
      await self.skipWaiting();
    })()
  );
});

// ── Activate

self.addEventListener('activate', event => {
  event.waitUntil(
    (async () => {

      const [currentShell, currentApi, currentMedia] = await Promise.all([
        getVersionedCacheKey('shell'),
        getVersionedCacheKey('api'),
        getVersionedCacheKey('media'),
      ]);

      SHELL_CACHE = currentShell;
      API_CACHE   = currentApi;
      MEDIA_CACHE = currentMedia;

      await Promise.all([

        caches.keys().then(keys =>
          Promise.all(
            keys
              .filter(k =>
                k.startsWith('tarditalk-') &&
                k !== SHELL_CACHE &&
                k !== API_CACHE &&
                k !== MEDIA_CACHE
              )
              .map(k => caches.delete(k))
          )
        ),

        self.clients.claim(),
      ]);
    })()
  );
});

// ── Fetch

self.addEventListener('fetch', event => {
  const { request } = event;
  const url = new URL(request.url);

  if (request.method !== 'GET' && request.method !== 'POST') return;
  if (url.origin !== self.location.origin && !url.pathname.startsWith('/api/')) return;

  if (SHELL_ASSETS.some(asset => url.pathname === asset || url.pathname === '/')) {
    event.respondWith(cacheFirst(request, SHELL_CACHE));
    return;
  }

  if (url.pathname.startsWith('/media/')) {
    event.respondWith(mediaCache(request));
    return;
  }

  if (request.method === 'GET' && url.pathname.startsWith('/api/')) {
    event.respondWith(staleWhileRevalidate(request, API_CACHE));
    return;
  }

  if (request.method === 'POST' && url.pathname.startsWith('/api/messages/send')) {
    event.respondWith(networkFirstWithQueue(request));
    return;
  }
});

// ── Background Sync

self.addEventListener('sync', event => {
  if (event.tag === 'ghost-outbox-sync') {
    event.waitUntil(replayOutbox());
  }
});

// ── Push

self.addEventListener('push', event => {
  if (!event.data) return;

  let data = {};
  try { data = event.data.json(); } catch {}

  event.waitUntil(
    shouldNotify(data).then(allowed => {
      if (!allowed) return;
      return self.registration.showNotification(data.sender || 'Ghost', {
        body:   data.snippet || '',
        tag:    data.thread_id,
        silent: false,
        badge:  '/icon.png',
        data:   { thread_id: data.thread_id },
      });
    })
  );
});

self.addEventListener('notificationclick', event => {
  event.notification.close();
  const threadId = event.notification.data?.thread_id;
  event.waitUntil(
    self.clients.matchAll({ type: 'window' }).then(clients => {
      if (clients.length > 0) {
        const client = clients[0];
        client.focus();
        if (threadId) {
          client.postMessage({ type: 'open_thread', thread_id: threadId });
        }
        return;
      }
      return self.clients.openWindow(`/?thread=${threadId}`);
    })
  );
});

// ── Strategies

/** Cache-First: serve from cache, fall back to network. */
async function cacheFirst(request, cacheName) {
  const cache = await caches.open(cacheName);
  const cached = await cache.match(request);
  if (cached) return cached;
  const r = await fetch(request);
  if (r.ok) cache.put(request, r.clone());
  return r;
}

/** Stale-While-Revalidate: return cached immediately, update in background. */
async function staleWhileRevalidate(request, cacheName) {
  const cache = await caches.open(cacheName);
  const cached = await cache.match(request);

  const fetchPromise = fetch(request).then(response => {
    if (response.ok) {
      cache.put(request, response.clone());
    }
    return response;
  }).catch(() => {

    // as a Response, triggering a TypeError / white-screen.  Return a proper
    // 503 fallback instead so the client can display a useful offline message.
    return cached || new Response(
      JSON.stringify({ error: 'offline', message: 'Network unavailable' }),
      { status: 503, headers: { 'Content-Type': 'application/json' } }
    );
  });

  // Serve the cached version immediately if available; otherwise wait for
  // the network (first visit, no cache yet).
  return cached || fetchPromise;
}

/** Media Cache-First with TTL (24h) based on Date header. */
async function mediaCache(request) {
  const cache = await caches.open(MEDIA_CACHE);
  const cached = await cache.match(request);

  if (cached) {
    const dateHeader = cached.headers.get('Date');
    if (dateHeader) {
      // Fix 6.5: only apply the TTL check when the Date header is actually
      // present.  Previously a missing Date header caused `new Date(null)`
      // which evaluates to epoch (0 ms), making cacheAge ≈ 1.7 × 10¹² ms —
      // always older than 24 h — so every media request bypassed the cache.
      const cacheAge = Date.now() - new Date(dateHeader).getTime();
      if (cacheAge < 86400000) { // 24 h
        return cached;
      }
    } else {
      // No Date header: treat as fresh (serve from cache, revalidate in bg).
      // A missing header is a server misconfiguration, not a sign of staleness.
      fetch(request).then(r => { if (r.ok) cache.put(request, r.clone()); }).catch(() => {});
      return cached;
    }
  }

  try {
    const response = await fetch(request);
    if (response.ok) cache.put(request, response.clone());
    return response;
  } catch {
    return cached || new Response('Media unavailable offline', { status: 503 });
  }
}

/** Network-First with offline queue for outbound messages. */
async function networkFirstWithQueue(request) {
  // Clone body before consuming it (can only read once).
  const bodyClone = await request.clone().text();

  try {
    const response = await fetch(request);
    return response;
  } catch {
    // Offline: queue the message in IndexedDB for replay.
    await enqueueOutbox({
      url:     request.url,
      method:  request.method,
      headers: Object.fromEntries(request.headers.entries()),
      body:    bodyClone,
      queued_at: Date.now(),
    });

    // Register for background sync.
    if ('sync' in self.registration) {
      await self.registration.sync.register('ghost-outbox-sync');
    }

    // Return an optimistic response so the UI still shows the message.
    return new Response(JSON.stringify({
      status: 'queued',
      offline: true,
    }), {
      status: 202,
      headers: { 'Content-Type': 'application/json' },
    });
  }
}

// ── Outbox Queue (IndexedDB) ──────────────────────────────────────────────────

async function openOutboxDB() {
  return new Promise((resolve, reject) => {
    const req = indexedDB.open('ghost_sw', 1);
    req.onupgradeneeded = e => {
      e.target.result.createObjectStore(OUTBOX_STORE, { keyPath: 'id', autoIncrement: true });
    };
    req.onsuccess = e => resolve(e.target.result);
    req.onerror   = () => reject(req.error);
  });
}

async function enqueueOutbox(item) {
  const db = await openOutboxDB();
  return new Promise((resolve, reject) => {
    const tx = db.transaction(OUTBOX_STORE, 'readwrite');
    tx.objectStore(OUTBOX_STORE).add(item);
    tx.oncomplete = resolve;
    tx.onerror    = () => reject(tx.error);
  });
}

async function replayOutbox() {
  const db = await openOutboxDB();

  const items = await new Promise((resolve, reject) => {
    const tx = db.transaction(OUTBOX_STORE, 'readonly');
    const req = tx.objectStore(OUTBOX_STORE).getAll();
    req.onsuccess = () => resolve(req.result || []);
    req.onerror   = () => reject(req.error);
  });

  for (const item of items) {
    try {
      const response = await fetch(item.url, {
        method:  item.method,
        headers: item.headers,
        body:    item.body,
      });

      if (response.ok) {
        // Remove successfully replayed item.
        const tx = db.transaction(OUTBOX_STORE, 'readwrite');
        tx.objectStore(OUTBOX_STORE).delete(item.id);

        // Notify the client that the queued message was sent.
        const clients = await self.clients.matchAll();
        for (const client of clients) {
          client.postMessage({
            type: 'outbox_sent',
            item,
            response: await response.json().catch(() => ({})),
          });
        }
      }
    } catch {
      // Still offline — will retry on next sync event.
    }
  }
}

// ── Notification Filter ───────────────────────────────────────────────────────

async function shouldNotify(data) {
  // Read settings from IDB (set by the main app).
  try {
    const db = await new Promise((resolve, reject) => {
      const req = indexedDB.open('ghost_cache', 1);
      req.onsuccess = e => resolve(e.target.result);
      req.onerror   = () => reject(req.error);
    });

    const settings = await new Promise((resolve, reject) => {
      const tx  = db.transaction('settings', 'readonly');
      const req = tx.objectStore('settings').get('notification_rules');
      req.onsuccess = () => resolve(req.result?.value || {});
      req.onerror   = () => reject(req.error);
    });

    // Block stickers.
    if (settings.suppressStickers && data.content_type === 'redirected') return false;

    // Pinned contacts only.
    if (settings.pinnedOnly && !data.is_pinned) return false;

    return true;
  } catch {
    // If settings can't be read, default to allowing notifications.
    return true;
  }
}
