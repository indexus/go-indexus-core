#!/usr/bin/env node
/**
 * Nearest-neighbor search: incremental batches of N closest items.
 *
 * Each Local.search() advances by --limit (step). Run --rounds times.
 *
 * Usage:
 *   ISSUER=... BOOTSTRAP=ip|21000 \
 *     node geo_nearest.js --lat 48.8566 --lng 2.3522 --limit 100 --rounds 20
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

const ISSUER = (process.env.ISSUER || "http://127.0.0.1:22000").replace(/\/$/, "");
const BOOTSTRAP = process.env.BOOTSTRAP || arg("bootstrap", "127.0.0.1|21000");
const COLLECTION_NAME = arg("collection", "FrGeoBenchAws00001");
const LAT = parseFloat(arg("lat", "48.8566")); // Paris
const LNG = parseFloat(arg("lng", "2.3522"));
const LIMIT = parseInt(arg("limit", "100"), 10);
const ROUNDS = parseInt(arg("rounds", "20"), 10);

const collection = new Collection(COLLECTION_NAME, [
  { name: "gps", type: "spherical", args: [-90, 90, -180, 180] },
]);
const space = new Space(collection.dimensions(), collection.mask(), collection.offset());
const gps = space.dimension(0);

/**
 * Stable network wrapper: one successful ping, then getSet with contact follow.
 * Avoids Network.discoverPeers race in the current SDK.
 */
class StableNetwork {
  constructor(protocol, api, host, concurrency = 32) {
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

function summarize(item, rank) {
  const metrics = item.metrics?.() || item._metrics || [];
  return {
    rank,
    id: item.id?.() || item._id || item.hash?.(),
    distance: item.distance?.() ?? item._distance,
    lat: (metrics[3] ?? 0) - 90,
    lng: (metrics[4] ?? 0) - 180,
  };
}

async function main() {
  const { data } = await axios.post(`${ISSUER}/v1/issue/token`, {
    client_id: `geo-nearest-${Date.now()}`,
    scopes: ["read", "write"],
  });
  globalThis.__INDEXUS_BEARER__ = data.token;
  process.env.INDEXUS_BEARER = data.token;

  let batch = [];
  const all = [];
  const output = {
    send: (searchResults) => {
      for (const r of searchResults) {
        batch.push(r);
        all.push(r);
      }
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

  console.log(
    JSON.stringify({
      origin: { lat: LAT, lng: LNG },
      collection: COLLECTION_NAME,
      step: LIMIT,
      rounds: ROUNDS,
      target_total: LIMIT * ROUNDS,
    })
  );

  const tAll = Date.now();
  const roundStats = [];
  let stalled = false;

  for (let round = 1; round <= ROUNDS; round++) {
    batch = [];
    const t0 = Date.now();
    await local.search();
    const ms = Date.now() - t0;

    const startRank = all.length - batch.length + 1;
    const items = batch.map((r, i) => summarize(r, startRank + i));
    const stat = {
      round,
      got: items.length,
      cumulative: all.length,
      latency_ms: ms,
      first: items[0] || null,
      last: items[items.length - 1] || null,
    };
    roundStats.push(stat);
    console.log(JSON.stringify(stat));

    if (items.length === 0) {
      stalled = true;
      break;
    }
  }

  const totalMs = Date.now() - tAll;
  const first = all[0] ? summarize(all[0], 1) : null;
  const last = all.length ? summarize(all[all.length - 1], all.length) : null;

  console.log(
    JSON.stringify(
      {
        summary: {
          origin: { lat: LAT, lng: LNG },
          collection: COLLECTION_NAME,
          rounds_done: roundStats.length,
          total_got: all.length,
          requested: LIMIT * ROUNDS,
          stalled,
          total_latency_ms: totalMs,
          avg_round_ms:
            roundStats.length > 0
              ? roundStats.reduce((s, r) => s + r.latency_ms, 0) / roundStats.length
              : 0,
          nearest: first,
          farthest: last,
        },
      },
      null,
      2
    )
  );

  if (all.length < Math.min(LIMIT, 10)) process.exitCode = 2;
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
