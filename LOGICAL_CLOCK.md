# Logical clocks and replication in go-indexus-core

## Current model (no logical clock)

- **`domain.Item`** carries only `collection`, `location`, `id`, and `metrics`. There is no version, Lamport timestamp, or replica id.
- **WAL / shard snapshots** store opaque `ItemContent(...)` lines; nothing records causal order or delivery generation.
- **Leaf state** in `domain.Set` is **replace-by-key**: `add` assigns `s.list[key] = abelian`. Two deliveries for the same `(location, id)` are **last writer wins** on the leaf; they do **not** automatically merge as a CRDT unless higher-level invariants guarantee single delivery.
- **Aggregates** along the ownership tree use commutative **`Abelian.Sum` / `Incr`**, which are order-insensitive **if each logical event is applied exactly once**.
- **`POST /transfer`** replays items through the normal `New` → queue → `insert` path. On transport failure after a sender has already called `Delegate`, `core/jobs.go` re-enqueues items through `n.New` so they are not lost; a **successful** transfer followed by a **retry** could still double-apply aggregates unless higher layers deduplicate (today they do not).

## Failure mode multinode tests are meant to surface

When ownership is split across nodes, an item may contribute to **ancestor aggregates** on the sender (above the delegated subtree) and again on the receiver after `Transfer`. There is no per-item signal that says “this increment was already counted upstream on peer P”. Tests in `app/simulation/mockup/` assert **global leaf count** (`Count()` summed over nodes) and **disjoint `(collection, location)` ownership**, plus `Check()` for structural drift; they do **not** yet assert bitwise equality of every internal `Abelian` aggregate across hypothetical duplicate paths.

## Options (cost vs. guarantee)

### 1. Lamport (or hybrid logical) clock per item — **small additive change**

- Add `uint64` clock to `Item`, bump monotonically per node, include in WAL line and `ItemContent`.
- Receiver keeps a small per-shard “last applied `(id, clock)`” map (or max clock per id) and **skips** ancestor `Incr` when `clock <= applied[id]` (or equivalent LWW rule on the leaf only).
- **Pros:** Cheap, fixes duplicate delivery / retry double-count on aggregates if scoped correctly.
- **Cons:** Requires WAL migration or dual-read for old lines without clocks; semantics for “concurrent same id from two writers” need a tie-break (node id).

### 2. Per-shard generation / epoch — **medium**

- Bump a generation on each ownership transition; tag transferred batches with that generation.
- **Pros:** Coarser than per-item; good for “this `Transfer` batch is idempotent”.
- **Cons:** Still need rules when generations overlap across partial failures.

### 3. Vector clock + LWW merge — **large**

- True multi-writer causal semantics for the same `(location, id)`.
- **Pros:** Rich conflict detection and ordering.
- **Cons:** Touches `Set` merge policy, HTTP contracts, snapshot/gob formats, and client SDKs.

## Recommendation

**Defer implementing clocks until** multinode tests (`late_join`, `add_while_joining`, `delegation_completude`, and the `batch-loader-indexus/multinode/` harness) show **aggregate inflation** or **duplicate leaf** anomalies that cannot be fixed by tightening transfer invariants alone.

If tests show only **leaf-level** duplicates, prefer **option 1** scoped to dedup on `(collection, location, id)` with a Lamport clock and a bounded LRU of applied ids per shard.

If tests stay green on **global leaf sum** and **disjoint ownership**, clocks remain optional observability (e.g. expose node boot time and last `Refresh` tick in `/cache`) rather than correctness-critical path.
