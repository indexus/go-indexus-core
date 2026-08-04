#!/usr/bin/env node
/**
 * Hotspot write load: all points cluster near one lat/lng so encoded locations
 * share a long XOR prefix and concentrate ownership / path-fill on few nodes.
 *
 * Usage:
 *   ISSUER=... NODES=http://ip:21000 \
 *     node hotspot_load.js --count 5000 --collection HotSpot01 \
 *       --lat 48.85 --lng 2.35 --spread 0.05
 */
import http from "http";
import https from "https";
import axios from "axios";
import { createRequire } from "module";
import { createRouter } from "./lib/mesh_route.js";

const require = createRequire(import.meta.url);
const { Collection, Space } = require("js-indexus-sdk");

function arg(name, fallback) {
  const i = process.argv.indexOf(`--${name}`);
  if (i >= 0 && process.argv[i + 1]) return process.argv[i + 1];
  return fallback;
}

const ISSUER = (process.env.ISSUER || arg("issuer", "http://127.0.0.1:22000")).replace(/\/$/, "");
const NODES = (process.env.NODES || arg("nodes", process.env.NODE || "http://127.0.0.1:21000"))
  .split(",")
  .map((s) => s.trim().replace(/\/$/, ""))
  .filter(Boolean);
const COUNT = parseInt(arg("count", "5000"), 10);
const CONCURRENCY = parseInt(arg("concurrency", "32"), 10);
const PRECISION = parseInt(arg("precision", "16"), 10);
const COLLECTION_NAME = arg("collection", "HotSpot01");
const LAT = parseFloat(arg("lat", "48.8566"));
const LNG = parseFloat(arg("lng", "2.3522"));
const SPREAD = parseFloat(arg("spread", "0.05")); // degrees ≈ a few km

const collection = new Collection(COLLECTION_NAME, [
  { name: "gps", type: "spherical", args: [-90, 90, -180, 180] },
]);
const space = new Space(collection.dimensions(), collection.mask(), collection.offset());

const agent = new http.Agent({ keepAlive: true, maxSockets: CONCURRENCY });
const agentHttps = new https.Agent({ keepAlive: true, maxSockets: CONCURRENCY });

function encodeLocation(lat, lng) {
  const point = [space.dimension(0).newPoint([lat, lng])];
  return space.encode(point, PRECISION);
}

function postItem(nodeBase, token, body) {
  const u = new URL(`${nodeBase}/item`);
  const payload = JSON.stringify(body);
  const lib = u.protocol === "https:" ? https : http;
  const a = u.protocol === "https:" ? agentHttps : agent;
  return new Promise((resolve, reject) => {
    const req = lib.request(
      {
        protocol: u.protocol,
        hostname: u.hostname,
        port: u.port || (u.protocol === "https:" ? 443 : 80),
        path: u.pathname,
        method: "POST",
        agent: a,
        headers: {
          "Content-Type": "application/json",
          Authorization: `Bearer ${token}`,
          "Content-Length": Buffer.byteLength(payload),
        },
        timeout: 30000,
      },
      (res) => {
        const code = res.statusCode;
        res.resume();
        if (code === 201) resolve({ ok: true, code });
        else if (code === 503) resolve({ ok: false, code });
        else reject(new Error(`HTTP ${code}`));
      }
    );
    req.on("error", reject);
    req.on("timeout", () => req.destroy(new Error("timeout")));
    req.write(payload);
    req.end();
  });
}

function commonPrefix(a, b) {
  let i = 0;
  while (i < a.length && i < b.length && a[i] === b[i]) i++;
  return i;
}

async function main() {
  const planned = [];
  for (let i = 0; i < COUNT; i++) {
    planned.push([
      LAT + (Math.random() - 0.5) * 2 * SPREAD,
      LNG + (Math.random() - 0.5) * 2 * SPREAD,
    ]);
  }
  const locs = planned.map(([lat, lng]) => encodeLocation(lat, lng));
  let prefixLen = locs[0]?.length || 0;
  for (const loc of locs) prefixLen = Math.min(prefixLen, commonPrefix(locs[0], loc));
  const prefix = (locs[0] || "").slice(0, prefixLen);

  console.log(
    JSON.stringify({
      ISSUER,
      NODES,
      COUNT,
      CONCURRENCY,
      COLLECTION_NAME,
      LAT,
      LNG,
      SPREAD,
      PRECISION,
      sample_location: locs[0],
      shared_prefix: prefix,
      shared_prefix_len: prefixLen,
    })
  );

  const { data } = await axios.post(`${ISSUER}/v1/issue/token`, {
    client_id: `hotspot-${Date.now()}`,
    scopes: ["read", "write"],
  });
  const token = data.token;

  const router = createRouter(NODES, {
    refreshEvery: 32,
    refreshMs: 3000,
    token,
  });
  const mesh = await router.peers();
  console.log(
    JSON.stringify({
      mesh_peers: mesh.map((p) => ({ hash: p.hash, base: p.base })),
      routing: "xor_nearest_item_key",
    })
  );

  let next = 0;
  let ok = 0;
  let retry503 = 0;
  let fail = 0;
  const t0 = Date.now();
  const hopCounts = new Map();

  async function worker() {
    while (true) {
      const i = next++;
      if (i >= COUNT) return;
      const [lat, lng] = planned[i];
      const location = locs[i];
      const node = await router.pick(COLLECTION_NAME, location);
      hopCounts.set(node, (hopCounts.get(node) || 0) + 1);
      const body = {
        item: {
          collection: COLLECTION_NAME,
          location,
          id: `hot-${i}-${Math.random().toString(16).slice(2, 8)}`,
          metrics: [1, 0, 0, lat + 90, lng + 180],
        },
        root: "@",
        current: location,
      };
      try {
        const r = await postItem(node, token, body);
        if (r.ok) ok++;
        else retry503++;
      } catch (e) {
        fail++;
        if (fail < 5) console.error("hotspot fail", e.message);
      }
    }
  }

  await Promise.all(Array.from({ length: CONCURRENCY }, () => worker()));
  const elapsed = Date.now() - t0;
  const summary = {
    ok,
    retry503,
    fail,
    elapsed_ms: elapsed,
    rps: ok / (elapsed / 1000),
    prefix,
    prefix_len: prefixLen,
    hop_distribution: Object.fromEntries(
      [...hopCounts.entries()].sort((a, b) => b[1] - a[1])
    ),
  };
  console.log(JSON.stringify(summary, null, 2));
  if (fail > ok * 0.05) process.exitCode = 2;
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
