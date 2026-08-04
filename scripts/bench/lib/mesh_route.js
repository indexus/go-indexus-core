/**
 * Discover mesh peers (ping + /neighbors + /registered) and pick the
 * XOR-nearest node for an item key — same model as the SDK write path.
 *
 * Newly discovered peers are quarantined until they advertise client_ready
 * (ownership arrived) and a short grace elapses, so clients keep writing to
 * healthy owners while a spawn joins — avoiding the write-rate V-dip.
 */
import http from "http";
import https from "https";
import { createRequire } from "module";

const require = createRequire(import.meta.url);
const { decodeUrl64, encodeUrl64, transform } = require("js-indexus-sdk");

/** How long a newly-seen peer stays off the client hop set after first ready. */
const JOIN_GRACE_MS = parseInt(process.env.MESH_JOIN_GRACE_MS || "2500", 10);

function httpJson(url, { method = "GET", body, headers = {}, timeout = 8000, token } = {}) {
  const u = new URL(url);
  const lib = u.protocol === "https:" ? https : http;
  const payload = body != null ? JSON.stringify(body) : null;
  const hdrs = {
    ...(payload
      ? {
          "Content-Type": "application/json",
          "Content-Length": Buffer.byteLength(payload),
        }
      : {}),
    ...headers,
  };
  if (token) hdrs.Authorization = `Bearer ${token}`;
  return new Promise((resolve, reject) => {
    const req = lib.request(
      {
        protocol: u.protocol,
        hostname: u.hostname,
        port: u.port || (u.protocol === "https:" ? 443 : 80),
        path: `${u.pathname}${u.search}`,
        method,
        headers: hdrs,
        timeout,
      },
      (res) => {
        const chunks = [];
        res.on("data", (c) => chunks.push(c));
        res.on("end", () => {
          const raw = Buffer.concat(chunks).toString("utf8");
          if (res.statusCode < 200 || res.statusCode >= 300) {
            reject(new Error(`HTTP ${res.statusCode} ${url}`));
            return;
          }
          if (!raw) {
            resolve(null);
            return;
          }
          try {
            resolve(JSON.parse(raw));
          } catch (e) {
            reject(e);
          }
        });
      }
    );
    req.on("error", reject);
    req.on("timeout", () => req.destroy(new Error("timeout")));
    if (payload) req.write(payload);
    req.end();
  });
}

function baseUrlFromContact(contact, fallbackHost) {
  const port = contact.port || 21000;
  const ip =
    contact.ip ||
    (contact.ips && Object.keys(contact.ips)[0]) ||
    fallbackHost;
  if (!ip) return null;
  return `http://${ip}:${port}`;
}

function xorDistance(a, b) {
  const len = Math.min(a.length, b.length);
  const out = Buffer.alloc(len);
  for (let i = 0; i < len; i++) out[i] = a[i] ^ b[i];
  return out;
}

/**
 * Walk the mesh the way a client can: ping a seed, ask it for neighbours, ping
 * those, repeat. Only the p2p port is used — monitoring is an operator port and
 * a client has no business needing it to find where to write.
 *
 * @param {string[]} seedBases - e.g. ["http://1.2.3.4:21000"]
 * @param {{ token?: string, rounds?: number, quarantine?: Map, now?: number }} [opts]
 * @returns {Promise<Array<{ base: string, hash: string, id: Buffer, clientReady: boolean, firstSeen: number }>>}
 */
