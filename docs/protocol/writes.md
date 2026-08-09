[← Membership](membership.md) · [Index](README.md) · [Ownership →](ownership.md)

# The write path

`core/ingress.go` and `core/placement.go`.

A write crosses two phases: **I** makes it durable and decides whether to admit
it, **P** decides which node holds it. They are separate because durability must
not depend on placement succeeding — a write with nowhere to go yet is still a
write the client was told about.

---

## 1. I — Ingress

Durable accept, in order: `checkKey` (the placement walk can terminate) → WAL
`SyncAppend` → bounded `TryAdd`.

Client writes are refused while joining (`ErrJoining`), near the memory line
(`ErrNodeFull`), or past `queueMax`. **Peer writes get `4×` that ceiling** —
they are existing load seeking its owner, and a mesh where full nodes refuse
each other's handoffs deadlocks (`TestHandoffQueuesAboveTheClientCeiling`). The
asymmetry is the whole of the [admission control](glossary.md#admission-control)
policy: shed new load, never shed load already inside the system.

- **I1 (durability).** An acknowledged write survives a crash: it is in the WAL
  before the ACK, snapshots carry the still-pending queue, and restore replays
  both. Tests: `durability_test.go`, `ingress_wal_test.go`,
  `storage_replay_test.go`, `snapshot_test.go`.
- **I2 (liveness).** Feed never drops an element: failures re-queue with
  per-worker [backoff](glossary.md#backoff); forwards are rate-limited and fail
  fast on busy peers (`peerBusy`), and the stall table (`IngressStats`) says why
  a queue is not moving.

Note the ordering consequence: durability runs *before* admission, so a write
bounced with `ErrQueueFull` is still in the WAL and replays after a crash. That
is an [accepted window](guarantees.md#3-accepted-windows), not a defect —
delivery is at-least-once of everything written, not exactly-what-was-acknowledged.

## 2. P — Placement

`insert`: every attempt starts at `item.Location` (no walk memo). Find the
XOR-nearest *registered* node for the current ancestor; forward if it is a peer
not already in `via`; otherwise apply locally, walking one character up until an
owned zone accepts or the root is reached. `via` (`domain.Visited`) grows on
each forward so divergent membership cannot trade a write forever; it is cleared
on requeue so a refusal does not poison the next attempt.

### Guarantees

**P1 — a quarantined peer is never routed to.** Route to the nearest *live*
peer, or hold with `ErrOwnerUnavailable` (element re-queues, retried within the
quarantine window). Falling back to self here is forbidden: it re-owns the
suspended peer's zones and ping-pongs with Refresh once the peer recovers.
Test: `dead_peer_test.go`.

**P2 — exactly-once effect.** Delivery is at-least-once (crash replay,
duplicated transfers); *apply* is [idempotent](glossary.md#idempotence): an
existing leaf with equal metrics is a no-op, differing metrics replace with
delta, and tombstone generations resolve delete/re-add races. Duplicates created
by an ACK lost in flight heal when the two owners next converge (R3) — the
second copy lands on the first via Transfer and is absorbed. At-least-once
delivery plus idempotent apply is what
[effectively-once](glossary.md#effectively-once) means here; nothing in the mesh
attempts exactly-once *delivery*.

**P3 — structural integrity.** A set never contains a key that aliases itself,
and no traversal can loop: `shrink` refuses `child == key` and guards the root
precision case, and `traverseWalk` carries a `visited` set, validates
`IsDirectChild`, and copies entries under the `Set` lock before recursing
*outside* it. Tests: `TestShrinkRootItemsDoNotSelfAlias`,
`TestTraverseSelfKeyNoStackOverflow`, `TestTraverseSiblingCycleNoStackOverflow`,
`TestIsDirectChild`.

**P4 — the key space belongs to locations.** No marker may live in the alphabet:
all 64 characters — `-` and `_` included — are legal locations, so a set carrying
a total without a child list holds it on its aggregate, not under a reserved key.
Reserving `"_"` for that purpose made `IsDirectChild` reject the zone `_`, and
traverse then walked past every item stored under it: present in the collection,
counted by nobody. Tests: `TestSummaryPlaceholderCarriesNoReservedKey`,
`TestItemsUnderEveryLetterAreCounted`.

**P5 — a placement attempt is a pure function of the address and the local
membership view.** There is no walk memo (`current` is gone). The same item
retried after a refusal climbs again from `item.Location` with an empty `via`.
What used to be "never re-descend a failed walk" is abrogated: under a split
during flight the holder can move *under* a truncated walk, and only restarting
from the address can reach it.

**W1 — a write terminates on its own path.** Symmetrical to K8: `via` names
peers that already declined this attempt and is capped (`viaCap`). Falling off
the root returns `ErrOwnerUnavailable`; a local nearest that refuses under a
mark returns `ErrDelegatedTo` and the ingress **parks** the element under that
child rather than spinning on the hot queue or absorbing into an ancestor
(Shrink would corrupt aggregates).

Tests: `TestAWriteUnderDelegatedChildForwardsFromTheAddress`,
`TestAFailedAttemptUnderMarkParksWithEmptyVia`,
`TestParkedWriteWakesOnClaim`, `model_test.go` (memo vs via).

> **Why the old P5 existed.** A memoised climb plus self-retry produced a
> line-rate loop in production (`_vSrV` stuck above `7x0`). The fix is not to
> stall harder but to delete the memo and bound the attempt with `via`.

## 3. The interaction that matters

P5/W1 and R10 are two halves of one fix:

| | P5 / W1 | R10 |
|---|---|---|
| Concern | every attempt restarts from the address; hops are bounded | a mark never blocks progress |
| Timescale | immediate | Claim / membership verdict |
| What it never does | infer topology from silence | reclaim on silence alone |

A write under a named mark parks; Claim (or membership death of the named peer)
wakes the garage.

---

## Related

- [ownership.md R10](ownership.md#3-r10--a-delegation-mark-never-blocks-progress) — park + Claim
- [guarantees.md](guarantees.md#3-accepted-windows) — via-bounded writes and parked depth
- [durability.md](durability.md) — what the WAL guarantees to I1

---

[← Membership](membership.md) · [Index](README.md) · [Ownership →](ownership.md)
