# Indexus mesh dash

Live mesh view **and experiment control plane** for AWS runs. Bound to **127.0.0.1** only.

## Run

```bash
cd scripts/bench/mesh_dash
BOOT_IP=x.x.x.x node server.js
```

Open **http://127.0.0.1:3847/**

Hard-refresh the browser after UI edits. Restart `server.js` only when the server code changes.

## Experiment controls

| Control | API | Behavior |
|---------|-----|----------|
| Apply curve | `POST /api/plan` | Save COUNT / CLIENTS / CONCURRENCY / RAMP_S / WATCH_AFTER_S |
| Start | `POST /api/experiment/start` | Prefers **remote** EC2 load hosts (`indexus-aws-load` via `spawn_load.sh`); falls back to local `traffic_sim.sh` |
| Stop | `POST /api/experiment/stop` | Stop remote storms + local traffic pids |
| Reset | `POST /api/experiment/cleanup` | **Async** (HTTP 202): fire EC2 terminate + S3 clear + optional wipe — does **not** block Start |
| Export | `GET /api/export` | Download history ring + latest mesh/plan/nodes JSON |

**Writers:** default `LOAD_MODE=remote` (separate machines). UI shows `writers: remote (k hosts)` vs `local (n pids)`. Set `LOAD_MODE=local` to force laptop traffic.

**Cleanup runs in background; Start can use a new collection / boot IP without waiting for terminate.**

`GET /api/plan` returns curve params + `experiment.status` (`idle` \| `running` \| `stopping` \| `cleaning`) + `experiment.writers`.

## Env

| Variable | Default | Notes |
|----------|---------|--------|
| `BOOT_IP` | (auto) | Bootstrap public IP |
| `REGION` | `eu-west-3` | AWS region |
| `PROJECT` | `indexus-aws` | Tag prefix |
| `PORT` | `3847` | Listen (localhost) |
| `PLAN_FILE` | `../out/current_plan.json` | Writable plan |
| `LOAD_MODE` | `remote` | `remote` = EC2 load VMs; `local` = Mac traffic_sim |
| `LOAD_HOSTS` | `2` | Number of `indexus-aws-load` writers |

## Thresholds (live from `/status`)

Dash charts read node config: mem/CPU floor ~**20%**, spawn ~**65%** mem/CPU (disk free &lt; **35%** ≈ 65% used).
