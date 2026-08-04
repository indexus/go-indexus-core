#!/usr/bin/env node
/**
 * Indexus mesh dashboard — live view for AWS experiments.
 *
 * How to run:
 *   BOOT_IP=x.x.x.x node server.js
 *   # or let it auto-detect from terraform / EC2 tags:
 *   node server.js
 *
 * Then open http://127.0.0.1:3847/
 *
 * Env:
 *   BOOT_IP       bootstrap public IP (optional if terraform/AWS available)
 *   REGION        default eu-west-3
 *   PROJECT       default indexus-aws
 *   MON_PORT      monitoring port, default 19000
 *   PORT          dashboard listen port, default 3847
 *   POLL_MS       server-side poll interval, default 3000
 *   HISTORY_MS    rolling sample window kept in /api/mesh, default 12m
 *   PLAN_FILE     experiment plan JSON (default: ../out/current_plan.json)
 *   ARTIFACTS_BUCKET  optional S3 bucket for snapshot listing
 */
import http from "node:http";
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { spawn, spawnSync, execFile } from "node:child_process";
import { promisify } from "node:util";

const execFileAsync = promisify(execFile);
const __dirname = path.dirname(fileURLToPath(import.meta.url));
const PUBLIC = path.join(__dirname, "public");
const BENCH_DIR = path.resolve(__dirname, "..");
const CORE_ROOT = path.resolve(BENCH_DIR, "../..");
const TF_DIR = path.resolve(CORE_ROOT, "deploy/terraform");
const OUT_DIR = path.join(BENCH_DIR, "out");
const PLAN_WRITE_PATH = path.join(OUT_DIR, "current_plan.json");
const TRAFFIC_SIM = path.join(BENCH_DIR, "traffic_sim.sh");
const TRAFFIC_LOG = path.join(OUT_DIR, "traffic_live.log");
const SPAWN_LOAD = path.join(BENCH_DIR, "spawn_load.sh");
const STOP_LOAD = path.join(BENCH_DIR, "stop_load.sh");
const SSH_KEY = process.env.SSH_KEY || path.join(process.env.HOME || "", ".ssh/id_ed25519");
/** Prefer remote EC2 load VMs unless LOAD_MODE=local. */
const LOAD_MODE = (process.env.LOAD_MODE || "remote").toLowerCase();
const LOAD_HOSTS = parseInt(process.env.LOAD_HOSTS || "2", 10);

const REGION = process.env.REGION || process.env.AWS_REGION || "eu-west-3";
const PROJECT = process.env.PROJECT || "indexus-aws";
const MON_PORT = parseInt(process.env.MON_PORT || "19000", 10);
const PORT = parseInt(process.env.PORT || "3847", 10);
const POLL_MS = parseInt(process.env.POLL_MS || "3000", 10);
const HISTORY_MS = parseInt(process.env.HISTORY_MS || String(12 * 60 * 1000), 10);
const PLAN_CANDIDATES = [
  process.env.PLAN_FILE,
  PLAN_WRITE_PATH,
  path.join(__dirname, "plan.json"),
].filter(Boolean);

/** Default continuous-ramp curve (editable from UI before Start).
 *  ramp_s 360 ≈ 2× faster climb vs the prior 720s experiment default. */
const DEFAULT_PLAN = {
  count: 3000000,
  clients: 48,
  concurrency: 4,
  ramp_s: 360,
  watch_after_s: 600,
  sample_s: 15,
  wps_per_slot: 25,
};

let bootIp = process.env.BOOT_IP || "";
let artifactsBucket = process.env.ARTIFACTS_BUCKET || "";
let cache = null;
let prevSample = null; // for items/sec delta
let pollError = null;
/** @type {Array<Record<string, unknown>>} */
const history = []; // rolling ring buffer of compact samples for charts
let planCache = { mtimeMs: -1, path: null, raw: null, derived: null };

/** @type {{status: string, pid: number|null, outDir: string|null, startedAt: number|null, lastError: string|null, lastAction: string|null, writers: object|null, cleanupGeneration: number, cleanupSteps: any}} */
let experiment = {
  status: "idle",
  pid: null,
  outDir: null,
  startedAt: null,
  lastError: null,
  lastAction: null,
  cleanupGeneration: 0,
  cleanupSteps: null,
  writers: null,
};

function tfOutput(name) {
  try {
    const r = spawnSync("terraform", ["output", "-raw", name], {
      cwd: TF_DIR,
      encoding: "utf8",
      timeout: 8000,
    });
    if (r.status === 0) return (r.stdout || "").trim();
  } catch {
    /* ignore */
  }
  return "";
}

async function resolveBootIp() {
  if (bootIp) return bootIp;
  const fromTf = tfOutput("bootstrap_public_ip");
  if (fromTf) {
    // Prefer live answerer over stale terraform IP
    if (await probeStatus(fromTf)) {
      bootIp = fromTf;
      return bootIp;
    }
  }
  try {
    const { stdout } = await execFileAsync(
      "aws",
      [
        "ec2", "describe-instances",
        "--region", REGION,
        "--filters",
        `Name=tag:Name,Values=${PROJECT}-bootstrap`,
        "Name=instance-state-name,Values=running",
        "--query", "Reservations[].Instances[].PublicIpAddress",
        "--output", "text",
      ],
      { timeout: 20000 },
    );
    const ip = (stdout || "").trim().split(/\s+/)[0];
    if (ip && ip !== "None") {
      bootIp = ip;
      return bootIp;
    }
  } catch {
    /* ignore */
  }
  if (fromTf) {
    bootIp = fromTf;
    return bootIp;
  }
  return "";
}

function resolveBucket() {
  if (artifactsBucket) return artifactsBucket;
  const fromTf = tfOutput("artifacts_bucket");
  if (fromTf) artifactsBucket = fromTf;
  return artifactsBucket;
}

async function probeStatus(ip) {
  try {
    const ctrl = AbortSignal.timeout(3000);
    const res = await fetch(`http://${ip}:${MON_PORT}/status`, { signal: ctrl });
    return res.ok;
  } catch {
    return false;
  }
}

async function fetchJson(url, timeoutMs = 4000) {
  try {
    const res = await fetch(url, { signal: AbortSignal.timeout(timeoutMs) });
    if (!res.ok) return null;
    return await res.json();
  } catch {
    return null;
  }
}

async function listSpawnedInstances() {
  try {
    const { stdout } = await execFileAsync(
      "aws",
      [
        "ec2", "describe-instances",
        "--region", REGION,
        "--filters",
        `Name=tag:Name,Values=${PROJECT}-spawned`,
        "Name=instance-state-name,Values=pending,running",
        "--query",
        "Reservations[].Instances[].{Id:InstanceId,Ip:PublicIpAddress,State:State.Name,LaunchTime:LaunchTime,PreferNear:Tags[?Key=='PreferNear']|[0].Value,Name:Tags[?Key=='Name']|[0].Value}",
        "--output", "json",
      ],
      { timeout: 25000 },
    );
    return JSON.parse(stdout || "[]");
  } catch (e) {
    return { _error: String(e.message || e) };
  }
}

