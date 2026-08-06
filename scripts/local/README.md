# Local mesh (no AWS)

Replace EC2 spawn/terminate with processes on `127.0.0.1`.

```bash
# start issuer + bootstrap (autoscale enabled, local spawn backend)
./scripts/local/mesh_up.sh

# manual spawn via issuer
curl -X POST http://127.0.0.1:22000/v1/scale \
  -H 'Content-Type: application/json' \
  -d '{"requester_id":"lab","local_inserts":9999,"spawn_count":1}'

./scripts/local/list_spawned.sh
./scripts/local/count.sh

./scripts/local/mesh_down.sh
```

Ops console + dataset loaders: sibling repo [`dashboard/`](../../../dashboard/) (`BOOT_IP=127.0.0.1 pnpm dev`).

| Script | Role |
|--------|------|
| `mesh_up.sh` | build + issuer (`-launchTemplate local`) + bootstrap |
| `spawn.sh` | started by issuer `/v1/scale` (node names itself via PreferNear) |
| `terminate.sh` | kill (`LEAVE=1` to drain first); used by `/v1/downscale` |
| `count.sh` / `list_spawned.sh` | live spawned inventory |
| `mesh_down.sh` | tear everything down (ports 19000–19020 / 21000–21020) |

## Capacity defaults (`mesh_up` / large loads)

| Env | Default | Role |
|-----|---------|------|
| `DELEGATION` / `INDEXUS_DELEGATION` | `5000` | Soft item count before Own split (`-delegation`). Override via dashboard Config → Restart mesh (`.data-local/mesh-config.env`) |
| `INDEXUS_TRANSFER_THRESHOLD` | `200` | Zones ≥ this count use snapshot delegation vs classic Transfer |
| `INDEXUS_DELEGATION_TIMEOUT` | `2m` | Snapshot-delegation session timeout |
| `INDEXUS_TRANSFER_TIMEOUT` | `5m` | Classic `/transfer` HTTP timeout |
| `SPAWN_MAX` | `4` | Issuer spawn cap |
| `QUEUE_PRESSURE` | `50000` | Scale-down backlog guard (not SoftLeave) |
| `SNAPSHOT_DIR` | `.data-local/snapshots` | Shared DirStore (S3-equivalent) |
| `INDEXUS_DELEGATION_S3` | `1` | Enable snapshot handoff protocol |

Data and logs live under `.data-local/` (gitignored).

### Sticky certs

`app/node` prefers an existing `.cert.json` over PreferNear. `mesh_up.sh` / `spawn.sh` wipe `local-*.cert.json` before start. Force kill from the dashboard (`POST /api/terminate`) also clears keys.