export async function discoverMesh(seedBases, opts = {}) {
  const token = opts.token || process.env.INDEXUS_BEARER || "";
  const rounds = opts.rounds ?? 3;
  const quarantine = opts.quarantine || new Map(); // hash -> { firstSeen, readySince }
  const now = opts.now ?? Date.now();
  const seedSet = new Set(
    seedBases.map((b) => b.replace(/\/$/, "")).filter(Boolean)
  );
  const byHash = new Map();
  const originBytes = Buffer.alloc(16);
  for (let i = 0; i < 16; i++) originBytes[i] = Math.floor(Math.random() * 256);
  const origin = encodeUrl64(originBytes);

  function isRoutable(peer) {
    if (seedSet.has(peer.base)) return true;
    const q = quarantine.get(peer.hash);
    if (!q) return false;
    // Old nodes without client_ready in /ping: treat as ready after grace
    // from first successful ping.
    const readySince = q.readySince || q.firstSeen;
    if (peer.clientReady === false) return false;
    return now - readySince >= JOIN_GRACE_MS;
  }

  async function ingest(base) {
    if (!base) return null;
    const cleaned = base.replace(/\/$/, "");
    try {
      const data = await httpJson(`${cleaned}/ping`, {
        method: "POST",
        body: {},
        timeout: 4000,
        token,
      });
      const c = data?.contact;
      if (!c?.name) return null;
      const peerBase = baseUrlFromContact(c, new URL(cleaned).hostname) || cleaned;
      // Missing field (old binary) ⇒ assume ready; explicit false ⇒ joining.
      const clientReady = data?.client_ready !== false;
      const peer = {
        base: peerBase.replace(/\/$/, ""),
        hash: c.name,
        id: Buffer.from(decodeUrl64(c.name)),
        clientReady,
        firstSeen: now,
      };
      let q = quarantine.get(c.name);
      if (!q) {
        q = { firstSeen: now, readySince: clientReady ? now : 0 };
        quarantine.set(c.name, q);
      } else if (clientReady && !q.readySince) {
        q.readySince = now;
      } else if (!clientReady) {
        q.readySince = 0;
      }
      peer.firstSeen = q.firstSeen;
      const known = byHash.has(c.name);
      byHash.set(c.name, peer);
      return known ? null : peer;
    } catch {
      return null; // unreachable, still booting, or not ours
    }
  }

  const seeds = seedBases.map((b) => b.replace(/\/$/, "")).filter(Boolean);
  let frontier = (await Promise.all(seeds.map((b) => ingest(b)))).filter(Boolean);

  for (let round = 0; round < rounds && frontier.length; round++) {
    const found = await Promise.all(
      frontier.map(async (peer) => {
        try {
          const data = await httpJson(
            `${peer.base}/neighbors?origin=${encodeURIComponent(origin)}`,
            { timeout: 4000, token }
          );
          const fresh = [];
          for (const c of data?.neighbors || []) {
            if (!c?.name || byHash.has(c.name)) continue;
            const base = baseUrlFromContact(c, new URL(peer.base).hostname);
            const added = await ingest(base);
            if (added) fresh.push(added);
          }
          return fresh;
        } catch {
          return [];
        }
      })
    );
    frontier = found.flat();
  }

  // Only return peers that are safe for client XOR routing.
  return [...byHash.values()].filter(isRoutable);
}

/**
 * Peer whose id is XOR-closest to transform(collection, location).
 * Optional `avoid` set of bases to skip (e.g. after 503).
 */
export function nearestForKey(peers, collection, location, avoid = null) {
  const key = Buffer.from(transform(collection, location));
  let best = null;
  let bestDist = null;
  for (const p of peers) {
    if (avoid && avoid.has(p.base)) continue;
    const d = xorDistance(p.id, key);
    if (!best || Buffer.compare(d, bestDist) < 0) {
      best = p;
      bestDist = d;
    }
  }
  return best;
}

/**
 * Refresh helper: rediscover often so newly spawned peers get traffic
 * gradually — only after client_ready + join grace.
 */
export function createRouter(
  seedBases,
  { refreshEvery = 64, refreshMs = 5000, token = "" } = {}
) {
  let peers = [];
  let n = 0;
  let ready = null;
  let lastRefresh = 0;
  const avoidUntil = new Map(); // base -> epoch ms
  const quarantine = new Map(); // hash -> { firstSeen, readySince }
  const bearer = token || process.env.INDEXUS_BEARER || "";

  async function refresh(force = false) {
    const now = Date.now();
    if (!force && ready && now - lastRefresh < refreshMs && n % refreshEvery !== 0) {
      return ready;
    }
    ready = discoverMesh(seedBases, { token: bearer, quarantine, now }).then((p) => {
      if (p.length) peers = p;
      else if (!peers.length) {
        peers = seedBases.map((base) => ({
          base: base.replace(/\/$/, ""),
          hash: base,
          id: Buffer.alloc(16),
          clientReady: true,
          firstSeen: Date.now(),
        }));
      }
      lastRefresh = Date.now();
      return peers;
    });
    return ready;
  }

  async function ensure() {
    if (!ready) await refresh(true);
    else await ready;
    if (n > 0 && (n % refreshEvery === 0 || Date.now() - lastRefresh > refreshMs)) {
      await refresh(true);
    }
  }

  return {
    async pick(collection, location) {
      await ensure();
      n++;
      const now = Date.now();
      const avoid = new Set();
      for (const [base, until] of avoidUntil) {
        if (until > now) avoid.add(base);
        else avoidUntil.delete(base);
      }
      let peer = nearestForKey(peers, collection, location, avoid);
      if (!peer) peer = nearestForKey(peers, collection, location, null);
      return peer?.base || seedBases[0];
    },
    /** Temporarily deprioritize a base after backpressure / errors. */
    markHot(base, ms = 2000) {
      if (!base) return;
      avoidUntil.set(base.replace(/\/$/, ""), Date.now() + ms);
    },
    async forceRefresh() {
      await refresh(true);
      return peers;
    },
    async peers() {
      await ensure();
      return peers;
    },
  };
}