async function listSnapshots(bucket) {
  if (!bucket) return { available: false, objects: [] };
  try {
    const { stdout } = await execFileAsync(
      "aws",
      [
        "s3api", "list-objects-v2",
        "--bucket", bucket,
        "--prefix", "snapshots/",
        "--max-keys", "8",
        "--query", "sort_by(Contents || `[]`, &LastModified)[-8:].{Key:Key,Size:Size,LastModified:LastModified}",
        "--output", "json",
      ],
      { timeout: 15000 },
    );
    const objects = JSON.parse(stdout || "[]") || [];
    // newest first
    objects.reverse();
    return { available: true, bucket, objects };
  } catch (e) {
    return { available: false, error: String(e.message || e), objects: [] };
  }
}

function nodeFromStatus(ip, status, instanceMeta) {
  if (!status) {
    return {
      ip,
      up: false,
      role: instanceMeta?.PreferNear ? "spawned" : undefined,
      instance_id: instanceMeta?.Id || null,
      instance_state: instanceMeta?.State || null,
      prefer_near: instanceMeta?.PreferNear || null,
    };
  }
  const a = status.autoscale || {};
  const p = a.pressure || {};
  const snap = status.snapshot || {};
  const delegIn = snap.deleg_in || 0;
  const delegOut = snap.deleg_out || 0;
  return {
    ip,
    up: true,
    name: status.name || null,
    role: a.role || null,
    items: status.items ?? null,
    zones: status.zones ?? null,
    peers: status.peers ?? null,
    queue: p.queue ?? status.queue ?? null,
    queue_delta: p.queue_delta ?? null,
    hot_signal: p.hot_signal || "",
    hot_for: p.hot_for || null,
    cpu_pct: round1(p.cpu_pct),
    mem_pct: round1(p.mem_pct),
    mem_projected: round1(p.mem_projected),
    disk_free_pct: round1(p.disk_free_pct),
    inserts_window: p.inserts_window ?? a.inserts_window ?? null,
    last_reason: a.last_reason || "",
    last_up_at: a.last_up_at || null,
    last_down_at: a.last_down_at || null,
    scale_ups_done: a.scale_ups_done ?? 0,
    up_in_flight: !!a.up_in_flight,
    down_in_flight: !!a.down_in_flight,
    admit_blocked: !!a.admit_blocked,
    rising_fast: !!a.rising_fast,
    last_prefer_near: a.last_prefer_near || null,
    leaving: !!status.leaving,
    client_ready: status.client_ready !== false,
    rebalancing: !!status.rebalancing,
    transferring: !!status.rebalancing || !!status.leaving || delegIn > 0 || delegOut > 0,
    store: !!snap.store,
    delegation: !!snap.delegation,
    snap_dirty: snap.dirty ?? 0,
    snap_zones: snap.snapped ?? 0,
    wal_segments: snap.wal_segments ?? 0,
    deleg_in: delegIn,
    deleg_out: delegOut,
    uptime_s: status.uptime_s ?? null,
    instance_id: instanceMeta?.Id || null,
    instance_state: instanceMeta?.State || null,
    prefer_near: instanceMeta?.PreferNear || null,
  };
}

function round1(v) {
  if (v == null || Number.isNaN(Number(v))) return null;
  return Math.round(Number(v) * 10) / 10;
}

/** Thresholds + rise annotations from /status → autoscale (boot preferred). */
function extractAutoscale(status) {
  const a = status?.autoscale || {};
  return {
    enabled: a.enabled ?? null,
    role: a.role || null,
    mem_limit_pct: a.mem_limit_pct ?? null,
    mem_floor_pct: a.mem_floor_pct ?? null,
    cpu_floor_pct: a.cpu_floor_pct ?? 25,
    cpu_limit_pct: a.cpu_limit_pct ?? null,
    mem_rise_pct: a.mem_rise_pct ?? null,
    cpu_rise_pct: a.cpu_rise_pct ?? null,
    mem_lead: a.mem_lead ?? null,
    pressure_hold: a.pressure_hold ?? null,
    rise_hold: a.rise_hold ?? null,
    window: a.window ?? null,
    last_reason: a.last_reason || "",
    rising_fast: !!a.rising_fast,
  };
}

/** Parse Go-style durations like "1m30s", "2m", "90s" into seconds. */
function parseDurationSeconds(raw) {
  if (raw == null || raw === "") return null;
  if (typeof raw === "number" && Number.isFinite(raw)) return raw;
  const s = String(raw).trim();
  let total = 0;
  const re = /(\d+(?:\.\d+)?)(ns|us|µs|ms|s|m|h)/g;
  let m;
  let matched = false;
  while ((m = re.exec(s))) {
    matched = true;
    const n = parseFloat(m[1]);
    switch (m[2]) {
      case "h":
        total += n * 3600;
        break;
      case "m":
        total += n * 60;
        break;
      case "s":
        total += n;
        break;
      case "ms":
        total += n / 1000;
        break;
      default:
        break;
    }
  }
  return matched && total > 0 ? total : null;
}

function maxOf(arr) {
  const nums = arr.filter((v) => v != null && !Number.isNaN(Number(v))).map(Number);
  return nums.length ? Math.max(...nums) : null;
}

function pushHistory(sample) {
  const up = (sample.nodes || []).filter((n) => n.up);
  const boot = (sample.nodes || []).find((n) => n.is_boot) || up[0] || null;
  const byIp = {};
  for (const n of sample.nodes || []) {
    if (!n.ip) continue;
    byIp[n.ip] = {
      mem_pct: n.mem_pct ?? null,
      cpu_pct: n.cpu_pct ?? null,
      mem_projected: n.mem_projected ?? null,
      inserts_window: n.inserts_window ?? null,
      items: n.items ?? null,
      queue: n.queue ?? null,
      transferring: !!n.transferring,
      rebalancing: !!n.rebalancing,
    };
  }
  history.push({
    t: sample.ts_ms,
    mem_max: maxOf(up.map((n) => n.mem_pct)),
    cpu_max: maxOf(up.map((n) => n.cpu_pct)),
    mem_proj_max: maxOf(up.map((n) => n.mem_projected)),
    mem_boot: boot?.mem_pct ?? null,
    cpu_boot: boot?.cpu_pct ?? null,
    mem_proj_boot: boot?.mem_projected ?? null,
    inserts_window: sample.throughput?.inserts_window ?? null,
    items_per_sec: sample.throughput?.items_per_sec ?? null,
    items: sample.totals?.items ?? null,
    by_ip: byIp,
  });
  const cutoff = Date.now() - HISTORY_MS;
  while (history.length > 0 && history[0].t < cutoff) history.shift();
  // hard cap in case clock skew / long poll
  const maxPts = Math.ceil(HISTORY_MS / Math.max(POLL_MS, 500)) + 30;
  while (history.length > maxPts) history.shift();
}

/** Linear client ramp: 0 → clients over ramp_s, then flat. */
function plannedClientsAt(plan, tSec) {
  const clients = Number(plan.clients) || 0;
  const ramp = Number(plan.ramp_s) || 0;
  if (clients <= 0) return 0;
  if (ramp <= 0) return clients;
  if (tSec <= 0) return 0;
  return Math.min(clients, (clients * tSec) / ramp);
}

