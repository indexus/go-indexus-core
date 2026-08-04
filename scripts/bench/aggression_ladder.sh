#!/usr/bin/env bash
# Stepped write aggression against bootstrap until it falls or the ladder ends.
# No topology scripting — pure traffic + observation. Between steps we record
# whether the bootstrap still answers /status and whether journalctl saw OOM.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
BENCH="$ROOT/scripts/bench"
TF_DIR="$ROOT/deploy/terraform"
REGION="${AWS_REGION:-eu-west-3}"
PROJECT="${PROJECT:-indexus-aws}"
OUT="${OUT:-$BENCH/out/aggression-$(date +%Y%m%d-%H%M%S)}"
# Absolute so later `cd` into terraform does not lose the output dir.
[[ "$OUT" = /* ]] || OUT="$ROOT/$OUT"
mkdir -p "$OUT"

SSH_OPTS=(-o StrictHostKeyChecking=no -o IdentitiesOnly=yes -o ConnectTimeout=8
  -i "${SSH_KEY:-$HOME/.ssh/id_ed25519}")

cd "$TF_DIR"
ISSUER=$(terraform output -raw issuer_url 2>/dev/null || true)
BOOT_IP_TF=$(terraform output -raw bootstrap_public_ip 2>/dev/null || true)

# Prefer a live answering IP over a possibly stale terraform output.
BOOT_IP="${BOOT_IP:-}"
if [[ -z "$BOOT_IP" ]]; then
  for cand in "$BOOT_IP_TF" "15.237.219.50"; do
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

SEED="http://${BOOT_IP}:21000"
echo "==> out=$OUT"
echo "==> bootstrap=$BOOT_IP issuer=$ISSUER"
echo "$BOOT_IP" >"$OUT/bootstrap_ip.txt"

ssh_boot() {
  ssh "${SSH_OPTS[@]}" "ec2-user@$BOOT_IP" "$@"
}

bootstrap_alive() {
  curl -sf --max-time 4 "http://${BOOT_IP}:19000/status" >/dev/null 2>&1
}

snapshot_status() {
  local label=$1
  curl -sf --max-time 5 "http://${BOOT_IP}:19000/status" >"$OUT/${label}.status.json" 2>/dev/null || echo '{"up":false}' >"$OUT/${label}.status.json"
  python3 - "$OUT/${label}.status.json" <<'PY'
import json,sys
p=sys.argv[1]
try:
  d=json.load(open(p))
except Exception:
  d={}
a=d.get("autoscale") or {}
pr=a.get("pressure") or {}
print(json.dumps({
  "items": d.get("items"),
  "queue": d.get("queue") if d.get("queue") is not None else pr.get("queue"),
  "peers": d.get("peers"),
  "zones": d.get("zones"),
  "mem_pct": pr.get("mem_pct"),
  "cpu_pct": pr.get("cpu_pct"),
  "hot_signal": pr.get("hot_signal"),
  "mem_rise": pr.get("mem_rise"),
  "reason": a.get("last_reason"),
  "ups": a.get("scale_ups_done"),
  "up_in_flight": a.get("up_in_flight"),
  "admit_blocked": a.get("admit_blocked"),
  "rising_fast": a.get("rising_fast"),
}, indent=2))
PY
}

capture_crash() {
  local label=$1
  {
    echo "=== alive? ==="
    bootstrap_alive && echo yes || echo no
    echo "=== journalctl indexus-node (last 80) ==="
    ssh_boot "sudo journalctl -u indexus-node -n 80 --no-pager" 2>&1 || true
    echo "=== dmesg OOM ==="
    ssh_boot "sudo dmesg -T | grep -iE 'oom|killed process|out of memory' | tail -30" 2>&1 || true
    echo "=== systemctl ==="
    ssh_boot "systemctl is-active indexus-node; systemctl status indexus-node --no-pager -l | head -40" 2>&1 || true
  } | tee "$OUT/${label}.crash.txt"
}

cleanup_spawned() {
  echo "==> terminate spawned instances"
  IDS=$(aws ec2 describe-instances --region "$REGION" \
    --filters "Name=tag:Name,Values=${PROJECT}-spawned" \
      "Name=instance-state-name,Values=pending,running" \
    --query 'Reservations[].Instances[].InstanceId' --output text)
  if [[ -n "${IDS// /}" ]]; then
    aws ec2 terminate-instances --region "$REGION" --instance-ids $IDS
    echo "terminated: $IDS"
  else
    echo "no spawned instances"
  fi
}

# Ladder: step|clients|concurrency|ramp_s|count
LADDER=(
  "1|4|1|60|50000"
  "2|8|2|60|100000"
  "3|16|4|60|150000"
  "4|24|8|30|200000"
  "5|32|16|10|250000"
)

SUMMARY="$OUT/SUMMARY.ndjson"
: >"$SUMMARY"
BREAKING=""

for row in "${LADDER[@]}"; do
  IFS='|' read -r STEP CLIENTS CONCURRENCY RAMP_S COUNT <<<"$row"
  LABEL="step${STEP}"
  echo
  echo "======== STEP $STEP: clients=$CLIENTS conc=$CONCURRENCY ramp=${RAMP_S}s count=$COUNT ========"

  if ! bootstrap_alive; then
    echo "bootstrap already down before step $STEP"
    capture_crash "${LABEL}_pre"
    BREAKING="before_step_${STEP}"
    break
  fi

  snapshot_status "${LABEL}_before" | tee "$OUT/${LABEL}_before.snap.json"

  # Short mesh observer for this step only.
  (
    python3 - "$OUT/${LABEL}.mesh.ndjson" "$BOOT_IP" "$REGION" "$PROJECT" 5 <<'PY'
import json, subprocess, sys, time, urllib.request
trace, boot, region, project, every = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4], float(sys.argv[5])
t0 = time.time()
def get(url):
    try:
        with urllib.request.urlopen(url, timeout=3) as r: return json.load(r)
    except Exception: return None
def instances():
    try:
        raw = subprocess.run([
            "aws","ec2","describe-instances","--region",region,
            "--filters",f"Name=tag:Name,Values={project}-spawned",
            "Name=instance-state-name,Values=pending,running",
            "--query","Reservations[].Instances[].{Id:InstanceId,Ip:PublicIpAddress}",
            "--output","json"], capture_output=True, text=True, timeout=20)
        return json.loads(raw.stdout or "[]")
    except Exception: return []
with open(trace,"a",buffering=1) as out:
    while True:
        insts = instances()
        st = get(f"http://{boot}:19000/status")
        a = (st or {}).get("autoscale") or {}
        p = a.get("pressure") or {}
        sample = {
            "t": round(time.time()-t0),
            "alive": st is not None,
            "spawned": len(insts),
            "queue": (st or {}).get("queue") if st and st.get("queue") is not None else p.get("queue"),
            "mem_pct": p.get("mem_pct"),
            "cpu_pct": p.get("cpu_pct"),
            "hot": p.get("hot_signal"),
            "reason": a.get("last_reason"),
            "ups": a.get("scale_ups_done"),
            "admit_blocked": a.get("admit_blocked"),
            "items": (st or {}).get("items"),
        }
        out.write(json.dumps(sample)+"\n")
        print(f"  [mesh] t={sample['t']}s alive={sample['alive']} spawned={sample['spawned']} "
              f"m={sample['mem_pct']} c={sample['cpu_pct']} q={sample['queue']} "
              f"ups={sample['ups']} {sample.get('hot') or ''} {sample.get('reason') or ''}", flush=True)
        time.sleep(every)
PY
  ) &
  OBS=$!

  START=$SECONDS
  set +e
  (
    cd "$BENCH"
    DENSITY_CSV="$BENCH/data/world_density_100k.csv" \
      ISSUER="$ISSUER" NODES="$SEED" \
      COUNT="$COUNT" CLIENTS="$CLIENTS" CONCURRENCY="$CONCURRENCY" \
      STORM_PARALLEL="$CLIENTS" RAMP_S="$RAMP_S" \
      STORM_OUT="$OUT/${LABEL}_clients" \
      COLLECTION="AggS${STEP}$(date +%s | tail -c 5)" \
      node multi_client_storm.js
  ) >"$OUT/${LABEL}.traffic.json" 2>"$OUT/${LABEL}.traffic.err"
  TRAFFIC_RC=$?
  set -e
  ELAPSED=$((SECONDS - START))

  kill "$OBS" 2>/dev/null || true
  wait "$OBS" 2>/dev/null || true

  ALIVE=0
  bootstrap_alive && ALIVE=1
  snapshot_status "${LABEL}_after" | tee "$OUT/${LABEL}_after.snap.json" || true

  # Peak from mesh samples
  PEAK=$(python3 - "$OUT/${LABEL}.mesh.ndjson" <<'PY'
import json,sys
mem=cpu=q=0
alive_min=True
ups=0
reasons={}
for line in open(sys.argv[1]):
    s=json.loads(line)
    if not s.get("alive"): alive_min=False
    mem=max(mem, s.get("mem_pct") or 0)
    cpu=max(cpu, s.get("cpu_pct") or 0)
    q=max(q, s.get("queue") or 0)
    ups=max(ups, s.get("ups") or 0)
    if s.get("reason"): reasons[s["reason"]]=reasons.get(s["reason"],0)+1
print(json.dumps({"peak_mem":mem,"peak_cpu":cpu,"peak_queue":q,"min_alive":alive_min,"ups":ups,"reasons":reasons}))
PY
)

  TRAFFIC=$(python3 - "$OUT/${LABEL}.traffic.json" <<'PY'
import json,sys,re
text=open(sys.argv[1]).read()
objs=[]; buf=""; depth=0
for ch in text:
    if ch=="{": depth+=1
    if depth: buf+=ch
    if ch=="}":
        depth-=1
        if depth==0:
            try: objs.append(json.loads(buf))
            except: pass
            buf=""
d=objs[-1] if objs else {}
print(json.dumps({"ok":d.get("ok"),"fail":d.get("fail"),"rps":d.get("rps"),"hops":d.get("hop_distribution")}))
PY
)

  ROW=$(python3 -c "
import json
peak=json.loads('''$PEAK''')
traf=json.loads('''$TRAFFIC''')
print(json.dumps({
  'step': $STEP,
  'clients': $CLIENTS,
  'concurrency': $CONCURRENCY,
  'ramp_s': $RAMP_S,
  'count': $COUNT,
  'elapsed_s': $ELAPSED,
  'traffic_rc': $TRAFFIC_RC,
  'bootstrap_alive': bool($ALIVE),
  **peak,
  **traf,
}))
")
  echo "$ROW" | tee -a "$SUMMARY"
  echo "$ROW" >"$OUT/${LABEL}.summary.json"

  if [[ "$ALIVE" -eq 0 ]]; then
    echo "==> BOOTSTRAP DOWN at step $STEP"
    capture_crash "$LABEL"
    BREAKING="step_${STEP}_clients=${CLIENTS}_conc=${CONCURRENCY}_ramp=${RAMP_S}_count=${COUNT}"
    break
  fi

  # Brief settle so the next step starts from a clearer baseline.
  echo "==> settle 45s"
  sleep 45
done

echo
echo "==> ladder done; breaking_point=${BREAKING:-none_survived_all}"
echo "$BREAKING" >"$OUT/breaking_point.txt"

# If it fell, optionally verify previous step still survives after clean restart
# (caller / deploy path handles wipe; here we only note).
cleanup_spawned

python3 - "$OUT" <<'PY' | tee "$OUT/REPORT.json"
import json, pathlib, sys
out = pathlib.Path(sys.argv[1])
rows = []
for line in (out/"SUMMARY.ndjson").read_text().splitlines():
    try: rows.append(json.loads(line))
    except: pass
bp = (out/"breaking_point.txt").read_text().strip()
print(json.dumps({
    "breaking_point": bp or None,
    "steps": rows,
    "bootstrap_ip": (out/"bootstrap_ip.txt").read_text().strip(),
}, indent=2))
PY

echo "==> done → $OUT"
