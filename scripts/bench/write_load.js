#!/usr/bin/env node
/**
 * Concurrent write load against an auth-gated Indexus node.
 *
 * Usage:
 *   ISSUER=http://HOST:22000 NODE=http://HOST:21000 \
 *     node write_load.js --count 200 --concurrency 20 --location z --collection bench
 */
import axios from "axios";

function arg(name, fallback) {
  const i = process.argv.indexOf(`--${name}`);
  if (i >= 0 && process.argv[i + 1]) return process.argv[i + 1];
  return fallback;
}

const ISSUER = (process.env.ISSUER || arg("issuer", "http://127.0.0.1:22000")).replace(/\/$/, "");
const NODE = (process.env.NODE || arg("node", "http://127.0.0.1:21000")).replace(/\/$/, "");
const COUNT = parseInt(arg("count", "200"), 10);
const CONCURRENCY = parseInt(arg("concurrency", "20"), 10);
const LOCATION = arg("location", "z");
const COLLECTION = arg("collection", "bench");

async function issueToken() {
  const { data } = await axios.post(`${ISSUER}/v1/issue/token`, {
    client_id: `bench-${Date.now()}`,
    scopes: ["read", "write"],
  });
  return data.token;
}

async function postItem(token, i) {
  const body = {
    item: {
      collection: COLLECTION,
      location: LOCATION,
      id: `id-${i}-${Math.random().toString(16).slice(2, 8)}`,
      metrics: [1, 2, 3, i % 100, (i * 7) % 100],
    },
    root: "@",
    current: LOCATION,
  };
  const t0 = Date.now();
  await axios.post(`${NODE}/item`, body, {
    headers: {
      Authorization: `Bearer ${token}`,
      "Content-Type": "application/json",
    },
    timeout: 15000,
  });
  return Date.now() - t0;
}

async function main() {
  console.log(JSON.stringify({ ISSUER, NODE, COUNT, CONCURRENCY, LOCATION, COLLECTION }));
  const token = await issueToken();
  const latencies = [];
  let ok = 0;
  let fail = 0;
  const t0 = Date.now();

  let next = 0;
  async function worker() {
    while (true) {
      const i = next++;
      if (i >= COUNT) return;
      try {
        const ms = await postItem(token, i);
        latencies.push(ms);
        ok++;
      } catch (e) {
        fail++;
        if (fail < 5) console.error("write fail", e.response?.status || e.message);
      }
    }
  }

  await Promise.all(Array.from({ length: CONCURRENCY }, () => worker()));
  const elapsed = Date.now() - t0;
  latencies.sort((a, b) => a - b);
  const pct = (p) => latencies[Math.min(latencies.length - 1, Math.floor((p / 100) * latencies.length))] || null;

  const summary = {
    ok,
    fail,
    elapsed_ms: elapsed,
    rps: ok / (elapsed / 1000),
    latency_ms: {
      p50: pct(50),
      p95: pct(95),
      p99: pct(99),
      max: latencies[latencies.length - 1] || null,
    },
  };
  console.log(JSON.stringify(summary, null, 2));
  if (fail > 0) process.exitCode = 2;
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
