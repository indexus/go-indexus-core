#!/usr/bin/env node
/**
 * Concurrent nearest-search read storm.
 *
 * Clients run sequentially by default (SDK Local is not safe under parallel
 * mutation of exploration state on an empty/partial tree). Use --parallel
 * to force concurrent clients.
 *
 * Usage:
 *   ISSUER=... BOOTSTRAP=ip|21000 \
 *     node read_storm.js --clients 8 --rounds 5 --limit 50 --collection FrScaleLab001
 */
import axios from "axios";
import { createRequire } from "module";
const require = createRequire(import.meta.url);
const { Collection, Space, Local, API, Peer } = require("js-indexus-sdk");

function arg(name, fallback) {
  const i = process.argv.indexOf(`--${name}`);
  if (i >= 0 && process.argv[i + 1]) return process.argv[i + 1];
  return fallback;
}
function hasFlag(name) {
  return process.argv.includes(`--${name}`);
}

const ISSUER = (process.env.ISSUER || "http://127.0.0.1:22000").replace(/\/$/, "");
const BOOTSTRAP = process.env.BOOTSTRAP || arg("bootstrap", "127.0.0.1|21000");
const COLLECTION_NAME = arg("collection", "FrScaleLab001");
const LAT = parseFloat(arg("lat", "48.8566"));
const LNG = parseFloat(arg("lng", "2.3522"));
const LIMIT = parseInt(arg("limit", "50"), 10);
const ROUNDS = parseInt(arg("rounds", "5"), 10);
const CLIENTS = parseInt(arg("clients", "8"), 10);
const PARALLEL = hasFlag("parallel");

class StableNetwork {
  constructor(protocol, api, host, concurrency = 16) {
    this._protocol = protocol;
    this._api = api;
    this._concurrency = concurrency;
    const [ip, port] = host.split("|");
    this._ready = api.pingPeer(protocol, ip, parseInt(port, 10)).then((p) => {
      this._peer = p;
    });
  }
  getConcurrency() {
    return this._concurrency;
  }
  async getSet(coll, location) {
    await this._ready;
    let peer = this._peer;
    for (let hop = 0; hop < 8; hop++) {
      const response = await this._api.getSet(this._protocol, peer, coll, location);
      if (
        response.contact instanceof Peer &&
        response.contact.hash() !== peer.hash() &&
        response.set === null
      ) {
        peer = response.contact;
        continue;
      }
      return response.set || [];
    }
    return [];
  }
  async addItem() {
    throw new Error("not implemented");
  }
}

function percentile(sorted, p) {
  if (!sorted.length) return null;
  const idx = Math.min(sorted.length - 1, Math.ceil((p / 100) * sorted.length) - 1);
  return sorted[idx];
}

async function runClient(token, clientId) {
  globalThis.__INDEXUS_BEARER__ = token;
  process.env.INDEXUS_BEARER = token;

  const collection = new Collection(COLLECTION_NAME, [
    { name: "gps", type: "spherical", args: [-90, 90, -180, 180] },
  ]);
  const space = new Space(collection.dimensions(), collection.mask(), collection.offset());
  const gps = space.dimension(0);

  const latencies = [];
  let totalGot = 0;
  let errors = 0;

  const results = [];
  const output = {
    send: (searchResults) => {
      for (const r of searchResults) results.push(r);
    },
  };
  const monitoring = { send: () => {} };
  const options = {
    cap: 1,
    step: LIMIT,
    origins: { gps: gps.newPoint([LAT, LNG]) },
    filters: { gps: gps.newFilter([0, 0], [0, 360]) },
  };

  const api = new API();
  const network = new StableNetwork("http", api, BOOTSTRAP);
  const local = new Local({ [COLLECTION_NAME]: space }, options, output, monitoring, network);

  for (let round = 1; round <= ROUNDS; round++) {
    const before = results.length;
    const t0 = Date.now();
    try {
      await local.search();
      const ms = Date.now() - t0;
      latencies.push(ms);
      totalGot += Math.max(0, results.length - before);
    } catch (e) {
      errors++;
      if (errors <= 3) console.error(`client ${clientId} round ${round}`, e.message);
    }
  }

  return { clientId, latencies, totalGot, errors };
}

async function main() {
  const { data } = await axios.post(`${ISSUER}/v1/issue/token`, {
    client_id: `read-storm-${Date.now()}`,
    scopes: ["read", "write"],
  });
  const token = data.token;

  console.log(
    JSON.stringify({
      ISSUER,
      BOOTSTRAP,
      COLLECTION_NAME,
      CLIENTS,
      ROUNDS,
      LIMIT,
      PARALLEL,
      origin: { lat: LAT, lng: LNG },
    })
  );

  const t0 = Date.now();
  let outcomes;
  if (PARALLEL) {
    outcomes = await Promise.all(
      Array.from({ length: CLIENTS }, (_, i) => runClient(token, i))
    );
  } else {
    outcomes = [];
    for (let i = 0; i < CLIENTS; i++) {
      outcomes.push(await runClient(token, i));
    }
  }
  const elapsed = Date.now() - t0;

  const allLat = outcomes.flatMap((o) => o.latencies).sort((a, b) => a - b);
  const totalGot = outcomes.reduce((s, o) => s + o.totalGot, 0);
  const errors = outcomes.reduce((s, o) => s + o.errors, 0);

  const summary = {
    clients: CLIENTS,
    rounds_per_client: ROUNDS,
    total_got: totalGot,
    errors,
    elapsed_ms: elapsed,
    round_latencies_ms: {
      count: allLat.length,
      p50: percentile(allLat, 50),
      p95: percentile(allLat, 95),
      p99: percentile(allLat, 99),
      max: allLat[allLat.length - 1] ?? null,
      avg: allLat.length ? allLat.reduce((a, b) => a + b, 0) / allLat.length : null,
    },
  };

  console.log(JSON.stringify(summary, null, 2));
  if (totalGot === 0 || errors > CLIENTS * ROUNDS * 0.5) process.exitCode = 2;
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
