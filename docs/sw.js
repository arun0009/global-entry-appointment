/* global self, clients */
self.addEventListener("install", (event) => {
  event.waitUntil(self.skipWaiting());
});

self.addEventListener("activate", (event) => {
  event.waitUntil(self.clients.claim());
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
      icon: "/favicon.png",
      badge: "/favicon.png",
      data: { url: payload.url },
    })
  );
});

self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const raw = (event.notification.data && event.notification.data.url) || "/";
  const target = new URL(raw, self.registration.scope).href;
  event.waitUntil(
    (async () => {
      const all = await self.clients.matchAll({ type: "window", includeUncontrolled: true });
      for (const c of all) {
        if ("focus" in c) {
          await c.focus();
          return;
        }
      }
      if (self.clients.openWindow) {
        await self.clients.openWindow(target);
      }
    })()
  );
});
