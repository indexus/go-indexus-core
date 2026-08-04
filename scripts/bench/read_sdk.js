#!/usr/bin/env node
/**
 * Read via js-indexus-sdk API (ping + getSet) with bearer token.
 * Uses API directly to avoid a Network.discoverPeers race in the SDK.
 */
import axios from "axios";
import { createRequire } from "module";
const require = createRequire(import.meta.url);
const { API } = require("js-indexus-sdk");

function arg(name, fallback) {
  const i = process.argv.indexOf(`--${name}`);
  if (i >= 0 && process.argv[i + 1]) return process.argv[i + 1];
  return fallback;
}

const ISSUER = (process.env.ISSUER || arg("issuer", "http://127.0.0.1:22000")).replace(/\/$/, "");
const BOOTSTRAP = process.env.BOOTSTRAP || arg("bootstrap", "127.0.0.1|21000");
const COLLECTION = arg("collection", "demo-read2");
const LOCATION = arg("location", "@");

const [ip, port] = BOOTSTRAP.split("|");
const { data } = await axios.post(`${ISSUER}/v1/issue/token`, {
  client_id: `sdk-read-${Date.now()}`,
  scopes: ["read", "write"],
});
globalThis.__INDEXUS_BEARER__ = data.token;
process.env.INDEXUS_BEARER = data.token;

const api = new API();
const t0 = Date.now();
const peer = await api.pingPeer("http", ip, parseInt(port, 10));
const result = await api.getSet("http", peer, COLLECTION, LOCATION);
const ms = Date.now() - t0;
const n = Array.isArray(result?.set) ? result.set.length : 0;

console.log(
  JSON.stringify(
    {
      bootstrap: BOOTSTRAP,
      collection: COLLECTION,
      location: LOCATION,
      latency_ms: ms,
      peer: peer.hash(),
      elements: n,
      contacted: result?.contact?.hash?.() || null,
    },
    null,
    2
  )
);
