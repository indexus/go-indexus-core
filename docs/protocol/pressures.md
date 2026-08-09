[← Bibliography](bibliography.md) · [Index](README.md) · [Roadmap →](roadmap.md)

# Pressures, proofs and known exits

This page sits **beside** the protocol, not inside it.

The protocol pages say what Indexus *does* and what tests enforce. This page
asks a different question: for each structural decision, what does the published
literature already know about the **advantage**, the **cost**, and the
**exits** others found when they hit the same wall?

Still no lineage — Indexus was not built from these papers. The value is
diagnostic: if a pressure shows up in production, someone has usually measured
it, named it, and sometimes solved a close variant.

Every claim below names a source. Loose analogies are marked as such.

---

## How to read this

| Column | Meaning |
|---|---|
| **Decision** | what Indexus chose |
| **Buys (advantage)** | why the choice was worth making |
| **Costs (pressure)** | the recurring problem class |
| **Evidence** | paper / result that characterises the pressure |
| **Known exits** | published or industrial mitigations Indexus could borrow |
| **Already in Indexus?** | what the mesh already does about it |

---

## 1. Keys are not hashed — locality over balance

**Decision.** Location bits stay in the high bits of the zone key; sibling
zones stay XOR-adjacent ([model.md §3d](model.md#3d-keys-are-not-hashed-so-the-location-tree-keeps-its-shape)).

| | |
|---|---|
| **Buys** | A region is a contiguous range → nearby and handoff are local walks, not scatters. Aggregate tree stays cheap to read. |
| **Costs** | Load follows the data. Dense cities / hot prefixes become hot contiguous key ranges. Hashing would have spread them for free. |
| **Evidence** | Classical consistent hashing without intervention already has max load `Θ(log n / log log n)` above average (balls-and-bins; restated in Mirrokni et al., *Consistent Hashing with Bounded Loads*, 2016). Order-preserving layouts are **worse** when the item distribution is skewed — there is no randomisation to lean on. Mercury (2004) and Bigtable both treat explicit rebalancing as mandatory once hashing is refused. |
| **Known exits** | **(a)** Move *nodes* toward load: Karger & Rühl, *Simple Efficient Load-Balancing…* (SPAA 2004) — protocol that relocates nodes “where they are needed,” with an application to **ordered** data / range search; constant-factor of optimal. **(b)** Virtual servers / leave-join moves (Rao et al., IPTPS 2003). **(c)** Contiguous-range rebalance with sampling histograms (Mercury). **(d)** Demand-aware online adjustment (Hash & Adjust, OPODIS 2024) — constant-competitive under structured, bursty demand. |
| **Already in Indexus?** | Load-driven split + `PreferNear` spawn (autoscale) is exit (a)/(c) in spirit: place the new node in the half-space that holds the weight. Missing: the inverse (merge / drain toward under-loaded peers) — [roadmap §1](roadmap.md#1-scaling-down-zone-merging). A partial, local exit for the worst case (identical coordinates) is low-order randomisation bits — [§6b](#6b-degenerate-cells-and-randomisation-bits-as-an-exit). |

**Practical reading.** PreferNear is not a nice-to-have; under unhashed keys it is
the load-balancer. Tuning thresholds without hysteresis will oscillate — the
same lesson HBase’s Region Normalizer encoded as `min_region_age` and split/merge
size bands.

---

## 2. Ownership by XOR extremum — evaluation over agreement

**Decision.** Owner of `k` = XOR-nearest live node; unique, locally computable
([model.md §3a](model.md#3a-ownership-is-decidable-locally-with-no-consensus-and-no-tie-break)).

| | |
|---|---|
| **Buys** | No consensus, no lease, no tie-break. Sidesteps FLP (Fischer, Lynch & Paterson, 1985) by never deciding ownership — only evaluating it. Minimal disruption when membership changes (same family as rendezvous hashing, Thaler & Ravishankar 1998). |
| **Costs** | **Agreement on inputs is not free.** Two nodes with different membership views compute different owners. The write path has no visited set → circulating forwards while views diverge ([accepted window](guarantees.md#3-accepted-windows)). Repair paths that conclude from silence read *ignorance as absence* ([membership.md §6](membership.md#6-why-completeness-matters-silence-as-evidence)). |
| **Evidence** | Rendezvous/HRW and Kademlia prove uniqueness and unidirectionality of XOR; they do **not** prove that divergent tables agree hop-by-hop — Indexus’s production loop was exactly that gap. Chandra & Toueg (1996): timeout detectors are unreliable; silence is never proof. |
| **Known exits** | **(a)** Drive membership to completeness (epidemic peer sampling — Jelasity 2007; Indexus M5). **(b)** Bound inference questions to a small candidate set (replication to `r` nearest makes “does anyone own this?” into “do the `r` nearest answer?”). **(c)** Path termination on writes (visited set) — reads already have it (K8); writes deliberately do not, and use P5 instead. |
| **Already in Indexus?** | M5 + widenMembership; named-mark reclaim only on quarantine / post-quarantine eviction (`verifyNamedMarks` + Claim), never on silence alone; P5 stops the line-rate loop. Write-path `via` bounds hops (stateless climb). |

---

## 3. One serving copy per zone — authority over availability

**Decision.** Exactly one writer for a zone’s aggregate; `/aggregates` certifies
ownership ([aggregates.md](aggregates.md#3-the-mechanism-authority)).

| | |
|---|---|
| **Buys** | Parent stubs have a correct-by-construction freshness rule (V1–V4). No quorum latency on the write path. No replica divergence to repair. |
| **Costs** | Owner death ⇒ zone unread/unwritable until an explicit membership verdict reassigns authority and content is restored. Object-store RPO for content since last snap. |
| **Evidence** | Primary-backup lower bounds (Budhiraja, Marzullo, Schneider et al.) constrain failover time and replication degree — you cannot make failover arbitrarily fast without paying elsewhere. Abadi PACELC (2012): even without partitions, replicated systems trade **latency vs consistency**; Indexus currently pays availability (on failure) to keep authority simple, with blocked writes durably parked rather than accepted everywhere. |
| **Known exits** | **(a)** Chain replication (van Renesse & Schneider, OSDI 2004): one serial order, head writes / tail reads, strong consistency, better throughput than classical primary/backup — fits authority. **(b)** Computed replica set = `r` XOR-nearest (Kademlia k-closest / Chord successors) so failover is re-evaluation, not election ([roadmap §2](roadmap.md#2-replication-inside-the-mesh)). **(c)** Worst-case replica placement (Gao et al., ICDCS 2015) if adversarial correlated failures matter. |
| **Already in Indexus?** | Durability via object store; named-mark Claim repair and repeatable Transfer handoffs. No standing replicas yet. |

**Warning from gray failure.** Huang et al., *Gray Failure* (HotOS 2017): the
dangerous faults are those with **differential observability** — detectors say
healthy, clients say broken. Indexus therefore does not turn write-path silence
into authority: blocked writes are parked, and only direct Claim responses or
explicit membership verdicts change repair state. Lifeguard (2017/2018) shows
the same need to control false positives inside SWIM.

---

## 4. Full membership + ping everyone — completeness over constant cost

**Decision.** `registered` aims at the whole mesh (M5); `Observe` pings every
registered peer each tick.

| | |
|---|---|
| **Buys** | Silence becomes meaningful evidence for R10/Q. Routing and inference share one table. |
| **Costs** | Mesh-wide work `O(N²)` messages per tick period. At hundreds of nodes the failure detector is the network. False suspicions under CPU starvation re-route zones for 90 s. |
| **Evidence** | SWIM (Das, Gupta & Motivala, DSN 2002) exists *because* all-to-all heartbeat does not scale; expected detection time and per-member load become independent of `N` under random probing. Lifeguard: false positives explode when the detector itself is slow. Coupon-collector: last few unknown peers dominate M5 convergence time ([membership measurements](membership.md#5-the-two-measured-together)). |
| **Known exits** | **(a)** SWIM / Lifeguard — `O(1)` probe + suspicion + local health. **(b)** HyParView — deliberate *partial* views (`log n` active). **(c)** Change the inference question via replication (§3) so completeness is no longer required. |
| **Already in Indexus?** | M5 for completeness; quarantine window; not SWIM. Roadmap sequences replication before membership rewrite because (c) may remove the forcing function. |

---

## 5. Distance-sorted gossip — cheap routing, biased census

**Decision.** `Neighbors` returns k-bucket extract around the asker;
`gossipBucketWidth = 20`.

| | |
|---|---|
| **Buys** | Right economy for next-hop placement; Kademlia’s proven structure. |
| **Costs** | Structural ceiling on what one answer can name — measured in `TestGossipChannelCeiling` (at 1024 nodes, k=20 reaches ~12% for the worst node). Using gossip alone for membership plateaus below 1.0. |
| **Evidence** | Kademlia: a band summarised by its nearest members routes; it does not enumerate. Peer-sampling literature (Jelasity 2007): distance bias ≠ uniform sample. Indexus’s own measurements are the evidence that matters here. |
| **Known exits** | Orthogonal random sample (M5); widen `k` (helps small N, does not remove the ceiling); separate channels for routing vs membership (already: narrow `routingNeighbors` vs wide gossip). |
| **Already in Indexus?** | Both channels shipped. Do not “fix” membership by only widening k. |

---

## 6. Prefix nearby — cheap spatial queries, fault lines

**Decision.** Nearby = walk / expand location prefixes; client plans Aggregate
and Nearby over the same tree.

| | |
|---|---|
| **Buys** | No secondary spatial index; incremental zoom is drill-down on materialised aggregates; linear feed maintains them (Gray cube lattice; DBSP group IVM). |
| **Costs** | **Edge / fault-line effect:** two points metres apart across a cell boundary can share no prefix. Naive single-prefix nearby misses true neighbours. Cell area also varies with latitude (Geohash). |
| **Evidence** | Geohash literature (Niemeyer; widely documented operationally): always query centre + neighbours (classically 8), then Haversine filter. H3 (Uber) and S2 (Google) exist partly because hex/Hilbert locality behaves better for neighbour gradients and poles/antimeridian. |
| **Known exits** | Neighbour expansion in the SDK (required); switch encoding to H3/S2 for geography-heavy collections; keep the aggregate tree abstraction regardless of the alphabet. |
| **Already in Indexus?** | Hierarchical alphabet + aggregates yes; neighbour-ring discipline is a **client** responsibility — bugs there look like “mesh bugs.” Document and test the 9-cell pattern for GPS dimensions. |

### 6b. Degenerate cells, and randomisation bits as an exit

A second, quieter cost of a pure location encoding: **identical coordinates are
unsplittable.** When many items share the same location at full precision — one
building, one address, one sensor site — there is no further character to
delegate on, so the leaf grows without bound and the split machinery has nothing
to work with.

The known exit is the one range-partitioned stores already use: **salting.**
HBase and Bigtable practice is to prefix or interleave hash bits so hot rows
spread across regions; the documented cost is that a range scan must then hit
every salt bucket.

In Indexus the mechanism is cheaper than in those systems, because the `Mask`
assigns dimensions **per bit** (6 bits per location character, see
`sdk-js/src/model/Mask.js`). So a randomisation dimension can be added one bit at
a time, and a query that leaves it free fans out by `2^b` where `b` is the number
of randomisation bits it crosses — not `64` per character. Placed *below* the
finest real resolution, `b = 0` for ordinary nearby queries and the cost is zero.

Two benefits beyond splittability: it gives dense areas finer split granularity
without hashing away locality (a partial answer to
[§1](#1-keys-are-not-hashed--locality-over-balance)), and a slice of that
dimension is a uniform sample of a cell, which is a genuinely useful primitive
for incremental loading.

The full design, including the fairness argument that motivates it and the
measurement needed before building it, is in
[`docs/ordering-and-fairness.md`](../../../docs/ordering-and-fairness.md) §4.

---

## 7. Zones split, never merge — growth without shrink

**Decision.** Delegation opens child zones; nothing merges them back
([lifecycle](lifecycle.md#1-the-life-of-a-zone), [roadmap §1](roadmap.md#1-scaling-down-zone-merging)).

| | |
|---|---|
| **Buys** | Simple ownership; no merge races with R3. |
| **Costs** | Fragmentation tracks the **peak**, not the current load. Per-tick tax: each delegated child is a pull. SoftLeave redistributes shards without consolidating. |
| **Evidence** | Every mature order-preserving store grew a merge path: HBase Region Normalizer (split if ≫ average, merge if ≪, with `min_region_age.days`, `min_region_count`, size floors — explicit hysteresis); Bigtable tablet merge; CockroachDB range merge. Without the low watermark, split/merge oscillates at the threshold. |
| **Known exits** | Pull-up merge owned by the parent (sum children < `mergeSize`); reuse R10’s reclaim/apply/reconcile machinery; cool-down and size band; settle drain-placement (XOR vs load) *before* enabling auto drain. |
| **Already in Indexus?** | SoftLeave only. Merge is the cheapest roadmap item because the primitives exist. |

---

## 8. Crash-stop trust model — simplicity over adversarial mesh

**Decision.** Peers may crash; they do not lie. `/aggregates` from a claimed
owner is trusted.

| | |
|---|---|
| **Buys** | Authority rule stays one RPC. No BFT overhead. |
| **Costs** | A Byzantine or simply buggy owner can poison every parent stub that pulls it. Malicious churn against identifier placement is a studied DHT attack. |
| **Evidence** | Byzantine-tolerant DHT constructions typically need quorums of size `Θ(log n)` and pay `O(log² n)`–`O(log³ n)` messages per op (Young, Kate, Goldberg et al.; Awerbuch & Scheideler). That cost is incompatible with the current authority shortcut. |
| **Known exits** | Stay crash-stop and treat authority as a **trust boundary** (operator-controlled mesh). If the threat model ever includes malicious peers, authority must move to quorum or attested snapshots — a different product. |
| **Already in Indexus?** | Crash-stop stated on the README. Do not silently assume BFT. |

---

## 9. Handoff as repeatable Transfer — availability during move

**Decision.** R6: one key is one independently acknowledged Transfer round;
failed keys remain residue for a later tick. R8: no half-mounted session —
invalid payloads fail before ownership, and a nominative ACK is what lets the
donor drop.

| | |
|---|---|
| **Buys** | No lost zone on failure paths; dual ownership is a legal window (Q); no session maps to deadlock. |
| **Costs** | Large zones still cross the donor's network (object-store bulk move is roadmap); temporary dual ownership until Q converges. |
| **Evidence** | ACK-before-drop chooses duplication over loss (standard). Repeatable rounds replace sagas/compensations: residue is simply retried. |
| **Known exits** | Continuous standing replication (roadmap); object-store bulk for very large zones. |
| **Already in Indexus?** | Nominative Transfer + residue restore tests (`TestSoftLeaveRestoresOnTransferFailure`, lossy-transfer convergence). |

---

## 10. Cross-cutting: what the literature suggests to watch next

Ordered by how expensive the pressure already is, or will become:

| Priority | Pressure | Why now | First move suggested by literature |
|---|---|---|---|
| 1 | Unhashed-key hotspots | PreferNear is the only balancer | Measure ownership weight skew under DVF-like loads; tune split hysteresis like HBase normalizer |
| 2 | Zone fragmentation after peaks | Update tick tax grows with delegated children | Implement parent pull-up merge with age/size bands |
| 3 | Single-copy RTO | Gray failures + orphan flaps already seen | Standing mirror to `r` XOR-nearest (chain-shaped), keep `/aggregates` primary-only |
| 4 | Observe `O(N²)` | Latent until N≳ few hundred | After (3), re-evaluate whether SWIM/HyParView is still forced |
| 5 | Prefix fault lines | Silent Nearby bugs | SDK neighbour expansion tests; consider H3 for GPS |
| 6 | False suspicion under load | 90 s quarantine is costly | Lifeguard-style local-health before shortening timeouts |
| 7 | Byzantine / poison stubs | Out of scope today | Keep stated; do not half-implement quorums |

---

## 11. Compact scorecard: decision vs literature

| Indexus decision | Literature advantage | Literature disadvantage | Strongest cited exit |
|---|---|---|---|
| Unhashed keys | range/nearby (Mercury, Bigtable, Skip graphs) | skew (Karger–Rühl; balls-and-bins) | move nodes to load; PreferNear |
| XOR ownership | unique, local, FLP-free (HRW, Kademlia) | divergent views | M5; bound inference via `r` replicas |
| One writer | authority / IVM (Alsberg; DBSP) | availability hole | chain replication; k-closest |
| Full membership | silence = evidence | `O(N²)` (SWIM motivation) | SWIM/Lifeguard; or drop need via replicas |
| Bucket gossip | O(log) route | census ceiling (measured + Kademlia) | peer sampling (Jelasity) |
| Prefix nearby | cheap zoom (Gray cube; Geohash) | fault lines | neighbour ring; H3/S2 |
| Split-only zones | simple | peak fragmentation | HBase-style merge + hysteresis |
| Crash-stop | simple authority | poison / BFT cost | stay operator-trusted |
| Mirrored handoff | no lost zone | dual own / deadlock | R8/R9; sagas analogy |

---

## Related

- Annotated paper mappings: [bibliography.md](bibliography.md)
- Open designs that absorb several rows above: [roadmap.md](roadmap.md)
- Guarantees vs accepted windows: [guarantees.md](guarantees.md)

---

[← Bibliography](bibliography.md) · [Index](README.md) · [Roadmap →](roadmap.md)
