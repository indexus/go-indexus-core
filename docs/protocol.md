# Indexus Mesh Protocol

This is the reference specification of the mesh: the model, the phases as
algorithms, the invariants each phase provides, and the crash / concurrency
edge cases with how they resolve. The code is organized to mirror it — one
file per phase under `core/` — and every guarantee below names the mechanism
and the test that checks it.

## 1. Model

- **Identifiers.** Every node and every zone key lives in a 160-bit ID space.
  A node's ID is the decoding of its BASE64 name; a zone key
  `(collection, location)` maps to `MergeEncodings(location, collection)`.
- **Metric.** Kademlia-style XOR distance. "Nearest" always means
  XOR-nearest in this space.
- **Zones and items.** A collection is a prefix tree of sets carrying abelian
  aggregates (count + metrics). Items are leaves (`location:id`); interior
  entries aggregate their subtree. A *zone* is a subtree an owner serves;
  ownership splits when a zone's running count reaches the delegation size.
- **Deletes.** Tombstones with a generation counter; a re-add clears the
  tombstone, a newer tombstone wins over an older one (`ApplyTombstone`).

**Target steady state.** For every zone key `k` with data, exactly one live
node owns `k`, and that node is the XOR-nearest live node to `k`. Every write
acknowledged to a client is placed under that owner exactly once.

## 2. Node state

| Field | Meaning | Phase |
|---|---|---|
| `acknowledged` | contacts heard of, not yet verified | M |
| `registered` | contacts that answered a Ping (or were gossiped) | M |
| `routing` | XOR k-bucket extract of `registered` + self | M |
| `suspect` | quarantined names (dead peers), with expiry | M |
| `owned` | BST of zone keys this node serves | R/P |
| `collections` | the data: sets, aggregates, tombstones | P |
| `queue` | ingress elements pending placement | I |
| `cache` | soft cache of remote sets (TTL by hops) | U |
| `storage` | WAL + snapshot | C |
| `leaving` | drain in progress; reject Ping/ingress | L |
| `rebalancing` | Refresh is applying a transfer plan | R |
| `transferMu` | **the single-mover lock**: any ownership movement | R/L |
| `refreshBusy` | single-flight latch for Refresh | R |
| `clientPublished` | blue/green latch: safe for client XOR routing | J |
| `full` | memory watchdog: refuse client writes | I |

## 3. Phases

Each phase runs on the worker cadence (`worker.Start`): `Observe`, `Update`
and `AutoscaleTick` on their own goroutines, `Refresh` on the main loop,
`Feed` continuously. First pass runs immediately at boot.

### M — Membership (`membership.go`)

A contact climbs `acknowledged → registered → routing`; death walks the other
way and adds a 90 s quarantine (`quarantineWindow`) so gossip cannot
re-register a dead peer before its zones were re-routed.

```
Observe():
  for c in registered\{self}:  Ping c;  on failure → suspend(c) + reject(c)
  for c in acknowledged\{self}: Ping c; ack answer → toRegister, else drop
  if both empty → re-acknowledge bootstraps          # partition self-heal
  registeredNew := register(toRegister)               # skips quarantined
  if registeredNew → subscribe(toRegister)            # routing immediately
                   → go Refresh()                     # out-of-band, single-flight
```

Guarantees:
- **M1** A peer enters `registered` only if dialable, and never while
  quarantined (`register`).
- **M2** A dead peer leaves `registered`+`routing` within one Delay tick and
  stays quarantined for 90 s (`Observe`, `suspend`). Gossip cannot resurrect
  it inside the window.
- **M3** A node whose tables emptied re-seeds from its bootstraps — a
  restarted mesh reconverges without an operator.
- **M4** A *new* registration (not a re-ack, not a quarantine skip) kicks one
  out-of-band Refresh, so a joiner receives ownership without waiting a full
  tick. `register` returning "actually inserted" is what prevents the
  Refresh storms observed when any ack ping re-triggered it.

### G — Gossip (first half of `Refresh`, `rebalance.go`)

Ask each routing peer (fallback: registered) for its `Neighbors` — the
XOR-bucket extract around *our* ID — and register what comes back. Gossiped
contacts are not Ping-verified; a dead one is evicted by the next Observe
(M2), and quarantine keeps known-dead ones out. Eventual full membership on
a connected mesh follows from bucket extracts always including the nearest
peers in each subtree.

### R — Rebalance (second half of `Refresh`, `rebalance.go` + `delegation.go`)

