// Couchlog service worker: makes the dashboard installable. No offline cache on purpose:
// the data is live (now playing, sessions), so every request goes to the network.
self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", e => e.waitUntil(self.clients.claim()));
self.addEventListener("fetch", () => {});
