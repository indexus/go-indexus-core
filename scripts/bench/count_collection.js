#!/usr/bin/env node
/**
 * Count items in a geo collection by exhaustive Local.search + HTTP tree walk.
 *
 * Usage:
 *   ISSUER=... BOOTSTRAP=ip|21000 NODES=http://ip:21000,... \
 *     node count_collection.js --collection FrScaleLab001 --expected 5000
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
const BOOTSTRAP = process.env.BOOTSTRAP || "127.0.0.1|21000";
const NODES = (process.env.NODES || `http://${BOOTSTRAP.split("|")[0]}:21000`)
  .split(",")
  .map((s) => s.trim().replace(/\/$/, ""))
  .filter(Boolean);
const COLL = arg("collection", "FrScaleLab001");
const EXPECTED = parseInt(arg("expected", "0"), 10);
const LAT = parseFloat(arg("lat", "48.8566"));
const LNG = parseFloat(arg("lng", "2.3522"));

class StableNetwork {
  constructor(protocol, api, host) {
    this._protocol = protocol;
    this._api = api;
    this._concurrency = 32;
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

function peerHost(contact) {
  if (!contact) return null;
  if (typeof contact.host === "string" && contact.host.includes("|")) return contact.host;
  if (typeof contact._host === "string" && contact._host.includes("|")) return contact._host;
  const ip = contact.ip || contact._ip;
  const port = contact.port || contact._port;
  if (ip) return `${ip}|${port || 21000}`;
  // name@ip|port
  const name = contact.name || contact._name;
  if (typeof name === "string" && name.includes("@")) {
    return name.split("@")[1];
  }
  return null;
}

async function httpGetSet(token, node, location) {
  const { data } = await axios.get(`${node}/set`, {
    params: { collection: COLL, location },
    headers: { Authorization: `Bearer ${token}` },
    timeout: 20000,
    validateStatus: () => true,
  });
  return data;
}

async function httpGetSetFollow(token, location) {
  for (const node of NODES) {
    let data = await httpGetSet(token, node, location);
    if (data?.set && Object.keys(data.set).length > 0) return data;
    const host = peerHost(data?.contact);
    if (host) {
      const [ip, port] = host.split("|");
      data = await httpGetSet(token, `http://${ip}:${port}`, location);
      if (data?.set && Object.keys(data.set).length > 0) return data;
    }
  }
  return { set: {} };
}

async function main() {
  const { data: tok } = await axios.post(`${ISSUER}/v1/issue/token`, {
    client_id: `count-${Date.now()}`,
    scopes: ["read", "write"],
  });
  const token = tok.token;
  globalThis.__INDEXUS_BEARER__ = token;
  process.env.INDEXUS_BEARER = token;

  // --- HTTP tree walk ---
  const visited = new Set();
  const queue = ["@"];
  const httpIds = new Set();
  let httpLeafKeys = 0;

  while (queue.length) {
    const loc = queue.shift();
    if (visited.has(loc)) continue;
    visited.add(loc);
    let data;
    try {
      data = await httpGetSetFollow(token, loc);
    } catch (e) {
      continue;
    }
    for (const [key, ab] of Object.entries(data.set || {})) {
      if (key.includes(":")) {
        httpLeafKeys++;
        httpIds.add(key.split(":").pop());
      } else if (!visited.has(key)) {
        queue.push(key);
      }
    }
  }

  // --- Nearest exhaust ---
  const collection = new Collection(COLL, [
    { name: "gps", type: "spherical", args: [-90, 90, -180, 180] },
  ]);
  const space = new Space(collection.dimensions(), collection.mask(), collection.offset());
  const gps = space.dimension(0);
  const nearestIds = new Set();
  const output = {
    send: (rs) => {
      for (const r of rs) nearestIds.add(r.id?.() || r._id);
    },
  };
  const local = new Local(
    { [COLL]: space },
    {
      cap: 1,
      step: 200,
      origins: { gps: gps.newPoint([LAT, LNG]) },
      filters: { gps: gps.newFilter([0, 0], [0, 360]) },
    },
    output,
    { send() {} },
    new StableNetwork("http", new API(), BOOTSTRAP)
  );

  let rounds = 0;
  let stalled = 0;
  while (rounds < 80 && stalled < 3) {
    const before = nearestIds.size;
    await local.search();
    rounds++;
    if (nearestIds.size === before) stalled++;
    else stalled = 0;
  }

  const result = {
    collection: COLL,
    expected: EXPECTED || null,
    http_tree: {
      locations_visited: visited.size,
      leaf_keys: httpLeafKeys,
      unique_ids: httpIds.size,
    },
    nearest_exhaust: {
      rounds,
      unique_ids: nearestIds.size,
    },
    match_expected:
      EXPECTED > 0
        ? {
            http: httpIds.size === EXPECTED,
            nearest: nearestIds.size === EXPECTED,
          }
        : null,
  };
  console.log(JSON.stringify(result, null, 2));
  if (EXPECTED > 0 && httpIds.size !== EXPECTED && nearestIds.size !== EXPECTED) {
    process.exitCode = 2;
  }
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
