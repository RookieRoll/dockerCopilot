const CACHE_NAME = 'docker-copilot-v2';
const BASE_PATH = new URL('.', self.registration.scope).pathname;
const urlsToCache = [
  BASE_PATH,
  `${BASE_PATH}index.html`,
  `${BASE_PATH}logo.png`,
  `${BASE_PATH}manifest.json`
];

// 安装事件
self.addEventListener('install', event => {
  event.waitUntil(
    caches.open(CACHE_NAME)
      .then(cache => {
        return cache.addAll(urlsToCache);
      })
      .catch(err => {
        console.log('Cache open failed:', err);
      })
  );
  self.skipWaiting();
});

// 激活事件
self.addEventListener('activate', event => {
  event.waitUntil(
    caches.keys().then(cacheNames => {
      return Promise.all(
        cacheNames.map(cacheName => {
          if (cacheName !== CACHE_NAME) {
            return caches.delete(cacheName);
          }
        })
      );
    })
  );
  self.clients.claim();
});

// 获取事件
self.addEventListener('fetch', event => {
  if (event.request.method !== 'GET') {
    return;
  }

  const fetchAndCache = () => fetch(event.request).then(response => {
    if (!response || response.status !== 200 || response.type !== 'basic') {
      return response;
    }

    const responseToCache = response.clone();
    caches.open(CACHE_NAME).then(cache => {
      cache.put(event.request, responseToCache);
    });

    return response;
  });

  // Always refresh HTML so a new frontend bundle is not hidden by an old cache.
  if (event.request.mode === 'navigate') {
    event.respondWith(
      fetchAndCache().catch(() => caches.match(`${BASE_PATH}index.html`))
    );
    return;
  }

  event.respondWith(
    caches.match(event.request)
      .then(response => response || fetchAndCache())
      .catch(() => caches.match(`${BASE_PATH}index.html`))
  );
});
