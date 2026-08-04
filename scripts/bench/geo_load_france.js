#!/usr/bin/env node
/**
 * Generate & insert N random geo points across metropolitan France.
 *
 * Usage:
 *   ISSUER=http://HOST:22000 NODES=ip1:21000,ip2:21000 \
 *     node geo_load_france.js --count 500000 --concurrency 64
 */
import http from "http";
import https from "https";
import axios from "axios";
import { createRequire } from "module";
const require = createRequire(import.meta.url);
const { Collection, Space } = require("js-indexus-sdk");

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
const COUNT = parseInt(arg("count", "500000"), 10);
const CONCURRENCY = parseInt(arg("concurrency", "64"), 10);
const PRECISION = parseInt(arg("precision", "16"), 10);
const COLLECTION_NAME = arg("collection", "FrGeoBenchAws00001");
// Optional: spread inserts over --duration seconds (paced). 0 = as fast as possible.
const DURATION_S = parseFloat(arg("duration", "0"));

// Metropolitan France (approx bbox)
const LAT_MIN = 41.3;
const LAT_MAX = 51.1;
const LNG_MIN = -5.2;
const LNG_MAX = 9.6;

const collection = new Collection(COLLECTION_NAME, [
  { name: "gps", type: "spherical", args: [-90, 90, -180, 180] },
]);
const space = new Space(collection.dimensions(), collection.mask(), collection.offset());

const agent = new http.Agent({ keepAlive: true, maxSockets: CONCURRENCY });
const agentHttps = new https.Agent({ keepAlive: true, maxSockets: CONCURRENCY });

function randomFrancePoint() {
  const lat = LAT_MIN + Math.random() * (LAT_MAX - LAT_MIN);
  const lng = LNG_MIN + Math.random() * (LNG_MAX - LNG_MIN);
  return [lat, lng];
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
        res.resume();
        if (res.statusCode === 201) resolve();
        else reject(new Error(`HTTP ${res.statusCode}`));
      }
    );
    req.on("error", reject);
    req.on("timeout", () => req.destroy(new Error("timeout")));
    req.write(payload);
    req.end();
  });
}

async function main() {
  console.log(
    JSON.stringify({
      ISSUER,
      NODES,
      COUNT,
      CONCURRENCY,
      COLLECTION_NAME,
      PRECISION,
      DURATION_S,
    })
  );

  const { data } = await axios.post(`${ISSUER}/v1/issue/token`, {
    client_id: `geo-load-${Date.now()}`,
    scopes: ["read", "write"],
  });
  const token = data.token;

  let next = 0;
  let ok = 0;
  let fail = 0;
  const t0 = Date.now();
  let lastLog = t0;
  const targetIntervalMs =
    DURATION_S > 0 ? (DURATION_S * 1000) / Math.max(COUNT, 1) : 0;

  async function worker(workerId) {
    while (true) {
      const i = next++;
      if (i >= COUNT) return;
      if (targetIntervalMs > 0) {
        const due = t0 + i * targetIntervalMs;
        const wait = due - Date.now();
        if (wait > 0) await new Promise((r) => setTimeout(r, wait));
      }
      const [lat, lng] = randomFrancePoint();
      const location = encodeLocation(lat, lng);
      const node = NODES[i % NODES.length];
      const body = {
        item: {
          collection: COLLECTION_NAME,
          location,
          id: `fr-${i}`,
          metrics: [1, 0, 0, lat + 90, lng + 180],
        },
        root: "@",
        current: location,
      };
      try {
        await postItem(node, token, body);
        ok++;
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

  await Promise.all(Array.from({ length: CONCURRENCY }, (_, w) => worker(w)));
  const elapsed = Date.now() - t0;
  console.log(
    JSON.stringify(
      {
        collection: COLLECTION_NAME,
        ok,
        fail,
        elapsed_ms: elapsed,
        rps: ok / (elapsed / 1000),
      },
      null,
      2
    )
  );
  if (fail > COUNT * 0.01) process.exitCode = 2;
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
