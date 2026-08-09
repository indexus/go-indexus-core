[← Convergence](convergence.md) · [Index](README.md) · [Lifecycle →](lifecycle.md)

# Durability and recovery

`core/storage.go`, `core/checkpoint.go`, `core/zone_snapshot.go`.

Phase **C**. The mesh's durability contract is I1: an acknowledged write
survives a crash. Everything on this page exists to hold that line, and to keep
holding it while ownership is moving — which is where the interesting problems
are.

---

## 1. WAL and snapshot

Snapshot = topology + owned items + tombstones + pending queue; saving rotates
the WAL. Restore loads the snapshot (a corrupt one falls through to the WAL —
**never both lost**), then replays the log unbounded: every line is a write a
client already saw succeed.

This is the ordinary [write-ahead log](glossary.md#write-ahead-log) discipline,
redo-only: there are no undo records because there are no transactions to roll
back. `Checkpoint` is serialized (`checkpointMu`) because the snapshot temp-file
name is fixed.

### The handoff gap, and `SealRotate`

`Checkpoint` must abstain during a handoff — it holds an `RLock` on the whole
collection — but the live WAL would then grow unbounded. A long delegation of a
large zone is exactly when the write rate is highest and the checkpoint is
forbidden.

`SealRotate` closes that gap: past 1 MiB it archives the live WAL *without*
taking a snapshot and marks the state dirty; `Stream` replays `pendingSealed`
then the live log with one continuous offset. Test:
`TestSealRotateReplaysInStream`.

## 2. The object store

When an ObjectStore is attached (`SNAPSHOT_BUCKET` + aws-sdk):

- Dirty owned zones are uploaded under `zones/<collection>/<id>/<seq>.snap` and
  described by `nodes/<name>/manifest.json`.
- Rotated WAL segments are uploaded under `nodes/<name>/wal/…`, closing the
  [RPO](glossary.md#rpo) gap that used to lose ACKed writes on instance death
  between full-node snapshot uploads.
- `PullLatestSnapshot` prefers the manifest layout, then falls back to the legacy
  `snapshots/<node>/latest/` aws-cli sync.

Tests: `TestZoneSnapshotRoundTrip`, `TestUploadWALRecordsManifest`,
`TestDirtyTrackingMarksOwnedZone`.

### The store is the authority for content, not for ownership

This split is used twice and is worth stating on its own:

> **The object store says what a zone held. The mesh says who serves it.**

- [R10](ownership.md#3-r10--marks-parking-and-claim) keeps blocked writes durable
  in the parked garage until a named holder answers or membership explicitly
  changes authority.
- [R6](ownership.md#2-guarantees) copies every handoff payload to the receiver
  before the donor drops the acknowledged zone.

Content and ownership therefore remain recoverable independently. It is also
the reason replication is currently *outside* the mesh
([roadmap.md §2](roadmap.md#2-replication-inside-the-mesh)).

## 3. The crash matrix

| Crash point | Outcome | Healing | Pinned by |
|---|---|---|---|
| Donor, after Delegate, before Transfer ACK | items only in donor WAL/snapshot | restart restores them; zone re-owned; next Refresh retries (R1) | `TestSoftLeaveRestoresOnTransferFailure` (process-local restore); full crash-restart pin not yet restored |
| Donor, after ACK, before checkpoint | receiver has the items; donor's old snapshot re-adds them on restart | duplicate ownership, absorbed by idempotent apply on next convergence (P2/R3/Q1) | `TestStaleSnapshotDuplicateOwnerHeals` |
| Receiver, after ACK | each transferred item was synchronously applied and appended before the ACK | replayed on restart (I1) | transfer/storage regression suite |
| Receiver, before ACK (donor timeout) | donor restores; receiver may still apply → duplicates | heals via P2/Q1 | `TestTransferDeliveredTwiceIsIdempotent` |
| Transfer refused or interrupted before nominative ACK | donor restores the delegated residue locally | the next Refresh repeats the same handoff round (R6/R8) | `TestSoftLeaveRestoresOnTransferFailure`, `TestConvergence_LossyTransfer_RecoversWithoutLoss` |
| Receiver encounters a named delegated child while applying | blocked leaves enter the durable parked garage | Claim or a membership verdict wakes and replays them from their address | `TestCheckpointKeepsParkedIngressOutsideHotQueue`, `TestParkedWriteWakesOnClaim`, `TestReclaimParkedWhenNamedPeerEvicted` |
| Node mid-SoftLeave | `leaving` is in-memory only | restarts as a normal member with its snapshot; nothing lost | not yet pinned (process restart mid-leave) |
| Bootstrap down | mesh keeps running on gossiped tables | empty-table nodes re-acknowledge bootstraps when it returns (M3) | `TestObserveSubscribesNewPeersIntoRouting` |
| Partition heal | both sides own the same zone, with diverged items | the far owner transfers and drops the claim; union by idempotent apply (Q1) | `TestConvergence_PartitionRemergeKeepsTheUnion` |

Read the *Healing* column as a whole and one pattern stands out: **every row
resolves into duplicate ownership or into a retry, and never into loss.** That
is deliberate. The two mechanisms that absorb them — idempotent apply (P2) and
convergent placement (R3) — are the only recovery machinery the protocol has,
and every failure mode is deliberately funnelled into one of them rather than
given its own handler.

---

## Related

- [writes.md I1](writes.md#1-i--ingress) — the durability contract this page implements
- [ownership.md](ownership.md#4-q--duplicate-ownership-and-partition-remerge) — the state most crash paths land in
- [guarantees.md](guarantees.md#3-accepted-windows) — the refused-write-still-replays window

---

[← Convergence](convergence.md) · [Index](README.md) · [Lifecycle →](lifecycle.md)
