# The Indexus Mesh Protocol

This folder is the reference specification of the mesh: what the network is,
what it guarantees, how each mechanism works, and what it deliberately does
not promise.

It is written to be read in order the first time and jumped into afterwards.
Start here, then follow [the reading path](#reading-path). Unfamiliar terms are
defined in the [glossary](glossary.md). Academic work that attacks the same
problems — after the fact, as enrichment, not as lineage — lives in the
[bibliography](bibliography.md). Advantages, costs, proofs and known exits for
each structural decision — beside the protocol — are in
[pressures.md](pressures.md). Why any of this matters outside engineering —
who captures value when discovery is enclosed, what an open indexing standard
changes, who runs the nodes, and who decides the order results appear in — is
argued in [`docs/`](../../../docs/unbundling-discovery.md)
([thesis](../../../docs/unbundling-discovery.md) ·
[node economics](../../../docs/who-runs-the-nodes.md) ·
[ordering](../../../docs/ordering-and-fairness.md)).

---

## 1. The design target: nearby, incremental, linear feed

The mesh was built so a client can do two things that usually fight each other:

1. **Nearby** — answer “what is around here?” by walking location prefixes (and
   expanding neighbour cells), without a second spatial index.
2. **Incremental** — refine that answer by loading *deltas* of pre-materialised
   aggregates, then leaves, rather than re-fetching pages or re-scanning a
   region.

Both need the same substrate: a prefix tree whose interior nodes already hold
the abelian summary of their subtree, keyed so a region is a contiguous range,
and updated so each write costs work proportional to location depth — a
**linear feed** of inserts and deletes that maintain the tree, never a
periodic recompute. The SDK’s Aggregate and Nearby views share that tree: the
client plans the load; the mesh’s job is to keep the tree true as data outgrows
one machine.

Everything else in this folder — XOR ownership, unhashed keys, authority for
parent stubs, handoff, membership — is load-bearing for that target. The
[bibliography §1](bibliography.md#1-the-ambition-that-organises-everything-else)
spells out the constraint table and which published systems rhyme with each
piece.

**No academic lineage.** The protocol was constructed autodidactically. Papers
cited in these pages were matched afterwards to similar problems, ambitions, or
solutions. They enrich the corpus; they did not prescribe the design. See the
[bibliography](bibliography.md) for verified mappings and explicit limits of
each analogy.

---

## 2. What the network is

A collection is a **prefix tree of sets**. A location is a string over a
64-character alphabet, `Parent` strips one character, the root is `@`. Leaves
are items; every interior node carries the **aggregate** of its subtree — a
count plus a vector of metrics. A *zone* is a subtree that one node serves.

The mesh is a set of peer nodes that divide that tree between them. There is no
coordinator, no metadata service, no leader and no quorum. Ownership of a zone
is not negotiated: it is *computed*. Every node and every zone key lives in the
same 96-bit identifier space, and the owner of a key is the live node whose ID
is XOR-nearest to it — a total function of the key and the membership set,
which two nodes holding the same view evaluate identically without exchanging
a message.

Six properties characterise the design. Each is a choice with a cost, and the
cost is named next to it.


| Property                                                                                                 | What it buys                                                                                                                                                                                                      | What it costs                                                                                                                                   |
| -------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------- |
| **Ownership by XOR distance** ([rendezvous hashing](glossary.md#rendezvous-hashing) with a metric score) | one owner per key, never a tie, no consensus, no lease; a membership change moves only the keys near the change                                                                                                   | nothing about *agreement* — two nodes with different views compute different owners, and the protocol has to absorb that rather than prevent it |
| **Keys are not hashed**                                                                                  | a subtree is a contiguous range of the keyspace: a handoff is one range, a spatial read is a local walk, aggregates are cheap                                                                                     | load follows the data's own distribution; the mesh cannot lean on hashing to balance                                                            |
| **Load-driven splitting**                                                                                | the mesh grows where the pressure is, and a new node is born XOR-close to the load it will take                                                                                                                   | a split is a decision under uncertainty, not an arithmetic fact; thresholds and hysteresis have to be tuned                                     |
| **Aggregates are an [abelian](glossary.md#abelian-group) algebra**                                       | a subtree's value is the sum of its children's, order-independent, incrementally updatable; a client can load deltas rather than pages                                                                            | requires every metric to have an inverse, which excludes min/max/percentiles from the incremental path                                          |
| **One serving copy per zone**                                                                            | no replica divergence, no read repair, no quorum latency; the [authority rule](aggregates.md#3-the-mechanism-authority) that keeps parent aggregates correct is only possible because there is exactly one writer | an owner's death is a read outage for its zones until the zone is reclaimed and rematerialised                                                  |
| **Durability is delegated to an object store**                                                           | the store is the authority for *what a zone held*; ownership only names *who serves it*, so the two can be repaired independently                                                                                 | a durability dependency outside the mesh, and an RPO equal to the WAL written since the last upload                                             |


In [CAP](glossary.md#cap-theorem) terms the mesh is **AP**: during a partition
both halves keep serving what they can reach, and the divergence is resolved on
remerge. Reads are [eventually consistent](glossary.md#eventual-consistency);
no [linearizability](glossary.md#linearizability) is claimed anywhere. The
failure model is [crash-stop](glossary.md#crash-stop-failure-model): nodes stop,
they do not lie.

## 3. What it is good at, and what it is not

**Good at.** Reading an aggregate over a spatial region, at any zoom level, in
one hop. Growing and shrinking with load without an operator. Surviving the
death of any node, including every bootstrap, without losing acknowledged
writes. Letting a client plan its own incremental loading, because the tree it
walks is the tree the mesh stores.

**Not good at, by construction.** Anything that needs a global instant: there
is no snapshot of the whole collection, no transaction across zones, no
read-your-writes across nodes. Skewed workloads: a hot region is a hot
contiguous key range and only splitting relieves it. Metrics without an
inverse: min, max and percentiles cannot ride the incremental path.

**Not good at yet, but should be.** Scaling *down* — zones split and never
merge, so a mesh that grew for a peak keeps the shard count after it. And
replication — the mesh has exactly one serving copy of each zone and leans on
an external object store for durability. Both are the subject of
[the roadmap](roadmap.md), which is the most useful page here if you are
deciding what to build next.

## 4. How to read this

Every mechanism in these pages is labelled by phase and number — `M1`, `R3`,
`P5`, `K6`. A property is only written as a **guarantee** if a named test
enforces it; the test names are given inline so a claim can be checked against
the suite rather than believed. Three other kinds of statement are kept
strictly apart from guarantees:

- **Accepted windows** — behaviour we know about and chose, listed in
[guarantees.md](guarantees.md#3-accepted-windows).
- **Known defects** — properties the design intends and the implementation does
not have, each with the mechanism that would establish it
([guarantees.md](guarantees.md#5-known-defects)).
- **Open work** — designs not yet built ([roadmap.md](roadmap.md)).



### Reading path

**To understand the system** — read in this order:

1. [The model](model.md) — identifiers, zones, the XOR metric, and the two
  different notions of "correct" that this protocol keeps apart.
2. [Aggregates](aggregates.md) — the algebra, and why parent summaries need a
  rule the algebra cannot provide.
3. [Membership](membership.md) — how a node learns the mesh, and why gossip
  alone provably cannot finish the job.
4. [Writes](writes.md) — ingress, placement, and what happens to a write with
  nowhere to go.
5. [Ownership](ownership.md) — rebalancing, repeatable handoff rounds, and
  duplicate ownership as a legal state.
6. [Reads](reads.md) — the two kinds of read, fan-out bounds, and the client
  ingress hint.
7. [Convergence](convergence.md) — Claim discovery and background aggregate pulls.
8. [Durability](durability.md) — WAL, snapshots, the object store, and the
  crash matrix.
9. [Lifecycle](lifecycle.md) — the life of a zone, and the mesh growing from
  one node to many.

**To check a claim** — go straight to [guarantees.md](guarantees.md): the
scorecards, the accepted windows and the known defects, all with their tests.

**To find where something lives** — [the code map](#6-code-map) below.

**To decide what to build** — [roadmap.md](roadmap.md).

**To place the design among published systems** — [bibliography.md](bibliography.md)
(analogies only; no lineage).

**To stress-test a decision against published results** —
[pressures.md](pressures.md) (advantage / cost / evidence / known exit).

## 5. The phases at a glance

The code is organised to mirror the protocol: one file per phase under `core/`.
Each phase runs on the worker cadence (`Delay()` — 10 s normally, 2 s for a
spawned node that is not yet client-ready), with the first pass immediately at
boot.


| Phase | Name                   | Question it answers                            | Page                                                                        |
| ----- | ---------------------- | ---------------------------------------------- | --------------------------------------------------------------------------- |
| **M** | Membership             | who else is alive?                             | [membership.md](membership.md)                                              |
| **G** | Gossip                 | who do *they* know?                            | [membership.md](membership.md#4-g--gossip)                                  |
| **I** | Ingress                | is this write durable and admitted?            | [writes.md](writes.md#1-i--ingress)                                         |
| **P** | Placement              | which node should hold this item?              | [writes.md](writes.md#2-p--placement)                                       |
| **R** | Rebalance              | which zones should move, and how?              | [ownership.md](ownership.md)                                                |
| **Q** | Duplicate resolution   | two owners for one zone — now what?            | [ownership.md](ownership.md#4-q--duplicate-ownership-and-partition-remerge) |
| **D** | Child discovery        | which children of my zone exist elsewhere?     | [convergence.md](convergence.md#1-d--child-discovery)                       |
| **U** | Background convergence | what is each of them worth?                    | [convergence.md](convergence.md#2-u--background-convergence)                |
| **K** | Reads                  | who can answer this read?                      | [reads.md](reads.md)                                                        |
| **X** | Client ingress hint    | is the client still talking to the right node? | [reads.md](reads.md#3-x--client-ingress-hint)                               |
| **J** | Join                   | when may a new node take client traffic?       | [lifecycle.md](lifecycle.md#3-j--join-bluegreen)                            |
| **L** | Leave                  | how does a node hand everything back?          | [lifecycle.md](lifecycle.md#4-l--leave)                                     |
| **C** | Recovery               | what survives a crash?                         | [durability.md](durability.md)                                              |




## 6. Code map


| File                                                             | Phase                                                                                                                                                                 |
| ---------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `core/node.go`                                                   | state + construction                                                                                                                                                  |
| `core/membership.go`                                             | M + G server side (`Neighbors` at `gossipBucketWidth`, `Ping`, `Random`), M5 random peer sampling (`discover`, `sampleFanout`), narrow `routingNeighbors` for `clean` |
| `core/rebalance.go`                                              | G + R, `Transfer` receiver                                                                                                                                            |
| `core/repair.go` / `core/park.go`                                | Claim repair loop (D1/D3, R10), parked writes under named marks                                                                                                         |
| `core/placement.go`                                              | P + dirty-zone + the owned-key inventory, per-peer forward backoff                                                                                                    |
| `core/ingress.go`                                                | I, including the queue element                                                                                                                                        |
| `core/join.go`                                                   | J                                                                                                                                                                     |
| `core/leave.go`                                                  | L                                                                                                                                                                     |
| `core/storage.go`, `core/checkpoint.go`, `core/zone_snapshot.go` | C — line codec, policy, incremental object store                                                                                                                      |
| `core/update.go`                                                 | U parent-stub pulls; `repair()` for Claim / named-mark verify (R10); silence reclaim retired                                                                         |
| `core/read.go`                                                   | K + X                                                                                                                                                                 |
| `core/autoscale.go`                                              | scaling decisions (owns no protocol state)                                                                                                                            |
| `core/autoscale_node.go`                                         | wiring those decisions to ownership                                                                                                                                   |
| `worker/worker.go`                                               | scheduling                                                                                                                                                            |
| `domain/`                                                        | trees, sets, queue, cache — all internally locked                                                                                                                     |
| `domain/contact.go`                                              | everything that crosses between nodes (`Contact`, `Visited`, `ChildEntry`, `ClaimPayload`)                                                                            |
| `domain/ownership.go`                                            | `Key` — the zone every phase agrees on — and the split threshold                                                                                                      |
| `traffic/`                                                       | per-route RPC accounting **implemented, not wired** into the production mux; package tests cover the map bounds                                                                 |
| `peer/contact.go`                                                | RPC transport (2 s discovery, 5 m transfer; `INDEXUS_TRANSFER_TIMEOUT`)                                                                                               |




## 7. Node state


| Field                  | Meaning                                           | Phase |
| ---------------------- | ------------------------------------------------- | ----- |
| `acknowledged`         | contacts heard of, not yet verified               | M     |
| `registered`           | contacts that answered a Ping (or were gossiped)  | M     |
| `routing`              | XOR k-bucket extract of `registered` + self       | M     |
| `suspect`              | quarantined names (dead peers), with expiry       | M     |
| `owned`                | BST of zone keys this node serves                 | R/P   |
| `collections`          | the data: sets, aggregates, tombstones            | P     |
| `queue`                | ingress elements pending placement                | I     |
| `cache`                | soft cache of remote sets (TTL by hops)           | U     |
| `storage`              | WAL + snapshot                                    | C     |
| `parked`               | durable writes blocked by named child marks       | I/R10 |
| `parentSyncAt`         | per-key debounce for background pulls             | U/Q   |
| `leaving`              | drain in progress; reject Ping/ingress            | L     |
| `rebalancing`          | Refresh is applying a transfer plan               | R     |
| `transferMu`           | **the single-mover lock**: any ownership movement | R/L   |
| `refreshBusy`          | single-flight latch for Refresh                   | R     |
| `clientPublished`      | blue/green latch: safe for client XOR routing     | J     |
| `full`                 | memory watchdog: refuse client writes             | I     |


`TransferBusy() = rebalancing` is the global "ownership is moving" gate. It
suppresses cache refresh while a synchronous handoff round is applying.

The `owned` tree stores a `map[domain.Key]any` per node and writers mutate it
in place, so the map must never leave the lock: `Get` hands out a live
reference and a caller that reads it afterwards races the writers — in Go a
concurrent map read/write is a runtime throw, not a warning. `BST.Read` is the
read-locked accessor for that shape. Test:
`TestOwnedTreeSurvivesConcurrentClaimsAndDirtyMarks` (run under `-race`).

---



## Index


| Page                             | Contents                                                                                         |
| -------------------------------- | ------------------------------------------------------------------------------------------------ |
| [model.md](model.md)             | identifiers, zones, tombstones, the placement/freshness split, what XOR buys and does not        |
| [aggregates.md](aggregates.md)   | the abelian algebra, the parent-stub problem, the authority rule, the version-counter dead end   |
| [membership.md](membership.md)   | M (ping ladder, quarantine), G (gossip), M5 (random peer sampling), the convergence measurements |
| [writes.md](writes.md)           | I (durable accept, admission control), P (stateless walk, parking, idempotence)                   |
| [ownership.md](ownership.md)     | R (the transfer plan), R6 (repeatable Transfer), R8–R10 (park + Claim), Q (duplicate ownership) |
| [reads.md](reads.md)             | K (client vs refresh reads, fan-out, staleness), X (ingress hint)                                |
| [convergence.md](convergence.md) | D (Claim discovery), U (background pulls and cache refresh)                                      |
| [durability.md](durability.md)   | C (WAL, snapshots, object store), the crash matrix                                               |
| [lifecycle.md](lifecycle.md)     | the life of a zone, J, L, growing from one node to many                                          |
| [guarantees.md](guarantees.md)   | accepted windows, CRDT scorecard, routing scorecard, known defects                               |
| [roadmap.md](roadmap.md)         | scale-down and zone merging, in-mesh replication, the membership ceiling                         |
| [pressures.md](pressures.md)     | beside the protocol: advantage / cost / evidence / known exit per structural decision            |
| [glossary.md](glossary.md)       | every academic term used here, with a short “in Indexus” note                                    |
| [bibliography.md](bibliography.md) | related papers verified against their content; analogies, not lineage; nearby/incremental ambition |