/**
 * Effort-shaped cumulative writes: ∫ clients(u)·concurrency du, scaled to count.
 * Horizon = ramp trapezoid + remaining at full rate (estimate peak_wps if absent).
 */
function derivePlanCurves(plan, nowMs = Date.now()) {
  if (!plan || typeof plan !== "object") return null;
  const clients = Number(plan.clients) || 0;
  const concurrency = Math.max(1, Number(plan.concurrency) || 1);
  const ramp = Math.max(0, Number(plan.ramp_s) || 0);
  const count = Math.max(0, Number(plan.count) || 0);
  // Accept started_at_unix, numeric started_at, or ISO started_at
  let startedAt = Number(plan.started_at_unix);
  if (!startedAt || Number.isNaN(startedAt)) {
    const raw = plan.started_at;
    if (typeof raw === "number") startedAt = raw;
    else if (typeof raw === "string" && /^\d+$/.test(raw)) startedAt = Number(raw);
    else if (typeof raw === "string") {
      const ms = Date.parse(raw);
      startedAt = Number.isNaN(ms) ? 0 : Math.floor(ms / 1000);
    } else startedAt = 0;
  }
  if (!startedAt || clients <= 0 || count <= 0) {
    return { available: false, reason: "incomplete plan", path: plan._path || null, raw: plan };
  }

  const startedMs = startedAt * 1000;
  const elapsed = Math.max(0, (nowMs - startedMs) / 1000);
  // Nominal peak writes/s from worker slots (clients*concurrency). Scale so
  // the integral of the ramp+plateau reaches `count` in a finite horizon.
  const peakSlots = clients * concurrency;
  const rampEffort = ramp > 0 ? (peakSlots * ramp) / 2 : 0; // triangle under ramp
  // Assume ~25 writes/s per concurrent worker as planning hint unless overridden
  const wpsPerSlot = Number(plan.wps_per_slot) > 0 ? Number(plan.wps_per_slot) : 25;
  const peakWps = peakSlots * wpsPerSlot;
  const afterRampCount = Math.max(0, count - rampEffort * wpsPerSlot);
  const plateauS = peakWps > 0 ? afterRampCount / peakWps : 0;
  const horizonS = ramp + plateauS;

  const step = Math.max(1, Math.round(Math.min(5, horizonS / 120 || 1)));
  const series = [];
  const endT = Math.max(elapsed, horizonS, ramp);
  for (let t = 0; t <= endT + step; t += step) {
    const c = plannedClientsAt(plan, t);
    const slots = c * concurrency;
    let effort;
    if (t <= 0) effort = 0;
    else if (ramp <= 0) effort = peakSlots * t;
    else if (t <= ramp) effort = (peakSlots * t * t) / (2 * ramp);
    else effort = rampEffort + peakSlots * (t - ramp);
    const cum = Math.min(count, effort * wpsPerSlot);
    const rps = slots * wpsPerSlot;
    series.push({
      t_s: Math.round(t * 10) / 10,
      ts_ms: startedMs + t * 1000,
      clients: Math.round(c * 10) / 10,
      expected_rps: Math.round(rps * 10) / 10,
      expected_cum: Math.round(cum),
    });
  }

  const nowClients = plannedClientsAt(plan, elapsed);
  const nowPt = series.reduce((best, p) =>
    Math.abs(p.t_s - elapsed) < Math.abs(best.t_s - elapsed) ? p : best,
  series[0]);

  return {
    available: true,
    path: plan._path || null,
    count,
    clients,
    concurrency,
    ramp_s: ramp,
    started_at: startedAt,
    started_at_iso: new Date(startedMs).toISOString(),
    seed: plan.seed || (plan.boot_ip ? `http://${plan.boot_ip}:21000` : null),
    collection: plan.collection || null,
    out: plan.out || null,
    status: plan.status || null,
    elapsed_s: Math.round(elapsed),
    horizon_s: Math.round(horizonS),
    now_clients: Math.round(nowClients * 10) / 10,
    now_expected_cum: nowPt?.expected_cum ?? null,
    now_expected_rps: nowPt?.expected_rps ?? null,
    wps_per_slot: wpsPerSlot,
    series,
  };
}

function loadPlanFile() {
  for (const p of PLAN_CANDIDATES) {
    try {
      if (!fs.existsSync(p)) continue;
      const st = fs.statSync(p);
      if (planCache.path === p && planCache.mtimeMs === st.mtimeMs && planCache.derived) {
        // refresh time-dependent "now_*" fields
        planCache.derived = derivePlanCurves(planCache.raw, Date.now());
        if (planCache.derived) planCache.derived.path = p;
        return planCache.derived;
      }
      const raw = JSON.parse(fs.readFileSync(p, "utf8"));
      raw._path = p;
      const derived = derivePlanCurves(raw, Date.now());
      planCache = { mtimeMs: st.mtimeMs, path: p, raw, derived };
      return derived;
    } catch (e) {
      return {
        available: false,
        path: p,
        error: String(e.message || e),
      };
    }
  }
  return { available: false, paths_tried: PLAN_CANDIDATES };
}

function readRawPlan() {
  try {
    if (fs.existsSync(PLAN_WRITE_PATH)) {
      return JSON.parse(fs.readFileSync(PLAN_WRITE_PATH, "utf8"));
    }
  } catch {
    /* ignore */
  }
  return { ...DEFAULT_PLAN, status: "idle" };
}

function writePlanFile(plan) {
  fs.mkdirSync(OUT_DIR, { recursive: true });
  const next = { ...plan };
  delete next._path;
  fs.writeFileSync(PLAN_WRITE_PATH, JSON.stringify(next, null, 2) + "\n");
  planCache = { mtimeMs: -1, path: null, raw: null, derived: null };
  return next;
}

function clampPlanParams(body = {}) {
  const src = { ...DEFAULT_PLAN, ...readRawPlan(), ...body };
  const num = (v, fallback, min, max) => {
    const n = Number(v);
    if (!Number.isFinite(n)) return fallback;
    return Math.min(max, Math.max(min, n));
  };
  return {
    count: Math.round(num(src.count, DEFAULT_PLAN.count, 1000, 50_000_000)),
    clients: Math.round(num(src.clients, DEFAULT_PLAN.clients, 1, 512)),
    concurrency: Math.round(num(src.concurrency, DEFAULT_PLAN.concurrency, 1, 64)),
    ramp_s: Math.round(num(src.ramp_s, DEFAULT_PLAN.ramp_s, 30, 7200)),
    watch_after_s: Math.round(num(src.watch_after_s, DEFAULT_PLAN.watch_after_s, 0, 7200)),
    sample_s: num(src.sample_s, DEFAULT_PLAN.sample_s, 5, 120),
    wps_per_slot: num(src.wps_per_slot, DEFAULT_PLAN.wps_per_slot, 1, 500),
  };
}

