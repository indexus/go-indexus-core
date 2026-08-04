#!/usr/bin/env bash
# Fast local peer-convergence lab — Mac-safe resource caps (no Docker Desktop VM).
#
# Why not Docker by default on macOS:
#   Docker Desktop itself reserves a Linux VM (often 2–8 GiB). Native Go
#   processes + GOMEMLIMIT / GOMAXPROCS burn far less host RAM/CPU and still
#   exercise PreferNear + autoscale (queue path; mem% is blind without /proc).
#   Optional: scripts/local/docker-compose.lab.yml with --memory=512m --cpus=1
#   only helps if you need Linux /proc mem% — cap Docker Desktop first.
#
# Usage:
#   ./scripts/local/peer_lab.sh
#   COUNT=1500 SPAWN_TARGET=2 ./scripts/local/peer_lab.sh
#   KEEP_UP=1 ./scripts/local/peer_lab.sh   # leave mesh up after analysis
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=env.sh
source "$SCRIPT_DIR/env.sh"
BENCH="$ROOT/scripts/bench"
OUT="$BENCH/out/peer-lab-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$OUT"

# --- Mac-safe caps -----------------------------------------------------------
export GOMAXPROCS="${GOMAXPROCS:-1}"
export GOMEMLIMIT="${GOMEMLIMIT:-512MiB}"
export SPAWN_MAX="${SPAWN_MAX:-2}"
export QUEUE_PRESSURE="${QUEUE_PRESSURE:-40}"
export PRESSURE_HOLD="${PRESSURE_HOLD:-2s}"
export SCALE_WINDOW="${SCALE_WINDOW:-15s}"
export SCALE_COOLDOWN="${SCALE_COOLDOWN:-8s}"
export SCALE_DOWN_HOLD="${SCALE_DOWN_HOLD:-30s}"
export SCALE_DOWN_THRESHOLD="${SCALE_DOWN_THRESHOLD:-30}"
export INDEXUS_SPAWN_GRACE="${INDEXUS_SPAWN_GRACE:-20s}"

COUNT="${COUNT:-1200}"
CONCURRENCY="${CONCURRENCY:-8}"
DURATION_S="${DURATION_S:-30}"
SPAWN_TARGET="${SPAWN_TARGET:-2}"
SETTLE_S="${SETTLE_S:-40}"
KEEP_UP="${KEEP_UP:-0}"
COLLECTION="${COLLECTION:-PeerLab$(date +%s | tail -c 5)}"
COLLECTION="$(python3 -c "c='$COLLECTION'; print((c+'XXXXXXXXXXXXXXXX')[:16])")"

# Hard ceilings so a typo cannot burn the Mac.
if (( COUNT > 1500 )); then COUNT=1500; fi
if (( CONCURRENCY > 8 )); then CONCURRENCY=8; fi
if (( DURATION_S > 40 )); then DURATION_S=40; fi
if (( SPAWN_TARGET > SPAWN_MAX )); then SPAWN_TARGET=$SPAWN_MAX; fi

CSV="$BENCH/data/world_density_100k.csv"
[[ -f "$CSV" ]] || { echo "missing $CSV" >&2; exit 1; }

echo "==> peer_lab out=$OUT"
echo "    GOMAXPROCS=$GOMAXPROCS GOMEMLIMIT=$GOMEMLIMIT SPAWN_MAX=$SPAWN_MAX"
echo "    COUNT=$COUNT CONCURRENCY=$CONCURRENCY DURATION_S=${DURATION_S}s SPAWN_TARGET=$SPAWN_TARGET"

cleanup() {
  if [[ "$KEEP_UP" == "1" ]]; then
    echo "==> KEEP_UP=1 — mesh left running (./scripts/local/mesh_down.sh to stop)"
    return
  fi
  echo "==> mesh_down"
  "$SCRIPT_DIR/mesh_down.sh" >/dev/null 2>&1 || true
}
trap cleanup EXIT

"$SCRIPT_DIR/mesh_up.sh" | tee "$OUT/mesh_up.log"

# Valid BASE64 PreferNear: bootstrap's own node id (issuer rejects garbage).
PREFER_NEAR="$(curl -sf "http://127.0.0.1:${BOOT_MON}/status" | python3 -c 'import sys,json; print(json.load(sys.stdin)["name"])')"
echo "    prefer_near=$PREFER_NEAR"
echo "$PREFER_NEAR" >"$OUT/prefer_near.txt"

