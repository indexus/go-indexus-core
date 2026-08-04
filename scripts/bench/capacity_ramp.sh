#!/usr/bin/env bash
# Capacity / distribution / consistency experiment.
# One instrumented ramp — no topology scripting, no wait-for-N-nodes.
# After traffic settles: consistency table, WAL/snapshot check, optional restart
# durability probe. SDK smoke is a separate step (see sdk_smoke.mjs).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
BENCH="$ROOT/scripts/bench"
REGION="${AWS_REGION:-eu-west-3}"
PROJECT="${PROJECT:-indexus-aws}"
OUT="${OUT:-$BENCH/out/capacity-$(date +%Y%m%d-%H%M%S)}"
[[ "$OUT" = /* ]] || OUT="$ROOT/$OUT"
mkdir -p "$OUT"

# Enough volume + ramp to force distribution and likely ≥1 scale-up on t3.micro,
# without dumping all clients at t=0.
COUNT="${COUNT:-350000}"
CLIENTS="${CLIENTS:-16}"
CONCURRENCY="${CONCURRENCY:-3}"
RAMP_S="${RAMP_S:-420}"
WATCH_AFTER_S="${WATCH_AFTER_S:-240}"
SAMPLE_S="${SAMPLE_S:-15}"
BOOT_IP="${BOOT_IP:-15.237.219.50}"

export BOOT_IP COUNT CLIENTS CONCURRENCY RAMP_S WATCH_AFTER_S SAMPLE_S OUT
export ISSUER="${ISSUER:-http://${BOOT_IP}:22000}"

echo "==> capacity ramp → $OUT"
echo "==> COUNT=$COUNT CLIENTS=$CLIENTS CONCURRENCY=$CONCURRENCY RAMP_S=$RAMP_S WATCH=$WATCH_AFTER_S"

# Ensure issuer reachable before spending the ramp budget.
if ! curl -sf --max-time 5 "${ISSUER}/health" >/dev/null; then
  echo "issuer not healthy at $ISSUER" >&2
  exit 1
fi

"$BENCH/traffic_sim.sh" 2>&1 | tee "$OUT/capacity_live.log"

# --- post verification (mesh-wide) ---
python3 - "$OUT" "$BOOT_IP" "$REGION" "$PROJECT" <<'PY' | tee "$OUT/VERIFY.json"
import json, subprocess, sys, urllib.request, time

out, boot, region, project = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4]

def get(url, timeout=5):
    try:
        with urllib.request.urlopen(url, timeout=timeout) as r:
            return json.load(r)
    except Exception as e:
        return {"_error": str(e)}

def spawned_ips():
    try:
        raw = subprocess.run([
            "aws", "ec2", "describe-instances", "--region", region,
            "--filters", f"Name=tag:Name,Values={project}-spawned",
            "Name=instance-state-name,Values=pending,running",
            "--query", "Reservations[].Instances[].PublicIpAddress",
            "--output", "json",
        ], capture_output=True, text=True, timeout=25)
        return [ip for ip in json.loads(raw.stdout or "[]") if ip]
    except Exception:
        return []

ips = [boot] + spawned_ips()
nodes = []
for ip in ips:
    st = get(f"http://{ip}:19000/status")
    ing = get(f"http://{ip}:19000/ingress")
    cnt = get(f"http://{ip}:19000/count")
    a = (st or {}).get("autoscale") or {}
    p = a.get("pressure") or {}
    nodes.append({
        "ip": ip,
        "up": "_error" not in st,
        "items": st.get("items") if isinstance(st, dict) else None,
        "count": cnt.get("count") if isinstance(cnt, dict) else None,
        "queue": st.get("queue") if isinstance(st, dict) else None,
        "peers": st.get("peers"),
        "zones": st.get("zones"),
        "mem_pct": p.get("mem_pct"),
        "cpu_pct": p.get("cpu_pct"),
        "reason": a.get("last_reason"),
        "ups": a.get("scale_ups_done"),
        "admit_blocked": a.get("admit_blocked"),
        "ingress": ing if isinstance(ing, dict) and "_error" not in ing else None,
    })

# traffic summary
def last_object(path):
    try:
        text = open(path).read()
    except Exception:
        return {}
    objs, buf, depth = [], "", 0
    for ch in text:
        if ch == "{":
            depth += 1
        if depth:
            buf += ch
        if ch == "}":
            depth -= 1
            if depth == 0:
                try:
                    objs.append(json.loads(buf))
                except Exception:
                    pass
                buf = ""
    return objs[-1] if objs else {}

traffic = last_object(f"{out}/traffic.json")
samples = []
try:
    for line in open(f"{out}/mesh_trace.ndjson"):
        samples.append(json.loads(line))
except Exception:
        pass

scale_events = []
prev_ups = 0
for s in samples:
    ups = s.get("scale_ups") or 0
    if ups > prev_ups:
        for n in s.get("nodes") or []:
            if n.get("reason"):
                scale_events.append({
                    "t": s.get("t"),
                    "ip": n.get("ip"),
                    "reason": n.get("reason"),
                    "mem_pct": n.get("mem_pct"),
                    "cpu_pct": n.get("cpu_pct"),
                    "ups": n.get("ups"),
                })
        prev_ups = ups

sum_items = sum(n.get("items") or 0 for n in nodes if n.get("up"))
sum_count = sum(n.get("count") or 0 for n in nodes if n.get("up") and n.get("count") is not None)
sum_queue = sum(n.get("queue") or 0 for n in nodes if n.get("up"))
ok = traffic.get("ok") or 0
fail = traffic.get("fail") or 0
hops = traffic.get("hop_distribution") or {}
total_hops = sum(hops.values()) or 1
spread = sorted(((v / total_hops, k) for k, v in hops.items()), reverse=True)

report = {
    "bootstrap_ip": boot,
    "answering_nodes": sum(1 for n in nodes if n.get("up")),
    "spawned_ips": [ip for ip in ips if ip != boot],
    "writes_ok": ok,
    "writes_fail": fail,
    "rps": traffic.get("rps"),
    "hop_distribution": hops,
    "busiest_node_share": round(spread[0][0], 4) if spread else None,
    "nodes_written_to": len(hops),
    "sum_items_status": sum_items,
    "sum_count": sum_count,
    "queue_total": sum_queue,
    "gap_ok_minus_items": ok - sum_items,
    "gap_note": (
        "ok is client ACKs (ingress accept); items are applied ownership counts. "
        "Gap ≈ in-flight queue + items still forwarding + any 503 retries that later succeeded counted once in ok. "
        "After settle, queue→0 and sum(items) should approach ok (single-owner items, no replication)."
    ),
    "scale_events": scale_events,
    "peak_nodes": max((s.get("answering") or 0 for s in samples), default=0),
    "peak_queue": max((s.get("queue_total") or 0 for s in samples), default=0),
    "nodes": nodes,
}
print(json.dumps(report, indent=2))
PY

echo "==> VERIFY written"
