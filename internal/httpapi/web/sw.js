const cacheName = "agentd-shell-v14";
const shellAssets = ["/", "/app.css", "/app.js", "/manifest.json", "/icon.svg", "/apple-touch-icon.png", "/icon-192.png", "/icon-512.png", "/badge-96.png"];

self.addEventListener("install", event => {
  event.waitUntil(caches.open(cacheName).then(cache => cache.addAll(shellAssets)));
  self.skipWaiting();
});

self.addEventListener("activate", event => {
  event.waitUntil(
    caches.keys()
      .then(keys => Promise.all(keys.filter(key => key !== cacheName).map(key => caches.delete(key))))
      .then(() => self.clients.claim()),
  );
});

self.addEventListener("fetch", event => {
  const url = new URL(event.request.url);
  if (
    event.request.method !== "GET" ||
    url.origin !== self.location.origin ||
    url.pathname.startsWith("/api/") ||
    url.pathname === "/mcp"
  ) return;
  event.respondWith(
    fetch(event.request)
      .then(response => {
        if (response.ok) {
          const copy = response.clone();
          void caches.open(cacheName).then(cache => cache.put(event.request, copy));
        }
        return response;
      })
      .catch(() => caches.match(event.request).then(response => response || caches.match("/"))),
  );
});

self.addEventListener("push", event => {
  let payload = {};
  try {
    payload = event.data?.json() || {};
  } catch {
    payload = { body: event.data?.text() || "An agent has an update." };
  }
  event.waitUntil(self.registration.showNotification(payload.title || "agentd", {
    body: payload.body || "An agent has an update.",
    icon: "/icon-192.png",
    badge: "/badge-96.png",
    tag: `agentd-${payload.sessionId || "gateway"}-${payload.category || "update"}`,
    data: { url: payload.url || "/" },
  }));
});

self.addEventListener("notificationclick", event => {
  event.notification.close();
  const target = new URL(event.notification.data?.url || "/", self.location.origin).href;
  event.waitUntil(
    self.clients.matchAll({ type: "window", includeUncontrolled: true }).then(clients => {
      const existing = clients.find(client => new URL(client.url).origin === self.location.origin);
      if (existing) return existing.navigate(target).then(() => existing.focus());
      return self.clients.openWindow(target);
    }),
  );
});

self.addEventListener("pushsubscriptionchange", event => {
  event.waitUntil(
    fetch("/api/v1/device", { headers: { Accept: "application/json" } })
      .then(response => {
        if (!response.ok) throw new Error(`HTTP ${response.status}`);
        return response.json();
      })
      .then(info => self.registration.pushManager.subscribe({
        userVisibleOnly: true,
        applicationServerKey: decodeBase64URL(info.vapidPublicKey),
      }))
      .then(subscription => fetch("/api/v1/device/push", {
        method: "PUT",
        headers: { "Content-Type": "application/json", Accept: "application/json" },
        body: JSON.stringify(subscription),
      }))
      .then(response => {
        if (!response.ok) throw new Error(`HTTP ${response.status}`);
      }),
  );
});

function decodeBase64URL(value) {
  const padding = "=".repeat((4 - value.length % 4) % 4);
  const raw = atob((value + padding).replaceAll("-", "+").replaceAll("_", "/"));
  return Uint8Array.from(raw, character => character.charCodeAt(0));
}
