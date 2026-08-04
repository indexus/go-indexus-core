#!/usr/bin/env node
/**
 * Concurrent authenticated GET /set storm (HTTP, no SDK Local).
 *
 * Usage:
 *   ISSUER=... NODES=http://ip:21000,http://ip2:21000 \
 *     node read_storm_http.js --collection FrScaleLab001 --location @ --clients 16 --requests 200
 */
import axios from "axios";

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
const COLLECTION = arg("collection", "FrScaleLab001");
const LOCATION = arg("location", "@");
const CLIENTS = parseInt(arg("clients", "16"), 10);
const REQUESTS = parseInt(arg("requests", "200"), 10);

function percentile(sorted, p) {
  if (!sorted.length) return null;
  const idx = Math.min(sorted.length - 1, Math.ceil((p / 100) * sorted.length) - 1);
  return sorted[idx];
}

async function main() {
  const { data } = await axios.post(`${ISSUER}/v1/issue/token`, {
    client_id: `http-storm-${Date.now()}`,
    scopes: ["read"],
  });
  const token = data.token;

  console.log(JSON.stringify({ ISSUER, NODES, COLLECTION, LOCATION, CLIENTS, REQUESTS }));

  const latencies = [];
  let ok = 0;
  let fail = 0;
  let keys = 0;
  let next = 0;
  const t0 = Date.now();

  async function worker() {
    while (true) {
      const i = next++;
      if (i >= REQUESTS) return;
      const node = NODES[i % NODES.length];
      const t = Date.now();
      try {
        const { data: body } = await axios.get(`${node}/set`, {
          params: { collection: COLLECTION, location: LOCATION },
          headers: { Authorization: `Bearer ${token}` },
          timeout: 15000,
        });
        latencies.push(Date.now() - t);
        ok++;
        if (body.set) keys = Math.max(keys, Object.keys(body.set).length);
      } catch (e) {
        fail++;
        if (fail <= 5) console.error("read fail", e.response?.status || e.message);
      }
    }
  }

  await Promise.all(Array.from({ length: CLIENTS }, () => worker()));
  const elapsed = Date.now() - t0;
  latencies.sort((a, b) => a - b);

  console.log(
    JSON.stringify(
      {
        ok,
        fail,
        elapsed_ms: elapsed,
        rps: ok / (elapsed / 1000),
        max_keys: keys,
        round_latencies_ms: {
          count: latencies.length,
          p50: percentile(latencies, 50),
          p95: percentile(latencies, 95),
          p99: percentile(latencies, 99),
          max: latencies[latencies.length - 1] ?? null,
          avg: latencies.length ? latencies.reduce((a, b) => a + b, 0) / latencies.length : null,
        },
      },
      null,
      2
    )
  );
  if (fail > REQUESTS * 0.1 || ok === 0) process.exitCode = 2;
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
