[← Aggregates](aggregates.md) · [Index](README.md) · [Writes →](writes.md)

# Membership and gossip

`core/membership.go`, plus the first half of `Refresh` in `core/rebalance.go`.

How a node learns who else is alive. This page has an unusually large
measurement section because the obvious mechanism — gossip — turns out to have
a hard ceiling, and the mesh needed a second one.

---

## 1. The ladder

A contact climbs `acknowledged → registered → routing`; death walks the other
way and adds a 90 s quarantine (`quarantineWindow`) so gossip cannot
re-register a dead peer before its zones were re-routed.

| Rung | Meaning |
|---|---|
| `acknowledged` | heard of, not yet verified — a lead |
| `registered` | answered a Ping, or arrived by gossip — usable as a peer |
| `routing` | the XOR k-bucket extract of `registered` + self — the next-hop table |
| `suspect` | quarantined, with expiry — refused re-entry until it lapses |

The quarantine is what makes death *stick*. Without it, a node evicted by
`Observe` is immediately re-registered by the next gossip answer that still
names it, and the mesh cannot re-route its zones. 90 s is chosen to exceed a
Refresh period comfortably.

> **Failure detection here is timeout-based and therefore
> [unreliable](glossary.md#unreliable-failure-detector) in the technical sense:
> it cannot distinguish a dead node from a slow one.** Everything downstream is
> designed around that. It is why the quarantine expires rather than being
> permanent, why a suspended peer is skipped rather than declared gone, and why
> [R10](ownership.md#3-r10--a-delegation-mark-always-has-an-owner-behind-it)
> treats a run of silence as a *trigger* and not a verdict.

## 2. Observe, and the M guarantees

```
Observe():
  for c in registered\{self}:  Ping c;  on failure → suspend(c) + reject(c)
  for c in acknowledged\{self}: Ping c; ack answer → toRegister, else drop
  if both empty → re-acknowledge bootstraps          # partition self-heal
  registeredNew := register(toRegister)               # skips quarantined
  if registeredNew → subscribe(toRegister)            # routing immediately
                   → go Refresh()                     # out-of-band, single-flight
  acknowledge(discover())                             # uniform random sample (M5)
```

- **M1** A peer enters `registered` only if dialable, and never while
  quarantined (`register`).
- **M2** A dead peer leaves `registered`+`routing` within one Delay tick and
  stays quarantined for 90 s (`Observe`, `suspend`). Gossip cannot resurrect it
  inside the window.
- **M3** A node whose tables emptied re-seeds from its bootstraps — a restarted
  mesh reconverges without an operator. Note the precondition: **emptied**. A
  node holding a *partial* table never re-seeds, which is the hole M5 closes and
  the direct cause of a production incident described in
  [R10](ownership.md#3-r10--a-delegation-mark-always-has-an-owner-behind-it).
- **M4** A *new* registration (not a re-ack, not a quarantine skip) kicks one
  out-of-band Refresh, so a joiner receives ownership without waiting a full
  tick. `register` returning "actually inserted" is what prevents the Refresh
  storms observed when any ack ping re-triggered it.

## 3. M5 — random peer sampling

**M5: every node eventually knows every other node on a connected mesh.**

Gossip cannot establish this on its own and never could — §4 measures why — so
`Observe` also draws a uniform random sample.

`discover` picks `log2(|registered|)` peers at random and asks each for a
`Random` introduction, which the peer answers out of *its* registered pool. That
pool is centred on the answering peer, not on us, so the introduction reaches
regions of the keyspace our own view does not cover. Composed over ticks it is a
[random walk](glossary.md#random-walk-on-the-knowledge-graph) on the knowledge
graph, and it keeps that graph connected under partition and churn.

The fanout is the standard [epidemic-protocol](glossary.md#epidemic-protocol)
figure and the one the validation scenarios use — twelve nodes fan out to four,
two hundred to eight, a thousand to ten.

An introduction is a **lead, not a credential**: it enters `acknowledged` and
climbs the ordinary ladder, so M1 and the quarantine still decide who is
registered. One contact crosses per call, never a table, which is what keeps the
cost `O(log N)` of messages and `O(1)` of payload per tick.

Tests: `TestObserveAsksKnownPeersToNameAStranger`,
`TestAChainOfNodesReachesFullKnowledgeOfTheMesh`,
`TestDiscoveryLeavesQuarantinedPeersAlone`, `TestRandomNeverNamesTheAsker`.

> **Where this comes from.** A service whose only job is to hand out a uniformly
> random live peer is a [peer sampling service](glossary.md#peer-sampling-service)
> (Jelasity et al., 2007), the abstraction underneath Cyclon, Newscast and
> HyParView. The `log N` fanout is the classic
> [randomised rumour spreading](glossary.md#randomised-rumour-spreading) figure
> (Demers et al., 1987; Karp et al., 2000). The shape of the tail in §5 —
> "almost everyone quickly, the last few slowly" — is the
> [coupon collector](glossary.md#coupon-collector) problem: collecting `N`
> distinct peers takes on the order of `N ln N` draws, so the final stragglers
> dominate the time.

## 4. G — Gossip

Ask each routing peer (fallback: registered) for its `Neighbors` — the XOR-bucket
extract around *our* ID — and register what comes back. Gossiped contacts are
not Ping-verified; a dead one is evicted by the next Observe (M2), and quarantine
keeps known-dead ones out.

Every answer is an extract around the *asker's* identifier, which is the right
economy for placement: a node only ever needs the nearest peer it knows, and any
disagreement is absorbed rather than prevented
([model.md §3b](model.md#3b-what-it-does-not-buy-is-agreement)).

Membership is learned through the same answer, and **there the sort by distance
is a ceiling**. A bucket carries its `gossipBucketWidth` nearest members and
drops the rest, and bucket 0 always holds about half the mesh, so what one
answer can name is bounded however many peers are asked and however long the
mesh runs — once every table is complete, every answer to `Neighbors(v)` is the
same set. This is [model.md §3c](model.md#3c-distance-is-unidirectional-which-is-what-makes-the-trie-exact)
showing up as a limit: a band summarised by its nearest members is enough to
route and not enough to enumerate.

Measured against the real trie (`TestGossipChannelCeiling`), the worst node's
share of the mesh a saturated answer reaches:

| nodes | k=1 | k=8 | k=20 | k=64 |
|---|---|---|---|---|
| 16 | 0.20 | 0.87 | **1.00** | 1.00 |
| 64 | 0.06 | 0.43 | 0.76 | 1.00 |
| 256 | 0.02 | 0.15 | 0.34 | 0.73 |
| 1024 | 0.01 | 0.05 | 0.12 | 0.30 |

`gossipBucketWidth` is 20, [Kademlia's k](glossary.md#k-bucket): enough to carry
a small mesh in a single round, and — as the last row shows — never enough to be
relied on alone. Widening raises the ceiling, it does not remove it; only a draw
that does not sort by distance does, which is M5.

**The routing table does not widen with it.** `clean` rebuilds from
`routingNeighbors`, the narrow one-per-bucket extract, because Refresh asks every
routing peer for its neighbours and `control` plans a donation against each:
widening routing multiplies both, for a table whose only job is to name the next
hop.

## 5. The two measured together

`TestMembershipConvergenceProbabilities` (a sweep, run with `INDEXUS_MEASURE=1`)
on the star topology the mesh actually forms as nodes spawn against a bootstrap.
Rounds are Observe/Refresh pairs:

| | k=1 | k=8 | k=20 |
|---|---|---|---|
| 16 nodes, G alone | never, stops at 0.41 | 64% of runs, rest at 0.93 | always, 1 round |
| 16 nodes, G + M5 | always, median 19 | always, median 2 | always, median 1 |
| 64 nodes, G alone | never, stops at 0.23 | never, stops at 0.70 | never, stops at 0.94 |
| 64 nodes, G + M5 | always, median 93 | always, median 72 | always, median 49 |

Read the two things that matter:

1. **M5 is the only entry that ever reaches "always."** Every `G alone` row
   plateaus below 1.0 — including the 0.94 at 64 nodes with k=20, which is the
   most dangerous number on this page because it looks like success.
2. **The bucket width is what makes M5 fast enough to be useful.** On the real
   mesh, `TestAChainOfNodesReachesFullKnowledgeOfTheMesh` went from 8–17 rounds
   to 3–5 when the width went from 1 to 20.

Neither mechanism replaces the other. G alone has a ceiling; M5 alone takes a
quarter of an hour at a ten-second tick. Shipping both is not redundancy, it is
one mechanism for speed and one for completeness.

## 6. Why completeness matters without treating silence as evidence

Repair no longer infers an orphan from unanswered probes. A child pushes a
`Claim` naming itself; a parent keeps that named mark while the holder is merely
silent, and blocked writes remain parked. Authority changes only on an explicit
membership verdict.

Completeness still matters for placement and recovery speed: all nodes should
compute the same XOR-nearest owner, and a healed peer must become reachable
again. Quarantined contacts therefore retain a probationary address. A direct
successful Ping clears quarantine and re-registers the peer; an introduction
alone does not.

Duplicate claims created by an explicit partition verdict still converge
through the ordinary R3 placement pass after healing. This argument depends on
eventual membership agreement, not on interpreting silence as proof of absence.

---

## Related

- [model.md §3c](model.md#3c-distance-is-unidirectional-which-is-what-makes-the-trie-exact) — why an extract is lossy
- [ownership.md R10](ownership.md#3-r10--a-delegation-mark-always-has-an-owner-behind-it) — the repair path that depends on M5
- [roadmap.md §3](roadmap.md#3-the-membership-ceiling) — `Observe` is `O(N)` per tick, and full tables make that bite

---

[← Aggregates](aggregates.md) · [Index](README.md) · [Writes →](writes.md)