function trafficPids() {
  const patterns = [
    "scripts/bench/traffic_sim.sh",
    "multi_client_storm",
    "geo_load_density.js",
    "run_traffic_live.sh",
  ];
  const pids = new Set();
  for (const pat of patterns) {
    try {
      const r = spawnSync("pgrep", ["-f", pat], { encoding: "utf8" });
      for (const line of (r.stdout || "").split(/\s+/)) {
        const n = parseInt(line, 10);
        if (n > 0 && n !== process.pid) pids.add(n);
      }
    } catch {
      /* ignore */
    }
  }
  return [...pids];
}

function refreshExperimentStatus() {
  if (experiment.status === "cleaning" || experiment.status === "stopping") {
    return experiment.status;
  }
  const pids = trafficPids();
  const remoteLive =
    experiment.writers?.mode === "remote" &&
    Array.isArray(experiment.writers.ips) &&
    experiment.writers.ips.length > 0;
  // Keep "running" while spawn_load is still provisioning (ips empty, starting=true).
  const remoteStarting =
    experiment.writers?.mode === "remote" && experiment.writers?.starting === true;
  if (pids.length > 0 || remoteStarting || (remoteLive && experiment.status === "running")) {
    experiment.status = "running";
    if (!experiment.pid && pids.length) experiment.pid = pids[0];
  } else if (experiment.status === "running") {
    // Remote writers: keep "running" until Stop clears writers.mode.
    if (remoteLive) {
      return experiment.status;
    }
    experiment.status = "idle";
    experiment.pid = null;
    const raw = readRawPlan();
    if (raw.status === "running") {
      writePlanFile({
        ...raw,
        status: "done",
        finished_at: new Date().toISOString(),
        finished_at_unix: Math.floor(Date.now() / 1000),
      });
    }
  } else {
    experiment.status = "idle";
  }
  return experiment.status;
}

function writersSummary() {
  const w = experiment.writers;
  const localPids = trafficPids();
  if (w?.mode === "remote" && w.starting) {
    return {
      mode: "remote",
      hosts: 0,
      ips: [],
      label: w.label || "writers: starting remote…",
      starting: true,
      local_pids: localPids,
    };
  }
  if (w?.mode === "remote" && (w.hosts > 0 || (w.ips || []).length)) {
    const n = w.hosts || (w.ips || []).length;
    return {
      mode: "remote",
      hosts: n,
      ips: w.ips || [],
      label: `writers: remote (${n} host${n === 1 ? "" : "s"})`,
      local_pids: localPids,
    };
  }
  if (localPids.length) {
    return {
      mode: "local",
      hosts: 0,
      ips: [],
      label: `writers: local (${localPids.length} pid${localPids.length === 1 ? "" : "s"})`,
      local_pids: localPids,
    };
  }
  return {
    mode: w?.mode || LOAD_MODE || "idle",
    hosts: 0,
    ips: [],
    label: "writers: none",
    local_pids: [],
  };
}

function listRemoteLoadHosts() {
  try {
    const r = spawnSync(
      "aws",
      [
        "ec2", "describe-instances",
        "--region", REGION,
        "--filters",
        `Name=tag:Name,Values=${PROJECT}-load,indexus-aws-load`,
        "Name=instance-state-name,Values=running",
        "--query", "Reservations[].Instances[].[InstanceId,PublicIpAddress]",
        "--output", "text",
      ],
      { encoding: "utf8", timeout: 12000 },
    );
    const hosts = [];
    for (const line of (r.stdout || "").trim().split("\n")) {
      const [id, ip] = line.trim().split(/\s+/);
      if (id && ip && ip !== "None") hosts.push({ id, ip });
    }
    return hosts;
  } catch {
    return [];
  }
}

function planApiPayload() {
  refreshExperimentStatus();
  const raw = readRawPlan();
  const derived = loadPlanFile();
  const params = clampPlanParams(raw);
  const writers = writersSummary();
  return {
    ...(derived?.available ? derived : { available: false }),
    params,
    raw,
    path: PLAN_WRITE_PATH,
    experiment: {
      status: experiment.status,
      pid: experiment.pid,
      out_dir: experiment.outDir,
      started_at: experiment.startedAt,
      last_error: experiment.lastError,
      last_action: experiment.lastAction,
      traffic_pids: trafficPids(),
      cleanup_steps: experiment.cleanupSteps || null,
      writers,
      load_mode_pref: LOAD_MODE,
      load_hosts_pref: LOAD_HOSTS,
    },
    defaults: DEFAULT_PLAN,
  };
}

function readBody(req) {
  return new Promise((resolve, reject) => {
    const chunks = [];
    req.on("data", (c) => chunks.push(c));
    req.on("end", () => {
      const raw = Buffer.concat(chunks).toString("utf8");
      if (!raw) return resolve({});
      try {
        resolve(JSON.parse(raw));
      } catch (e) {
        reject(e);
      }
    });
    req.on("error", reject);
  });
}

function json(res, code, body) {
  res.writeHead(code, {
    "Content-Type": "application/json",
    "Cache-Control": "no-cache",
  });
  res.end(JSON.stringify(body));
}

async function sshBootstrap(script, bootOverride) {
  const boot = bootOverride || (await resolveBootIp());
  if (!boot) throw new Error("bootstrap IP unresolved");
  const { stdout, stderr } = await execFileAsync(
    "ssh",
    [
      "-o", "StrictHostKeyChecking=no",
      "-o", "IdentitiesOnly=yes",
      "-o", "ConnectTimeout=12",
      "-i", SSH_KEY,
      `ec2-user@${boot}`,
      "bash", "-s",
    ],
    { input: script, timeout: 90000, maxBuffer: 4 * 1024 * 1024 },
  );
  return { boot, stdout: stdout || "", stderr: stderr || "" };
}

async function stopExperiment() {
  refreshExperimentStatus();
  const prev = experiment.status;
  experiment.status = "stopping";
  experiment.lastAction = "stop";
  const before = trafficPids();
  let remoteStop = null;
  if (experiment.writers?.mode === "remote" || LOAD_MODE === "remote") {
    try {
      if (fs.existsSync(STOP_LOAD)) {
        const r = spawnSync("bash", [STOP_LOAD], {
          cwd: CORE_ROOT,
          encoding: "utf8",
          timeout: 120000,
          env: { ...process.env, REGION, AWS_REGION: REGION, PROJECT, SSH_KEY },
        });
        remoteStop = {
          code: r.status,
          stdout: (r.stdout || "").slice(-500),
          stderr: (r.stderr || "").slice(-500),
        };
      }
    } catch (e) {
      remoteStop = { error: String(e.message || e) };
    }
  }
  // Careful patterns: experiment traffic only — never mesh_dash / server.js
  const patterns = [
    "scripts/bench/traffic_sim.sh",
    "multi_client_storm",
    "geo_load_density.js",
    "run_traffic_live.sh",
  ];
  for (const pat of patterns) {
    try {
      spawnSync("pkill", ["-f", pat], { encoding: "utf8" });
    } catch {
      /* ignore */
    }
  }
  await new Promise((r) => setTimeout(r, 400));
  for (const pat of patterns) {
    try {
      spawnSync("pkill", ["-9", "-f", pat], { encoding: "utf8" });
    } catch {
      /* ignore */
    }
  }
  const after = trafficPids();
  const raw = readRawPlan();
  writePlanFile({
    ...raw,
    ...clampPlanParams(raw),
    status: "idle",
    stopped_at: new Date().toISOString(),
    stopped_at_unix: Math.floor(Date.now() / 1000),
  });
  // Don't clobber an in-flight background cleanup status
  experiment.status = prev === "cleaning" ? "cleaning" : "idle";
  experiment.pid = null;
  experiment.lastError = null;
  experiment.writers = { mode: "idle", hosts: 0, ips: [], label: "writers: none" };
  return { killed_before: before, still_running: after, remote_stop: remoteStop };
}