snap_mesh() {
  local tag="$1"
  python3 - <<PY
import json, urllib.request, pathlib, time
out = pathlib.Path("$OUT")
spawned_dir = pathlib.Path("$SPAWNED_DIR")
tag = "$tag"
boot_mon = $BOOT_MON
boot_p2p = $BOOT_P2P

def get(url, timeout=2):
    try:
        with urllib.request.urlopen(url, timeout=timeout) as r:
            return json.load(r)
    except Exception as e:
        return {"error": str(e)}

def hosts(payload):
    if isinstance(payload, list):
        return payload
    if isinstance(payload, dict) and "hosts" in payload:
        return payload["hosts"]
    if isinstance(payload, dict) and "error" in payload:
        return []
    return []

nodes = [{"id": "bootstrap", "mon": boot_mon, "p2p": boot_p2p}]
for f in sorted(spawned_dir.glob("local-*.json")):
    meta = json.load(open(f))
    nodes.append({"id": meta.get("id"), "name": meta.get("name"), "mon": meta["mon"], "p2p": meta["p2p"]})

rows = []
for n in nodes:
    mon = n["mon"]
    status = get(f"http://127.0.0.1:{mon}/status")
    registered = get(f"http://127.0.0.1:{mon}/registered")
    acknowledged = get(f"http://127.0.0.1:{mon}/acknowledged")
    routing = get(f"http://127.0.0.1:{mon}/routing")
    auto = get(f"http://127.0.0.1:{mon}/autoscale")
    reg_h = hosts(registered)
    ack_h = hosts(acknowledged)
    route_h = hosts(routing)
    rows.append({
        "id": n["id"],
        "name": status.get("name") or n.get("name"),
        "up": "error" not in status,
        "items": status.get("items"),
        "peers_status": status.get("peers"),
        "registered": len(reg_h),
        "acknowledged": len(ack_h),
        "routing": len(route_h),
        "registered_hosts": reg_h,
        "acknowledged_hosts": ack_h,
        "routing_hosts": route_h,
        "client_ready": status.get("client_ready"),
        "rebalancing": status.get("rebalancing"),
        "role": (auto.get("role") if isinstance(auto, dict) else None),
        "last_prefer_near": (auto.get("last_prefer_near") if isinstance(auto, dict) else None),
        "inserts_window": (auto.get("inserts_window") if isinstance(auto, dict) else None) or ((auto.get("pressure") or {}).get("inserts_window") if isinstance(auto, dict) else None),
        "mem_pct": ((auto.get("pressure") or {}).get("mem_pct") if isinstance(auto, dict) else None),
    })

report = {"t": time.time(), "tag": tag, "nodes": rows}
path = out / f"{tag}.mesh.json"
path.write_text(json.dumps(report, indent=2))
print(f"[{tag}] nodes={len(rows)} " + ", ".join(
    f"{r['id']}:reg={r['registered']}/ack={r['acknowledged']}/route={r['routing']}/ready={r['client_ready']}/xfer={r['rebalancing']}/items={r['items']}"
    for r in rows
))
PY
}

echo "==> baseline"
snap_mesh baseline

echo "==> light density write (bounded)"
(cd "$BENCH" && \
  ISSUER="$ISSUER_URL" NODES="http://127.0.0.1:${BOOT_P2P}" \
  DENSITY_CSV="$CSV" \
  node geo_load_density.js \
    --count "$COUNT" \
    --duration "$DURATION_S" \
    --concurrency "$CONCURRENCY" \
    --collection "$COLLECTION" \
  | tee "$OUT/write.json") &
WRITE_PID=$!

echo "==> wait for spawn_target=$SPAWN_TARGET (prefer_near=$PREFER_NEAR)"
deadline=$((SECONDS + 90))
while (( SECONDS < deadline )); do
  n=$("$SCRIPT_DIR/count.sh" || echo 0)
  echo "  spawned=$n"
  snap_mesh "during_write_$(date +%H%M%S)" || true
  if [[ "$n" -ge "$SPAWN_TARGET" ]]; then
    break
  fi
  # Explicit scale nudge with a valid BASE64 PreferNear (bootstrap name).
  if [[ "$n" -lt "$SPAWN_TARGET" ]]; then
    curl -sf -X POST "$ISSUER_URL/v1/scale" \
      -H 'Content-Type: application/json' \
      -d "{\"requester_id\":\"peer-lab\",\"spawn_count\":$((SPAWN_TARGET - n)),\"prefer_near\":\"$PREFER_NEAR\"}" \
      >"$OUT/scale_nudge.json" 2>/dev/null || true
  fi
  sleep 3
done

wait "$WRITE_PID" || true
snap_mesh post_write
"$SCRIPT_DIR/list_spawned.sh" | tee "$OUT/spawned.json"