```
Refresh():
  if !refreshBusy.CAS  → return                 # single flight
  gossip (G)
  if !leaving && transferMu.TryLock():          # single mover
      plan := control()                          # read-only planning
      rebalancing = true                         # pauses Feed apply (Transfer path)
      for (peer, keys) in plan, in parallel: startOutboundDelegation(peer, keys)
      rebalancing = false; transferMu.Unlock()
  clean()                                        # rebuild routing
  Checkpoint()                                   # node snap + dirty zone snaps

startOutboundDelegation(peer, keys):
  if !INDEXUS_DELEGATION_S3 or no ObjectStore or zone < threshold:
      return transferToPeer(peer, keys)          # classic path
  ForceCheckpointZones(large keys)
  DelegationOffer(refs S3) → receiver pulls snaps
  mirror inbound writes while ownership stays local
  WALDelta(filtered lines) → receiver CaughtUp
  donor: flush cutover queue → SwitchAck(receiver) → Delegate+removeOwnedKey
  # ACK-before-drop (R1/R4): ownership never drops if SwitchAck fails

transferToPeer(peer, keys):                      # small zones / fallback
  for key in keys:
      if refused || leaving → skip
      items := collection.Delegate(key)
      if peer.Transfer(key, items) fails:
          restoreDelegated(key, items); refused = true
      else:
          removeOwnedKey(key)
```

Guarantees:
- **R1 (no lost zone).** Ownership drops only after the receiver ACKed
  (Transfer HTTP 201, or CaughtUp→SwitchAck on the S3 path). On Transfer
  refusal the donor restores (`restoreDelegated`, including tombstones via
  `ApplyTombstone`). Tests: `TestRefreshKeepsOwnershipUntilTransferAck`,
  `TestSoftLeaveRestoresOnTransferFailure`, `TestTransferFailsWhenQueueIsFull`,
  `TestRestoreDelegatedKeepsTombstones`, `TestDelegationOfferApplyAndSwitch`.
- **R2 (single mover).** All ownership movement happens under `transferMu`;
  `Refresh` is additionally single-flight. Without it, two movers Delegate
  the same key, the second ACKs an *empty* batch and drops ownership while
  the first still holds the items. Tests: `TestRefreshSingleFlight`,
  `TestSoftLeaveWaitsForInflightRefresh`, `TestTransferToPeerYieldsToLeaving`,
  `TestSoftLeaveSerializesConcurrentCalls`.
- **R3 (convergence).** `control` only donates keys in the XOR half-space
  closer to the candidate (`owned.Range(self, candidate)`), so each movement
  strictly decreases the owner–key distance; with stable membership the
  process terminates with every zone at its nearest node. Test:
  `app/simulation/mockup/convergence_test.go`.
- **R4 (no thrash while moving).** `rebalancing` pauses Feed apply on the
  classic Transfer path; the S3 delegation path keeps the donor as owner
  (and serving) until SwitchAck succeeds, flushes cutover-queued writes, then
  Delegates — so `Items()` never dips into a prep-only hole. Get serves local
  data while the donor still owns the location even if XOR nearest is already
  the receiver. A suspended peer is never planned for.
  Tests: `TestCaughtUpAckBeforeDropKeepsOwnershipOnAckFail`,
  `TestGetServesLocalWhileOwnedEvenIfNearerPeer`,
  `TestCaughtUpFlushesCutoverQueueBeforeDrop`.
- **R5 (metering).** Transfers and peer handoffs never count toward the
  autoscale insert window (`meter=false`); only client writes do. Test:
  `TestHandoffDoesNotMeterAutoscale`.
- **R6 (S3 delegation).** Large zones move via zone snapshots in the object
  store + WAL delta + write mirror. The donor does not push the full item
  batch over its own network. Cancelled sessions leave ownership unchanged.
  Multi-donor joins: `client_ready` waits for open inbound sessions (or the
  join grace). Flag: `INDEXUS_DELEGATION_S3=1`.
### I — Ingress (`ingress.go`)

Durable accept: `checkKey` (the placement walk can terminate) → WAL
`SyncAppend` → bounded `TryAdd`. Client writes are refused while joining
(`ErrJoining`), near the memory line (`ErrNodeFull`), or past `queueMax`;
peer writes get `4×` that ceiling — they are existing load seeking its
owner, and a mesh where full nodes refuse each other's handoffs deadlocks
(`TestHandoffQueuesAboveTheClientCeiling`).

- **I1 (durability).** An acknowledged write survives a crash: it is in the
  WAL before the ACK, snapshots carry the still-pending queue, and restore
  replays both. Tests: `durability_test.go`, `ingress_wal_test.go`,
  `storage_replay_test.go`, `snapshot_test.go`.
- **I2 (liveness).** Feed never drops an element: failures re-queue with
  per-worker backoff; forwards are rate-limited and fail fast on busy peers
  (`peerBusy`), and the stall table (`IngressStats`) says why a queue is not
  moving.

### P — Placement (`placement.go`)

`insert`: find the XOR-nearest *registered* node for `current`; forward if
it is a peer; otherwise apply locally, walking `current` one character up
until the owned zone accepts. The walk terminates (checkKey guarantees the
key is well-formed; falling off the root re-queues from the item's own
location).

