[← Durability](durability.md) · [Index](README.md) · [Guarantees →](guarantees.md)

# Lifecycle

The phases compose. This page reads the protocol twice as a narrative: once from
the point of view of a single zone, once from the point of view of a mesh
growing from one node to many. Nothing here is new — it is the same mechanisms,
arranged so they can be held in mind at once.

---

## 1. The life of a zone

A zone key `k = (collection, location)` on a given node is in exactly one of
four states:

```
        ┌──────────── absent ────────────┐
        │  no set, no claim              │
        │                                │
   first write lands here (P)       Transfer / applyZone arrives (R)
        ▼                                ▼
   ┌─────────┐   count ≥ delegation   ┌─────────┐
   │  owned  │ ──────────────────────►│  owned  │  (the child is a new zone,
   │         │      Add opens a       │ + child │   claimed by whoever is
   │         │      child area (P)    │  area   │   XOR-nearest to it)
   └────┬────┘                        └─────────┘
        │
        │  a peer is XOR-closer to k  →  control() plans the move (R3)
        ▼
   ┌──────────────┐   ACK   ┌───────────┐
   │ handing off  │ ───────►│ delegated │  local stub only; value refreshed
   │ (still owner,│         │           │  from the new owner via /aggregates
   │ still serves)│◄────────┤           │  (U). A write for k forwards (P).
   └──────────────┘  no ACK └───────────┘
                     restore
```

Two properties make this safe, and **both are about the edges, not the states**:

- **The `owned → delegated` edge is ACK-gated** (R1). Until the receiver
  acknowledges, the donor is still the owner and still answers reads and writes.
  Every failure path in R6 lands back on `owned`.
- **The `delegated` state carries no authority.** A delegated stub is a replica,
  and [the authority rule](aggregates.md#3-the-mechanism-authority) says only the
  current owner may write it. That is why `/children` discovery marks the edge
  without setting a value (D3).

A node can hold `owned` for a key another node also holds `owned` for. That is
the duplicate case, and it is a **legal state, not a corruption** —
[ownership.md §4](ownership.md#4-q--duplicate-ownership-and-partition-remerge).

One arrow is missing from the diagram, and its absence is the subject of
[roadmap.md §1](roadmap.md#1-scaling-down-zone-merging): there is no edge back
from `owned + child area` to plain `owned`. **Zones split and never merge.**

## 2. Growing from one node to many

Read top to bottom as the mesh grows.

**One node.** Owns `@` and everything under it. `control()` finds no candidate,
so Refresh is gossip + checkpoint only. Reads resolve locally on the first branch
of `resolveClient`. `RoutingHint` returns `nil` — the node is nearest to itself.
Nothing in D or U has work to do.

**Pressure.** `AutoscaleTick` watches owned items, memory, disk and CPU. Past a
threshold held for `PressureHold`, it computes a `PreferNear` target — an ID in
the half-space carrying about half its item weight — and asks the issuer to spawn
a node there. **Placement is chosen before the node exists**, so the new node is
born XOR-close to the load it is meant to take. That is only possible because
keys are unhashed ([model.md §3d](model.md#3d-keys-are-not-hashed-so-the-location-tree-keeps-its-shape)):
with hashed keys, "the half of the load I want to shed" is not a contiguous
region and there is no ID to aim at.

**Join.** The new node pings its bootstrap (M), gets registered, and the
bootstrap's `register` returning "actually inserted" triggers an out-of-band
Refresh (M4). It is mesh-visible immediately but refuses client writes (J).

**First handoff.** `control()` finds a candidate and donates the keys in its
half-space (R3). Every zone moves through the same repeatable `Transfer` round:
copy the zone, receive an ACK naming the final receiver, then drop only the
acknowledged residue (R1/R6). The recipient publishes itself client-ready once
the synchronous install settles (J).

**Convergence of placement.** Each Refresh strictly decreases owner–key distance
and the donation relation is antisymmetric, so with stable membership the mesh
reaches the target steady state and stops moving zones (R3).

**Convergence of aggregates.** The parent of a handed-off zone is usually on a
third node. The child pushes `Claim(child, owner)` to that parent; the parent
records the named mark and pulls the child's authoritative `/aggregates`
summary. The donor also pulls the receiver's summary immediately at the end of
`transferToPeer`, so the parent's window of staleness is one RPC rather than one
Update period.

**Departure.** Scale-down or SIGTERM triggers `SoftLeave` (L): drain every owned
zone with ACK-before-drop, hand off the residual queue, fail Ping so the ring
evicts the node. A failed drain un-leaves and the node resumes serving.

**Partition.** Each side keeps serving what it owns and each elects its own owner
for the zones it can reach; M3 re-seeds empty tables from bootstraps. On remerge
the two halves see each other again, and the duplicate claims resolve through the
ordinary placement pass — the far owner transfers its subtree to the near one and
drops the claim (Q1). **Nothing in the merge path is specific to partitions; a
remerge is just a handoff that was owed for a while.**

## 3. J — Join, blue/green

`core/join.go`.

A spawned node is mesh-visible immediately (Ping, receives Transfers) but returns
`ErrJoining` to client writes until it *owns* something and its Transfer backlog
settled (or a 5 s grace). The latch (`clientPublished`) never clears short of a
leave — a node that publishes itself and then grows a backlog again stays
published, because un-publishing would bounce clients that already routed to it.
Test: `client_ready_test.go`.

The asymmetry is the point: **accepting peer load early and client load late.**
A joining node that refused handoffs could never acquire the ownership that would
make it client-ready (R7).

## 4. L — Leave

`core/leave.go`.

`SoftLeave` sets `leaving` **before** taking `transferMu` — an in-flight Refresh
sees the flag per key and stops moving zones, releasing the lock — then drains
every owned key with the same ACK-before-drop as R, preferring bootstraps (the
one peer that is not also leaving in a group scale-down), then hands the residual
queue over.

`leaving` makes Ping fail so the ring evicts the node (its zones must not route
back), makes ingress refuse, and pauses Feed apply. **A failed drain clears
`leaving` and the node resumes serving** — a node that cannot hand its data over
must keep it, not drop it. Drains are additionally serialized cluster-wide by the
issuer's drain lock. Tests: `leave_owned_test.go`, `leave_drain_test.go`.

Note what `SoftLeave` does *not* do: it does not merge the zones it hands back
into their new owner's parent zone. The receiving node ends up with the donor's
shard boundaries, so a mesh that scales down keeps the shard count of its peak.
That is the gap [roadmap.md §1](roadmap.md#1-scaling-down-zone-merging) is about.

---

## Related

- [ownership.md](ownership.md) — the mechanisms behind every arrow on this page
- [durability.md §3](durability.md#3-the-crash-matrix) — what happens if a node dies at each step
- [roadmap.md](roadmap.md) — the missing edges

---

[← Durability](durability.md) · [Index](README.md) · [Guarantees →](guarantees.md)
