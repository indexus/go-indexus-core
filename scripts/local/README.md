# Local mesh (no AWS)

Replace EC2 spawn/terminate with processes on `127.0.0.1`.

```bash
# start issuer + bootstrap (autoscale enabled, local spawn backend)
./scripts/local/mesh_up.sh

# manual spawn via issuer (same path the bootstrap uses when hot)
curl -X POST http://127.0.0.1:22000/v1/scale \
  -H 'Content-Type: application/json' \
  -d '{"requester_id":"lab","local_inserts":9999,"spawn_count":1}'

./scripts/local/list_spawned.sh
./scripts/local/count.sh

# SoftLeave battle (writes → scale-up → quiet → SoftLeave → assert counts)
# Defaults are tuned for a fast local loop (~2–3 min). Override COUNT etc. to stress.
COUNT=1200 SPAWN_TARGET=2 \
  ./scripts/local/downscale_battle.sh

# Peer / rebalance convergence lab (Mac-safe: GOMEMLIMIT + GOMAXPROCS, SPAWN_MAX=2)
./scripts/local/peer_lab.sh
# leave mesh up for manual curl checks:
KEEP_UP=1 ./scripts/local/peer_lab.sh

./scripts/local/mesh_down.sh
```

| Script | Role |
|--------|------|
| `mesh_up.sh` | build + issuer (`-launchTemplate local`) + bootstrap |
| `spawn.sh` | started by issuer `/v1/scale` |
| `terminate.sh` | kill (`LEAVE=1` to drain first); used by `/v1/downscale` |
| `count.sh` / `list_spawned.sh` | live spawned inventory |
| `downscale_battle.sh` | local equivalent of `scripts/bench/downscale_battle.sh` |
| `peer_lab.sh` | light write + spawn + settle; snapshots registered/routing (Mac-safe caps) |
| `mesh_down.sh` | tear everything down (also frees ports 19000–19020 / 21000–21020) |
| `docker-compose.lab.yml` | optional Linux cgroup caps — heavier on Mac |

**Mac note:** Prefer `peer_lab.sh` with native limits (`GOMEMLIMIT=512MiB`, `GOMAXPROCS=1`, `SPAWN_MAX=2`, `COUNT≤1500`, `concurrency≤8`, `duration≤40s`). Docker Desktop’s VM often costs more RAM than the lab itself; use `docker-compose.lab.yml` (`mem_limit: 512m`, `cpus: 1`) only when you need Linux `/proc` mem% for autoscale — and cap Docker Desktop to ≤2 CPU / ≤2 GiB first. On Darwin, mem% autoscale stays blind; local scale-up uses queue pressure + explicit `/v1/scale` nudges with a valid BASE64 `prefer_near`.

Data and logs live under `.data-local/` (gitignored).