- **P1.** A quarantined nearest peer is never used: route to the nearest
  *live* peer, or hold with `ErrOwnerUnavailable` (element re-queues, retried
  within the quarantine window). Falling back to self here is forbidden — it
  re-owns the suspended peer's zones and ping-pongs with Refresh once the
  peer recovers. Test: `dead_peer_test.go`.
- **P2 (exactly-once effect).** Delivery is at-least-once (crash replay,
  duplicated transfers); *apply* is idempotent: an existing leaf with equal
  metrics is a no-op, differing metrics replace with delta, and tombstone
  generations resolve delete/re-add races. Duplicates created by an ACK lost
  in flight heal when the two owners next converge (R3) — the second copy
  lands on the first via Transfer and is absorbed.

### J — Join, blue/green (`join.go`)

A spawned node is mesh-visible immediately (Ping, receives Transfers) but
returns `ErrJoining` to client writes until it *owns* something and its
Transfer backlog settled (or a 5 s grace). The latch (`clientPublished`)
never clears short of a leave. Test: `client_ready_test.go`.

### L — Leave (`leave.go`)

`SoftLeave` sets `leaving` *before* taking `transferMu` — an in-flight
Refresh sees the flag per key and stops moving zones, releasing the lock —
then drains every owned key with the same ACK-before-drop as R, preferring
bootstraps (the one peer that is not also leaving in a group scale-down),
then hands the residual queue over. `leaving` makes Ping fail so the ring
evicts the node (its zones must not route back), makes ingress refuse, and
pauses Feed apply. A failed drain clears `leaving` and the node resumes
serving. Drains are additionally serialized cluster-wide by the issuer's
drain lock. Tests: `leave_owned_test.go`, `leave_drain_test.go`.

### C — Recovery (`storage.go`, `checkpoint.go`, `zone_snapshot.go`)

Snapshot = topology + owned items + tombstones + pending queue; saving
rotates the WAL. Restore loads the snapshot (a corrupt one falls through to
the WAL — never both lost), then replays the log unbounded: every line is a
write a client already saw succeed. `Checkpoint` is serialized
(`checkpointMu`) because the snapshot temp-file name is fixed.

When an ObjectStore is attached (`SNAPSHOT_BUCKET` + aws-sdk):
- Dirty owned zones are uploaded under `zones/<collection>/<id>/<seq>.snap`
  and described by `nodes/<name>/manifest.json`.
- Rotated WAL segments are uploaded under `nodes/<name>/wal/…`, closing the
  RPO gap that used to lose ACKed writes on instance death between full-node
  snapshot uploads.
- `PullLatestSnapshot` prefers the manifest layout, then falls back to the
  legacy `snapshots/<node>/latest/` aws-cli sync.

Tests: `TestZoneSnapshotRoundTrip`, `TestUploadWALSegmentRecordsManifest`,
`TestDirtyTrackingMarksOwnedZone`.
### U — Cache update (`update.go`)

Owners re-pull aggregates of delegated children; cache entries refresh near
expiry with probabilistic skip η to avoid stampedes. Read path (`read.go`)
is eventually consistent by design.

## 4. Crash matrix

| Crash point | Outcome | Healing |
|---|---|---|
| Donor, after Delegate, before Transfer ACK | items only in donor WAL/snapshot | restart restores them; zone re-owned; next Refresh retries (R1) |
| Donor, after ACK, before checkpoint | receiver has the items; donor's old snapshot re-adds them on restart | duplicate ownership, absorbed by idempotent apply on next convergence (P2/R3) |
| Receiver, after ACK | batch is in receiver's WAL (ACK implies durable enqueue) | replayed on restart (I1) |
| Receiver, before ACK (donor timeout) | donor restores; receiver may still apply → duplicates | heals via P2/R3 |
| Node mid-SoftLeave | `leaving` is in-memory only | restarts as a normal member with its snapshot; nothing lost |
| Bootstrap down | mesh keeps running on gossiped tables | empty-table nodes re-acknowledge bootstraps when it returns (M3) |
| Partition heal | both sides may own the same zone | duplicate-owner healing (P2/R3) |

## 5. Accepted windows (by design)

- **Reads during movement**: between the donor's Delegate and the receiver's
  Feed apply, the zone answers empty. Reads are eventually consistent.
- **Empty zone transfer**: a zone whose items were all deleted transfers an
  empty batch and simply stops being owned; the next write re-creates it at
  the nearest node.
- **Suspended-owner stall**: writes for a quarantined peer's zones hold up to
  90 s when no live alternative exists (better than re-owning them).
- **Feed pause is global** while a transfer plan applies — acceptable because
  plans are short and serialized; a per-zone gate would buy little.
