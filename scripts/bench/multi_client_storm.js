#!/usr/bin/env node
/**
 * Multi-client density storm: N independent simulated clients push slices of a
 * large COUNT sampled from world population density (same CSV as simulations).
 *
 * Each client gets its own issuer token (client_id), discovers the mesh from
 * the seed it was given, and sends every insert to the node XOR-nearest the
 * item key — load arrives as if from many edges.
 *
 * Usage:
 *   ISSUER=... NODES=http://ip:21000 \
 *     node multi_client_storm.js --count 2000000 --clients 128 --concurrency 8
 *
 * Env:
 *   DENSITY_CSV  path to lat,lng CSV
 *   CLIENTS / COUNT / CONCURRENCY / RAMP_S overrides
 */
import { spawn } from "child_process";
import path from "path";
import fs from "fs";
import { fileURLToPath } from "url";

const __dirname = path.dirname(fileURLToPath(import.meta.url));

function arg(name, fallback) {
  const i = process.argv.indexOf(`--${name}`);
  if (i >= 0 && process.argv[i + 1]) return process.argv[i + 1];
  return fallback;
}

const COUNT = parseInt(process.env.COUNT || arg("count", "800000"), 10);
const CLIENTS = parseInt(process.env.CLIENTS || arg("clients", "24"), 10);
const CONCURRENCY = parseInt(
  process.env.CONCURRENCY || arg("concurrency", "2"),
  10
);
const COLLECTION =
  process.env.COLLECTION ||
  arg("collection", `DensStorm${Date.now().toString(36).slice(-8)}`);
const COLLECTION16 = (COLLECTION + "XXXXXXXXXXXXXXXX").slice(0, 16);
const ISSUER = (process.env.ISSUER || "http://127.0.0.1:22000").replace(
  /\/$/,
  ""
);
const NODES = process.env.NODES || "http://127.0.0.1:21000";
const CSV =
  process.env.DENSITY_CSV ||
  path.join(__dirname, "data", "world_density_100k.csv");
const OUT_DIR = process.env.STORM_OUT || "";
const WORKER = path.join(__dirname, "geo_load_density.js");

if (!fs.existsSync(CSV)) {
  console.error(`missing density csv: ${CSV}`);
  process.exit(1);
}
if (!fs.existsSync(WORKER)) {
  console.error(`missing worker: ${WORKER}`);
  process.exit(1);
}

const perClient = Math.floor(COUNT / CLIENTS);
const rem = COUNT - perClient * CLIENTS;

console.log(
  JSON.stringify({
    mode: "multi_client_density_storm",
    ISSUER,
    NODES,
    COUNT,
    CLIENTS,
    CONCURRENCY_PER_CLIENT: CONCURRENCY,
    per_client: perClient,
    collection: COLLECTION16,
    CSV,
  })
);

const t0 = Date.now();
const children = [];
const summaries = [];

function runClient(idx, count) {
  return new Promise((resolve) => {
    const env = {
      ...process.env,
      ISSUER,
      NODES,
      DENSITY_CSV: CSV,
      // Distinct logical client — token client_id uses this in geo_load.
      STORM_CLIENT_ID: `storm-c${idx}-${Date.now().toString(36)}`,
    };
    const args = [
      WORKER,
      "--count",
      String(count),
      "--concurrency",
      String(CONCURRENCY),
      "--collection",
      COLLECTION16,
      "--duration",
      "0",
    ];
    const child = spawn(process.execPath, args, {
      env,
      stdio: ["ignore", "pipe", "pipe"],
    });
    children.push(child);
    let stdout = "";
    let stderr = "";
    child.stdout.on("data", (d) => {
      stdout += d.toString();
      if (idx === 0) process.stdout.write(d);
    });
    child.stderr.on("data", (d) => {
      stderr += d.toString();
      if (idx < 3) process.stderr.write(`[c${idx}] ${d}`);
    });
    child.on("close", (code) => {
      let last = null;
      const objs = [];
      let buf = "";
      let depth = 0;
      for (const ch of stdout) {
        if (ch === "{") depth++;
        if (depth) buf += ch;
        if (ch === "}") {
          depth--;
          if (depth === 0) {
            try {
              objs.push(JSON.parse(buf));
            } catch {
              /* ignore */
            }
            buf = "";
          }
        }
      }
      if (objs.length) last = objs[objs.length - 1];
      const hop = last?.hop_distribution || {};
      summaries.push({
        client: idx,
        code,
        ok: last?.ok ?? 0,
        fail: last?.fail ?? 0,
        elapsed_ms: last?.elapsed_ms ?? 0,
        hop_distribution: hop,
        stderr_tail: stderr.slice(-400),
      });
      if (OUT_DIR) {
        fs.mkdirSync(OUT_DIR, { recursive: true });
        fs.writeFileSync(
          path.join(OUT_DIR, `client_${idx}.json`),
          JSON.stringify({ code, last, stderr_tail: stderr.slice(-2000) }, null, 2)
        );
      }
      resolve();
    });
  });
}

const PARALLEL = Math.min(
  CLIENTS,
  parseInt(process.env.STORM_PARALLEL || arg("parallel", String(CLIENTS)), 10)
);
// Clients join over the ramp instead of all at once: real load arrives as
// edges come online, and a step function tells you nothing about how the mesh
// tracks a rise.
const RAMP_S = parseFloat(process.env.RAMP_S || arg("ramp_s", "0"));

async function main() {
  let next = 0;
  const rampMs = RAMP_S > 0 ? (RAMP_S * 1000) / Math.max(PARALLEL, 1) : 0;
  async function poolWorker(slot) {
    if (rampMs > 0) await new Promise((r) => setTimeout(r, slot * rampMs));
    while (true) {
      const i = next++;
      if (i >= CLIENTS) return;
      const n = perClient + (i < rem ? 1 : 0);
      if (n <= 0) continue;
      await runClient(i, n);
    }
  }
  await Promise.all(Array.from({ length: PARALLEL }, (_, slot) => poolWorker(slot)));

  const ok = summaries.reduce((s, x) => s + x.ok, 0);
  const fail = summaries.reduce((s, x) => s + x.fail, 0);
  const hops = {};
  for (const s of summaries) {
    for (const [k, v] of Object.entries(s.hop_distribution || {})) {
      hops[k] = (hops[k] || 0) + v;
    }
  }
  const elapsed = Date.now() - t0;
  const report = {
    collection: COLLECTION16,
    clients: CLIENTS,
    count_requested: COUNT,
    ok,
    fail,
    elapsed_ms: elapsed,
    rps: ok / (elapsed / 1000),
    hop_distribution: hops,
    hop_bases: Object.keys(hops).length,
    clients_ok: summaries.filter((s) => s.code === 0).length,
    clients_fail: summaries.filter((s) => s.code !== 0).length,
  };
  console.log(JSON.stringify(report, null, 2));
  if (OUT_DIR) {
    fs.writeFileSync(
      path.join(OUT_DIR, "storm_summary.json"),
      JSON.stringify(report, null, 2)
    );
  }
  if (fail > COUNT * 0.02 || ok < COUNT * 0.9) process.exitCode = 2;
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
