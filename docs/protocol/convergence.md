[← Ownership](ownership.md) · [Index](README.md) · [Aggregates →](aggregates.md)

# Convergence — Claim, parent stubs, cache refresh

Two jobs share the Update tick: making remote children visible to their parent
holders, and keeping parent stubs / cache rows fresh. Discovery-by-probe
(`/children` alphabet fan-out) and silence-based orphan reclaim are retired.

---

## 1. D — Child discovery via Claim

The child that owns a zone knows it exists; the parent can only guess. Each
repair tick, every locally owned zone pushes `Claim(parent, child, me)` to the
XOR-nearest holder of its parent. The parent, if it owns that parent location,
records a **named** mark and later pulls `/aggregates` for the value.

### Guarantees

- **D1** A child zone owned by a remote node becomes visible to the parent
  owner within one repair tick of the child being able to reach that parent
  (Claim push). Tests: `TestPushClaimNamesMarkOnLocalParent`.
- **D2** Repair terminates: each owned zone issues one Claim per tick; mark
  verification fans out to named peers only, not the whole alphabet.
- **D3** Claim writes no stub value. It plants/refreshes `Handoff{Peer,At}` and
  then pulls `/aggregates` for the abelian. Discovery never invents mass.

`/children` remains available as a peer RPC for diagnostics and redirects, but
it is not the production discovery loop.

---

## 2. U — Background convergence

One tick, in order:

```
Update():
  cache.SetMax(settings.cacheMax)
  repair()                         # Claim pushes + named-mark verify (R10)
  reconcileDelegatedParents()      # /aggregates pulls for known marks (U3)
  if TransferBusy() → return
  for key in shuffle(cache.Refresh(expiration, beta)):     # ≤ UpdateMaxPulls
      skip with probability η
      peer := find(key); if !remote(peer) → skip
      /aggregates(batched by peer)  → applyPulledAbelian
      or peer.Get(refresh=true)     → applyPulledSet
```

**The two apply paths stay the freshness rule.** `applyPulledAbelian` handles an
`/aggregates` response (owner-only) and writes the parent stub; `applyPulledSet`
handles `/set` and touches only the cache.

### Guarantees

- **U1 (no stampede).** Cache refreshes are capped, shuffled, and probabilistically
  skipped.
- **U2 (refresh is not a client read).** See [K2](reads.md#4-guarantees).
- **U3 (the stub follows the owner).** A parent stub converges to the value its
  child's current owner serves. Tests: `TestParentStubFollowsOwnerAcrossHandoff`,
  `TestParentStubIgnoresNonAuthoritativeSource`,
  `TestParentStubAcceptsOwnerShrink`.

## 3. Where the tick meets ownership

| Step | Reads | Writes | Gated by |
|---|---|---|---|
| `repair` | Claim RPC, `/aggregates` | named marks, Own on membership death | every tick |
| `reconcileDelegatedParents` | `/aggregates` | parent stubs | every tick |
| cache refresh | `/set`, `/aggregates` | cache, parent stubs | `!TransferBusy()` |

Silence never lifts a mark — see
[R10](ownership.md#3-r10--a-delegation-mark-never-blocks-progress).

---

## Related

- [ownership.md R10](ownership.md#3-r10--a-delegation-mark-never-blocks-progress)
- [aggregates.md](aggregates.md)
- [guarantees.md](guarantees.md)

---

[← Ownership](ownership.md) · [Index](README.md) · [Aggregates →](aggregates.md)
