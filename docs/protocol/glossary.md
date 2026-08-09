[← Roadmap](roadmap.md) · [Index](README.md)

# Glossary

Every technical term used in the specification, what it means in general, and
what it refers to here. Entries are alphabetical. Papers that develop these
ideas — matched after the fact, not as a design lineage — are collected in
[bibliography.md](bibliography.md).

Terms are defined at the level needed to read this documentation, not at the
level needed to write a paper about them.

---

### Abelian group

A set with an associative, commutative operation, an identity, **and an inverse
for every element**. Adding the inverse to a
[commutative monoid](#commutative-monoid) is what makes subtraction meaningful.

*In Indexus:* the `Abelian` type. Deletes subtract a contribution rather than
re-scanning the subtree, which is only sound because every metric has an
inverse. It is also why `min`, `max` and percentiles are excluded from the
incremental path — see [mergeable summary](#mergeable-summary).

### ACK-before-drop

A handoff discipline: the sender only relinquishes its copy *after* the receiver
has acknowledged. The failure mode it prevents is a gap where neither side holds
the data; the failure mode it accepts is a window where both do.

*In Indexus:* [R1](ownership.md#2-guarantees). Choosing duplication over loss is
what makes [duplicate ownership](ownership.md#4-q--duplicate-ownership-and-partition-remerge)
a routine state rather than an emergency.

### Admission control

Deciding at the boundary whether to accept work, as opposed to accepting it and
degrading. The interesting part is usually *which* work to shed.

*In Indexus:* [I](writes.md#1-i--ingress). Client writes are shed at `queueMax`;
peer writes are allowed `4×` that, because they are load already inside the
system, and a mesh where full nodes refuse each other's handoffs deadlocks.

### Alpha concurrency

In Kademlia, the number of peers queried in parallel during a lookup, written α
and conventionally 3. It trades round trips against messages: too small and a
slow peer stalls the lookup, too large and every lookup floods.

*In Indexus:* `zoneFanout = 3`. See [K6](reads.md#4-guarantees).

### Anti-entropy

A background process that repeatedly compares or re-reads replicas to repair
drift, as opposed to relying on the delivery of every update. Coined in the
epidemic-algorithms literature (Demers et al., 1987); Dynamo and Cassandra use
Merkle-tree exchanges for it.

*In Indexus:* the [U](convergence.md#2-u--background-convergence) tick. Because
there is one authoritative copy, it is a *pull from the owner* rather than a
reconciliation between equals.

### Astrolabe

A system (Van Renesse, Birman & Vogels, 2003) that maintains a hierarchy of
aggregates over a large set of machines using gossip, so that any node can query
a summary of any subtree.

*In Indexus:* the same shape, applied to application data rather than to machine
attributes. The difference matters: aggregating *node* attributes has no
ownership problem, because a node is the authority on itself.

### Backoff

Increasing the delay between retries after a failure, so a struggling target is
not retried into the ground.

*In Indexus:* per-worker backoff in the Feed loop, and per-peer forward backoff
in `placement.go`. [I2](writes.md#1-i--ingress).

### CAP theorem

Under a network partition, a distributed system must give up either consistency
or availability (Brewer's conjecture, 2000; proved by Gilbert & Lynch, 2002).
The refinement worth knowing is **PACELC** (Abadi, 2012): if there is a
**P**artition, choose **A** or **C**; **E**lse, choose **L**atency or
**C**onsistency.

*In Indexus:* **AP** under CAP; roughly **PA/EL** under PACELC — both sides of a
partition keep serving what they can reach, and otherwise prefer cheap eventual
reads over linearizable coordination. Twist: writes blocked by named marks park (P5/W1)
rather than accept everywhere. Remerge by ordinary placement
([Q1](ownership.md#4-q--duplicate-ownership-and-partition-remerge)). See
[pressures §3](pressures.md#3-one-serving-copy-per-zone--authority-over-availability).

### Chord

An early distributed hash table (Stoica et al., 2001) that arranges nodes on a
ring of hashed identifiers, with each node keeping a "finger table" of
exponentially spaced shortcuts and a "successor list" for fault tolerance.

*In Indexus:* the successor list is the direct ancestor of the replication scheme
sketched in [roadmap.md §2](roadmap.md#2-replication-inside-the-mesh). The
ring-of-hashed-keys part is what Indexus deliberately does not do — see
[order-preserving partitioning](#order-preserving-partitioning).

### Commutative monoid

A set with an associative, commutative operation and an identity element — but
no inverse. Sums, maxima, unions and counts are all commutative monoids.

*In Indexus:* the aggregate algebra, before deletes are considered. The
commutativity is what makes a subtree's value independent of the order its items
arrived in. See [abelian group](#abelian-group) for what deletes add.

### Coupon collector

The classical result that drawing uniformly at random until you have seen all
`N` distinct items takes about `N ln N` draws — the last few stragglers dominate
the total.

*In Indexus:* the shape of the M5 convergence tail
([membership.md §5](membership.md#5-the-two-measured-together)). It is why
"almost complete membership" arrives quickly and *complete* membership does not,
and why widening the gossip buckets — which is not a random draw — is what makes
M5 fast enough to ship.

### CRDT

Conflict-free Replicated Data Type (Shapiro, Preguiça, Baquero & Zawirski,
2011): a data type whose replicas converge without coordination, either because
operations commute (op-based, CmRDT) or because states merge by a least upper
bound (state-based, CvRDT).

*In Indexus:* the item layer is effectively an op-based CRDT, scored in
[guarantees.md §4a](guarantees.md#4a-crdt-scorecard). The one property it fails
is add/delete commutation. Note that the *ownership* layer is not a CRDT at all
— it converges by a [variant function](#variant-function), not by a merge.

### Crash-stop failure model

Nodes fail by halting, permanently, and never send incorrect messages. The
alternatives are crash-recovery (halt and come back), omission (drop messages)
and Byzantine (arbitrary, including malicious).

*In Indexus:* crash-recovery, in practice — nodes restart from a snapshot and a
WAL. **Byzantine faults are explicitly out of scope**: a peer's `/aggregates`
answer is trusted because it claims ownership, and nothing checks that claim.

### Distributed deadlock

A cycle of processes each blocked waiting for the next, spanning machines. Harder
than the local kind because no single process can see the cycle.

*In Indexus:* [R9](ownership.md#2-guarantees). The former bidirectional
`CaughtUp`/`SwitchAck` session protocol could form this cycle and has been
retired. A handoff is now one synchronous, repeatable Transfer request with a
nominative response; no inbound/outbound session locks are held across callback
RPCs.

### Effectively-once

At-least-once delivery combined with idempotent application, so that duplicate
messages have no observable effect. Frequently and misleadingly sold as
"exactly-once"; exactly-once *delivery* is not achievable over an unreliable
network.

*In Indexus:* [P2](writes.md#2-p--placement). Nothing in the mesh attempts
exactly-once delivery; every path is designed to be replayed.

### Epidemic protocol

A protocol in which nodes exchange information with randomly chosen peers, so
that knowledge spreads like an infection. Introduced for replicated-database
maintenance by Demers et al. (1987), which also named *anti-entropy* and *rumour
mongering*.

*In Indexus:* [M5](membership.md#3-m5--random-peer-sampling). The distinctive
feature of the family is that reliability comes from randomness and repetition
rather than from acknowledgement, which is why it tolerates churn so well.

### Eventual consistency

If updates stop, all replicas eventually converge to the same value. It says
nothing about *when*, and nothing about what a read observes in the meantime.

*In Indexus:* the read contract. See [strong eventual
consistency](#strong-eventual-consistency) for the stronger version the item
layer nearly achieves.

### FLP impossibility

Fischer, Lynch & Paterson (1985): in an asynchronous system where even one
process may crash, no deterministic protocol solves consensus. It is the reason
practical systems either add timing assumptions, use randomisation, or avoid
consensus.

*In Indexus:* avoided entirely. Ownership is never *agreed*; it is *evaluated*
from the key and the membership set
([model.md §3a](model.md#3a-ownership-is-decidable-locally-with-no-consensus-and-no-tie-break)).
Disagreement between nodes is then a transient consequence of differing inputs,
resolved by convergence rather than by a decision procedure.

### Hysteresis

Making a system's response depend on its history, so that the threshold for
entering a state differs from the threshold for leaving it. Without it, a system
sitting at the threshold oscillates.

*In Indexus:* `PressureHold` in autoscale, and the gap that a
[merge threshold](roadmap.md#1-scaling-down-zone-merging) would need to keep from
the split threshold.

### Idempotence

Applying an operation twice has the same effect as applying it once.

*In Indexus:* [P2](writes.md#2-p--placement). An existing leaf with equal metrics
is a no-op; differing metrics replace with a delta; tombstone generations resolve
delete/re-add races.

### k-bucket

In Kademlia, the routing table is split into buckets by shared prefix length with
the node's own ID, and each bucket holds up to `k` contacts (conventionally 20).
Nearby regions of the keyspace are covered in fine detail, distant ones coarsely.

*In Indexus:* `Extract` (one per bucket, for routing) and `ExtractK`
(`gossipBucketWidth = 20`, for gossip answers). The
[measured ceiling](membership.md#4-g--gossip) on what a bucketed answer can
reach is a direct consequence of this structure.

### Kademlia

A distributed hash table (Maymounkov & Mazières, 2002) using
[XOR distance](#xor-metric) between hashed identifiers, with
[k-buckets](#k-bucket) and α-parallel lookups.

*In Indexus:* the metric, the bucket structure and α are taken directly. The
hashing of keys is not — see
[model.md §3d](model.md#3d-keys-are-not-hashed-so-the-location-tree-keeps-its-shape).

### Lamport clock

A scalar counter (Lamport, 1978) incremented on each local event and carried on
messages, giving a partial order consistent with causality. It orders causally
related events correctly and says nothing useful about concurrent ones.

*In Indexus:* considered and rejected for parent-stub freshness — see
[version vector](#version-vector) and
[aggregates.md §4](aggregates.md#4-the-alternative-we-rejected).

### Linearizability

Every operation appears to take effect instantaneously at some point between its
invocation and its response, consistent with real time. The strongest
single-object consistency model.

*In Indexus:* **not claimed anywhere.** Reads are
[eventually consistent](#eventual-consistency); there is no read-your-writes
guarantee across nodes.

### Mergeable summary

A sketch that can be combined associatively — HyperLogLog for distinct counts,
t-digest or KLL for quantiles (Agarwal et al., 2012, for the general framework).
Mergeable is not the same as invertible: two sketches can be combined, but a
contribution cannot generally be removed.

*In Indexus:* the route to supporting `min`, `max` and percentiles
([roadmap.md §4](roadmap.md#4-smaller-open-items)). The consequence to surface in
the API is that deletes would force a subtree re-aggregation rather than a
subtraction.

### Merkle tree

A tree whose interior nodes hold a hash of their children (Merkle, 1979), so that
two parties can locate differences between large datasets in logarithmic
exchanges. The basis of Dynamo-style anti-entropy.

*In Indexus:* structurally the same tree, with an [abelian](#abelian-group)
aggregate in place of the hash. Substituting a summable value for a hash is what
turns a difference-detection structure into a query-answering one.

### Minimal disruption

The property that adding or removing a node relocates only the keys that must
move — `O(K/N)` of them rather than a global reshuffle. The headline result of
consistent hashing (Karger et al., 1997) and of
[rendezvous hashing](#rendezvous-hashing).

*In Indexus:* inherited from the XOR-nearest rule. A joining node claims only the
keys nearest to it, which is exactly what
[R3](ownership.md#2-guarantees)'s half-space donation computes.

### Order-preserving partitioning

Partitioning by key *order* rather than by hash, so that adjacent keys land on
the same or adjacent nodes. It enables range queries and destroys the free load
balance that hashing provides. Used by Bigtable tablets, HBase regions and
CockroachDB ranges.

*In Indexus:* [model.md §3d](model.md#3d-keys-are-not-hashed-so-the-location-tree-keeps-its-shape).
The compensating mechanism for the lost balance is load-driven splitting, and it
is the reason autoscale exists at all.

### OR-set

Observed-Remove set: a [CRDT](#crdt) in which each add mints a unique tag and a
remove deletes only the tags it has observed, so a concurrent add/remove pair
resolves as add-wins *without* order dependence.

*In Indexus:* the mechanism that would lift the
[add/delete commutation window](guarantees.md#3-accepted-windows). It requires a
per-write tag in the wire format, which is why it has not been done casually.

### Peer sampling service

An abstraction (Jelasity et al., 2007) whose only job is to return a uniformly
random live peer, decoupled from whatever protocol consumes it. Cyclon, Newscast
and HyParView are implementations.

*In Indexus:* `Random` + `discover`, i.e. [M5](membership.md#3-m5--random-peer-sampling),
plays the same role. The reason it exists as a separate mechanism from gossip is
that gossip answers are sorted by distance and therefore *not* a uniform sample
— the ceiling measured in [membership.md §4](membership.md#4-g--gossip). See
[bibliography](bibliography.md#jelasity-et-al-2007--gossip-based-peer-sampling).

### Prefix Hash Tree

A trie distributed over a DHT (Ramabhadran, Ratnasamy, Hellerstein & Shenker,
2004) that supports range queries on top of a hash-based lookup layer. Its
notable robustness property: the trie's structure is an *optimisation* over a
mapping that can always be recomputed, so nodes need not agree on it.

*In Indexus:* the idea behind
[R10](ownership.md#3-r10--a-delegation-mark-always-has-an-owner-behind-it). The
delegation mark is indexing state; the object store and the live mesh are the
mapping; a disagreement between them is repaired from the mapping rather than
trusted.

### Primary-copy replication

One replica is designated the writer; the others follow. Classical (Alsberg &
Day, 1976), and the family that includes chain replication and most
leader-follower databases. The design question is always how the primary is
chosen and how failover works.

*In Indexus:* the parent-stub [authority rule](aggregates.md#3-the-mechanism-authority)
is primary-copy replication of a single value, with an unusual twist: the primary
is *computed* by the XOR rule rather than elected or leased, so failover is a
re-evaluation rather than a decision. This is what
[roadmap.md §2](roadmap.md#2-replication-inside-the-mesh) would generalise.

### Probabilistic early expiration

A cache-stampede countermeasure (Vattani, Chierichetti & Lowenstein, 2015): a key
becomes eligible for refresh slightly *before* its TTL, with a probability that
rises as expiry approaches, so refreshes of a hot key spread out instead of
firing simultaneously.

*In Indexus:* the `beta` parameter of `cache.Refresh`, together with the η = 0.3
skip and the per-tick cap. [U1](convergence.md#2-u--background-convergence).

### Quorum replication

Reads and writes each contact a subset of replicas, with `R + W > N` ensuring
overlap. Dynamo (DeCandia et al., 2007) is the canonical distributed-store
example. Any replica may accept a write, so divergence is normal and
reconciliation is required.

*In Indexus:* **rejected for the replication design**, because the
[authority rule](aggregates.md#3-the-mechanism-authority) requires exactly one
writer per zone. See [roadmap.md §2](roadmap.md#2-replication-inside-the-mesh).

### Random walk on the knowledge graph

Model the mesh as a graph where an edge means "knows about". Asking a known peer
to name one of *its* peers is a step of a random walk on that graph; repeating it
each tick reaches every node of a connected component.

*In Indexus:* the mechanism behind M5's completeness. What makes it work where
gossip does not is that the answer is drawn from the *answering* peer's pool,
which is centred on that peer rather than on the asker.

### Randomised rumour spreading

The analysis of push/pull gossip (Demers et al., 1987; Karp et al., 2000) showing
that `O(log N)` rounds suffice to reach every node with high probability, with
`O(log N)` fanout.

*In Indexus:* the source of the `sampleFanout = log2(|registered|)` figure.

### Rendezvous hashing

Also HRW, Highest Random Weight (Thaler & Ravishankar, 1998): assign key `k` to
the node maximising a deterministic score `h(node, k)`. Gives a unique owner, no
coordination, and [minimal disruption](#minimal-disruption), with no ring and no
virtual nodes.

*In Indexus:* XOR-nearest ownership is rendezvous hashing with the score function
replaced by the metric — `argmin_n d(n, k)` instead of `argmax_n h(n, k)`. That
single substitution is what buys locality
([model.md §3d](model.md#3d-keys-are-not-hashed-so-the-location-tree-keeps-its-shape))
and what costs uniform balance.

### RPO

Recovery Point Objective: how much recently acknowledged work a system may lose
when a machine dies. Distinct from RTO, Recovery Time Objective, which is how
long recovery takes.

*In Indexus:* the WAL written since the last upload
([durability.md](durability.md#2-the-object-store)). Uploading rotated WAL
segments, rather than only full-node snapshots, is what shrank it.

### SDIMS

Scalable Distributed Information Management System (Yalagandula & Dahlin, 2004):
hierarchical aggregation over a DHT with *parametric* propagation — each
attribute chooses how far up and down the tree its updates travel, so read-heavy
and write-heavy attributes get different treatment.

*In Indexus:* the closest published relative of the aggregate tree, and the
source of an idea not yet adopted: today every zone propagates its summary the
same way, regardless of whether anyone reads it.

### Segment tree

A tree over an ordered domain whose nodes hold the aggregate of their range,
answering range queries in `O(log n)`. The Fenwick tree (Fenwick, 1994) is the
compact variant for prefix sums.

*In Indexus:* the sequential ancestor of the aggregate tree. The distributed
version's extra problem is that no single process holds the whole tree, which is
where [authority](aggregates.md#3-the-mechanism-authority) comes in.

### Split-brain

Two halves of a partitioned system each continuing as though they were the whole,
typically both accepting writes. Recovery requires reconciling the divergence.

*In Indexus:* an accepted state, not an emergency. Both halves serve; on remerge
the duplicate claims resolve through the ordinary placement pass
([Q1](ownership.md#4-q--duplicate-ownership-and-partition-remerge)), and item
divergence is absorbed by [idempotent](#idempotence) apply.

### Strong eventual consistency

[Eventual consistency](#eventual-consistency) plus: any two replicas that have
received the same set of updates are in the same state, with no conflict
resolution needed at read time. The defining property of a [CRDT](#crdt).

*In Indexus:* the standard the item layer is scored against in
[guarantees.md §4a](guarantees.md#4a-crdt-scorecard).

### SWIM

Scalable Weakly-consistent Infection-style Process Group Membership (Das, Gupta &
Motivala, 2002): each node pings **one** random peer per period, falls back to
`k` indirect probes through other peers, and piggybacks membership updates on
those same messages. Work per node is `O(1)` per period and detection time is
independent of `N`. The Lifeguard refinements (2018) reduce false positives under
load.

*In Indexus:* not used. `Observe` pings every peer every tick, which is `O(N)`
per node — see [roadmap.md §3](roadmap.md#3-the-membership-ceiling).

### Tombstone

A marker recording that an item was deleted, kept because the absence of an item
is indistinguishable from never having seen it. The cost is that tombstones
accumulate and eventually need collecting.

*In Indexus:* tombstones carry a generation counter; a re-add clears one, a newer
one wins over an older (`ApplyTombstone`). They travel with a zone on every
transfer and restore, which several tests exist specifically to pin.

### Unreliable failure detector

A failure detector that may be wrong — suspecting live processes, or missing dead
ones. Chandra & Toueg (1996) classified them by completeness and accuracy and
showed which classes suffice for consensus. Every timeout-based detector is
unreliable, because a slow node and a dead node are indistinguishable in an
asynchronous system.

*In Indexus:* the Ping/quarantine mechanism. Everything downstream is designed
around its unreliability: the quarantine expires rather than being permanent, and
[R10](ownership.md#3-r10--a-delegation-mark-always-has-an-owner-behind-it) treats
a run of silence as a trigger rather than a verdict.

### Variant function

A quantity that strictly decreases on every step of a process, over an order with
no infinite descending chain, therefore proving that the process terminates.
Floyd's method for loops; the same argument works for distributed convergence.

*In Indexus:* [R3](ownership.md#2-guarantees). The sum of owner–key distances
decreases on every zone movement and cannot decrease forever. That one argument
covers misplacement, duplicate ownership, partition remerge and stale-snapshot
revenants, which is why none of them has dedicated repair code.

### Version vector

One counter per node, compared componentwise (Parker et al., 1983), giving a
partial order that detects concurrent updates instead of silently ordering them.

*In Indexus:* the natural repair for the rejected epoch counter, and still not
enough — it would restore cross-node comparability (V2) but not survive a handoff
for free (V3), because the vector has to be handed over at the cutover, which is
itself a distributed agreement about an instant.
[aggregates.md §4](aggregates.md#4-the-alternative-we-rejected).

### Visited set

Carrying the set of nodes a request has already crossed, and refusing it when it
returns to one. Terminates on the actual topology rather than after a fixed
number of hops, and costs one entry per hop in the message.

*In Indexus:* the `via` list on reads ([K8](reads.md#4-guarantees)) and writes
([P5/W1](writes.md#2-p--placement)). Each request refuses a peer already in the
list and also has a fixed cap, so divergent routing views terminate without
remembering a failed tree position.

### Write-ahead log

Record the intent durably before performing the operation, so that a crash can be
recovered by replaying the log. Redo-only here: there are no undo records because
there are no transactions to roll back.

*In Indexus:* [I1](writes.md#1-i--ingress). `SyncAppend` runs before the client
ACK, which is why a refused write can still replay — see the
[accepted windows](guarantees.md#3-accepted-windows).

### XOR metric

`d(x, y) = x ⊕ y`, read as an unsigned integer. It is a genuine metric —
symmetric, zero only on equality, and it satisfies the triangle inequality — with
two extra properties: distances are unique for a fixed `x`, and the space
partitions into disjoint bands by shared prefix length.

*In Indexus:* the whole of [model.md §3](model.md#3-what-the-xor-metric-buys-and-what-it-does-not),
including the property it is routinely assumed to have and does not.

---

## Related literature

Terms above name ideas; the papers that develop them — verified against their
content, with explicit limits of each analogy to Indexus — are in
[bibliography.md](bibliography.md). That page also states the design ambition
(nearby + incremental over a linear feed) and the no-lineage disclaimer.

---

[← Roadmap](roadmap.md) · [Bibliography →](bibliography.md) · [Index](README.md)
