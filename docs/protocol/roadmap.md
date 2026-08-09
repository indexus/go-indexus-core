[← Guarantees](guarantees.md) · [Index](README.md) · [Glossary →](glossary.md) · [Pressures](pressures.md)

# Open work

**Nothing on this page is implemented.** These are the structural gaps large
enough to be projects rather than defects, written up with the constraints each
one has to respect so that the design work does not have to start from scratch.

The three main items are not independent: §2 largely dissolves §3, and §1 is the
prerequisite for the mesh being able to shrink at all. §5 sequences them.

Published advantages, costs and known exits for the decisions these items
relax (split-only zones, single copy, full-mesh Observe) are collected in
[pressures.md](pressures.md) — especially §1, §3, §4 and §7.

---

## 1. Scaling down: zone merging

### The gap

Ownership splits when a zone's running count reaches the delegation size.
**Nothing ever merges it back.** There is no arrow from `owned + child area`
back to plain `owned` in [the zone lifecycle](lifecycle.md#1-the-life-of-a-zone),
and `SoftLeave` hands zones to their new owner with the donor's shard boundaries
intact.

The consequence is that **fragmentation is monotonic in the peak, not in the
current load.** A collection that grew to a hundred zones during an ingest and
then had most of its items deleted still has a hundred zones. That is not just
untidy; each zone costs:

| Per-zone cost | Where |
|---|---|
| an entry in `owned` and a dirty-tracking unit | `placement.go` |
| a zone snapshot object per sequence | `zone_snapshot.go` |
| a `/children` entry and a delegation mark | `update.go` |
| **one `pullDelegatedAggregate` per Update tick** | `update.go` |

The last row is the one that bites: the Update tick is `O(delegated children)`,
so over-fragmentation is a permanent per-tick RPC tax that no amount of idleness
relieves.

It also blocks node-level scale-down from being useful. `SoftLeave` works, but a
mesh that drains a node redistributes its fragments rather than consolidating
them, so the surviving nodes inherit the peak's layout.

### What a merge has to respect

**A trigger with hysteresis.** Split at `delegationSize`, merge below
`mergeSize`, with `mergeSize` comfortably under `delegationSize / 2`. Without
that band, two halves merge into a zone that immediately exceeds the split
threshold and the pair oscillates forever. This is the standard arrangement in
range-partitioned stores — HBase region merge, Bigtable tablet merge,
CockroachDB range merge all use a low watermark plus a cool-down — and the
[hysteresis](glossary.md#hysteresis) is not optional.

**Pull-up, not push-down.** The decision belongs to the *parent's* owner: it
holds the child stubs and can sum them, whereas a small child cannot see its
siblings. So the parent notices `sum(children) < mergeSize`, asks each child
owner to hand its zone back, applies the items, and then lifts the delegation
mark.

That sequence is worth staring at, because **it is R10 with a different
trigger.** `Collection.Reclaim` already lifts a mark, `applyZone` already
installs a zone's contents, and `reconcileReclaimedStub` already rewrites the
parent's stub afterwards. A merge is the same three steps with a live donor
instead of a snapshot, which makes this the cheapest of the three projects on
this page.

**Acquire before you lift.** The mark must be lifted *after* the parent holds
the data, never before — a mark lifted early is precisely the orphan condition
that [P5](writes.md#2-p--placement) turns into a stalled write. In other words a
merge is [ACK-before-drop](glossary.md#ack-before-drop) run backwards, and it
must carry tombstones the way `restoreDelegated` does, or deleted ids resurrect.

**One mover, one direction.** A merge is an ownership movement, so it runs under
`transferMu` (R2) and claims the direction slot with the peer (R9). And it must
compose with R3: after the merge, the merged zone's key is the *parent* key,
whose XOR-nearest node may be a third node entirely — so a merge may be followed
immediately by a move. That is correct, but the two must not interleave.

### The unresolved question

`SoftLeave` places its zones by the placement order, and **the placement order
knows nothing about load.** A drained node hands its zones to the XOR-nearest
peers, which may be the busiest ones. Today that is masked because draining is
rare and manual. A symmetric autoscale — sustained low pressure triggers a drain,
with a node-count floor and a cool-down against spawn/drain oscillation — would
make it routine, and the question becomes real: should a drain be allowed to
prefer an under-loaded peer over the nearest one?

Answering "yes" means introducing a second, load-aware ordering alongside the
XOR one, and [model.md §2](model.md#2-the-two-orders) is a standing warning about
how much trouble a second order causes. Answering "no" means accepting that a
scale-down can create the hotspot that triggers the next scale-up. This should
be settled before the merge work lands, not after.

## 2. Replication inside the mesh

### What is missing, precisely

Durability is already handled: the object store holds zone snapshots and rotated
WAL segments ([durability.md](durability.md#2-the-object-store)), and that
service replicates underneath us. **What the mesh lacks is availability.**

When membership explicitly quarantines a node, its zones remain unavailable
until authority is reassigned and content is restored. In that window, reads
for those zones return misses (K4) and writes stay in the durable parked garage
(P5/W1). Nothing is lost; everything is late. Silence by itself never starts
that recovery.

So the goal of in-mesh replication is **to cut time-to-recovery, not to add
durability.** Keeping that framing narrow is what makes the design tractable.

### The constraint that decides the design

[The authority rule](aggregates.md#3-the-mechanism-authority) requires that "the
owner of `c`" name **exactly one node**. A Dynamo-style
[quorum](glossary.md#quorum-replication) scheme, where any replica accepts writes
and divergence is reconciled later, would destroy that — and with it the only
argument the mesh has for parent aggregates being correct.

What preserves it is [primary-backup](glossary.md#primary-copy-replication),
with the primary *computed* rather than elected.

### The scheme that fits

**Replicate each zone to the `r` XOR-nearest live nodes; the nearest is the
primary.** This is Kademlia's "store on the k closest nodes" and Chord's
successor list, and it fits this codebase unusually well for four reasons:

1. **Failover is a re-evaluation, not a decision.** The XOR minimum over the
   surviving set *is* the second minimum over the old set, so when a primary
   dies the next-nearest node is already the answer — no election, no lease, no
   consensus. This is
   [model.md §3a](model.md#3a-ownership-is-decidable-locally-with-no-consensus-and-no-tie-break)
   paying off a second time.
2. **The trie already computes the replica set.** `nearestK` and `ExtractK` were
   added for the gossip work and give exactly "the `r` nearest" for free.
3. **The mesh already has an idempotent transfer primitive.** Repeatable,
   nominatively acknowledged `Transfer` rounds copy a complete zone before the
   donor drops it ([R6](ownership.md#2-guarantees)). Replication could reuse its
   payload and idempotent application, but would still need a continuous or
   synchronous write stream; the retired handoff mirror is not available.
4. **The endpoint split already encodes the read rule.** The amendment
   replication needs is: *a parent stub may only be written from the primary,
   while a client read may fall back to a backup.* Those are already two
   different endpoints — so the rule becomes **backups answer `/set`, never
   `/aggregates`**, and the authority argument in
   [aggregates.md](aggregates.md#3-the-mechanism-authority) survives untouched.

### What still has to be decided

**Sync or async.** Async mirroring keeps write latency as it is but loses the
un-mirrored tail on failover. Sync-to-one — the primary waits for a single
backup ack — costs one round trip and would upgrade I1 from a node-level
durability claim to a mesh-level one. Given that I1 already pays for a
`SyncAppend` before acknowledging, the marginal cost is smaller than it looks.

**Replica-set repair.** When membership changes, the replica set changes: a node
that becomes `r`-th nearest must acquire the zone, one that falls out must drop
it. That is `control()` generalised from "the half-space closer to a candidate"
to "zones for which I am no longer in the top `r`". **R3's antisymmetry argument
is stated for a single owner and has to be re-derived for `r`** — this is the
part of the project with genuine proof obligations attached, and it should be
done before any code.

**Cost.** `r×` storage and `r×` write bandwidth, against which: the object store
stops being load-bearing for recovery, `ForceCheckpointZones` could hand off from
a backup through the same repeatable Transfer protocol, and named-mark repair
becomes a backstop rather than a frontline mechanism.

## 3. The membership ceiling

`Observe` pings **every** registered peer on every tick. M5 succeeding is what
makes this expensive: with full membership, `registered` is the whole mesh, so
the work per node per tick is `O(N)` and mesh-wide it is `O(N²)`. At a hundred
nodes that is fine; at a thousand it is roughly a million pings per ten-second
tick, and the failure detector becomes the largest consumer of the mesh's own
network.

The standard answer is [SWIM](glossary.md#swim) (Das, Gupta & Motivala, 2002):
each node pings **one** random peer per period, falls back to `k` indirect probes
through other peers when that fails, and disseminates membership changes by
piggybacking them on the same messages. The work per node per period is `O(1)`
and the expected detection time is independent of `N`. Its Lifeguard refinements
address the false-positive rate under load, which matters here because a false
suspicion costs a 90 s quarantine and a zone re-route.

There is a second, more interesting exit. Full membership is not needed for
*routing* — one contact per bucket was always enough. It is needed for
*inference*: R10 and Q conclude from silence, and silence is only evidence if the
sample was fair ([membership.md §6](membership.md#6-why-completeness-matters-silence-as-evidence)).
With replication (§2), R10's question changes from *"does anyone own this zone?"*
— unanswerable without full membership — to *"do any of the `r` nearest nodes own
this zone?"*, which is bounded and answerable from a partial view.

**So §2 removes the forcing function behind M5's completeness requirement, and
with it most of the pressure behind §3.** That dependency is the single most
useful thing on this page for planning purposes.

## 4. Smaller open items

- **Add/delete commutation.** Add-wins is an
  [accepted window](guarantees.md#3-accepted-windows) today. Lifting it needs
  generation-carrying adds — an [OR-set](glossary.md#or-set) tag minted per
  write — which changes the wire format for every item.
- **Non-invertible metrics.** `min`, `max` and percentiles cannot ride the
  incremental path because the aggregate must be an
  [abelian group](glossary.md#abelian-group). The way in is
  [mergeable summaries](glossary.md#mergeable-summary) — HyperLogLog for distinct
  counts, t-digest or KLL for quantiles — which are associative and mergeable but
  *not* invertible, so a delete would force a subtree re-aggregation rather than
  a subtraction. That asymmetry has to be surfaced in the API rather than hidden.
- **A randomisation dimension in the encoding.** Optional per collection: a
  dimension whose coordinate is derived per item (hash of identity plus a
  collection salt), which the client may weight against the real dimensions by
  choosing its own origin. It would give three things at once — leaves that stay
  splittable when many items share identical coordinates
  ([pressures §6b](pressures.md#6b-degenerate-cells-and-randomisation-bits-as-an-exit)),
  uniform sampling of a cell for incremental loading, and an ordering that a
  client can recompute and verify rather than trust. The cost is fan-out `2^b`
  for queries that leave the dimension free, where `b` is the number of
  randomisation bits crossed, so **placement of the bits is the whole design**.
  Measurement first: distinct zones touched per query as a function of the
  requested blend, against a skewed distribution. Rationale and full design in
  [`docs/ordering-and-fairness.md`](../../../docs/ordering-and-fairness.md) §4.
- **`Collection.Complete` holds the exclusive lock while materialising sets**
  ([known defect](guarantees.md#5-known-defects)).
- **The validation simulator models no zones and no delegation**, so the
  scenarios exercise membership and routing but not the handoff state machine.
  Every R-guarantee is currently pinned by unit tests and the mockup harness
  only.

## 5. Sequencing

| Order | Item | Why here |
|---|---|---|
| 1 | Settle the drain-placement question (§1) | it is a design decision, costs nothing to make, and blocks the merge work from being finished |
| 2 | Zone merging (§1) | cheapest of the three, reuses R10's machinery, and unblocks node scale-down being worth doing |
| 3 | Re-derive R3 for `r` owners (§2) | proof work with no code, and it gates everything else in replication |
| 4 | Continuous mirror to backups (§2) | the transport exists; this is lifetime management plus replica-set repair |
| 5 | SWIM or the bounded-inference exit (§3) | only becomes urgent past a few hundred nodes, and §2 may change the requirement entirely |
| — | Randomisation dimension (§4) | independent of the three above; gated by a measurement, not by a design decision, so it can be evaluated at any time |

---

[← Guarantees](guarantees.md) · [Index](README.md) · [Glossary →](glossary.md)
