/* global self, caches, clients */
const CACHE = "gea-shell-v12";
const SHELL = ["/", "/index.html", "/styles.css", "/favicon.png", "/icon-192.png", "/manifest.webmanifest"];

self.addEventListener("install", (event) => {
  event.waitUntil(
    caches.open(CACHE).then((cache) => cache.addAll(SHELL)).then(() => self.skipWaiting())
  );
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    caches.keys().then((keys) =>
      Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k)))
    ).then(() => self.clients.claim())
  );
});

self.addEventListener("fetch", (event) => {
  if (event.request.method !== "GET") return;
  const url = new URL(event.request.url);
  if (url.origin !== self.location.origin) return;
  if (!SHELL.some((p) => url.pathname === p || url.pathname.endsWith(p))) return;

  event.respondWith(
    caches.match(event.request).then((cached) =>
      cached ||
      fetch(event.request).then((res) => {
        if (res.ok) {
          const copy = res.clone();
          caches.open(CACHE).then((cache) => cache.put(event.request, copy));
        }
        return res;
      })
    )
  );
});

self.addEventListener("push", (event) => {
  let payload = { title: "Global Entry Alerts", body: "New appointment availability.", url: "/" };
  if (event.data) {
    try {
      const j = event.data.json();
      if (j.title) payload.title = j.title;
      if (j.body) payload.body = j.body;
      if (j.url) payload.url = j.url;
    } catch {
      const t = event.data.text();
      if (t) payload.body = t;
    }
  }
  event.waitUntil(
    self.registration.showNotification(payload.title, {
      body: payload.body,
      icon: "/icon-192.png",
      badge: "/favicon.png",
      data: { url: payload.url },
    })
  );
});

self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const raw = (event.notification.data && event.notification.data.url) || "/";
  event.waitUntil(openNotificationTarget(raw));
});

/** Focus an open window and navigate, or open a new one — works in browser tab and installed PWA. */
async function openNotificationTarget(rawUrl) {
  const urlToOpen = new URL(rawUrl, self.registration.scope);
  const targetHref = urlToOpen.href;
  const targetPath = urlToOpen.pathname + urlToOpen.search + urlToOpen.hash;
  const allClients = await self.clients.matchAll({ type: "window", includeUncontrolled: true });

  for (const client of allClients) {
    try {
      if (new URL(client.url).origin !== urlToOpen.origin) continue;
      if ("focus" in client) await client.focus();
      if ("navigate" in client) {
        return client.navigate(targetHref);
      }
      client.postMessage({ type: "gea-notification-navigate", url: targetPath });
      return;
    } catch {
      /* try next client */
    }
  }

  if (self.clients.openWindow) {
    return self.clients.openWindow(targetHref);
  }
}
