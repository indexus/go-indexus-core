[← Glossary](glossary.md) · [Index](README.md) · [Pressures →](pressures.md)

# Related literature

This page is a **corpus**, not a genealogy.

The Indexus mesh was built autodidactically. No paper in this list was a
blueprint for the protocol, and none of the mechanisms below were implemented
*because* of a citation. The bibliography exists afterwards: once a design
choice had been made and observed to work, it became possible to ask which
published systems attack the same problem, with which ambition, and with which
trade-off. The mapping is **analogical** — same pressure, similar shape of
answer — not historical.

Where the analogy is tight, the paper is named next to the mechanism. Where it
is loose, that is said explicitly. Where Indexus deliberately diverges, the
divergence is the interesting sentence.

**Decision-side digest.** For each Indexus structural choice — what it buys,
what it costs, which result characterises the pressure, and which published
exit exists — see [pressures.md](pressures.md). That page is the argument
layer; this page remains the annotated corpus.

---

## 1. The ambition that organises everything else

Almost every structural choice in this protocol is there so that a client can
do two things at once:

1. **Nearby** — answer “what is around this location?” by walking a prefix of
   the location tree (or expanding a ring of sibling prefixes), without a
   secondary spatial index.
2. **Incremental** — refine that answer over time by loading *deltas* of
   pre-materialised aggregates (and later leaves), not by re-fetching pages or
   re-scanning the region.

Both require the same substrate: a **prefix tree whose interior nodes already
carry the abelian summary of their subtree**, stored so that a spatial region
is a contiguous key range, and updated so that each write costs work
proportional to the depth of the location — a **linear feed** of inserts and
deletes that maintain the tree, never a periodic recompute.

That is the design target. The rest of the mesh exists to keep that target
true as the data grows past one machine:

| Constraint the client needs | What the mesh therefore does |
|---|---|
| A region is a contiguous key range | keys are **not hashed**; location bits stay in the high bits of the zone key ([model.md §3d](model.md#3d-keys-are-not-hashed-so-the-location-tree-keeps-its-shape)) |
| An aggregate at any zoom is already there | every interior set carries `(count, metrics)` and updates by `+` / `−` ([aggregates.md](aggregates.md)) |
| A write must not force a subtree rebuild | metrics form an [abelian group](glossary.md#abelian-group); the feed applies one leaf and walks parents |
| The client plans its own loading | the tree the SDK walks *is* the tree the mesh stores; `/sets` returns deltas the client merges |
| Growing past one node must not scatter a region | ownership is XOR-nearest over **unhashed** keys, so a handoff moves a contiguous slice |
| Parent summaries stay trustworthy after handoff | [authority](aggregates.md#3-the-mechanism-authority): only the current owner may write a stub |

Spatial systems that hash keys (typical DHTs) buy load balance and lose
nearby-as-prefix. Analytical databases that recompute rollups buy arbitrary
metrics and lose the linear feed. Indexus takes the other side of both trades
on purpose.

In the published literature, the closest *shapes* are:

- **hierarchical spatial codes** (Geohash / Z-order) for “nearby = shared
  prefix”;
- **order-preserving range stores** (Bigtable tablets; Mercury hubs) for “a
  region is a contiguous range that can be split on load”;
- **order-preserving P2P overlays** (Skip graphs, BATON, PHT) for range queries
  without hashing keys away;
- **distributed aggregation trees** (Astrolabe, SDIMS) for “interior nodes
  carry summaries” — usually of *node* attributes, not of application items;
- **OLAP cubes / IVM** (Gray’s cube, prefix-sum range aggregates, DBSP) for
  pre-materialised rollups maintained by an abelian feed;
- **geohash-partitioned analytics stores** (Galileo / STASH / GeoLens) for
  interactive hierarchical spatial aggregation — usually with a cluster
  coordinator, not XOR ownership.

None of them combines all of that with a client-driven incremental feed over a
coordinator-free XOR mesh. That combination is the Indexus-specific claim; the
papers below are the surrounding conversation.

---

## 2. How to read a mapping

Each entry below has four fields:

- **Paper** — bibliographic reference and a stable URL when one exists.
- **What it actually says** — checked against the paper’s abstract and body,
  not against secondary summaries.
- **Where Indexus rhymes** — the mechanism or problem that looks similar.
- **Where it does not** — the limit of the analogy; read this before citing
  the paper as “prior art for Indexus.”

---

## 3. Placement without a coordinator

### Thaler & Ravishankar 1998 — rendezvous hashing (HRW)

**Paper.** David G. Thaler and Chinya V. Ravishankar. *Using Name-Based
Mappings to Increase Hit Rates.* IEEE/ACM Transactions on Networking, 6(4),
1998. [PDF](http://www.cs.ucr.edu/~ravi/Papers/Jrnl/HRW98.pdf)

**What it actually says.** Given an object name and a set of servers, assign
each server a pseudo-random weight `h(name, server)` and pick the highest.
Clients compute the same mapping locally with no shared state. Adding or
removing a server moves only the objects that mapped to it — the
minimal-disruption property. Originally motivated by web proxies and multicast
rendezvous points (later adopted by PIMv2 / CBTv2).

**Where Indexus rhymes.** Ownership is `argmin_n d(n, k)` over XOR distance —
the same *shape* as HRW (a deterministic per-`(node, key)` score with a unique
winner and no coordinator), with the metric substituted for the hash. Unique
owner, local evaluation, minimal disruption when membership changes
([model.md §3a](model.md#3a-ownership-is-decidable-locally-with-no-consensus-and-no-tie-break)).

**Where it does not.** HRW hashes the pair precisely to get *uniform* balance.
Indexus scores with the metric itself and keeps location bits unhashed, so
load follows the data. The paper’s hit-rate analysis for caches does not apply;
the useful inheritance is the coordination-free mapping, not the load model.

### Karger et al. 1997 — consistent hashing

**Paper.** David Karger, Eric Lehman, Tom Leighton, Matthew Levine, Daniel
Lewin, and Rina Panigrahy. *Consistent Hashing and Random Trees: Distributed
Caching Protocols for Relieving Hot Spots on the World Wide Web.* STOC 1997.

**What it actually says.** Map keys and caches onto a circle so that adding or
removing a cache remaps only an `O(1/N)` fraction of keys. Designed so clients
need not share a consistent view of the cache set. Companion “random trees”
protocol for hot-spot relief.

**Where Indexus rhymes.** Minimal disruption under membership change — the same
property HRW and XOR-nearest ownership share. Dynamo later popularised the
ring form of this idea for key-value stores.

**Where it does not.** Consistent hashing *destroys* key order by design.
Indexus refuses that destruction ([model.md §3d](model.md#3d-keys-are-not-hashed-so-the-location-tree-keeps-its-shape)). Cite this paper for the disruption
bound, not for the key layout.

### Maymounkov & Mazières 2002 — Kademlia

**Paper.** Petar Maymounkov and David Mazières. *Kademlia: A Peer-to-Peer
Information System Based on the XOR Metric.* IPTPS 2002.
[PDF](https://pdos.csail.mit.edu/~petar/papers/maymounkov-kademlia.pdf)

**What it actually says.** Distance is bitwise XOR interpreted as an integer.
The metric is symmetric and unidirectional (for fixed `x` and distance `Δ`
there is exactly one `y`). Routing tables are k-buckets by shared prefix
length. Lookups run with concurrency parameter α. Keys are 160-bit hashes;
values are stored on the k nodes closest to the key.

**Where Indexus rhymes.** The XOR metric, the bucket partition, α as
`zoneFanout`, and the “store on the k closest” idea that
[roadmap.md §2](roadmap.md#2-replication-inside-the-mesh) would reuse.
Unidirectionality is exactly why the trie is exact
([model.md §3c](model.md#3c-distance-is-unidirectional-which-is-what-makes-the-trie-exact)).

**Where it does not.** Kademlia hashes keys for uniform placement. Indexus
does not. Kademlia’s gossip-of-contacts is sufficient for *routing*; Indexus
measured that the same extract is insufficient for *membership inference*
([membership.md](membership.md#4-g--gossip)), which is why M5 exists.

### Stoica et al. 2001 — Chord

**Paper.** Ion Stoica, Robert Morris, David Karger, M. Frans Kaashoek, and
Hari Balakrishnan. *Chord: A Scalable Peer-to-peer Lookup Service for
Internet Applications.* SIGCOMM 2001.

**What it actually says.** Nodes and keys on a ring of hashed identifiers;
successor owns the key; finger tables give `O(log N)` hops; successor lists
provide fault tolerance for replication.

**Where Indexus rhymes.** Successor lists are the closest published ancestor of
“replicate to the next `r` along a total order” in the replication sketch.

**Where it does not.** Chord’s ring and hashing are not used. Do not cite Chord
as the Indexus routing model; cite Kademlia for the metric and Chord only for
the successor-list replication pattern.

---

## 4. Order, locality, and spatial queries

### Chang et al. 2006 — Bigtable

**Paper.** Fay Chang et al. *Bigtable: A Distributed Storage System for
Structured Data.* OSDI 2006.
[PDF](http://static.googleusercontent.com/media/research.google.com/en/us/archive/bigtable-osdi06.pdf)

**What it actually says.** A sparse, distributed, persistent multi-dimensional
sorted map. Rows are kept in lexicographic order and dynamically partitioned
into tablets — the unit of distribution and load balancing. Clients choose row
keys to exploit locality (e.g. reversed hostnames in Webtable). Tablets split
under load.

**Where Indexus rhymes.** Order-preserving partitioning so a spatial (or
hierarchical) region stays contiguous; load-driven split of that contiguous
range; the cost that load follows the data’s distribution. Zone handoff over a
contiguous XOR range is the peer-mesh analogue of a tablet move.

**Where it does not.** Bigtable has a master for tablet assignment and a
separate metadata path. Indexus has neither: ownership is computed. Bigtable
does not maintain an abelian aggregate tree for incremental client loads.

### Niemeyer 2008 — Geohash (and Morton 1966 Z-order)

**Paper / note.** Gustavo Niemeyer, Geohash (2008), building on G. M. Morton’s
Z-order curve (1966). Hierarchical spatial codes where a shared prefix implies
geographic proximity (with well-known edge faults at cell boundaries).

**What it actually says.** Encode lat/lng into a string such that common
prefixes nest spatially; proximity search is “this cell plus neighbours,” then
refine.

**Where Indexus rhymes.** Locations are already a hierarchical alphabet string;
nearby is a prefix walk (and neighbour expansion at the client). The aggregate
tree is the server-side materialisation that lets that walk return summaries
instead of points until the client asks for detail.

**Where it does not.** Geohash is an encoding, not a distributed ownership or
aggregation protocol. Indexus generalises the *prefix* idea beyond geography
(any dimension the collection declares) and adds ownership, handoff, and
authority.

### Bharambe, Agrawal & Seshan 2004 — Mercury

**Paper.** Ashwin R. Bharambe, Mukesh Agrawal, and Srinivasan Seshan.
*Mercury: Supporting Scalable Multi-Attribute Range Queries.* SIGCOMM 2004.
[PDF](https://www.cs.cmu.edu/~ashu/papers/sigcomm2004-paper.pdf)

**What it actually says.** Multi-attribute range queries on a P2P overlay.
One routing hub per attribute; within a hub, nodes form a ring and each holds
a **contiguous** range of attribute values (explicitly not hashed). Random
sampling builds load histograms so ranges can be rebalanced under skewed
access. Queries are conjunctions of ranges; a data item is published to every
hub for which it has an attribute.

**Where Indexus rhymes.** Contiguous placement of ordered keys so a range
(nearby region) stays local; explicit load balancing because hashing was
refused; multi-attribute schema echoes Indexus collections with multiple
dimensions. PreferNear / load-driven split is the same *pressure* Mercury
answers with sampling-based rebalance.

**Where it does not.** Mercury is a query routing layer, not an abelian
aggregate tree. It does not maintain parent summaries or a client incremental
feed. Hubs are per-attribute rings, not XOR-nearest ownership of a location
trie.

### Aspnes & Shah 2003/2007 — Skip graphs

**Paper.** James Aspnes and Gauri Shah. *Skip Graphs.* SODA 2003; ACM Trans.
Algorithms, 2007. [PDF](http://www.cs.yale.edu/homes/aspnes/papers/skip-graphs-journal.pdf)

**What it actually says.** A distributed skip-list-like overlay that preserves
key order, so range queries (`≥ x`, interval `[x,y]`, …) are native. Highly
resilient to node failures; search in `O(log n)` expected hops. Explicitly
motivated by DHT’s inability to support near-match and range queries.

**Where Indexus rhymes.** Same motivation as unhashed keys: **order is the
feature**. Range = walk along ordered peers. Resilience without a single root.

**Where it does not.** Skip graphs locate keys; they do not materialise
subtree aggregates. No abelian feed, no authority rule, no spatial prefix
algebra beyond total order on keys.

### Jagadish, Ooi & Vu 2005 — BATON

**Paper.** H. V. Jagadish, Beng Chin Ooi, and Quang Hieu Vu. *BATON: A Balanced
Tree Structure for Peer-to-Peer Networks.* VLDB 2005.
[PDF](https://www.vldb.org/archives/website/2005/program/paper/thu/p661-jagadish.pdf)

**What it actually says.** Peers form a height-balanced binary tree overlay;
each node owns a contiguous key interval and splits on join. Exact and range
queries in `O(log N)`; sideways routing tables for fault tolerance so the tree
is not a single-path SPOF. Aimed at the classic objection that tree roots are
hot.

**Where Indexus rhymes.** Contiguous intervals + split-on-growth; tree-shaped
key space; concern that hierarchical structure creates hot spots (Indexus
answers with XOR ownership of zones rather than routing every query through
ancestors).

**Where it does not.** BATON’s tree *is* the overlay routing structure.
Indexus’s location tree is the *data* structure; routing is XOR on a flat
membership set. Different layer, similar pressure.

### Ramabhadran et al. 2004 — Prefix Hash Tree (PHT)

**Paper.** Sriram Ramabhadran, Sylvia Ratnasamy, Joseph M. Hellerstein, and
Scott Shenker. *Prefix Hash Tree: An Indexing Data Structure over Distributed
Hash Tables.* 2004.
[PDF](https://people.eecs.berkeley.edu/~sylvia/papers/pht.pdf)

**What it actually says.** A trie of prefixes layered on any DHT’s `put`/`get`,
supporting range and (limited) proximity queries. Leaves hold up to `B` keys
and split/merge. Critically (§2.3.3): *“in some sense, the indexing state in
the trie is used only as an optimization”* — linear search over leaf linkage
still works if internal nodes fail; the trie accelerates lookup but is not the
authority for data availability.

**Where Indexus rhymes.** Directly the idea behind
[R10](ownership.md#3-r10--a-delegation-mark-always-has-an-owner-behind-it): a
delegation mark is indexing state; the object store and the live mesh are the
mapping; disagreement is repaired from the mapping. Also shares the ambition of
range/proximity queries over a peer system that would otherwise hash them away.

**Where it does not.** PHT still sits on a hashed DHT underneath; Indexus makes
the location tree *be* the keyspace. PHT does not define abelian parent
aggregates or a client incremental feed.

---

## 5. Aggregation trees (and what they aggregate)

### van Renesse, Birman & Vogels 2003 — Astrolabe

**Paper.** Robbert van Renesse, Kenneth P. Birman, and Werner Vogels.
*Astrolabe: A Robust and Scalable Technology for Distributed System
Monitoring, Management, and Data Mining.* ACM TOCS, 21(2), 2003.
[PDF](https://www.cs.cornell.edu/home/rvr/papers/astrolabe.pdf)

**What it actually says.** Gossip-maintained hierarchy of zones; each zone
aggregates attributes of its children with SQL-like aggregation functions;
eventual consistency; used for monitoring, resource location, and self-
configuration at large scale (propagation in tens of seconds).

**Where Indexus rhymes.** A tree of aggregates refreshed asynchronously; clients
(or applications) query summaries at the granularity they need.

**Where it does not.** Astrolabe aggregates **machine / system attributes**,
and each node is trivially authoritative about itself. Indexus aggregates
**application items**, so ownership of a subtree is a real distributed problem
and parent stubs need the authority rule. Do not cite Astrolabe as solving the
Indexus freshness problem — it never faces it.

### Yalagandula & Dahlin 2004 — SDIMS

**Paper.** Praveen Yalagandula and Mike Dahlin. *A Scalable Distributed
Information Management System.* SIGCOMM 2004.
[PDF](https://www.cs.utexas.edu/~dahlin/projects/sdims/papers/sdims-sigcomm.pdf)

**What it actually says.** DHT-derived aggregation trees; API that lets each
attribute choose how far updates propagate up and how far reads pull down
(*parametric* aggregation); lazy and on-demand reaggregation; administrative
isolation. Explicit goal: detailed nearby views and summary global views.

**Where Indexus rhymes.** Closest published relative of the aggregate tree as a
system service. “Detailed nearby + summary global” is almost the client
ambition in §1. Lazy/on-demand reaggregation parallels holding a stub when no
authority answers (U3 / accepted windows). Parametric up/down levels are an
idea Indexus has *not* adopted yet — every zone propagates the same way today
([roadmap.md](roadmap.md#4-smaller-open-items)).

**Where it does not.** Again, primarily system/attribute aggregation on DHT
trees, not an abelian application-data tree with XOR ownership of spatial
zones.

### Bhagwan, Varghese & Voelker 2003 — Cone

**Paper.** Ranjita Bhagwan, George Varghese, and Geoffrey M. Voelker. *Cone:
Augmenting DHTs to Support Distributed Resource Discovery.* UCSD Technical
Report, 2003.
[PDF](https://www.microsoft.com/en-us/research/wp-content/uploads/2016/02/cone-ucsdtr2003.pdf)

**What it actually says.** Augment a DHT with a prefix trie on node IDs;
store aggregate operators (MAX, MIN, SUM, …) on trie nodes to answer resource-
discovery queries (“a machine with at least X free RAM”) in `O(log N)`
messages.

**Where Indexus rhymes.** Aggregates living on a trie over an identifier space;
logarithmic update/query paths.

**Where it does not.** Cone aggregates **node resource attributes for
discovery**, not application data for analytics. Citing Cone for Indexus
aggregates would misstate both systems. Useful as a cautionary sibling: same
vocabulary (SUM on a trie), different problem.

### Mitra et al. / Galileo line — STASH & GeoLens

**Papers.**
- Saptashwa Mitra et al. *STASH: Fast Hierarchical Aggregation Queries for
  Effective Visual Analytics over Spatiotemporal Data.* IEEE CLUSTER
  (Galileo-backed). [PDF](https://www.cs.colostate.edu/~shrideep/papers/Stash-CLUSTER-Mitra.pdf)
- GeoLens (IEEE Big Data Congress 2014) — interactive visual analytics over
  Galileo with geohash-aligned tiles.

**What they actually say.** Galileo partitions multidimensional spatiotemporal
data with Geohash so proximate points co-locate. STASH is an in-memory
middleware cache for hierarchical aggregation queries at interactive latency;
GeoLens aligns image tiles to geohash partitions so zoom changes do not pull
the whole dataset. Explicit goal: exploratory analytics with roll-up / drill-
down over large geo datasets.

**Where Indexus rhymes.** Hierarchical spatial aggregation for interactive
nearby/zoom; geohash (prefix) as the partition key; client exploration that
loads coarser then finer summaries. Closest *product-shape* sibling to the
Aggregate/Nearby SDK ambition.

**Where it does not.** Cluster + DHT storage with a front-end analytics path,
not a coordinator-free XOR mesh with abelian parent stubs and ACK-gated
handoffs. Caching of past query results ≠ always-on materialised aggregate
tree updated by a linear feed.

---

## 5b. OLAP cubes and incremental view maintenance

These are the database-theory cousins of the abelian feed: pre-materialise
aggregates, answer roll-ups from the materialisation, update by difference
when the domain is a group.

### Gray et al. 1995/1996 — Data Cube

**Paper.** Jim Gray et al. *Data Cube: A Relational Aggregation Operator
Generalizing Group-By, Cross-Tab, and Sub-Totals.* ICDE 1996 / MSR-TR-95-22.
[PDF](https://www.microsoft.com/en-us/research/wp-content/uploads/2016/02/tr-95-22.pdf)

**What it actually says.** The CUBE operator: N-dimensional generalisation of
GROUP BY with `ALL` as the super-aggregate sentinel. Roll-up, drill-down,
histograms and cross-tabs as special cases. Motivates materialising the lattice
of group-bys for analysis workloads.

**Where Indexus rhymes.** The location prefix tree *is* a (spatial) roll-up
lattice: `@` is `ALL`, each character is a dimension cut, and every ancestor
holds the aggregate of its descendants. Drill-down = lengthen the prefix;
roll-up = shorten it. The client’s Aggregate view is cube navigation.

**Where it does not.** Gray’s cube is a relational operator and warehouse
materialisation problem, not a P2P ownership protocol. Cube maintenance is
typically batch; Indexus insists on a per-write linear feed.

### Ho, Agrawal, Megiddo & Srikant 1997 — Range queries in OLAP cubes

**Paper.** Ching-Tien Ho, Rakesh Agrawal, Nimrod Megiddo, and Ramakrishnan
Srikant. *Range Queries in OLAP Data Cubes.* SIGMOD 1997.

**What it actually says.** Precompute prefix sums (and hierarchical MAX trees)
so range-SUM queries answer in constant time (or with small extra cell
accesses) from auxiliary structures of size comparable to the cube; discuss
incremental update of the auxiliary info under batched changes.

**Where Indexus rhymes.** Pre-materialised aggregates so a spatial/region query
is a read of prepared structure, not a scan. Prefix-sum thinking is the 1-D
ancestor of “parent = sum of children.”

**Where it does not.** Dense array / warehouse setting; constant-time range SUM
via prefix arrays is not the sparse trie Indexus stores. Still the right
*ambition* citation for “pay at write time so reads are cheap.”

### Budiu et al. 2023 — DBSP

**Paper.** Mihai Budiu et al. *DBSP: Automatic Incremental View Maintenance for
Rich Query Languages.* PVLDB 16(7), 2023.
[PDF](https://vldb.org/pvldb/vol16/p1601-budiu.pdf)

**What it actually says.** Incremental view maintenance by differentiating
*streams* when values live in a **commutative group**. Z-sets (weighted
relations) turn set updates into group elements so integration/differentiation
are well-defined. Expresses relational algebra, aggregation, recursion, and
streaming queries in one circuit model; implemented for full SQL.

**Where Indexus rhymes.** The deepest formal rhyme for the linear feed: updates
are group elements; maintenance is applying `+` / `−` along a circuit (here,
up the location trie). COUNT as sum of weights is exactly Indexus `count`.
This is why metrics must be abelian — DBSP states the mathematical
prerequisite Indexus took as an engineering constraint.

**Where it does not.** DBSP is a query compiler / streaming IVM framework, not
a distributed ownership mesh. It does not place zones by XOR or serve nearby
over P2P. Cite for the algebra of the feed, not for the network.

### Koch et al. 2016 — DBToaster / distributed IVM

**Paper.** Christoph Koch et al. *How to Win a Hot Dog Eating Contest:
Distributed Incremental View Maintenance with Batch Updates.* SIGMOD 2016.
[PDF](https://www.cs.ox.ac.uk/files/9133/sigmod2016-dbtoaster-batching_divm.pdf)

**What it actually says.** Compiles SQL into specialised incremental engines;
studies tuple-at-a-time vs batched IVM; shows how to lift local IVM to Spark-
scale distributed maintenance. Nested aggregates, aggressive specialisation.

**Where Indexus rhymes.** Distributed maintenance of pre-materialised
aggregates under a high-rate update feed; the tension between single-tuple
(Indexus Feed) and batching (handoff WAL deltas, MirrorBatch).

**Where it does not.** Assumes a data-parallel cluster with a driver, not
peer XOR ownership. Query language is SQL, not a spatial prefix tree.

---

## 6. Membership, gossip, and failure detection

### Demers et al. 1987 — epidemic algorithms

**Paper.** Alan Demers et al. *Epidemic Algorithms for Replicated Database
Maintenance.* PODC 1987.
[PDF](https://www.cis.upenn.edu/~bcpierce/courses/dd/papers/demers-epidemic.pdf)

**What it actually says.** Three mechanisms for replica consistency: direct
mail, **anti-entropy** (periodic random pairwise exchange that resolves
differences), and **rumour mongering** (infective sites push updates until
they go dormant). Few guarantees required from the network; eventual
reflection of every update.

**Where Indexus rhymes.** Anti-entropy is the family name for the U tick’s
pull-from-owner repair ([convergence.md](convergence.md#2-u--background-convergence)).
Rumour-style random exchange is the family name for M5’s introductions
([membership.md](membership.md#3-m5--random-peer-sampling)).

**Where it does not.** Demers assumes many writable replicas of the same
database. Indexus’s aggregate path has one authority; anti-entropy here is
“pull the owner,” not “merge equals.”

### Jelasity et al. 2007 — gossip-based peer sampling

**Paper.** Márk Jelasity, Spyros Voulgaris, Rachid Guerraoui, Anne-Marie
Kermarrec, and Maarten van Steen. *Gossip-based Peer Sampling.* ACM TOCS,
25(3), 2007.

**What it actually says.** Factor out a **peer-sampling service**: an
abstraction whose job is to hand each node a stream of (ideally uniform)
random live peers. Implement it by gossiping membership itself over dynamic
unstructured overlays. Local randomness can be good while the global overlay
still fails theoretical uniformity assumptions; design choices hit load balance
and fault tolerance hard.

**Where Indexus rhymes.** M5’s `Random` + `discover` is exactly this
abstraction layered beside distance-sorted gossip. The measurement that G alone
has a ceiling ([membership.md §4](membership.md#4-g--gossip)) is the same
insight: a distance-biased view is not a uniform sample.

**Where it does not.** The paper’s Cyclon/Newscast/HyParView instantiations are
richer than Indexus’s `log₂ N` fanout of single introductions. Indexus did not
implement those protocols; it implemented the *need* they isolate.

### Karp et al. 2000 — randomised rumour spreading

**Paper.** Richard Karp, Christian Schindelhauer, Scott Shenker, and Berthold
Vöcking. *Randomized Rumor Spreading.* FOCS 2000.

**What it actually says.** Analyses push/pull random phone-call models. Classic
push for `Θ(log n)` rounds needs `Θ(n log n)` transmissions; push&pull can
reach `O(n log log n)` transmissions in `O(log n)` rounds; address-oblivious
algorithms have matching lower bounds; time- and communication-optimality
cannot be achieved simultaneously in that model.

**Where Indexus rhymes.** The folklore `O(log N)` rounds / logarithmic fanout
figure that `sampleFanout` follows. Useful for bounding expectation, not for
claiming Indexus matches Karp’s optimal algorithm (it does not).

**Where it does not.** Indexus ships one contact per RPC and mixes random
sampling with distance-sorted gossip; that hybrid is outside Karp’s pure model.

### Das, Gupta & Motivala 2002 — SWIM

**Paper.** Abhinandan Das, Indranil Gupta, and Ashish Motivala. *SWIM: Scalable
Weakly-consistent Infection-style Process Group Membership Protocol.* DSN
2002. [PDF](https://research.cs.cornell.edu/projects/Quicksilver/public_pdfs/SWIM.pdf)

**What it actually says.** Separates failure detection from dissemination.
Each member pings **one** random peer per period (`O(1)` load), uses indirect
probes, piggybacks membership changes (infection-style), and introduces
suspicion to cut false positives. Explicitly motivated by heartbeating’s
`O(N²)` collapse.

**Where Indexus rhymes.** The diagnosis in
[roadmap.md §3](roadmap.md#3-the-membership-ceiling): `Observe` pings every
registered peer, so full membership makes the detector `O(N)` per node.
SWIM is the standard published fix. Lifeguard (Dadgar et al., 2017) then cuts
SWIM false positives under slow processing by >50× — see
[pressures §4](pressures.md#4-full-membership--ping-everyone--completeness-over-constant-cost)
and [§9](#9-pressures-that-the-protocol-already-chose--further-mappings).

**Where it does not.** Not implemented. Citation is for the ceiling and the
known exit, not for current behaviour.

### Chandra & Toueg 1996 — unreliable failure detectors

**Paper.** Tushar Deepak Chandra and Sam Toueg. *Unreliable Failure Detectors
for Reliable Distributed Systems.* JACM, 1996.

**What it actually says.** Classifies failure detectors by completeness and
accuracy; shows which classes suffice for consensus. Timeout-based detectors
are unreliable: slow ≠ dead in an asynchronous system.

**Where Indexus rhymes.** Why quarantine expires, why R10 treats silence as a
*trigger* not a verdict, why P1 refuses to re-own a suspended peer’s zones.

### Leitão, Pereira & Rodrigues 2007 — HyParView

**Paper.** João Leitão, José Pereira, and Luís Rodrigues. *HyParView: A
Membership Protocol for Reliable Gossip-Based Broadcast.* DSN 2007.
[PDF](http://www.gsd.inesc-id.pt/~ler/reports/dsn07-leitao.pdf)

**What it actually says.** Each node keeps two **partial** views: a small
active view (`≈ log n`) used for dissemination, and a larger passive view
shuffled cyclically for repair. Designed so gossip stays reliable under high
churn without full membership.

**Where Indexus rhymes.** The roadmap tension: full `registered` membership
makes Observe `O(N)`, but R10/Q need fair samples. HyParView is the other
classic exit besides SWIM — keep partial views on purpose. Also sharpens why
M5 exists: distance-sorted gossip is a biased partial view; random sampling
restores fairness.

**Where it does not.** Not implemented. Indexus currently aims at *complete*
membership via M5 rather than embracing partial views; HyParView would be a
design fork, not a drop-in.

### Plaxton, Rajaraman & Richa 1997 — nearby copies

**Paper.** C. Greg Plaxton, Rajmohan Rajaraman, and Andréa W. Richa.
*Accessing Nearby Copies of Replicated Objects in a Distributed Environment.*
SPAA 1997.

**What it actually says.** Randomized algorithm so each access is satisfied by
a *nearby* replica under hierarchical network cost models; location info
distributed with low memory; precursor ideas later visible in Pastry /
Tapestry / Kademlia-style locality.

**Where Indexus rhymes.** “Satisfy from a nearby copy” as a first-class goal —
echoed in PreferNear spawn placement and client ingress hints (X). Name
collision with Indexus “Nearby” (spatial data) is accidental but the locality
ambition is real.

**Where it does not.** About replica *access* locality in a network metric,
not about spatial aggregates of application data. Do not conflate the two
“nearby”s when citing.

---

## 7. Consistency models and what Indexus refuses

### Shapiro et al. 2011 — CRDTs

**Paper.** Marc Shapiro, Nuno Preguiça, Carlos Baquero, and Marek Zawirski.
*Conflict-free Replicated Data Types.* SSS 2011 / INRIA RR-7687.
[PDF](https://inria.hal.science/file/index/docid/617341/filename/RR-7687.pdf)

**What it actually says.** Formalises Strong Eventual Consistency; defines
state-based (CvRDT) and operation-based (CmRDT) types that converge without
coordination when updates commute or states join by LUB. Studies sets with
add/remove (including OR-set) and composition.

**Where Indexus rhymes.** The item layer is scored as an op-based CRDT with
at-least-once delivery ([guarantees.md §4a](guarantees.md#4a-crdt-scorecard)):
distinct adds commute, duplicate apply is a no-op, tombstones join on max
generation. The accepted add-wins window is exactly the gap an OR-set would
close.

**Where it does not.** Ownership is **not** a CRDT — it converges by a
variant function (R3), not by a merge. Parent stubs are **not** CRDTs — they
follow authority. Citing “Indexus is a CRDT database” is wrong; citing “item
apply is CRDT-shaped” is right.

### DeCandia et al. 2007 — Dynamo

**Paper.** Giuseppe DeCandia et al. *Dynamo: Amazon’s Highly Available
Key-value Store.* SOSP 2007.
[PDF](https://read.seas.harvard.edu/~kohler/class/08w-dsi/decandia07dynamo.pdf)

**What it actually says.** Consistent-hashing ring, `N` replicas, sloppy quorum
with `R`/`W`, vector clocks, application-assisted conflict resolution,
gossip membership. Always-on preference over strict consistency.

**Where Indexus rhymes.** AP posture; gossip for membership; willingness to
serve during partitions and heal later.

**Where it does not.** Dynamo’s quorum model is the design
[roadmap.md §2](roadmap.md#2-replication-inside-the-mesh) **rejects** for
aggregates: any-replica writes would break the authority rule. Cite Dynamo as
the fork in the road, not as the template.

### Lamport 1978 — Time, Clocks…

**Paper.** Leslie Lamport. *Time, Clocks, and the Ordering of Events in a
Distributed System.* CACM, 1978.

**What it actually says.** Logical clocks induce a partial order consistent
with causality; concurrent events remain unordered.

**Where Indexus rhymes.** The rejected alternative for stub freshness
([aggregates.md §4](aggregates.md#4-the-alternative-we-rejected)): a clock
beside the value cannot survive handoff (V3) without becoming an agreement
about the cutover instant. Authority derives order from ownership instead.

### Fischer, Lynch & Paterson 1985 — FLP

**Paper.** Michael J. Fischer, Nancy A. Lynch, and Michael S. Paterson.
*Impossibility of Distributed Consensus with One Faulty Process.* JACM, 1985.

**What it actually says.** In pure asynchrony, no deterministic protocol solves
consensus if even one process may crash.

**Where Indexus rhymes.** Ownership is never agreed — it is evaluated — so FLP
is sidestepped rather than solved ([model.md §3a](model.md#3a-ownership-is-decidable-locally-with-no-consensus-and-no-tie-break)).

### Gilbert & Lynch 2002 — CAP

**Paper.** Seth Gilbert and Nancy Lynch. *Brewer’s Conjecture and the
Feasibility of Consistent, Available, Partition-Tolerant Web Services.*
SIGACT News, 2002.

**What it actually says.** Formalises Brewer’s conjecture: under partition,
linearizability and availability cannot both be guaranteed.

**Where Indexus rhymes.** Stated AP posture on the README; remerge via Q1.

---

## 8. Operational mechanisms

### Vattani, Chierichetti & Lowenstein 2015 — cache stampede

**Paper.** Andrea Vattani, Flavio Chierichetti, and Keegan Lowenstein.
*Optimal Probabilistic Cache Stampede Prevention.* PVLDB, 8(8), 2015.
[PDF](http://www.vldb.org/pvldb/vol8/p886-vattani.pdf)

**What it actually says.** Probabilistic early expiration (XFetch): before
TTL, each request may regenerate with a probability that rises toward expiry
(`beta`, recomputation cost, `log(random)`). Optimal under their model; no
distributed lock required.

**Where Indexus rhymes.** The `beta` parameter of `cache.Refresh` plus the
η-skip and per-tick cap ([convergence.md U1](convergence.md#2-u--background-convergence)).

**Where it does not.** Indexus combines that idea with hard caps and shuffle;
it is not a pure XFetch deployment.

### Alsberg & Day 1976 — primary copy

**Paper.** Peter A. Alsberg and John D. Day. *A Principle for Resilient Sharing
of Distributed Resources.* ICSE 1976.

**What it actually says.** Designate one copy as primary for updates; others
follow. Classical primary-copy replication.

**Where Indexus rhymes.** Parent-stub authority is primary-copy of a single
value, with the primary *computed* by XOR rather than elected
([aggregates.md](aggregates.md#3-the-mechanism-authority)).

### van Renesse & Schneider 2004 — chain replication

**Paper.** Robbert van Renesse and Fred B. Schneider. *Chain Replication for
Supporting High Throughput and Availability.* OSDI 2004.
[PDF](https://www.usenix.org/legacy/events/osdi04/tech/full_papers/renesse/renesse.pdf)

**What it actually says.** Primary/backup arranged as a chain: head sequences
writes, updates propagate to the tail, reads served at the tail — strong
consistency with better throughput than classical primary/backup. Explicitly
compared to DHT object placement. Fail-stop model; configuration service for
membership of the chain.

**Where Indexus rhymes.** The replication sketch in
[roadmap.md §2](roadmap.md#2-replication-inside-the-mesh) wants **one writer
(primary) + ordered backups**, not Dynamo quorums — chain replication is the
canonical strong-consistency primary-backup refinement. Also: durability and
availability without sacrificing a single serial order of updates (needed for
authority).

**Where it does not.** Needs a configuration service for chain membership;
Indexus would want the chain = the `r` XOR-nearest nodes, recomputed rather
than configured. Not implemented.

### Agarwal et al. 2012 — mergeable summaries

**Paper.** Pankaj K. Agarwal et al. *Mergeable Summaries.* PODS 2012 / ACM
Trans. Algorithms.

**What it actually says.** Sketches that merge associatively (e.g. for
quantiles and heavy hitters) with proven error bounds.

**Where Indexus rhymes.** The open path to non-invertible metrics
([roadmap.md](roadmap.md#4-smaller-open-items)): mergeable ≠ invertible, so
deletes would re-aggregate rather than subtract.

### Kleppmann et al. 2019 — local-first software

**Paper.** Martin Kleppmann, Adam Wiggins, Peter van Hardenberg, and Mark
McGranaghan. *Local-First Software: You Own Your Data, in spite of the Cloud.*
Onward! 2019. [PDF](https://martin.kleppmann.com/papers/local-first.pdf)

**What it actually says.** Principles for apps that keep data local, work
offline, sync via CRDTs, and treat the cloud as optional infrastructure rather
than the system of record. Motivated by ownership, latency, and longevity.

**Where Indexus rhymes.** Loose but real: the **client plans its own
incremental load** over a tree it understands; the mesh is infrastructure that
serves that plan, not a black-box query engine. “Database inside-out” flavour —
state is a feed of updates maintaining a structure the client also navigates.

**Where it does not.** Local-first is about end-user documents and CRDT sync
between devices. Indexus is a server-side spatial aggregate mesh. Kleppmann is
not a blueprint; useful when explaining why the SDK, not the server, owns the
query plan.

### García-Molina & Salem 1987 — Sagas

**Paper.** Hector García-Molina and Kenneth Salem. *Sagas.* SIGMOD 1987.

**What it actually says.** Long-lived work as a sequence of local transactions
with **compensating** transactions if the saga aborts mid-way — avoid holding
locks for the whole LLT.

**Where Indexus rhymes.** Loose analogy for [R8](ownership.md#2-guarantees): a
delegation session that installs then rolls back is closer to a compensated
partial execution than to 2PC. `cancelInbound` / `releaseZones` are
compensations for `applyZone` / `own()`.

**Where it does not.** Sagas are for multi-step business transactions in a
DBMS. Do not over-cite; the rhyme is “compensation over atomic global commit,”
nothing more.

---

## 9. Pressures that the protocol already chose — further mappings

These entries exist to arm [pressures.md](pressures.md): proofs, measurements,
and industrial exits next to Indexus decisions. Still analogical.

### Karger & Rühl (2004) — move nodes where load is

**Paper.** Karger, D. R., and Rühl, M. *Simple Efficient Load-Balancing
Algorithms for Peer-to-Peer Systems.* SPAA / IPTPS 2004; Theory of Computing
Systems journal version.
https://ic.unicamp.br/~celio/peer2peer/miscelanea/load-balancing-karger.pdf

**What it says.** When items **cannot** be randomised in the address space
(ordered / range data), balance by relocating nodes “where they are needed,”
with constant-factor of optimal and an application to distributed range search.

**Where Indexus rhymes.** Unhashed keys put Indexus in exactly that regime.
`PreferNear` + load-driven spawn is the same *shape*: place capacity in the
half-space that holds the weight ([pressures §1](pressures.md#1-keys-are-not-hashed--locality-over-balance)).

**Where it does not.** Their analysis is for Chord-style rings with node moves
in identifier space; Indexus grows by spawning and splitting zones, not by
sliding IDs continuously.

### Mirrokni et al. (2016) — bounded loads when hashing is allowed

**Paper.** Mirrokni, V., Thorup, M., and Zadimoghaddam, M. *Consistent Hashing
with Bounded Loads.* arXiv:1608.01350 / SODA-adjacent line.

**What it says.** With (randomised) consistent hashing, one can keep max load
within a small additive of the average — restoring the classic balls-and-bins
gap that naive CH leaves as `Θ(log n / log log n)`.

**Where Indexus rhymes.** Contrast only: this is the exit *if* Indexus ever
hashed keys. Taking it would buy balance and **kill** contiguous nearby
([model.md §3d](model.md#3d-keys-are-not-hashed-so-the-location-tree-keeps-its-shape)).

### Abadi PACELC (2012) — latency even when there is no partition

**Paper.** Abadi, D. J. (2012). *Consistency Tradeoffs in Modern Distributed
Database System Design.* IEEE Computer (PACELC).

**What it says.** CAP is incomplete: if there is a **P**artition, choose A or C;
**E**lse, choose **L**atency or **C**onsistency.

**Where Indexus rhymes.** Rough class **PA/EL**: under partition both sides
serve what they can; otherwise prefer cheap eventual reads over linearizable
coordination. Twist: writes blocked by named marks **park** (P5/W1) rather than accept
everywhere — availability is not absolute on the write path
([pressures §3](pressures.md#3-one-serving-copy-per-zone--authority-over-availability)).

### Huang et al. (2017) — gray failure / differential observability

**Paper.** Huang, P., et al. *Gray Failure: The Achilles’ Heel of
Cloud-Scale Systems.* HotOS 2017.

**What it says.** The hard faults are those where components disagree on
whether a peer is healthy — detectors say up, clients say down.

**Where Indexus rhymes.** Ping/gossip can miss what the write path feels.
Indexus parks blocked writes but deliberately does not promote that silence to
an authority verdict
([pressures §3](pressures.md#3-one-serving-copy-per-zone--authority-over-availability)).

### Dadgar, Phillips & Currey — Lifeguard (2017/2018)

**Paper.** Dadgar, A., Phillips, J., and Currey, J. *Lifeguard: Local Health
Awareness for More Accurate Failure Detection.* arXiv:1707.00788 / DSN-W 2018.
https://arxiv.org/abs/1707.00788

**What it says.** SWIM false-positives explode under slow local processing;
local-health extensions cut FPs **>50×** without slowing true failure
detection.

**Where Indexus rhymes.** Full-mesh Observe + quarantine (90 s) is sensitive to
the same class of load-induced suspicion. Lifeguard is the known exit before
shortening timeouts ([pressures §4](pressures.md#4-full-membership--ping-everyone--completeness-over-constant-cost)).

### HBase Region Normalizer — merge hysteresis (operational)

**Source.** Apache HBase Region Normalizer (docs + `min_region_age`,
`min_region_count`, split/merge size bands).

**What it says.** Merge only adjacent undersized regions; refuse young
regions; keep a size band so split and merge do not oscillate at one
threshold.

**Where Indexus rhymes.** Direct recipe for [roadmap zone merge](roadmap.md#1-scaling-down-zone-merging)
and for PreferNear split thresholds ([pressures §7](pressures.md#7-zones-split-never-merge--growth-without-shrink)).

### Geohash fault lines — neighbour expansion

**Sources.** Niemeyer Geohash; operational geohash literature on edge effects;
H3 (Uber), S2 (Google) as encodings with better neighbour gradients.

**What it says.** Shared prefix is necessary but not sufficient for geographic
nearby: cell boundaries create “fault lines” where true neighbours share no
prefix. Query centre + neighbours, then distance-filter.

**Where Indexus rhymes.** Client Nearby that walks one prefix only will miss
real neighbours — looks like a mesh bug, is an encoding bug
([pressures §6](pressures.md#6-prefix-nearby--cheap-spatial-queries-fault-lines)).

---

## 10. Formal bibliography

Sorted by year. Links are convenience copies; prefer the publisher of record
when citing externally. **~48 entries** — the annotated mappings above are the
reading guide; this list is for citation lookup.

1. Alsberg, P. A., and Day, J. D. (1976). *A Principle for Resilient Sharing of
   Distributed Resources.* ICSE.
2. Lamport, L. (1978). *Time, Clocks, and the Ordering of Events in a
   Distributed System.* CACM, 21(7).
3. Fischer, M. J., Lynch, N. A., and Paterson, M. S. (1985). *Impossibility of
   Distributed Consensus with One Faulty Process.* JACM, 32(2).
4. Demers, A., et al. (1987). *Epidemic Algorithms for Replicated Database
   Maintenance.* PODC.
5. García-Molina, H., and Salem, K. (1987). *Sagas.* SIGMOD.
6. Gray, J., et al. (1995/1996). *Data Cube: A Relational Aggregation Operator
   Generalizing Group-By, Cross-Tab, and Sub-Totals.* ICDE / MSR-TR-95-22.
   https://www.microsoft.com/en-us/research/wp-content/uploads/2016/02/tr-95-22.pdf
7. Chandra, T. D., and Toueg, S. (1996). *Unreliable Failure Detectors for
   Reliable Distributed Systems.* JACM, 43(2).
8. Ho, C.-T., Agrawal, R., Megiddo, N., and Srikant, R. (1997). *Range Queries
   in OLAP Data Cubes.* SIGMOD.
9. Karger, D., et al. (1997). *Consistent Hashing and Random Trees.* STOC.
10. Plaxton, C. G., Rajaraman, R., and Richa, A. W. (1997). *Accessing Nearby
    Copies of Replicated Objects in a Distributed Environment.* SPAA.
11. Thaler, D. G., and Ravishankar, C. V. (1998). *Using Name-Based Mappings to
    Increase Hit Rates.* IEEE/ACM ToN, 6(4).
    http://www.cs.ucr.edu/~ravi/Papers/Jrnl/HRW98.pdf
12. Karp, R., Schindelhauer, C., Shenker, S., and Vöcking, B. (2000).
    *Randomized Rumor Spreading.* FOCS.
13. Stoica, I., et al. (2001). *Chord.* SIGCOMM.
14. Das, A., Gupta, I., and Motivala, A. (2002). *SWIM.* DSN.
    https://research.cs.cornell.edu/projects/Quicksilver/public_pdfs/SWIM.pdf
15. Gilbert, S., and Lynch, N. (2002). *Brewer’s Conjecture…* SIGACT News.
16. Maymounkov, P., and Mazières, D. (2002). *Kademlia.* IPTPS.
    https://pdos.csail.mit.edu/~petar/papers/maymounkov-kademlia.pdf
17. Aspnes, J., and Shah, G. (2003/2007). *Skip Graphs.* SODA / ACM TALG.
    http://www.cs.yale.edu/homes/aspnes/papers/skip-graphs-journal.pdf
18. Bhagwan, R., Varghese, G., and Voelker, G. M. (2003). *Cone.* UCSD TR.
    https://www.microsoft.com/en-us/research/wp-content/uploads/2016/02/cone-ucsdtr2003.pdf
19. van Renesse, R., Birman, K. P., and Vogels, W. (2003). *Astrolabe.* ACM
    TOCS. https://www.cs.cornell.edu/home/rvr/papers/astrolabe.pdf
20. Bharambe, A. R., Agrawal, M., and Seshan, S. (2004). *Mercury.* SIGCOMM.
    https://www.cs.cmu.edu/~ashu/papers/sigcomm2004-paper.pdf
21. Karger, D. R., and Rühl, M. (2004). *Simple Efficient Load-Balancing
    Algorithms for Peer-to-Peer Systems.* SPAA / IPTPS.
    https://ic.unicamp.br/~celio/peer2peer/miscelanea/load-balancing-karger.pdf
22. Ramabhadran, S., et al. (2004). *Prefix Hash Tree.*
    https://people.eecs.berkeley.edu/~sylvia/papers/pht.pdf
23. van Renesse, R., and Schneider, F. B. (2004). *Chain Replication.* OSDI.
    https://www.usenix.org/legacy/events/osdi04/tech/full_papers/renesse/renesse.pdf
24. Yalagandula, P., and Dahlin, M. (2004). *SDIMS.* SIGCOMM.
    https://www.cs.utexas.edu/~dahlin/projects/sdims/papers/sdims-sigcomm.pdf
25. Jagadish, H. V., Ooi, B. C., and Vu, Q. H. (2005). *BATON.* VLDB.
    https://www.vldb.org/archives/website/2005/program/paper/thu/p661-jagadish.pdf
26. Chang, F., et al. (2006). *Bigtable.* OSDI.
    http://static.googleusercontent.com/media/research.google.com/en/us/archive/bigtable-osdi06.pdf
27. DeCandia, G., et al. (2007). *Dynamo.* SOSP.
    https://read.seas.harvard.edu/~kohler/class/08w-dsi/decandia07dynamo.pdf
28. Jelasity, M., et al. (2007). *Gossip-based Peer Sampling.* ACM TOCS, 25(3).
29. Leitão, J., Pereira, J., and Rodrigues, L. (2007). *HyParView.* DSN.
    http://www.gsd.inesc-id.pt/~ler/reports/dsn07-leitao.pdf
30. Niemeyer, G. (2008). *Geohash.* (see also Morton 1966, Z-order; fault-line /
    neighbour-expansion operational notes).
31. Shapiro, M., Preguiça, N., Baquero, C., and Zawirski, M. (2011). *CRDTs.*
    SSS / INRIA RR-7687.
    https://inria.hal.science/file/index/docid/617341/filename/RR-7687.pdf
32. Abadi, D. J. (2012). *Consistency Tradeoffs in Modern Distributed Database
    System Design* (PACELC). IEEE Computer.
33. Agarwal, P. K., et al. (2012). *Mergeable Summaries.* PODS.
34. GeoLens / Galileo line (2014). *Enabling Interactive Visual Analytics over
    Large-Scale Multidimensional Geospatial Datasets.* IEEE Big Data Congress.
35. Vattani, A., Chierichetti, F., and Lowenstein, K. (2015). *Optimal
    Probabilistic Cache Stampede Prevention.* PVLDB, 8(8).
    http://www.vldb.org/pvldb/vol8/p886-vattani.pdf
36. Mirrokni, V., Thorup, M., and Zadimoghaddam, M. (2016). *Consistent Hashing
    with Bounded Loads.* arXiv:1608.01350.
37. Koch, C., et al. (2016). *How to Win a Hot Dog Eating Contest* (distributed
    IVM / DBToaster). SIGMOD.
    https://www.cs.ox.ac.uk/files/9133/sigmod2016-dbtoaster-batching_divm.pdf
38. Dadgar, A., Phillips, J., and Currey, J. (2017). *Lifeguard: Local Health
    Awareness for More Accurate Failure Detection.* arXiv:1707.00788 / DSN-W
    2018. https://arxiv.org/abs/1707.00788
39. Huang, P., et al. (2017). *Gray Failure: The Achilles’ Heel of Cloud-Scale
    Systems.* HotOS.
40. Mitra, S., et al. *STASH: Fast Hierarchical Aggregation Queries…* IEEE
    CLUSTER. https://www.cs.colostate.edu/~shrideep/papers/Stash-CLUSTER-Mitra.pdf
41. Kleppmann, M., et al. (2019). *Local-First Software.* Onward!
    https://martin.kleppmann.com/papers/local-first.pdf
42. Budiu, M., et al. (2023). *DBSP: Automatic Incremental View Maintenance for
    Rich Query Languages.* PVLDB, 16(7).
    https://vldb.org/pvldb/vol16/p1601-budiu.pdf
43. Apache HBase. *Region Normalizer* (operational docs — merge hysteresis,
    `min_region_age`, size bands). https://hbase.apache.org/

---

## 11. Quick map: mechanism → papers

| Indexus mechanism | Primary rhyme | Secondary / contrast |
|---|---|---|
| XOR ownership, no consensus | Thaler & Ravishankar 1998; Maymounkov & Mazières 2002 | Karger 1997; FLP 1985; Plaxton 1997 (locality) |
| Unhashed keys, contiguous zones | Bigtable 2006; Mercury 2004; Skip graphs; BATON 2005 | Kademlia/Chord (hash — deliberate opposite); Mirrokni 2016 (bounded load *if* hashing) |
| Load skew under ordered keys | **Karger & Rühl 2004** (move nodes to load) | PreferNear / autoscale (Indexus exit in use) |
| Abelian aggregate tree + linear feed | **DBSP 2023** (group IVM); Gray cube 1996; Ho et al. 1997 | SDIMS/Astrolabe (node attrs); Cone (discovery) |
| Nearby + incremental client load | STASH/GeoLens; Geohash; SDIMS “nearby detail” | Kleppmann 2019 (client owns the plan — loose); H3/S2 (fault lines) |
| Load-driven split / PreferNear | Karger–Rühl 2004; Mercury sampling; Bigtable tablet split | HBase normalizer (hysteresis) |
| Delegation mark vs durable mapping (R10) | PHT 2004 §2.3.3 | — |
| Stub authority | Alsberg & Day 1976 | Lamport 1978 (rejected) |
| Item apply / scorecard | Shapiro et al. 2011 (OR-set gap) | — |
| Handoff rollback (R8) | Sagas 1987 (compensation — loose) | 2PC (not used) |
| M5 random introductions | Jelasity 2007; Demers 1987 | Karp 2000; HyParView 2007 (partial views) |
| Gossip ceiling | Kademlia k-buckets + measurements here | — |
| Silence / false suspicion | Chandra & Toueg 1996; **Lifeguard 2017**; Gray Failure 2017 | named marks + parked writes; silence does not reassign authority |
| CAP / latency class | Gilbert & Lynch 2002; **PACELC 2012** (PA/EL-ish) | — |
| U tick anti-entropy | Demers 1987 | Dynamo Merkle (multi-writer contrast) |
| Cache `beta` | Vattani et al. 2015 | — |
| Replication sketch (roadmap) | Chain replication 2004; Kademlia k-closest | Dynamo quorum (**rejected**) |
| Membership `O(N)` ceiling (roadmap) | SWIM 2002; Lifeguard 2017; HyParView 2007 | — |
| Zone merge (roadmap) | HBase Region Normalizer; Bigtable tablet merge | — |
| Non-invertible metrics (roadmap) | Agarwal et al. 2012 | — |

**Argument layer:** [pressures.md](pressures.md) (advantage / cost / evidence / exit per decision).

---

[← Glossary](glossary.md) · [Index](README.md) · [Pressures →](pressures.md)