function newCollectionName() {
  const stamp = Date.now().toString(36).slice(-6);
  return (`ContRamp${stamp}XXXXXXXX`).slice(0, 16);
}

async function fastWipeBootstrap(bootOverride) {
  const boot = bootOverride || (await resolveBootIp());
  if (!boot) throw new Error("bootstrap IP unresolved");
  // Minimal downtime: stop → wipe WAL → start. No long sleeps.
  const remote = await sshBootstrap(`set -euxo pipefail
sudo systemctl stop indexus-node || true
sudo rm -f /var/lib/indexus/backup.logs /var/lib/indexus/backup.snapshot
sudo rm -rf /var/lib/indexus/leave/* /var/lib/indexus/archive/* 2>/dev/null || true
sudo mkdir -p /var/lib/indexus/backup /var/lib/indexus/leave /var/lib/indexus/archive
sudo systemctl start indexus-issuer || true
sudo systemctl start indexus-node
for i in 1 2 3 4 5 6 7 8; do
  if curl -sf --max-time 2 http://127.0.0.1:19000/status >/tmp/st.json; then
    python3 -c 'import json; d=json.load(open("/tmp/st.json")); print(d.get("items"), d.get("name"))'
    exit 0
  fi
  sleep 1
done
exit 1
`, boot);
  return { boot: remote.boot || boot, out: remote.stdout.trim().slice(-300) };
}

async function fireTerminateSpawned() {
  const { stdout } = await execFileAsync(
    "aws",
    [
      "ec2", "describe-instances",
      "--region", REGION,
      "--filters",
      `Name=tag:Name,Values=${PROJECT}-spawned`,
      "Name=instance-state-name,Values=pending,running",
      "--query", "Reservations[].Instances[].InstanceId",
      "--output", "text",
    ],
    { timeout: 20000 },
  );
  const ids = (stdout || "").trim().split(/\s+/).filter((id) => id && id !== "None");
  if (ids.length) {
    // Fire-and-forget terminate — do not wait for instance state
    spawn("aws", ["ec2", "terminate-instances", "--region", REGION, "--instance-ids", ...ids], {
      detached: true,
      stdio: "ignore",
    }).unref();
  }
  return ids;
}

async function fireClearSnapshots() {
  const bucket = resolveBucket();
  if (!bucket) return { skipped: "no bucket" };
  spawn(
    "aws",
    ["s3", "rm", `s3://${bucket}/snapshots/`, "--recursive", "--region", REGION],
    { detached: true, stdio: "ignore" },
  ).unref();
  return { bucket, async: true };
}

