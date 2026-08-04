#!/usr/bin/env node
/**
 * SDK smoke against a live Indexus mesh.
 * Usage:
 *   ISSUER=http://IP:22000 SEED=IP|21000 node scripts/bench/sdk_smoke.mjs
 */
import { createRequire } from "module";
import { pathToFileURL } from "url";
import path from "path";
import { fileURLToPath } from "url";

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const SDK_ROOT = path.resolve(__dirname, "../../../sdk-js");
const require = createRequire(path.join(SDK_ROOT, "package.json"));

// Prefer source over dist so authHeaders / depth semantics match the repo.
const { Network, API } = await import(
  pathToFileURL(path.join(SDK_ROOT, "src/index.js")).href
);

const ISSUER = (process.env.ISSUER || "http://127.0.0.1:22000").replace(/\/$/, "");
const SEED = process.env.SEED || "127.0.0.1|21000";
const COLLECTION = (process.env.COLLECTION || `SdkSmoke${Date.now().toString(36)}`).slice(0, 16).padEnd(16, "X");

async function issueToken() {
  const res = await fetch(`${ISSUER}/v1/issue/token`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({
      client_id: `sdk-smoke-${Date.now()}`,
      scopes: ["read", "write"],
    }),
  });
  if (!res.ok) throw new Error(`issue token ${res.status}`);
  const body = await res.json();
  return body.token;
}

function randomLoc(n = 8) {
  const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";
  let s = "";
  for (let i = 0; i < n; i++) s += alphabet[(Math.random() * alphabet.length) | 0];
  return s;
}

const report = { collection: COLLECTION, seed: SEED, issuer: ISSUER, steps: [] };

try {
  const token = await issueToken();
  process.env.INDEXUS_BEARER = token;
  globalThis.__INDEXUS_BEARER__ = token;
  report.steps.push({ step: "issue_token", ok: true });

  const network = new Network("http", new API(), [SEED], 8, 100);

  const writes = [];
  for (let i = 0; i < 5; i++) {
    const loc = randomLoc(10);
    const id = `sdk-${i}-${Date.now().toString(36)}`;
    await network.addItem(COLLECTION, "@", loc, [1, 2, 3, 4, 5], id);
    writes.push({ location: loc, id });
    report.steps.push({ step: "addItem", ok: true, location: loc, id });
  }

  // Read back via getSet at root / first write location.
  const first = writes[0];
  const set = await network.getSet(COLLECTION, first.location, 2);
  report.steps.push({
    step: "getSet",
    ok: true,
    location: first.location,
    has_data: !!set,
    preview: set ? JSON.stringify(set).slice(0, 240) : null,
  });

  // Neighbors / exploration path if available
  if (typeof network.getNeighbors === "function") {
    const neigh = await network.getNeighbors(COLLECTION, first.location);
    report.steps.push({
      step: "getNeighbors",
      ok: true,
      count: Array.isArray(neigh) ? neigh.length : Object.keys(neigh || {}).length,
    });
  } else if (typeof network.explore === "function") {
    const expl = await network.explore(COLLECTION, first.location);
    report.steps.push({ step: "explore", ok: true, preview: JSON.stringify(expl).slice(0, 200) });
  } else {
    report.steps.push({ step: "neighbors", ok: true, skipped: "no getNeighbors on Network" });
  }

  report.ok = true;
} catch (e) {
  report.ok = false;
  report.error = e?.message || String(e);
  report.steps.push({ step: "fatal", ok: false, error: report.error });
}

console.log(JSON.stringify(report, null, 2));
process.exit(report.ok ? 0 : 1);
