#!/usr/bin/env node
/**
 * Insert geo points sampled from real-world population density.
 *
 * Source: indexus/simulation density.tif → items-100000-sigmoid.csv
 * (see simulation/random.go generateRandomPoints — color bands map to
 * people/km²; oceans/empty cells are skipped).
 *
 * Dense clusters (South Asia, Europe, Nigeria, …) appear far more often
 * than sparse regions — unlike uniform France bbox sampling.
 *
 * Usage:
 *   ISSUER=... NODES=http://ip:21000 \
 *     node geo_load_density.js --count 40000 --duration 600 --collection WorldDens01
 *
 * Env:
 *   DENSITY_CSV  path to lat,lng[,time] CSV (default: ./data/world_density_100k.csv)
 */
import fs from "fs";
import path from "path";
import http from "http";
import https from "https";
import axios from "axios";
import { fileURLToPath } from "url";
import { createRequire } from "module";
import { createRouter } from "./lib/mesh_route.js";

const require = createRequire(import.meta.url);
const { Collection, Space } = require("js-indexus-sdk");
const __dirname = path.dirname(fileURLToPath(import.meta.url));

function arg(name, fallback) {
  const i = process.argv.indexOf(`--${name}`);
  if (i >= 0 && process.argv[i + 1]) return process.argv[i + 1];
  return fallback;
}

const ISSUER = (process.env.ISSUER || "http://127.0.0.1:22000").replace(/\/$/, "");
const NODES = (process.env.NODES || process.env.NODE || "http://127.0.0.1:21000")
  .split(",")
  .map((s) => s.trim().replace(/\/$/, ""))
  .filter(Boolean);
const COUNT = parseInt(arg("count", "40000"), 10);
const CONCURRENCY = parseInt(arg("concurrency", "32"), 10);
const PRECISION = parseInt(arg("precision", "16"), 10);
const COLLECTION_NAME = arg("collection", "WorldDensAws00001");
const DURATION_S = parseFloat(arg("duration", "0"));
const CSV_PATH =
  process.env.DENSITY_CSV ||
  arg("csv", path.join(__dirname, "data", "world_density_100k.csv"));

const collection = new Collection(COLLECTION_NAME, [
  { name: "gps", type: "spherical", args: [-90, 90, -180, 180] },
]);
const space = new Space(collection.dimensions(), collection.mask(), collection.offset());

const agent = new http.Agent({ keepAlive: true, maxSockets: CONCURRENCY });
const agentHttps = new https.Agent({ keepAlive: true, maxSockets: CONCURRENCY });

function loadDensityPoints(csvPath) {
  const text = fs.readFileSync(csvPath, "utf8");
  const lines = text.split(/\r?\n/).filter(Boolean);
  const start = lines[0].toLowerCase().startsWith("lat") ? 1 : 0;
  const pts = [];
  for (let i = start; i < lines.length; i++) {
    const [latS, lngS] = lines[i].split(",");
    const lat = parseFloat(latS);
    const lng = parseFloat(lngS);
    if (!Number.isFinite(lat) || !Number.isFinite(lng)) continue;
    if (lat < -90 || lat > 90 || lng < -180 || lng > 180) continue;
    pts.push([lat, lng]);
  }
  if (!pts.length) throw new Error(`no points in ${csvPath}`);
  return pts;
}

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
        else if (code === 503) resolve({ ok: false, code: 503 });
        else reject(new Error(`HTTP ${code}`));
      }
    );
    req.on("error", reject);
    req.on("timeout", () => req.destroy(new Error("timeout")));
    req.write(payload);
    req.end();
  });
}

/** Coarse density histogram for the sample we actually insert. */
function densityReport(samples) {
  const cells = new Map();
  for (const [lat, lng] of samples) {
    const key = `${Math.floor(lat / 10) * 10},${Math.floor(lng / 20) * 20}`;
    cells.set(key, (cells.get(key) || 0) + 1);
  }
  return [...cells.entries()]
    .sort((a, b) => b[1] - a[1])
    .slice(0, 10)
    .map(([cell, n]) => ({ cell, n }));
}