async function startExperiment(body = {}) {
  refreshExperimentStatus();
  const remoteStill = experiment.writers?.mode === "remote" && (experiment.writers.ips || []).length;
  if (experiment.status === "running" || trafficPids().length > 0 || remoteStill) {
    const err = new Error("experiment already running — stop first");
    err.code = 409;
    throw err;
  }
  // Start is allowed while cleanup is still finishing in the background.
  const params = clampPlanParams(body);
  if (params.ramp_s < 60) {
    const err = new Error("ramp_s must be ≥ 60s for a continuous ramp (not a peak dump)");
    err.code = 400;
    throw err;
  }

  // Optional boot override (fresh bootstrap / warm standby). New collection always.
  if (body.boot_ip && typeof body.boot_ip === "string" && body.boot_ip.trim()) {
    bootIp = body.boot_ip.trim();
  }
  const boot = bootIp || (await resolveBootIp());
  if (!boot) throw new Error("bootstrap IP unresolved");

  const wipe = body.wipe !== false; // default: fast wipe before start
  let wipeResult = null;
  if (wipe) {
    try {
      wipeResult = await fastWipeBootstrap(boot);
    } catch (e) {
      // Continue — new collection still isolates logical data; warn caller
      wipeResult = { error: String(e.message || e) };
    }
  }

  const issuer = `http://${boot}:22000`;
  const collection = (body.collection && String(body.collection).slice(0, 16)) || newCollectionName();
  const d = new Date();
  const p2 = (n) => String(n).padStart(2, "0");
  const stamp = `${d.getFullYear()}${p2(d.getMonth() + 1)}${p2(d.getDate())}-${p2(d.getHours())}${p2(d.getMinutes())}${p2(d.getSeconds())}`;
  const outDir = path.join(OUT_DIR, `traffic-live-${stamp}`);
  fs.mkdirSync(outDir, { recursive: true });
  fs.mkdirSync(OUT_DIR, { recursive: true });

  const nowUnix = Math.floor(Date.now() / 1000);
  const wantRemote =
    (body.load_mode || LOAD_MODE) !== "local" && fs.existsSync(SPAWN_LOAD);
  let writers = null;
  let child = null;

  const plan = writePlanFile({
    ...params,
    collection,
    boot_ip: boot,
    issuer,
    seed: `http://${boot}:21000`,
    out: outDir,
    started_at: nowUnix,
    started_at_unix: nowUnix,
    started_at_iso: new Date(nowUnix * 1000).toISOString(),
    status: "running",
    mode: "continuous_ramp",
    writers_mode: wantRemote ? "remote" : "local",
  });

  const logFd = fs.openSync(TRAFFIC_LOG, "a");
  fs.writeSync(
    logFd,
    `\n==== dash start ${new Date().toISOString()} out=${outDir} collection=${collection} mode=${wantRemote ? "remote" : "local"} ====\n`,
  );

  if (wantRemote) {
    fs.writeSync(logFd, `spawning remote load hosts=${LOAD_HOSTS}… (async)\n`);
    writers = {
      mode: "remote",
      hosts: 0,
      ips: [],
      label: `writers: starting remote (${body.load_hosts || LOAD_HOSTS} hosts)…`,
      starting: true,
    };
    experiment = {
      status: "running",
      pid: null,
      outDir,
      startedAt: nowUnix,
      lastError: null,
      lastAction: "start",
      cleanupGeneration: experiment.cleanupGeneration,
      cleanupSteps: experiment.cleanupSteps,
      writers,
    };
    // Do not block the HTTP / poll loop — spawn_load can take minutes (EC2 + SSH).
    const loadEnv = {
      ...process.env,
      COUNT: String(params.count),
      CLIENTS: String(params.clients),
      CONCURRENCY: String(params.concurrency),
      RAMP_S: String(params.ramp_s),
      COLLECTION: collection,
      BOOT_IP: boot,
      ISSUER: issuer,
      LOAD_HOSTS: String(body.load_hosts || LOAD_HOSTS),
      REGION,
      AWS_REGION: REGION,
      PROJECT,
      ARTIFACTS_BUCKET: artifactsBucket || resolveBucket() || "",
      SSH_KEY,
    };
    // Detach so a dash restart does not SIGTERM mid-provision / mid-storm.
    const childLoad = spawn("bash", [SPAWN_LOAD], {
      cwd: CORE_ROOT,
      env: loadEnv,
      stdio: ["ignore", "pipe", "pipe"],
      detached: true,
    });
    childLoad.unref();
    let loadOut = "";
    let loadErr = "";
    childLoad.stdout?.on("data", (c) => {
      const s = c.toString();
      loadOut += s;
      try {
        fs.writeSync(logFd, s);
      } catch {
        /* logFd may already be closed on late chunks */
      }
    });
    childLoad.stderr?.on("data", (c) => {
      const s = c.toString();
      loadErr += s;
      try {
        fs.writeSync(logFd, s);
      } catch {
        /* ignore */
      }
    });
    const startGeneration = nowUnix;
    childLoad.on("close", (code) => {
      let parsed = null;
      try {
        const lines = loadOut.trim().split("\n");
        for (let i = lines.length - 1; i >= 0; i--) {
          if (lines[i].trim().startsWith("{")) {
            parsed = JSON.parse(lines.slice(i).join("\n"));
            break;
          }
        }
      } catch {
        /* ignore */
      }
      // Only update if this start is still the active experiment.
      if (experiment.startedAt !== startGeneration || experiment.status !== "running") {
        try {
          fs.closeSync(logFd);
        } catch {
          /* ignore */
        }
        return;
      }
      if (code !== 0 || !parsed?.ips?.length) {
        try {
          fs.writeSync(
            logFd,
            `\nremote load failed (code=${code}); falling back to local traffic_sim\n`,
          );
        } catch {
          /* ignore */
        }
        try {
          fs.closeSync(logFd);
        } catch {
          /* ignore */
        }
        // Fallback local writers for this same plan/outDir.
        if (!fs.existsSync(TRAFFIC_SIM)) {
          experiment.lastError = `remote load failed (code=${code}); no traffic_sim for fallback`;
          experiment.writers = {
            mode: "local",
            hosts: 0,
            ips: [],
            label: "writers: none (remote failed)",
            remote_error: (loadErr || loadOut || "").slice(-400),
          };
          return;
        }
        const localFd = fs.openSync(TRAFFIC_LOG, "a");
        const child = spawn(TRAFFIC_SIM, [], {
          cwd: CORE_ROOT,
          env: {
            ...process.env,
            COUNT: String(params.count),
            CLIENTS: String(params.clients),
            CONCURRENCY: String(params.concurrency),
            RAMP_S: String(params.ramp_s),
            WATCH_AFTER_S: String(params.watch_after_s),
            SAMPLE_S: String(params.sample_s),
            COLLECTION: collection,
            BOOT_IP: boot,
            ISSUER: issuer,
            OUT: outDir,
          },
          detached: true,
          stdio: ["ignore", localFd, localFd],
        });
        fs.closeSync(localFd);
        child.unref();
        experiment.pid = child.pid;
        experiment.writers = {
          mode: "local",
          hosts: 0,
          ips: [],
          label: "writers: local (fallback)",
          remote_error: (loadErr || loadOut || "").slice(-400),
        };
        child.on("exit", (exitCode, signal) => {
          if (experiment.pid === child.pid) {
            experiment.pid = null;
            if (experiment.status === "running") experiment.status = "idle";
            if (exitCode && exitCode !== 0) {
              experiment.lastError = `traffic_sim exited code=${exitCode} signal=${signal || ""}`;
            }
          }
        });
        return;
      }
      try {
        fs.closeSync(logFd);
      } catch {
        /* ignore */
      }
      experiment.writers = {
        mode: "remote",
        hosts: parsed.hosts || parsed.ips.length,
        ips: parsed.ips,
        label: `writers: remote (${parsed.ips.length} hosts)`,
      };
      experiment.lastError = null;
    });

    return {
      async: true,
      plan,
      pid: null,
      out: outDir,
      log: TRAFFIC_LOG,
      collection,
      boot_ip: boot,
      wipe: wipeResult,
      writers,
      note: "Remote load hosts provisioning in background — dashboard stays responsive.",
    };
  }

  if (!fs.existsSync(TRAFFIC_SIM)) {
    fs.closeSync(logFd);
    throw new Error(`missing traffic_sim: ${TRAFFIC_SIM}`);
  }
  child = spawn(
    TRAFFIC_SIM,
    [],
    {
      cwd: CORE_ROOT,
      env: {
        ...process.env,
        COUNT: String(params.count),
        CLIENTS: String(params.clients),
        CONCURRENCY: String(params.concurrency),
        RAMP_S: String(params.ramp_s),
        WATCH_AFTER_S: String(params.watch_after_s),
        SAMPLE_S: String(params.sample_s),
        COLLECTION: collection,
        BOOT_IP: boot,
        ISSUER: issuer,
        OUT: outDir,
      },
      detached: true,
      stdio: ["ignore", logFd, logFd],
    },
  );
  fs.closeSync(logFd);
  child.unref();

  writers = writers || {
    mode: "local",
    hosts: 0,
    ips: [],
    label: "writers: local (traffic_sim pids)",
  };
  experiment = {
    status: "running",
    pid: child.pid,
    outDir,
    startedAt: nowUnix,
    lastError: null,
    lastAction: "start",
    cleanupGeneration: experiment.cleanupGeneration,
    cleanupSteps: experiment.cleanupSteps,
    writers,
  };
  child.on("exit", (code, signal) => {
    if (experiment.pid === child.pid) {
      experiment.pid = null;
      if (experiment.status === "running") experiment.status = "idle";
      if (code && code !== 0) {
        experiment.lastError = `traffic_sim exited code=${code} signal=${signal || ""}`;
      }
    }
  });

  return {
    plan,
    pid: child.pid,
    out: outDir,
    log: TRAFFIC_LOG,
    collection,
    boot_ip: boot,
    wipe: wipeResult,
    writers,
    note: writers.mode === "local"
      ? "Local writers on the dashboard host — set LOAD_MODE=remote for EC2 load VMs."
      : "Cleanup (if any) may still run in background; this start uses a new collection.",
  };
}

