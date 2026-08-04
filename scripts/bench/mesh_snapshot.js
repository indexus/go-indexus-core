#!/usr/bin/env node
/**
 * Snapshot mesh monitoring endpoints (registered / ownership / queue / routing).
 *
 * Usage:
 *   BOOT_IP=52.x.x.x node mesh_snapshot.js [--out dir] [--label baseline]
 *   HOSTS=ip1,ip2 node mesh_snapshot.js
 */
import fs from "fs";
import path from "path";
import axios from "axios";

function arg(name, fallback) {
  const i = process.argv.indexOf(`--${name}`);
  if (i >= 0 && process.argv[i + 1]) return process.argv[i + 1];
  return fallback;
}

const BOOT_IP = process.env.BOOT_IP || arg("boot", "127.0.0.1");
const LABEL = arg("label", "snap");
const OUT_DIR = arg("out", process.env.OUT_DIR || "out");
const MON_PORT = parseInt(process.env.MON_PORT || "19000", 10);

async function getJson(url) {
  try {
    const { data } = await axios.get(url, { timeout: 8000 });
    return { ok: true, data };
  } catch (e) {
    return { ok: false, error: e.message };
  }
}

/** Count Own zones + delegated children across ownership tree. */
export function summarizeOwnership(ownership) {
  let ownedZones = 0;
  let delegatedChildren = 0;
  const byCollection = {};
  for (const [coll, zones] of Object.entries(ownership || {})) {
    let oz = 0;
    let dc = 0;
    for (const [, children] of Object.entries(zones || {})) {
      oz++;
      dc += Object.keys(children || {}).length;
    }
    byCollection[coll] = { owned_zones: oz, delegated_children: dc };
    ownedZones += oz;
    delegatedChildren += dc;
  }
  return { owned_zones: ownedZones, delegated_children: delegatedChildren, by_collection: byCollection };
}

function parseHosts(registered) {
  const hosts = registered?.hosts || [];
  return hosts.map((h) => {
    // name@ip|port
    const at = h.indexOf("@");
    const rest = at >= 0 ? h.slice(at + 1) : h;
    const [ip, port] = rest.split("|");
    return { contact: h, ip, port: parseInt(port || "21000", 10) };
  });
}

async function snapshotHost(ip) {
  const base = `http://${ip}:${MON_PORT}`;
  const [registered, ownership, queue, routing] = await Promise.all([
    getJson(`${base}/registered`),
    getJson(`${base}/ownership`),
    getJson(`${base}/queue`),
    getJson(`${base}/routing`),
  ]);
  const ownData = ownership.ok ? ownership.data : {};
  return {
    ip,
    registered,
    ownership,
    queue,
    routing,
    summary: {
      ownership: summarizeOwnership(ownData),
      queue_pending: queue.ok ? queue.data?.pending ?? null : null,
      registered_count: registered.ok ? (registered.data?.hosts || []).length : null,
    },
  };
}

async function main() {
  const hostsEnv = process.env.HOSTS;
  let ips = [];
  if (hostsEnv) {
    ips = hostsEnv.split(",").map((s) => s.trim()).filter(Boolean);
  } else {
    const boot = await snapshotHost(BOOT_IP);
    ips = [BOOT_IP];
    if (boot.registered.ok) {
      for (const h of parseHosts(boot.registered.data)) {
        if (h.ip && !ips.includes(h.ip)) ips.push(h.ip);
      }
    }
  }

  const nodes = [];
  for (const ip of ips) {
    nodes.push(await snapshotHost(ip));
  }

  // Ownership fingerprint: map "coll:zone" -> node ip that owns it (non-empty children or leaf)
  const ownershipIndex = {};
  for (const n of nodes) {
    if (!n.ownership.ok) continue;
    for (const [coll, zones] of Object.entries(n.ownership.data || {})) {
      for (const zone of Object.keys(zones || {})) {
        const key = `${coll}:${zone}`;
        if (!ownershipIndex[key]) ownershipIndex[key] = [];
        ownershipIndex[key].push(n.ip);
      }
    }
  }

  const snap = {
    label: LABEL,
    ts: new Date().toISOString(),
    boot_ip: BOOT_IP,
    nodes,
    ownership_index: ownershipIndex,
    totals: {
      nodes: nodes.length,
      owned_zones: nodes.reduce((s, n) => s + (n.summary.ownership.owned_zones || 0), 0),
      queue_pending: nodes.reduce((s, n) => s + (n.summary.queue_pending || 0), 0),
    },
  };

  fs.mkdirSync(OUT_DIR, { recursive: true });
  const file = path.join(OUT_DIR, `${LABEL}-${Date.now()}.json`);
  fs.writeFileSync(file, JSON.stringify(snap, null, 2));
  console.log(JSON.stringify({ file, label: LABEL, totals: snap.totals, ownership_keys: Object.keys(ownershipIndex).length }, null, 2));
  // also print path alone on stderr for shell capture
  console.error(file);
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
