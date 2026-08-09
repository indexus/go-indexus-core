[← Index](README.md) · [Aggregates →](aggregates.md)

# The model

Everything else in this specification rests on the four definitions in §1 and
on the distinction drawn in §2. If you read only one page, read this one.

---

## 1. Definitions

**Identifiers.** Every node and every zone key lives in a **96-bit** ID space
(`encoding.BASE64 = NewBase(64, 96)`), written as 16 base64 characters. A
node's ID is the decoding of its name; a zone key `(collection, location)` maps
to `MergeEncodings(location, collection)`.

**Metric.** Kademlia-style [XOR distance](glossary.md#xor-metric). "Nearest"
always means XOR-nearest in this space, and the trie really computes it —
`TestNearestIsXorMinimum` brute-forces it against every member. What that
metric does and does not settle is §3.

**Zones and items.** A collection is a prefix tree of sets carrying
[abelian](glossary.md#abelian-group) aggregates (count + metrics). A location is
a string over the 64-character alphabet; `Parent` strips one character; the root
is `@`. Items are leaves (`location:id`); interior entries aggregate their
subtree. A *zone* is a subtree an owner serves; ownership splits when a zone's
running count reaches the delegation size.

**Deletes.** [Tombstones](glossary.md#tombstone) with a generation counter; a
re-add clears the tombstone, a newer tombstone wins over an older one
(`ApplyTombstone`).

### Target steady state

For every zone key `k` with data, exactly one live node owns `k`, and that node
is the XOR-nearest live node to `k`. Every write acknowledged to a client is
placed under that owner exactly once. Every parent stub equals the aggregate its
owner currently serves.

This is a *fixed point*, not an invariant. The mesh is not always in it; the
protocol's job is to always be moving towards it and to never be stuck away
from it. Which of the two things below is drifting decides whether that
self-correction happens at all.

---

## 2. The two orders

The mesh maintains two different notions of "correct", and conflating them is
the single largest source of bugs in this codebase. They are kept separate
throughout this documentation.

| | **Placement order** | **Freshness order** |
|---|---|---|
| Question | *Who should own zone `k`?* | *Which of two observed summaries of `k` is newer?* |
| Answer | XOR-nearest live node | the one from the node that owns `k` now |
| Mechanism | `owned.Range` half-space donation (R3) | authority of the owner ([aggregates.md](aggregates.md#3-the-mechanism-authority)) |
| Failure if wrong | zone served by a far node — a performance loss | parent aggregate wrong or frozen — a correctness loss |

Placement is **self-correcting**: R3 moves any misplaced zone on the next
Refresh. Freshness is **not** self-correcting: a wrong decision is absorbed into
the parent aggregate and stays there.

The asymmetry sets the design standard for each. Placement can be *right on
average* — it is allowed to be wrong at any instant as long as every step
reduces the error, which is a [variant-function](glossary.md#variant-function)
argument and is what R3 supplies. Freshness has to be *right by construction*,
because there is no later step that would undo a bad merge.

---

## 3. What the XOR metric buys, and what it does not

The placement order rests entirely on properties of XOR. Three of them hold and
carry design decisions the rest of this documentation depends on. A fourth is
routinely assumed, does not hold, and is where a recurring class of routing bug
comes from; it is stated second, before anything is built on it.

### 3a. Ownership is decidable locally, with no consensus and no tie-break

For a key `k` and two distinct node IDs `x ≠ y`, `d(x,k) ≠ d(y,k)`: equality
would mean `x⊕k = y⊕k`, hence `x = y`. The minimum is therefore unique and can
never tie, so *who owns `k`* is a total function of the key and the membership
set rather than a negotiated fact. Two nodes holding the same membership set
compute the same owner without exchanging a message.

That is what lets placement run with no coordinator, no lease, no quorum and no
tie-break rule — a rule that, being arbitrary, would have to be agreed on
separately. It also sidesteps [FLP](glossary.md#flp-impossibility) entirely:
the mesh never needs agreement on ownership because ownership is not a decision,
it is an evaluation. `TestNearestIsXorMinimum` brute-forces the minimum against
every member.

> **Where this comes from.** Choosing the owner as the extremum of a
> deterministic per-`(node, key)` score is
> [rendezvous hashing](glossary.md#rendezvous-hashing) (Thaler & Ravishankar,
> 1998), and it inherits that scheme's two useful properties: a unique owner
> with no coordination, and [minimal disruption](glossary.md#minimal-disruption)
> — a node joining or leaving only moves the keys nearest to it. The one
> substitution Indexus makes is the score function: rendezvous hashing scores
> with a hash of the pair, precisely to get a uniform spread; Indexus scores
> with the metric itself. §3d is the consequence.

### 3b. What it does not buy is agreement

XOR removes the need to agree on the *rule*; it does nothing about the
*inputs*. Two nodes with different membership views compute different owners,
and there is no per-hop guarantee anywhere in the write path that papers over
it: `find` returns the nearest peer *this* node has registered, the receiver
recomputes it from its own table, and nothing carries a hop count or a visited
set. Two divergent tables can hand the same write back and forth.

What bounds that is not the metric. It is:

- **R3** — the donation relation is antisymmetric, so ownership itself only
  ever moves one way ([ownership.md](ownership.md#2-guarantees));
- **P1** — pacing the retry ([writes.md](writes.md#2-p--placement));
- **P5** — refusing to re-descend a walk that has already failed;
- **M5** — stopping the views from diverging in the first place
  ([membership.md](membership.md#3-m5--random-peer-sampling)).

Placement is self-correcting, not correct at every instant. This is the single
most misread property of the system, and every routing defect found in
production so far has been a variant of assuming otherwise.

### 3c. Distance is unidirectional, which is what makes the trie exact

For any `x` and any distance `Δ` there is exactly one `y` with `d(x,y) = Δ`.
Distances partition into disjoint bands by shared prefix length, so one bucket
per band covers the whole space without overlap and `Nearest` descends the trie
in `O(log N)` instead of scanning.

The same property is why a bucket extract is **lossy** rather than merely
partial: a band is summarised by its single nearest member, which is enough to
route towards a key and not enough to enumerate the mesh. That is the whole of
the gossip-ceiling result in [membership.md](membership.md#4-g--gossip).

### 3d. Keys are not hashed, so the location tree keeps its shape

`zoneKeyID` merges the location into the *high* bits and lets the collection
fill the rest (`MergeEncodings`). A zone and its parent therefore differ only in
the bits the extra location character occupies, sibling zones stay adjacent
under XOR, and a node ends up owning a contiguous slice of the location tree.

That is what makes a subtree handoff a single `Delegate` over a contiguous range
instead of a scatter of unrelated keys, and what makes a spatial read a local
walk. Test: `TestZoneKeysKeepTheLocationTreeAdjacent`.

This is a deliberate departure from [Kademlia](glossary.md#kademlia) and
[Chord](glossary.md#chord), which hash the key precisely to *destroy* this
locality and get uniform load spreading for free. The cost of keeping it is that
load follows the data's own distribution — a dense region is a hot contiguous
key range, not a spread of random ones — so the mesh cannot lean on hashing to
balance and has to split on measured load instead (R3, autoscale).

**The locality is what makes the aggregate tree cheap to read; load-driven
splitting is what pays for it.** That trade — give up hash-uniformity, buy
range-queryability, pay for balance separately — is the same one taken by
[order-preserving](glossary.md#order-preserving-partitioning) range-partitioned
stores (Bigtable tablets, HBase regions, CockroachDB ranges) and by
[Prefix Hash Tree](glossary.md#prefix-hash-tree) on top of a DHT.

---

## 4. Where each property is used

| Property | Relied on by |
|---|---|
| §3a unique minimum | R3 planning, P placement, Q duplicate resolution, X ingress hint |
| §3b no agreement | the accepted window on circulating writes, P5, M5 |
| §3c unidirectional bands | `Nearest`/`Extract` in `domain/tree.go`, the gossip ceiling |
| §3d unhashed keys | `Delegate` over a range, spatial reads, the aggregate tree itself |

---

[← Index](README.md) · [Aggregates →](aggregates.md)