echo "==> settle ${SETTLE_S}s (watch peer tables; no writers)"
end=$((SECONDS + SETTLE_S))
while (( SECONDS < end )); do
  snap_mesh "settle_$(date +%H%M%S)" || true
  sleep 5
done

echo "==> analyze peer convergence"
python3 - <<PY | tee "$OUT/analysis.txt"
import json, pathlib
out = pathlib.Path("""$OUT""")
snaps = sorted(out.glob("*.mesh.json"), key=lambda p: p.stat().st_mtime)
print(f"snapshots: {len(snaps)}")

spawned = json.loads((out / "spawned.json").read_text()) if (out / "spawned.json").exists() else []
names = [s.get("name") for s in spawned]
print("spawned names:", names)
print("unique names:", len(set(names)), "/", len(names))

print("\n--- timeline (registered / routing asymmetry) ---")
asym_events = []
for p in snaps:
    s = json.loads(p.read_text())
    ups = [n for n in s["nodes"] if n.get("up")]
    if not ups:
        print(f"[{s.get('tag')}] no nodes up")
        continue
    regs = [n.get("registered") or 0 for n in ups]
    routes = [n.get("routing") or 0 for n in ups]
    asym = max(regs) - min(regs) if regs else 0
    route_asym = max(routes) - min(routes) if routes else 0
    line = (
        f"[{s.get('tag'):28}] "
        + " | ".join(
            f"{n['id']}:reg={n.get('registered')} ack={n.get('acknowledged')} rte={n.get('routing')} peers={n.get('peers_status')}"
            for n in ups
        )
    )
    if asym > 0 or route_asym > 0:
        line += f"  << ASYM regΔ={asym} rteΔ={route_asym}"
        asym_events.append({"tag": s.get("tag"), "reg_delta": asym, "route_delta": route_asym})
    print(line)

last = json.loads(snaps[-1].read_text()) if snaps else {"nodes": []}
print("\nfinal mesh:")
for n in last["nodes"]:
    print(
        f"  {n['id']:12} name={n.get('name')} ready={n.get('client_ready')} "
        f"reg={n.get('registered')} ack={n.get('acknowledged')} route={n.get('routing')} "
        f"peers={n.get('peers_status')} xfer={n.get('rebalancing')} items={n.get('items')}"
    )
    if n.get("registered_hosts"):
        print(f"             reg_hosts={n['registered_hosts']}")
    if n.get("routing_hosts"):
        print(f"             rte_hosts={n['routing_hosts']}")

regs = [n["registered"] for n in last["nodes"] if n.get("up") and n.get("registered") is not None]
routes = [n["routing"] for n in last["nodes"] if n.get("up") and n.get("routing") is not None]
up_n = sum(1 for n in last["nodes"] if n.get("up"))
print(f"\nup_nodes={up_n}")
if regs and max(regs) - min(regs) > 0:
    print("WARN: registered table sizes still diverge — peer gossip / Observe lag")
elif regs and up_n >= 2 and min(regs) >= up_n:
    print("OK: registered converged (each up node sees the full mesh)")
elif regs:
    print("WARN: registered sizes match but look incomplete vs up_nodes")
else:
    print("WARN: no registered samples")

if routes and max(routes) - min(routes) > 0:
    print("WARN: routing table sizes still diverge")
elif routes and up_n >= 2 and min(routes) >= up_n:
    print("OK: routing converged")
else:
    print("NOTE: routing not fully symmetric yet (may still be warming)")

print(f"asymmetry events during run: {len(asym_events)}")
for e in asym_events[:12]:
    print(f"  - {e['tag']}: regΔ={e['reg_delta']} rteΔ={e['route_delta']}")

settle = [json.loads(p.read_text()) for p in snaps if "settle_" in p.name]
xfer_tail = []
for s in settle[-6:]:
    xfer_tail.append(any(n.get("rebalancing") for n in s["nodes"] if n.get("up")))
print("rebalancing during late settle:", xfer_tail)
if xfer_tail and all(xfer_tail):
    print("WARN: still rebalancing after writers stopped — ownership thrash / incomplete peer join")

# Prove bootstrap learned joiners (the reported asymmetry).
boot = next((n for n in last["nodes"] if n.get("id") == "bootstrap" and n.get("up")), None)
if boot and up_n >= 2:
    if (boot.get("registered") or 0) >= up_n and (boot.get("routing") or 0) >= up_n:
        print("OK: bootstrap registered+routing include the full mesh")
    else:
        print(
            f"FAIL: bootstrap still incomplete "
            f"reg={boot.get('registered')} rte={boot.get('routing')} want>={up_n}"
        )
PY

echo "==> done → $OUT"
