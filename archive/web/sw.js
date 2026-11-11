/* Telegram Archive — service worker.
 *
 * Scope of caching is deliberately tiny. This is a live archive panel, not an
 * offline app: the only thing worth caching is the shell (HTML + CSS + icons)
 * so the home-screen icon has something to paint before the network answers.
 *
 * Two paths are never touched:
 *   /api/    — a cached response would show yesterday's archive and could
 *              replay a stale auth state after the password changes.
 *   /media/  — attachment bytes are per-session (Cache-Control: private) and on
 *              a shared device a cached object would outlive the logout.
 *
 * Strategy is network-first with a cache fallback. Cache-first would be faster
 * but it serves the previous build after an upgrade, and a single-operator
 * panel upgrades far more often than it goes offline.
 */
const CACHE = 'tdl-archive-v2';
const SHELL = [
  '/',
  '/static/theme.css',
  '/static/app.css',
  '/manifest.webmanifest',
  '/static/icon-192.png',
];

self.addEventListener('install', (e) => {
  e.waitUntil(
    caches
      .open(CACHE)
      .then((c) => c.addAll(SHELL))
      // A shell that fails to pre-cache must not block activation: the fetch
      // handler below still warms the cache on the first real navigation.
      .catch(() => {})
      .then(() => self.skipWaiting())
  );
});

self.addEventListener('activate', (e) => {
  e.waitUntil(
    caches
      .keys()
      .then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
      .then(() => self.clients.claim())
  );
});

self.addEventListener('fetch', (e) => {
  const req = e.request;
  if (req.method !== 'GET') return;

  let url;
  try {
    url = new URL(req.url);
  } catch {
    return;
  }
  if (url.origin !== self.location.origin) return;
  if (
    url.pathname.startsWith('/api/') ||
    url.pathname.startsWith('/media/') ||
    url.pathname.startsWith('/avatar/')
  ) {
    return; // live data and attachment bytes stay off the cache
  }

  e.respondWith(
    fetch(req)
      .then((res) => {
        // Only cache a complete, same-origin, successful response. Opaque and
        // partial (206) responses would poison the shell with a truncated body.
        if (res && res.ok && res.type === 'basic' && res.status === 200) {
          const copy = res.clone();
          caches.open(CACHE).then((c) => c.put(req, copy)).catch(() => {});
        }
        return res;
      })
      .catch(() =>
        caches.match(req).then((hit) => hit || caches.match('/'))
      )
  );
});
