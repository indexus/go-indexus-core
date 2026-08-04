#!/usr/bin/env bash
# Simulate traffic against a live mesh and watch what the mesh does with it.
#
# The harness sends writes and records. It never asks for a node, never waits
# for one, never restarts anything: growing and shrinking is the mesh's own
# decision, taken by whichever node finds itself under pressure. If the load
# never produces a backlog, nothing should spawn — that is a result, not a
# failure of the run.
#
# Clients get one seed address and find the rest of the mesh themselves over
# the p2p port, then send each write to the node XOR-nearest the item key.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
BENCH="$ROOT/scripts/bench"
TF_DIR="$ROOT/deploy/terraform"
REGION="${AWS_REGION:-eu-west-3}"
OUT="${OUT:-$BENCH/out/traffic-$(date +%Y%m%d-%H%M%S)}"
# Absolute path: we later cd into terraform, and plan/observer writes must still land.
[[ "$OUT" = /* ]] || OUT="$(pwd)/$OUT"
mkdir -p "$OUT"

COUNT="${COUNT:-800000}"
CLIENTS="${CLIENTS:-24}"
CONCURRENCY="${CONCURRENCY:-2}"
# Clients join over RAMP_S so the load grows the way real traffic does.
RAMP_S="${RAMP_S:-150}"
PARALLEL="${PARALLEL:-$CLIENTS}"
# Keep watching after the traffic stops: scale-down is part of the behaviour.
WATCH_AFTER_S="${WATCH_AFTER_S:-300}"
SAMPLE_S="${SAMPLE_S:-15}"
COLLECTION="${COLLECTION:-Traffic$(date +%s | tail -c 6)}"
COLLECTION="$(python3 -c "c='$COLLECTION'; print((c+'XXXXXXXXXXXXXXXX')[:16])")"
PROJECT="${PROJECT:-indexus-aws}"

CSV="${DENSITY_CSV:-$BENCH/data/world_density_100k.csv}"
if [[ ! -f "$CSV" ]]; then
  echo "missing density csv: $CSV" >&2
  exit 1
fi

cd "$TF_DIR"
ISSUER_TF=$(terraform output -raw issuer_url 2>/dev/null || true)
BOOT_IP_TF=$(terraform output -raw bootstrap_public_ip 2>/dev/null || true)

# Prefer a live answering bootstrap over a possibly stale terraform IP
# (EIP/public IP can move while terraform output lags).
BOOT_IP="${BOOT_IP:-}"
if [[ -z "$BOOT_IP" ]]; then
  for cand in "$BOOT_IP_TF"; do
    [[ -z "$cand" || "$cand" == "None" ]] && continue
    if curl -sf --max-time 3 "http://${cand}:19000/status" >/dev/null 2>&1; then
      BOOT_IP="$cand"
      break
    fi
  done
fi
if [[ -z "$BOOT_IP" ]]; then
  BOOT_IP=$(aws ec2 describe-instances --region "$REGION" \
    --filters "Name=tag:Name,Values=${PROJECT}-bootstrap" \
      "Name=instance-state-name,Values=running" \
    --query 'Reservations[0].Instances[0].PublicIpAddress' --output text 2>/dev/null || true)
fi
if [[ -z "$BOOT_IP" || "$BOOT_IP" == "None" ]]; then
  echo "could not resolve bootstrap public IP" >&2
  exit 1
fi

# Issuer co-lives on bootstrap in this deploy; terraform issuer_url may still
# point at an old public IP after a reboot/IP change.
ISSUER="${ISSUER:-}"
if [[ -z "$ISSUER" ]]; then
  if curl -sf --max-time 3 "http://${BOOT_IP}:22000/health" >/dev/null 2>&1; then
    ISSUER="http://${BOOT_IP}:22000"
  elif [[ -n "$ISSUER_TF" ]] && curl -sf --max-time 3 "${ISSUER_TF}/health" >/dev/null 2>&1; then
    ISSUER="$ISSUER_TF"
  else
    ISSUER="http://${BOOT_IP}:22000"
  fi
fi
SEED="http://${BOOT_IP}:21000"

echo "==> out=$OUT"
echo "==> $COUNT writes from $CLIENTS clients over a ${RAMP_S}s ramp, collection=$COLLECTION"
echo "==> seed=$SEED issuer=$ISSUER (tf boot=$BOOT_IP_TF issuer=$ISSUER_TF)"

# Plan file for mesh_dash (and any other live observers). Written as soon as the
# run is committed so the dash can show intended load alongside mesh pressure.
PLAN_JSON="$BENCH/out/current_plan.json"
mkdir -p "$BENCH/out"
python3 - "$PLAN_JSON" "$OUT" "$COUNT" "$CLIENTS" "$CONCURRENCY" "$RAMP_S" "$WATCH_AFTER_S" "$SAMPLE_S" "$COLLECTION" "$BOOT_IP" "$ISSUER" <<'PY'
import json, sys, time
from datetime import datetime, timezone
path, out, count, clients, conc, ramp, watch, sample, coll, boot, issuer = sys.argv[1:]
plan = {
    "count": int(count),
    "clients": int(clients),
    "concurrency": int(conc),
    "ramp_s": int(ramp),
    "watch_after_s": int(watch),
    "sample_s": float(sample),
    "collection": coll,
    "boot_ip": boot,
    "issuer": issuer,
    "seed": f"http://{boot}:21000",
    "out": out,
    "started_at": int(time.time()),
    "started_at_unix": int(time.time()),
    "started_at_iso": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
    "status": "running",
}
open(path, "w").write(json.dumps(plan, indent=2) + "\n")
open(out + "/plan.json", "w").write(json.dumps(plan, indent=2) + "\n")
print(f"==> plan → {path}")
PY

# --- observer: what the mesh looks like, sampled from the nodes themselves ---
observe() {
  python3 - "$OUT/mesh_trace.ndjson" "$BOOT_IP" "$REGION" "$PROJECT" "$SAMPLE_S" <<'PY' &
import json, subprocess, sys, time, urllib.request

trace, boot, region, project, every = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4], float(sys.argv[5])
t0 = time.time()


def get(url, timeout=4):
    try:
        with urllib.request.urlopen(url, timeout=timeout) as r:
            return json.load(r)
    except Exception:
        return None


def instances():
    try:
        raw = subprocess.run([
            "aws", "ec2", "describe-instances", "--region", region,
            "--filters", f"Name=tag:Name,Values={project}-spawned",
            "Name=instance-state-name,Values=pending,running",
            "--query",
            "Reservations[].Instances[].{Id:InstanceId,Ip:PublicIpAddress,State:State.Name,PreferNear:Tags[?Key=='PreferNear']|[0].Value}",
            "--output", "json",
        ], capture_output=True, text=True, timeout=25)
        return json.loads(raw.stdout or "[]")
    except Exception:
        return []


with open(trace, "a", buffering=1) as out:
    while True:
        insts = instances()
        nodes = []
        for ip in [boot] + [i["Ip"] for i in insts if i.get("Ip")]:
            status = get(f"http://{ip}:19000/status")
            if not status:
                nodes.append({"ip": ip, "up": False})
                continue
            a = status.get("autoscale") or {}
            p = a.get("pressure") or {}
            nodes.append({
                "ip": ip,
                "up": True,
                "role": a.get("role"),
                "items": status.get("items"),
                "zones": status.get("zones"),
                "peers": status.get("peers"),
                "queue": p.get("queue"),
                "hot_signal": p.get("hot_signal"),
                "hot_for": p.get("hot_for"),
                "cpu_pct": round(p.get("cpu_pct") or 0, 1),
                "mem_pct": round(p.get("mem_pct") or 0, 1),
                "inserts_window": p.get("inserts_window"),
                "reason": a.get("last_reason"),
                "ups": a.get("scale_ups_done"),
            })
        sample = {
            "t": round(time.time() - t0),
            "instances": len(insts),
            "answering": sum(1 for n in nodes if n.get("up")),
            "items": sum(n.get("items") or 0 for n in nodes if n.get("up")),
            "queue_total": sum(n.get("queue") or 0 for n in nodes if n.get("up")),
            "scale_ups": sum(n.get("ups") or 0 for n in nodes if n.get("up")),
            "nodes": nodes,
        }
        out.write(json.dumps(sample) + "\n")
        detail = " ".join(
            f"{n['ip'].split('.')[-1]}[m{n.get('mem_pct')} c{n.get('cpu_pct')} q{n.get('queue')}"
            + (f" {n['hot_signal']}" if n.get("hot_signal") else "")
            + "]"
            for n in nodes if n.get("up")
        )
        print(f"  [mesh] t={sample['t']}s nodes={sample['answering']}/{len(insts)+1} "
              f"items={sample['items']} ups={sample['scale_ups']} {detail}", flush=True)
        time.sleep(every)
PY
  OBSERVER=$!
}

observe
trap 'kill "$OBSERVER" 2>/dev/null || true' EXIT

# --- traffic ---
echo "==> traffic start"
START=$SECONDS
(
  cd "$BENCH"
  DENSITY_CSV="$CSV" ISSUER="$ISSUER" NODES="$SEED" \
    COUNT="$COUNT" CLIENTS="$CLIENTS" CONCURRENCY="$CONCURRENCY" \
    STORM_PARALLEL="$PARALLEL" RAMP_S="$RAMP_S" \
    STORM_OUT="$OUT/clients" COLLECTION="$COLLECTION" \
    node multi_client_storm.js
) >"$OUT/traffic.json" 2>"$OUT/traffic.err" || true
echo "==> traffic done in $((SECONDS - START))s"

echo "==> watching the mesh settle for up to ${WATCH_AFTER_S}s"
sleep "$WATCH_AFTER_S"

kill "$OBSERVER" 2>/dev/null || true
wait "$OBSERVER" 2>/dev/null || true

python3 - "$OUT" <<'PY' | tee "$OUT/SUMMARY.json"
import json, sys, pathlib

out = pathlib.Path(sys.argv[1])


def last_object(path):
    try:
        text = path.read_text()
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


traffic = last_object(out / "traffic.json")
samples = []
for line in (out / "mesh_trace.ndjson").read_text().splitlines():
    try:
        samples.append(json.loads(line))
    except Exception:
        pass

peak_nodes = max((s["answering"] for s in samples), default=0)
peak_queue = max((s["queue_total"] for s in samples), default=0)
reasons = {}
for s in samples:
    for n in s["nodes"]:
        if n.get("reason"):
            reasons[n["reason"]] = reasons.get(n["reason"], 0) + 1

final = samples[-1] if samples else {}
hops = traffic.get("hop_distribution") or {}
total_hops = sum(hops.values()) or 1
spread = sorted(((v / total_hops, k) for k, v in hops.items()), reverse=True)

print(json.dumps({
    "collection": traffic.get("collection"),
    "writes_ok": traffic.get("ok"),
    "writes_failed": traffic.get("fail"),
    "rps": round(traffic.get("rps") or 0, 1),
    "nodes_written_to": len(hops),
    "busiest_node_share": round(spread[0][0], 3) if spread else None,
    "hop_distribution": hops,
    "peak_nodes_answering": peak_nodes,
    "peak_queue_total": peak_queue,
    "scale_reasons_seen": reasons,
    "items_at_end": final.get("items"),
    "nodes_at_end": final.get("answering"),
    "instances_at_end": final.get("instances"),
}, indent=2))
PY

# Mark plan finished for mesh_dash consumers.
python3 - "$PLAN_JSON" "$OUT" <<'PY'
import json, sys, time
from datetime import datetime, timezone
from pathlib import Path
plan_path, out = Path(sys.argv[1]), Path(sys.argv[2])
for p in (plan_path, out / "plan.json"):
    try:
        plan = json.loads(p.read_text()) if p.exists() else {}
    except Exception:
        plan = {}
    plan["status"] = "done"
    plan["finished_at"] = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    plan["finished_at_unix"] = int(time.time())
    if (out / "SUMMARY.json").exists():
        try:
            plan["summary"] = json.loads((out / "SUMMARY.json").read_text())
        except Exception:
            pass
    p.write_text(json.dumps(plan, indent=2) + "\n")
PY

echo "==> done → $OUT"
