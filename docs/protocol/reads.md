[← Ownership](ownership.md) · [Index](README.md) · [Convergence →](convergence.md)

# Reads

`core/read.go`, plus `http/p2p/p2p.go` for the ingress hint.

Reads are [eventually consistent](glossary.md#eventual-consistency) by design;
no [linearizability](glossary.md#linearizability) is claimed. What the read path
does guarantee is narrower and more useful: it never invents an empty zone, it
never serves a copy it knows is stale while an owner is reachable, and it always
terminates.

---

## 1. Two callers, two resolutions

Collapsing them into one `Get` made the cache immortal: an internal refresh
reset the TTL, so the more a key was refreshed the less it expired.

```
Get(collection, location, deep, via, refresh):
  via ∋ self? → cancel (errVisited)        # the walk closed on itself
  via := via + self                        # what we forward downstream
  if refresh → resolveRefresh(via)
  → resolveClient(deep, via)

resolveRefresh:                            # source of truth only
  own it?            → return (self, set)          # no cache read, no cache write
  nearest remote and ∉ via? → nearest.Get(via, refresh)
  tryPeersGet(1 peer, off while TransferBusy)
  → nil

resolveClient:
  local set exists?
      own it?              → return (self, set)
      !deep?               → serveStale(nearest, set)
      else                 → stale := set          # a fallback, not an answer
  cache hit (not a summary placeholder)? → return (nearest, cached)   # touches LRU
  !deep?             → cache.SetHops(nil, 0); return (nearest, nil)
  nearest remote and ∉ via? → nearest.Get(via); on non-empty → cache + return
  tryPeersGet(zoneFanout peers, skip=nearest) → on non-empty → cache + return
  stale != nil?      → serveStale(nearest, stale)
  → (nearest, nil)
```

## 2. The three bounds

### 2a. Termination is by path, not by budget

A forwarded read carries **the path it walked**, not a hop budget. Every node
prepends itself to `via` before forwarding and refuses a request that already
names it, so a routing cycle — two nodes with divergent views each naming the
other XOR-nearest, the normal state right after a partition heals — dies *where
it closes* instead of after a fixed number of steps.

Peers already on the path are dropped from `zoneCandidates` for the same reason:
asking them would only earn a cancellation. `len(via)` is the distance from the
client, and it is what the cache stamps as its hop count, so TTL decays with the
real depth of the answer.

This is the read-path counterpart to the missing hop count in the write path
([model.md §3b](model.md#3b-what-it-does-not-buy-is-agreement)) — reads got the
[visited set](glossary.md#visited-set) and writes did not, which is exactly why
P5 had to exist.

### 2b. Fan-out is bounded by ordering, not by luck

A miss walks outward, and how far it may walk is a bound the protocol has to
set. `zoneCandidates` orders the live peers by XOR distance **to the zone key** —
not by traversal order — and returns the first `zoneFanout = 3` of them,
[Kademlia's α](glossary.md#alpha-concurrency).

Ordering by the key is what makes the small cap sufficient: the peers most likely
to own the zone are asked first, so a stale route heals on the same read instead
of degrading into a scan of the mesh.

### 2c. A non-owned local copy is a last resort

The `stale` demotion is the correction that made multi-node reads correct. The
previous code returned a locally present set immediately, *even when it knew it
was not the owner* — it named another node as the contact and served its own copy
anyway. After a handoff the donor keeps a copy that no longer receives updates,
so the ingress node answered from a frozen snapshot.

A non-owned copy is now only served when nothing else answered, and it is
credited to the owner so the client re-routes on its next call.

## 3. X — client ingress hint

A client picks its entry node by XOR proximity between its session key and the
known peers. As the mesh grows, that node stops being the nearest, and the client
can only find out by re-probing. Instead, the server volunteers the information
on traffic it is already serving.

`RoutingHint(clientKey)` returns the XOR-nearest *registered* peer, or `nil` if
that is the node itself. On `/sets`, `writeIngressHint` decodes
`X-Indexus-Routing-Key` as base64url(session bytes) and sets
`X-Indexus-Ingress-{Name,IP,Port}`. Those headers are listed in CORS
`ExposedHeaders` so browsers can read them. The hint travels in headers because
the `/sets` body is a binary stream shared with other callers.

The client adopts a hint **only if it is strictly closer to its own key** than
the current ingress, measured with the same XOR prefix order the server used.
Without that check the server's ranking (a superset routing table) can make A
point to B and B point to A forever — the same divergence problem as §2a, one
layer out. Tests: `TestRoutingHintReturnsCloserPeer`,
`TestRoutingHintNilWhenSelfNearest`, `TestGetMultipleExposesIngressHintHeaders`,
and `network/ingressHint.test.mjs`.

## 3b. `/sets` redirect envelope (client-managed navigation)

`GET /sets` defaults to the legacy raw `EncodeSets` body. Callers that opt in
with `envelope=1` receive an IXS1 framed payload:

```
IXS1 | u32 redirectCount | repeats(u16 loc, bytes, u16 name, bytes, u16 ip, bytes, u16 port)
     | EncodeSets body
```

When `deep=false`, unresolved locations contribute redirects instead of
triggering inter-node pulls. Clients group follow-up `/sets` requests by owner
and carry `via` to bound redirect loops. Inter-node `Contact.Get` uses the same
`/sets?envelope=1` path so core and browsers cannot drift. `/set` remains a
JSON adapter over the shared `Get` primitive. Tests:
`TestGetMultipleEnvelopeRedirectsWithoutPull`,
`TestGetMultipleEnvelopeAbsentKeepsLegacyBody`,
`TestSetsEnvelopeRoundTrip`.

## 4. Guarantees

- **K1 (owner precedence).** A deep read never serves a non-owned local copy
  while any peer can answer. Tests: `TestGetPrefersOwnerOverStaleLocalCopy`,
  `TestGetMultiplePrefersOwnerOverStaleLocalCopy`.
- **K2 (refresh isolation).** A refresh read never reads the cache, never writes
  it, and never touches the LRU. Tests:
  `TestRefreshBypassesLocalCacheAndPullsOwner`, `TestRefreshDoesNotTouchLRU`,
  `TestRefreshOnOwnedZoneLeavesCacheEntryCold`.
- **K3 (shallow reads are local).** `deep=false` issues no RPC. Tests:
  `TestGetServesStaleLocalCopyWhenNotDeep`, `TestGetDeepFalseRedirectsWithoutPull`.
- **K4 (liveness over precision).** If no peer answers, the stale local copy is
  served rather than an empty set — credited to the owner, so the client
  re-routes on its next call. **An unreachable owner is a miss, never an empty
  zone.** Tests: `TestGetFallsBackToStaleLocalCopyWhenNoPeerAnswers`,
  `TestServeStaleCreditsTheOwnerNotUs`,
  `TestEmptyAnswerFromNearestIsAMissNotATruth`,
  `TestPartitionedReadIsAMissNotAnEmptyZone`.
- **K5 (aggregates are authoritative).** `GET /aggregates` answers only for
  locally owned locations and never forwards, so a response *certifies*
  ownership — and a donor falls silent on a zone it handed off even while it
  still owns the parent above it. This is the transport-level half of the
  [authority rule](aggregates.md#3-the-mechanism-authority). Tests:
  `TestGetAggregatesServesOwnedLocationsOnly`,
  `TestGetAggregatesRefusesALocalCopyItDoesNotOwn`,
  `TestGetAggregatesFallsSilentOnADelegatedChild`.
- **K6 (bounded fan-out).** A miss asks the XOR-nearest peer and at most
  `zoneFanout` others, ordered by distance to the zone key; a refresh miss asks
  one. Tests: `TestClientReadFanoutIsBounded`,
  `TestRefreshReadAsksOneExtraPeerAtMost`,
  `TestZoneCandidatesOrderedByDistanceToKey`.
- **K7 (batch reads are concurrent).** `GetMultiple` resolves up to
  `batchParallelism = 8` locations at a time, so a cold `/sets` batch costs
  `ceil(n/8)` round trips rather than `n`. Bounding it matters as much as
  parallelising it: one client must not open a connection per location on every
  peer.
- **K8 (a read terminates on its own path).** A forwarded read carries the nodes
  it crossed and is cancelled when it reaches one of them again, so a routing
  cycle costs one round trip per node and no more. Tests:
  `TestDeepReadTerminatesOnATwoNodeRoutingCycle`,
  `TestGetCancelsWhenThePathReturnsToUs`, `TestGetSkipsPeersAlreadyOnThePath`,
  `TestGetForwardsThePathItWalked`.

---

## Related

- [aggregates.md](aggregates.md#3-the-mechanism-authority) — why `/aggregates` and `/set` are different endpoints
- [convergence.md](convergence.md#2-u--background-convergence) — the refresh read's caller
- [guarantees.md](guarantees.md#3-accepted-windows) — reads during movement, monitoring staleness

---

[← Ownership](ownership.md) · [Index](README.md) · [Convergence →](convergence.md)