async function main() {
  const pool = loadDensityPoints(CSV_PATH);
  // Fisher–Yates shuffle then cycle — preserves density ratios of the pool.
  for (let i = pool.length - 1; i > 0; i--) {
    const j = Math.floor(Math.random() * (i + 1));
    [pool[i], pool[j]] = [pool[j], pool[i]];
  }

  const planned = [];
  for (let i = 0; i < COUNT; i++) {
    const [lat, lng] = pool[i % pool.length];
    // tiny jitter so id collisions don't collapse when cycling the pool
    const jlat = lat + (Math.random() - 0.5) * 0.01;
    const jlng = lng + (Math.random() - 0.5) * 0.01;
    planned.push([jlat, jlng]);
  }

  console.log(
    JSON.stringify({
      ISSUER,
      NODES,
      COUNT,
      CONCURRENCY,
      COLLECTION_NAME,
      PRECISION,
      DURATION_S,
      CSV_PATH,
      pool_size: pool.length,
      density_top10: densityReport(planned),
    })
  );

  const { data } = await axios.post(`${ISSUER}/v1/issue/token`, {
    client_id: process.env.STORM_CLIENT_ID || `dens-load-${Date.now()}`,
    scopes: ["read", "write"],
  });
  const token = data.token;

  // Each insert goes to the peer whose id is closest to the item key.
  // Refresh often so PreferNear-spawned nodes enter the hop set quickly.
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
  let fail = 0;
  let retry503 = 0;
  const t0 = Date.now();
  let lastLog = t0;
  const targetIntervalMs =
    DURATION_S > 0 ? (DURATION_S * 1000) / Math.max(COUNT, 1) : 0;
  const hopCounts = new Map();

  // A refusal means the mesh is at capacity right now, not that the write is
  // impossible: back off long enough for a node to join and take a share,
  // the way a client that wants its data stored would.
  const ATTEMPTS = 7;
  async function writeWithRetry(location, body) {
    for (let attempt = 0; attempt < ATTEMPTS; attempt++) {
      const node = await router.pick(COLLECTION_NAME, location);
      try {
        const r = await postItem(node, token, body);
        if (r.ok) {
          hopCounts.set(node, (hopCounts.get(node) || 0) + 1);
          return true;
        }
        retry503++;
        router.markHot(node, 1500 + attempt * 1000);
        if (attempt % 2 === 1) await router.forceRefresh();
      } catch (e) {
        router.markHot(node, 2000);
        if (attempt === ATTEMPTS - 1) throw e;
      }
      // 0.2s, 0.4s, 0.8s … capped at 5s, jittered so clients do not all
      // come back at the same instant.
      const backoff = Math.min(200 * 2 ** attempt, 5000);
      await new Promise((r) => setTimeout(r, backoff * (0.5 + Math.random())));
    }
    return false;
  }

  async function worker() {
    while (true) {
      const i = next++;
      if (i >= COUNT) return;
      if (targetIntervalMs > 0) {
        const due = t0 + i * targetIntervalMs;
        const wait = due - Date.now();
        if (wait > 0) await new Promise((r) => setTimeout(r, wait));
      }
      const [lat, lng] = planned[i];
      const location = encodeLocation(lat, lng);
      const body = {
        item: {
          collection: COLLECTION_NAME,
          location,
          id: `wd-${i}`,
          metrics: [1, 0, 0, lat + 90, lng + 180],
        },
        root: "@",
        current: location,
      };
      try {
        if (await writeWithRetry(location, body)) ok++;
        else {
          fail++;
          if (fail <= 10) console.error(`fail#${fail}`, "exhausted 503 retries");
        }
      } catch (e) {
        fail++;
        if (fail <= 10) console.error(`fail#${fail}`, e.message);
      }
      const now = Date.now();
      if (now - lastLog > 5000) {
        lastLog = now;
        const done = ok + fail;
        const rps = ok / ((now - t0) / 1000);
        const eta = rps > 0 ? ((COUNT - done) / rps).toFixed(0) : "?";
        console.log(
          JSON.stringify({
            done,
            ok,
            fail,
            pct: ((100 * done) / COUNT).toFixed(2),
            rps: rps.toFixed(1),
            eta_s: eta,
          })
        );
      }
    }
  }

  await Promise.all(Array.from({ length: CONCURRENCY }, () => worker()));
  const elapsed = Date.now() - t0;
  console.log(
    JSON.stringify(
      {
        collection: COLLECTION_NAME,
        ok,
        fail,
        retry503,
        elapsed_ms: elapsed,
        rps: ok / (elapsed / 1000),
        density_top10: densityReport(planned),
        hop_distribution: Object.fromEntries(
          [...hopCounts.entries()].sort((a, b) => b[1] - a[1])
        ),
      },
      null,
      2
    )
  );
  if (fail > COUNT * 0.02) process.exitCode = 2;
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
