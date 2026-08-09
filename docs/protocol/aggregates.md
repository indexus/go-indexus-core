[← Model](model.md) · [Index](README.md) · [Membership →](membership.md)

# Aggregates

The aggregate tree is what the mesh exists to serve — and the reason the rest of
the protocol is shaped the way it is. A client can do **nearby** (walk location
prefixes) and **incremental** loading (merge deltas of pre-materialised
summaries) only if every interior node already carries an updatable summary and
every write is a **linear feed** step up the tree, not a recompute. See
[bibliography §1](bibliography.md#1-the-ambition-that-organises-everything-else).

This page is the algebra that makes that feed sound, the one place the algebra
is not enough, and the rule that covers the gap.

---

## 1. The algebra

Let `A = (count, metrics)` with pointwise `+`. `A` is a
[commutative monoid](glossary.md#commutative-monoid), so the *value* of a
subtree is order-independent: adds of distinct ids commute, duplicate applies
are absorbed, and a subtree's aggregate is the sum of its children's aggregates
regardless of the order they arrive.

Because deletes subtract rather than re-scan, the metrics an aggregate may carry
must also have an inverse — the structure is an
[abelian group](glossary.md#abelian-group), which is where the `Abelian` type
gets its name. That requirement is the reason `min`, `max` and percentiles are
not on the incremental path: they are monoidal but not invertible, so removing a
contribution means recomputing the subtree rather than subtracting.

> **Where this sits.** A tree whose interior nodes carry an associative summary
> of their subtree is the same shape as a
> [Merkle tree](glossary.md#merkle-tree) — with an aggregate in place of a hash
> — and the same shape as a [Fenwick or segment tree](glossary.md#segment-tree)
> in the sequential world. Distributing it over a peer mesh, so that each subtree
> summary is maintained by whichever node owns that subtree, is what
> [Astrolabe](glossary.md#astrolabe) and
> [SDIMS](glossary.md#sdims) do for node attributes. Indexus does it for
> application data, which is the reason the freshness problem below shows up
> here and not there.

## 2. The one place the algebra is not enough

The algebra handles the data. It does **not** handle the **parent stub**.

A stub is the entry a parent zone keeps for a child zone owned by another node.
It is not accumulated locally — it is a *replica of a remote value* that is
re-observed periodically. Replication needs a conflict-resolution rule: given
the stub currently held and a newly observed value for the same child, which one
survives?

For that rule to be correct it must satisfy:

- **V1 — decidable.** Any two observations of the same child are comparable.
- **V2 — cross-node comparable.** An observation produced by node `A` and one
  produced by node `B` can be ordered against each other.
- **V3 — survives handoff.** After zone `k` moves `A → B`, every observation
  from `B` sorts strictly after every observation from `A`.
- **V4 — lag-tolerant.** Within one owner generation, an observation that is
  *smaller* is a partial read (ingestion in flight), not a shrink, and must not
  win.

This is the [freshness order](model.md#2-the-two-orders), and per that section
it has to be right by construction — a wrong merge here is summed into an
ancestor and never comes back out.

## 3. The mechanism: authority

**A parent stub for child `c` is only ever written from a value served by the
current owner of `c`.**

Not versioning — *authority*. `GET /aggregates` already has exactly this
property: it answers only for locally owned locations and refuses to forward, so
a response to it is self-certifying (K5).

Against the four requirements:

| | How authority satisfies it |
|---|---|
| V1 | the rule is a replacement, not a comparison — there is nothing to decide |
| V2 | there is only ever one authority at a time, so no cross-node ordering arises |
| V3 | authority changes exactly at the handoff, so the boundary is the handoff itself |
| V4 | becomes a transport concern (do not accept a partial response) rather than an ordering one |

`Collection.SetChildSummary` is the only writer of a parent stub, and it takes
the value as given. That is deliberate: the guarantee lives in *who is allowed
to call it*, not in a check inside it. Two callers qualify — a `/aggregates`
response (`applyPulledAbelian`) and a local `Delegate` — and a `/set` response
does **not**, because it may have come from a cache or a path-fill.

Tests: `TestParentStubFollowsOwnerAcrossHandoff`,
`TestParentStubIgnoresNonAuthoritativeSource`,
`TestParentStubAcceptsOwnerShrink`.

> **Where this comes from.** Designating one replica as the sole writer is
> [primary-copy replication](glossary.md#primary-copy-replication). What is
> unusual here is that the primary is not elected or leased — it is *computed*,
> by the same XOR rule that decides placement
> ([model.md §3a](model.md#3a-ownership-is-decidable-locally-with-no-consensus-and-no-tie-break)).
> Placement and freshness end up sharing one mechanism without sharing a
> decision procedure, and that is the load-bearing idea of this section.

## 4. The alternative we rejected

Ordering observations by a version counter (`epoch uint64` per zone, keep the
higher) looks like the natural answer and was implemented first. It cannot work
here:

- a counter minted by a node is only comparable to counters minted by the same
  node — **V2 fails**;
- a zone that moves to a new owner gets a counter from that owner's sequence,
  which is usually *lower* than the donor's — **V3 fails**, and every scale-up
  hands zones from a mature node to a fresh one.

The result was not a lost update but a **permanently frozen parent**: the
parent's owner rejected every summary the new owner would ever publish. The full
analysis, and the removal, are in [`../plans/zone-authority.md`](../plans/zone-authority.md).

The repair usually proposed at this point is a
[version vector](glossary.md#version-vector) or a
[Lamport clock](glossary.md#lamport-clock), and either would restore V2. Neither
restores V3 for free: a vector still has to be handed over at the cutover and
merged, which is a distributed agreement about the handoff instant — exactly the
thing authority avoids by deriving the order from ownership, which the mesh
already tracks.

**The lesson generalises: a freshness order must be derived from the thing that
makes a value true, not from a clock kept alongside it.**

## 5. Consequences elsewhere

The authority rule is why several things in this documentation look stricter
than they need to be:

- **`/children` records existence only** and drops the aggregates it carries,
  because the responder need not own the child it is naming
  ([convergence.md D3](convergence.md#1-d--child-discovery)).
- **A donor falls silent on `/aggregates` for a zone it handed off**, even while
  it still owns the parent above it ([reads.md K5](reads.md#4-guarantees)).
- **A refresh read never populates a stub**, only the cache — the two apply
  paths `applyPulledAbelian` / `applyPulledSet` are the enforcement point
  ([convergence.md](convergence.md#2-u--background-convergence)).
- **One serving copy per zone is not a limitation to be lifted casually.**
  Authority is only well-defined because "the owner of `c`" names exactly one
  node. Adding replicas without designating a primary among them would break
  this rule, which is the central constraint on
  [in-mesh replication](roadmap.md#2-replication-inside-the-mesh).

---

[← Model](model.md) · [Index](README.md) · [Membership →](membership.md)