- **Add vs delete of the same id does not commute** (add-wins on the later
  arrival). Chosen so a client can always re-create what it deleted — adds
  carry no causal generation, so a "newer" delete cannot be told apart from
  an older one. Consequence: a *duplicated* stale add (residual forward,
  crash replay) can resurrect a deleted id until its writer stops retrying.
  Pinned by `TestAddTombstoneOrderDependence` and
  `TestTransferredTombstoneThenLateAddResurrects`; lifting it requires
  generation-carrying adds (an OR-set tag per write).
- **A refused write may still apply**: durability (WAL) runs before admission
  (queue TryAdd), so a write bounced with `ErrQueueFull` replays after a
  crash. Harmless — the client's retry of the same id is absorbed by
  idempotent apply — but delivery is at-least-once of everything written,
  not exactly-what-was-acknowledged. Pinned by
  `TestRefusedWriteStillReplaysFromWAL`.

## 5b. CRDT scorecard

Against the strong-eventual-consistency requirements (op-based CRDT with
at-least-once delivery):

| Requirement | Status | Evidence |
|---|---|---|
| Adds of distinct ids commute | ✓ | `TestAddOrderIndependence` |
| Duplicate apply is a no-op | ✓ | `TestAddSecondPassIsNoOp`, `TestTransferDeliveredTwiceIsIdempotent` |
| Same id, new metrics: replace-with-delta | ✓ | `TestCollectionAddReplaceMetrics` |
| Tombstones join on max generation (commutative, idempotent) | ✓ | `TestTombstoneMaxGenCommutes` |
| Delete before add ever seen (out-of-order) | ✓ tombstone-first holds | `TestRemoveAbsentPlantsTombstone` |
| Duplicate-owner state heals without double count | ✓ | `TestStaleSnapshotDuplicateOwnerHeals`, `TestConvergence_*` |
| Add/delete of the same id commute | ✗ add-wins (accepted, see §5) | `TestAddTombstoneOrderDependence` |

## 5c. Routing & transition scorecard

The Kademlia trie and the ownership-movement protocol, checked against
brute-force XOR arithmetic and end-to-end scenarios:

| Property | Status | Evidence |
|---|---|---|
| `Nearest` is the true XOR minimum (unique owner per key) | ✓ | `TestNearestIsXorMinimum` |
| `Extract` fills each k-bucket with its XOR-nearest member | ✓ | `TestExtractFillsEachBucketWithItsNearest` |
| `Range` yields exactly the candidate's half-space | ✓ | `TestRangeIsExactlyTheCandidateHalfSpace` |
| No self-donation; donation edges are antisymmetric (no ping-pong) | ✓ | `TestRangeSelfIsEmpty`, `TestRangeIsAntisymmetric` |
| Split area refuses writes until `Own` claims it | ✓ | `TestSplitAreaRefusesAddsUntilOwned` |
| `Delegate` hands subtree + tombstones exactly once, then refuses | ✓ | `TestDelegateHandsSubtreeOnceAndDropsOwnership` |
| Failed transfer restore round-trips the exact state | ✓ | `TestDelegateThenReAddRoundTrips` |
| Post-ACK: donor owns nothing, cache purged, residuals forward | ✓ | `TestHandoffThenResidualWriteForwardsToNewOwner` |
| client_ready latch survives backlog regrowth; leaving clears it | ✓ | `TestClientReadyLatchSurvivesBacklogRegrowth`, `TestLeavingNodeIsNotClientReady` |
| Path-fill caches once; dense leaves land in a shorter TTL class | ✓ | `TestGetPathFillsOnceThenServesFromCache`, `TestGetDenseLeafStampsShorterTTLClass` |
| depth=0 redirects without proxy pull (placeholder only) | ✓ | `TestGetDepthZeroRedirectsWithoutPull` |

## 6. Code map

| File | Phase |
|---|---|
| `core/node.go` | state + construction |
| `core/membership.go` | M + G server side (`Neighbors`, `Ping`) |
| `core/rebalance.go` | G + R, `Transfer` receiver |
| `core/delegation.go` | R6 S3 delegation (offer / delta / mirror / switch) |
| `core/placement.go` | P + dirty-zone + mirrorWrite |
| `core/ingress.go` | I |
| `core/join.go` | J |
| `core/leave.go` | L |
| `core/storage.go`, `core/checkpoint.go`, `core/zone_snapshot.go` | C |
| `core/update.go`, `core/read.go` | U / reads |
| `core/autoscale*.go` | scaling decisions (owns no protocol state) |
| `worker/worker.go` | scheduling |
| `domain/` | trees, sets, queue, cache — all internally locked |
| `peer/contact.go` | RPC transport (2 s discovery, 5 m transfer; `INDEXUS_TRANSFER_TIMEOUT`) |