/** Background lab reset — returns immediately; work continues async. */
function beginCleanupLab(opts = {}) {
  if (experiment.status === "cleaning") {
    return { ok: true, async: true, already: true, experiment: planApiPayload().experiment };
  }
  const generation = Date.now();
  experiment.status = "cleaning";
  experiment.lastAction = "cleanup";
  experiment.cleanupGeneration = generation;
  experiment.cleanupSteps = [{ step: "queued", at: new Date().toISOString() }];

  // Kick off without awaiting — HTTP returns ASAP
  (async () => {
    const steps = experiment.cleanupSteps;
    try {
      // 1) Stop traffic fast (skip if Start already took over)
      if (experiment.status === "cleaning") {
        try {
          const stop = await stopExperiment();
          // stopExperiment may set idle; restore cleaning if still our generation
          if (experiment.cleanupGeneration === generation) experiment.status = "cleaning";
          steps.push({ step: "stop_traffic", ...stop });
        } catch (e) {
          steps.push({ step: "stop_traffic", error: String(e.message || e) });
        }
      }

      // 2) Fire terminate — do not wait for EC2 state
      try {
        const ids = await fireTerminateSpawned();
        steps.push({ step: "terminate_spawned", ids, async: true });
      } catch (e) {
        steps.push({ step: "terminate_spawned", error: String(e.message || e) });
      }

      // 3) Fire S3 clear — do not block
      try {
        const s3 = await fireClearSnapshots();
        steps.push({ step: "clear_s3_snapshots", ...s3 });
      } catch (e) {
        steps.push({ step: "clear_s3_snapshots", error: String(e.message || e) });
      }

      // 4) Fast WAL wipe only if no experiment started meanwhile
      if (experiment.cleanupGeneration === generation && experiment.status !== "running" && trafficPids().length === 0) {
        try {
          const wipe = await fastWipeBootstrap();
          steps.push({ step: "wipe_bootstrap", ...wipe });
        } catch (e) {
          steps.push({ step: "wipe_bootstrap", error: String(e.message || e) });
        }
      } else {
        steps.push({
          step: "wipe_bootstrap",
          skipped: "experiment started during cleanup — use new collection / Start wipe",
        });
      }

      if (experiment.cleanupGeneration === generation && experiment.status !== "running") {
        const boot = await resolveBootIp();
        const params = clampPlanParams(readRawPlan());
        writePlanFile({
          ...params,
          boot_ip: boot || undefined,
          issuer: boot ? `http://${boot}:22000` : undefined,
          seed: boot ? `http://${boot}:21000` : undefined,
          status: "idle",
          mode: "continuous_ramp",
          cleaned_at: new Date().toISOString(),
        });
        history.length = 0;
        prevSample = null;
        experiment.status = "idle";
        experiment.pid = null;
        experiment.lastError = null;
        experiment.lastAction = "cleanup";
      }
      steps.push({ step: "done", at: new Date().toISOString() });
    } catch (e) {
      steps.push({ step: "fatal", error: String(e.message || e) });
      if (experiment.cleanupGeneration === generation && experiment.status === "cleaning") {
        experiment.status = "idle";
        experiment.lastError = String(e.message || e);
      }
    }
  })();

  return {
    ok: true,
    async: true,
    message: "Cleanup runs in background; Start can use a new collection / boot IP without waiting.",
    experiment: planApiPayload().experiment,
  };
}

async function sampleMesh() {
  const boot = await resolveBootIp();
  if (!boot) {
    throw new Error("BOOT_IP not set and could not resolve from terraform/EC2");
  }

  const [instResult, snapshots] = await Promise.all([
    listSpawnedInstances(),
    listSnapshots(resolveBucket()),
  ]);

  const instances = Array.isArray(instResult) ? instResult : [];
  const awsError = instResult?._error || null;

  const byIp = new Map();
  for (const inst of instances) {
    if (inst.Ip) byIp.set(inst.Ip, inst);
  }

  const ips = [boot];
  for (const inst of instances) {
    if (inst.Ip && !ips.includes(inst.Ip)) ips.push(inst.Ip);
  }

  const statuses = await Promise.all(
    ips.map((ip) => fetchJson(`http://${ip}:${MON_PORT}/status`)),
  );

  const nodes = ips.map((ip, i) =>
    nodeFromStatus(ip, statuses[i], byIp.get(ip) || (ip === boot ? { Name: `${PROJECT}-bootstrap` } : null)),
  );

  // Mark bootstrap role if missing
  if (nodes[0] && nodes[0].up && !nodes[0].role) nodes[0].role = "bootstrap";
  if (nodes[0]) nodes[0].is_boot = true;

  const upNodes = nodes.filter((n) => n.up);
  const insertsWindow = upNodes.reduce((s, n) => s + (n.inserts_window || 0), 0);
  const itemsSum = upNodes.reduce((s, n) => s + (n.items || 0), 0);
  const queueSum = upNodes.reduce((s, n) => s + (n.queue || 0), 0);
  const scaleUps = upNodes.reduce((s, n) => s + (n.scale_ups_done || 0), 0);

  const now = Date.now();
  let itemsPerSec = null;
  if (prevSample && prevSample.ts_ms && prevSample.items_sum != null) {
    const dt = (now - prevSample.ts_ms) / 1000;
    if (dt > 0.5) {
      itemsPerSec = Math.round(((itemsSum - prevSample.items_sum) / dt) * 10) / 10;
    }
  }

  const pending = instances.filter((i) => i.State === "pending").length;
  const running = instances.filter((i) => i.State === "running").length;

  // Prefer bootstrap status for threshold config; fall back to any up node
  let autoscale = extractAutoscale(statuses[0]);
  if (autoscale.mem_limit_pct == null) {
    for (let i = 1; i < statuses.length; i++) {
      const cand = extractAutoscale(statuses[i]);
      if (cand.mem_limit_pct != null || cand.cpu_limit_pct != null) {
        autoscale = cand;
        break;
      }
    }
  }

  const windowSec =
    parseDurationSeconds(autoscale.window) ||
    parseDurationSeconds((statuses.find((s) => s?.autoscale?.window)?.autoscale || {}).window) ||
    60;
  const writeRps =
    insertsWindow > 0 && windowSec > 0
      ? Math.round((insertsWindow / windowSec) * 10) / 10
      : itemsPerSec;
  const throughput = {
    inserts_window: insertsWindow,
    items_per_sec: itemsPerSec,
    window_s: windowSec,
    write_rps: writeRps,
    rps_hint: writeRps,
  };

  const sample = {
    ts: new Date(now).toISOString(),
    ts_ms: now,
    boot_ip: boot,
    region: REGION,
    project: PROJECT,
    mon_port: MON_PORT,
    poll_ms: POLL_MS,
    history_ms: HISTORY_MS,
    throughput,
    autoscale,
    totals: {
      answering: upNodes.length,
      known: nodes.length,
      items: itemsSum,
      queue: queueSum,
      scale_ups: scaleUps,
      zones: upNodes.reduce((s, n) => s + (n.zones || 0), 0),
      snap_zones: upNodes.reduce((s, n) => s + (n.snap_zones || 0), 0),
      snap_dirty: upNodes.reduce((s, n) => s + (n.snap_dirty || 0), 0),
      wal_segments: upNodes.reduce((s, n) => s + (n.wal_segments || 0), 0),
      deleg_in: upNodes.reduce((s, n) => s + (n.deleg_in || 0), 0),
      deleg_out: upNodes.reduce((s, n) => s + (n.deleg_out || 0), 0),
      store_nodes: upNodes.filter((n) => n.store).length,
      mem_max: maxOf(upNodes.map((n) => n.mem_pct)),
      cpu_max: maxOf(upNodes.map((n) => n.cpu_pct)),
      mem_projected_max: maxOf(upNodes.map((n) => n.mem_projected)),
    },
    instances: {
      pending,
      running,
      total: instances.length,
      list: instances,
      error: awsError,
    },
    snapshots,
    nodes,
    history: [], // filled after push so client gets including this sample
  };

  prevSample = { ts_ms: now, items_sum: itemsSum };
  pushHistory(sample);
  sample.history = history.slice();
  sample.plan = loadPlanFile();
  return sample;
}

