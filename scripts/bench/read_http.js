#!/usr/bin/env node
/** Direct authenticated GET /set (no SDK ping/discovery). */
import axios from "axios";

function arg(name, fallback) {
  const i = process.argv.indexOf(`--${name}`);
  if (i >= 0 && process.argv[i + 1]) return process.argv[i + 1];
  return fallback;
}

const ISSUER = (process.env.ISSUER || "http://127.0.0.1:22000").replace(/\/$/, "");
const NODE = (process.env.NODE || "http://127.0.0.1:21000").replace(/\/$/, "");
const COLLECTION = arg("collection", "bench-deleg");
const LOCATION = arg("location", "@");

const { data: tok } = await axios.post(`${ISSUER}/v1/issue/token`, {
  client_id: `http-read-${Date.now()}`,
  scopes: ["read"],
});
const t0 = Date.now();
const { data, status } = await axios.get(`${NODE}/set`, {
  params: { collection: COLLECTION, location: LOCATION },
  headers: { Authorization: `Bearer ${tok.token}` },
});
console.log(
  JSON.stringify(
    {
      status,
      latency_ms: Date.now() - t0,
      keys: data.set ? Object.keys(data.set).length : 0,
      contact: data.contact?.name || null,
      sample: data.set ? Object.entries(data.set).slice(0, 3) : [],
    },
    null,
    2
  )
);