async function pollLoop() {
  try {
    cache = await sampleMesh();
    pollError = null;
  } catch (e) {
    pollError = String(e.message || e);
    if (cache) {
      cache.error = pollError;
      cache.ts = new Date().toISOString();
    }
  }
  setTimeout(pollLoop, POLL_MS);
}

const MIME = {
  ".html": "text/html; charset=utf-8",
  ".js": "application/javascript; charset=utf-8",
  ".css": "text/css; charset=utf-8",
  ".svg": "image/svg+xml",
  ".ico": "image/x-icon",
};

function serveStatic(req, res) {
  let urlPath = decodeURIComponent((req.url || "/").split("?")[0]);
  if (urlPath === "/") urlPath = "/index.html";
  const file = path.normalize(path.join(PUBLIC, urlPath));
  if (!file.startsWith(PUBLIC)) {
    res.writeHead(403);
    res.end("forbidden");
    return;
  }
  fs.readFile(file, (err, data) => {
    if (err) {
      res.writeHead(404);
      res.end("not found");
      return;
    }
    const ext = path.extname(file);
    const headers = {
      "Content-Type": MIME[ext] || "application/octet-stream",
      // Always revalidate so UI edits show up on refresh without server restart.
      "Cache-Control": "no-cache, must-revalidate",
    };
    res.writeHead(200, headers);
    res.end(data);
  });
}

const server = http.createServer(async (req, res) => {
  res.setHeader("Access-Control-Allow-Origin", "*");
  res.setHeader("Access-Control-Allow-Methods", "GET, POST, OPTIONS");
  res.setHeader("Access-Control-Allow-Headers", "Content-Type");
  if (req.method === "OPTIONS") {
    res.writeHead(204);
    res.end();
    return;
  }

  const url = (req.url || "/").split("?")[0];

  try {
    if (url === "/api/mesh" && req.method === "GET") {
      refreshExperimentStatus();
      const body = cache
        ? {
            ...cache,
            error: pollError || cache.error || null,
            history: history.slice(),
            plan: loadPlanFile(),
            experiment: planApiPayload().experiment,
          }
        : {
            ts: new Date().toISOString(),
            boot_ip: bootIp || null,
            error: pollError || "warming up…",
            throughput: { inserts_window: 0, items_per_sec: null, rps_hint: null },
            totals: { answering: 0, known: 0, items: 0, queue: 0, scale_ups: 0, zones: 0 },
            autoscale: {},
            history: history.slice(),
            history_ms: HISTORY_MS,
            plan: loadPlanFile(),
            experiment: planApiPayload().experiment,
            instances: { pending: 0, running: 0, total: 0, list: [] },
            snapshots: { available: false, objects: [] },
            nodes: [],
          };
      return json(res, 200, body);
    }

    if (url === "/api/plan" && req.method === "GET") {
      return json(res, 200, planApiPayload());
    }

    if (url === "/api/plan" && req.method === "POST") {
      const body = await readBody(req);
      const raw = readRawPlan();
      const params = clampPlanParams({ ...raw, ...body });
      // Updating curve while idle (or for next start). Keep status unless starting.
      const next = writePlanFile({
        ...raw,
        ...params,
        status: experiment.status === "running" || raw.status === "running"
          ? raw.status || "running"
          : "idle",
        mode: "continuous_ramp",
        updated_at: new Date().toISOString(),
      });
      experiment.lastAction = "update_plan";
      return json(res, 200, { ok: true, plan: planApiPayload(), written: next });
    }

    if (url === "/api/experiment/start" && req.method === "POST") {
      const body = await readBody(req);
      const result = await startExperiment(body);
      const code = result.async ? 202 : 200;
      return json(res, code, { ok: true, ...result, experiment: planApiPayload().experiment });
    }

    if (url === "/api/experiment/stop" && req.method === "POST") {
      const result = await stopExperiment();
      return json(res, 200, { ok: true, ...result, experiment: planApiPayload().experiment });
    }

    if (url === "/api/experiment/cleanup" && req.method === "POST") {
      const result = beginCleanupLab();
      return json(res, 202, result);
    }

    if (url === "/api/export" && req.method === "GET") {
      refreshExperimentStatus();
      const payload = {
        exported_at: new Date().toISOString(),
        boot_ip: bootIp || null,
        region: REGION,
        project: PROJECT,
        history: history.slice(),
        mesh: cache,
        plan: planApiPayload(),
        experiment: planApiPayload().experiment,
        nodes: cache?.nodes || [],
        instances: cache?.instances || null,
        snapshots: cache?.snapshots || null,
        autoscale: cache?.autoscale || null,
      };
      res.writeHead(200, {
        "Content-Type": "application/json",
        "Content-Disposition": `attachment; filename="mesh-export-${Date.now()}.json"`,
        "Cache-Control": "no-store",
      });
      res.end(JSON.stringify(payload, null, 2));
      return;
    }

    if (url === "/api/health" && req.method === "GET") {
      return json(res, 200, {
        ok: true,
        boot_ip: bootIp || null,
        has_cache: !!cache,
        history_points: history.length,
        plan: !!(loadPlanFile()?.available),
        experiment: planApiPayload().experiment,
      });
    }
  } catch (e) {
    const code = e.code === 409 ? 409 : e.code === 400 ? 400 : 500;
    return json(res, typeof code === "number" ? code : 500, {
      ok: false,
      error: String(e.message || e),
    });
  }

  if (req.method !== "GET" && req.method !== "HEAD") {
    return json(res, 404, { error: "not found" });
  }

  serveStatic(req, res);
});

server.listen(PORT, "127.0.0.1", async () => {
  await resolveBootIp();
  resolveBucket();
  fs.mkdirSync(OUT_DIR, { recursive: true });
  if (!fs.existsSync(PLAN_WRITE_PATH)) {
    writePlanFile({ ...DEFAULT_PLAN, status: "idle", mode: "continuous_ramp" });
  }
  refreshExperimentStatus();
  console.log(`Indexus mesh dash → http://127.0.0.1:${PORT}/`);
  console.log(`  BOOT_IP=${bootIp || "(resolving…)"}  REGION=${REGION}  PROJECT=${PROJECT}`);
  console.log(`  poll every ${POLL_MS}ms  history ${Math.round(HISTORY_MS / 60000)}m  mon :${MON_PORT}`);
  console.log(`  plan ${PLAN_WRITE_PATH}`);
  console.log(`  experiment controls: POST /api/experiment/{start,stop,cleanup} (localhost only)`);
  if (artifactsBucket) console.log(`  snapshots bucket=${artifactsBucket}`);
  pollLoop();
});
