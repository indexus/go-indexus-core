[← Lifecycle](lifecycle.md) · [Index](README.md) · [Roadmap →](roadmap.md)

# Guarantees, accepted windows and defects

This page is the audit surface. Everything the mesh claims, everything it
knowingly does not, and everything it intends to but does not yet.

---

## 1. Three registers, kept apart

| Register | Meaning | Where |
|---|---|---|
| **Guarantee** | a property a named test enforces | §2, and the phase pages |
| **Accepted window** | behaviour we know about and chose | §3 |
| **Known defect** | a property the design intends and the code lacks | §5 |

A fourth register — designs not yet built — lives in [roadmap.md](roadmap.md).

The rule that keeps this page honest: **a property is only written as a
guarantee if a test enforces it.** Everything else is one of the other three.

## 2. Guarantee index

| Label | Property | Page |
|---|---|---|
| **M1** | a peer is registered only if dialable, never while quarantined | [membership](membership.md#2-observe-and-the-m-guarantees) |
| **M2** | a dead peer is evicted within one tick and quarantined 90 s | [membership](membership.md#2-observe-and-the-m-guarantees) |
| **M3** | a node with *empty* tables re-seeds from its bootstraps | [membership](membership.md#2-observe-and-the-m-guarantees) |
| **M4** | a genuinely new registration kicks one out-of-band Refresh | [membership](membership.md#2-observe-and-the-m-guarantees) |
| **M5** | every node eventually knows every other node | [membership](membership.md#3-m5--random-peer-sampling) |
| **I1** | an acknowledged write survives a crash | [writes](writes.md#1-i--ingress) |
| **I2** | Feed never drops an element | [writes](writes.md#1-i--ingress) |
| **P1** | a quarantined peer is never routed to, and never replaced by self | [writes](writes.md#2-p--placement) |
| **P2** | apply is idempotent: at-least-once delivery, exactly-once effect | [writes](writes.md#2-p--placement) |
| **P3** | no set self-aliases, no traversal loops | [writes](writes.md#2-p--placement) |
| **P4** | all 64 alphabet characters are usable locations | [writes](writes.md#2-p--placement) |
| **P5** | a placement attempt depends only on the address and the local membership view (stateless walk; `via` bounds hops) | [writes](writes.md#2-p--placement) |
| **R1** | ownership drops only after the receiver ACKed | [ownership](ownership.md#2-guarantees) |
| **R2** | all ownership movement is under one lock | [ownership](ownership.md#2-guarantees) |
| **R3** | every move strictly decreases owner–key distance; placement terminates | [ownership](ownership.md#2-guarantees) |
| **R4** | a zone never dips into a hole while it moves | [ownership](ownership.md#2-guarantees) |
| **R5** | handoffs never meter autoscale | [ownership](ownership.md#2-guarantees) |
| **R6** | one key is one independently acknowledged Transfer round; failures leave residue for a later tick | [ownership](ownership.md#2-guarantees) |
| **R7** | a handoff is placed by the receiver, not imposed by the donor | [ownership](ownership.md#2-guarantees) |
| **R8** | no half-mounted session: invalid payloads fail before ownership; a nominative ACK lets the donor drop exactly that zone | [ownership](ownership.md#2-guarantees) |
| **R9** | crossed handoffs cannot interlock (no callback RPCs inside a Transfer; single-mover lock) | [ownership](ownership.md#2-guarantees) |
| **R10** | a delegation mark never blocks progress (park + Claim; silence alone never lifts) | [ownership](ownership.md#3-r10--marks-parking-and-claim) |
| **Q1** | duplicate ownership converges to one owner holding the union | [ownership](ownership.md#4-q--duplicate-ownership-and-partition-remerge) |
| **Q2** | two claimants never hand over to each other | [ownership](ownership.md#4-q--duplicate-ownership-and-partition-remerge) |
| **Q3** | duplicate resolution never disturbs a live handoff | [ownership](ownership.md#4-q--duplicate-ownership-and-partition-remerge) |
| **D1** | a remote child becomes visible to the parent via Claim within one repair tick | [convergence](convergence.md#1-d--child-discovery) |
| **D2** | Claim / redirect following terminates | [convergence](convergence.md#1-d--child-discovery) |
| **D3** | Claim writes no stub value (mark + later `/aggregates` pull) | [convergence](convergence.md#1-d--child-discovery) |
| **W1** | a write terminates on its own path (`via`, symmetric of K8) | [writes](writes.md#2-p--placement) |
| **U1** | cache refreshes do not stampede | [convergence](convergence.md#2-u--background-convergence) |
| **U3** | a parent stub converges to its child owner's value | [convergence](convergence.md#2-u--background-convergence) |
| **K1** | a deep read prefers the owner over a stale local copy | [reads](reads.md#4-guarantees) |
| **K2** | a refresh read never touches the cache or the LRU | [reads](reads.md#4-guarantees) |
| **K3** | a shallow read issues no RPC | [reads](reads.md#4-guarantees) |
| **K4** | an unreachable owner is a miss, never an empty zone | [reads](reads.md#4-guarantees) |
| **K5** | `/aggregates` certifies ownership | [reads](reads.md#4-guarantees) |
| **K6** | fan-out is bounded and ordered by distance to the key | [reads](reads.md#4-guarantees) |
| **K7** | batch reads are concurrent, and bounded | [reads](reads.md#4-guarantees) |
| **K8** | a read terminates on its own path | [reads](reads.md#4-guarantees) |
| **V1–V4** | the requirements a freshness rule must meet | [aggregates](aggregates.md#2-the-one-place-the-algebra-is-not-enough) |

## 3. Accepted windows

Behaviour that is known, chosen, and not a defect.

- **Reads during movement.** Between the donor's Delegate and the receiver's
  Feed apply, the zone answers empty. Reads are eventually consistent.
- **Empty zone transfer.** A zone whose items were all deleted transfers an empty
  batch and simply stops being owned; the next write re-creates it at the nearest
  node.
- **Suspended-owner stall.** Writes for a quarantined peer's zones hold up to
  90 s when no live alternative exists — better than re-owning them (P1).
- **Divergent membership on a write is bounded by `via`.** Two nodes that
  compute different owners can forward a write at most once per peer per
  attempt (`domain.Visited`, cap `viaCap`); the attempt then fails with
  `ErrOwnerUnavailable` or parks under a named mark. Retries clear `via`, so a
  refusal does not poison the next attempt. Pinned by
  `TestAFailedAttemptUnderMarkParksWithEmptyVia` and the A/B harness in
  `model_test.go`.
- **Feed pause is global** while a transfer plan applies — acceptable because
  plans are short and serialized; a per-zone gate would buy little.
- **Parked depth replaces unplaceable as the alarm.** A write blocked by a
  named mark is garaged off the hot queue (`parked[child]`) until Claim, mark
  rewrite, or membership-declared death of the named peer wakes it.
- **Add vs delete of the same id does not commute** (add-wins on the later
  arrival). Chosen so a client can always re-create what it deleted — adds carry
  no causal generation, so a "newer" delete cannot be told apart from an older
  one. Consequence: a *duplicated* stale add (residual forward, crash replay) can
  resurrect a deleted id until its writer stops retrying. Pinned by
  `TestAddTombstoneOrderDependence` and
  `TestTransferredTombstoneThenLateAddResurrects`; lifting it requires
  generation-carrying adds (an [OR-set](glossary.md#or-set) tag per write).
- **A refused write may still apply.** Durability (WAL) runs before admission
  (queue TryAdd), so a write bounced with `ErrQueueFull` replays after a crash.
  Harmless — the client's retry of the same id is absorbed by idempotent apply —
  but delivery is at-least-once of everything written, not
  exactly-what-was-acknowledged. Pinned by `TestRefusedWriteStillReplaysFromWAL`.
- **Monitoring reads may be stale.** `/status`, `/count` and `/ownership` serve a
  non-blocking snapshot (`TryOwnershipBrowse`) rather than take the collection
  mutex shared with the protocol.
- **A parent may disagree with its child mid-handoff.** The root summarises
  depth-1 children, so the read-during-movement window shows up in `@` as well as
  in the zone. It closes when the receiver's Feed applies. What is *not* accepted
  is a disagreement that outlives the handoff — unlike a count, a wrong aggregate
  does not self-correct once summed into a parent. Pinned by
  `TestRootAggregateNeverContradictsItsChildrenDuringHandoff`, which samples
  throughout a two-node handoff and then holds the settled mesh to the invariant:
  1–3 disagreements in flight per run, all closed on settle.
- **Traffic counters fold unknown paths.** The `traffic/` package implements
  `Middleware` and bounded path maps (covered by its own tests) but is not yet
  wired into the production P2P mux. When attached above the mux it sees paths
  no route serves; declared routes keep their own counter and past `maxPaths`
  everything else shares one bucket.

## 4. Scorecards

### 4a. CRDT scorecard

Against the [strong eventual consistency](glossary.md#strong-eventual-consistency)
requirements (op-based [CRDT](glossary.md#crdt) with at-least-once delivery):

| Requirement | Status | Evidence |
|---|---|---|
| Adds of distinct ids commute | ✓ | `TestAddOrderIndependence` |
| Duplicate apply is a no-op | ✓ | `TestAddSecondPassIsNoOp`, `TestTransferDeliveredTwiceIsIdempotent` |
| Same id, new metrics: replace-with-delta | ✓ | `TestCollectionAddReplaceMetrics` |
| Tombstones join on max generation (commutative, idempotent) | ✓ | `TestTombstoneMaxGenCommutes` |
| Delete before add ever seen (out-of-order) | ✓ tombstone-first holds | `TestRemoveAbsentPlantsTombstone` |
| Duplicate-owner *items* heal without double count | ✓ | `TestStaleSnapshotDuplicateOwnerHeals`, `TestConvergence_*` |
| Duplicate-owner *ownership* resolves to one node | ✓ | `TestConvergence_NoDoubleOwnership`, `TestConvergence_PartitionRemergeKeepsTheUnion` |
| Parent stub converges to the owner's value | ✓ | `TestParentStubFollowsOwnerAcrossHandoff`, `TestParentStubIgnoresNonAuthoritativeSource` |
| Add/delete of the same id commute | ✗ add-wins (accepted, §3) | `TestAddTombstoneOrderDependence` |

### 4b. Routing and transition scorecard

| Property | Status | Evidence |
|---|---|---|
| `Nearest` is the true XOR minimum (unique owner per key, never a tie) | ✓ | `TestNearestIsXorMinimum` |
| Zone keys are unhashed: a subtree is a contiguous XOR range | ✓ | `TestZoneKeysKeepTheLocationTreeAdjacent` |
| `Extract` fills each k-bucket with its XOR-nearest member | ✓ | `TestExtractFillsEachBucketWithItsNearest` |
| `ExtractK` fills each bucket with its k nearest, head unchanged | ✓ | `TestExtractKFillsEachBucketWithItsKNearest`, `TestExtractKAgreesWithExtractOnTheFirstContact` |
| The gossip answer's reach is bounded, and the shipped width is honest about it | ✓ measured | `TestGossipChannelCeiling` |
| Membership reaches every node, not just the near ones | ✓ via M5; ✗ under gossip alone | `TestAChainOfNodesReachesFullKnowledgeOfTheMesh` |
| An introduction is verified before it is trusted | ✓ | `TestObserveAsksKnownPeersToNameAStranger` |
| `Range` yields exactly the candidate's half-space | ✓ | `TestRangeIsExactlyTheCandidateHalfSpace` |
| No self-donation; donation edges are antisymmetric (no ping-pong) | ✓ | `TestRangeSelfIsEmpty`, `TestRangeIsAntisymmetric` |
| Split area refuses writes until `Own` claims it | ✓ | `TestSplitAreaRefusesAddsUntilOwned` |
| `Delegate` hands subtree + tombstones exactly once, then refuses | ✓ | `TestDelegateHandsSubtreeOnceAndDropsOwnership` |
| Failed transfer restore round-trips the exact state | ✓ | `TestDelegateThenReAddRoundTrips` |
| Post-ACK: donor owns nothing, cache purged, residuals forward | ✓ | `TestHandoffThenResidualWriteForwardsToNewOwner` |
| client_ready latch survives backlog regrowth; leaving clears it | ✓ | `TestClientReadyLatchSurvivesBacklogRegrowth`, `TestLeavingNodeIsNotClientReady` |
| Path-fill caches once; dense leaves land in a shorter TTL class | ✓ | `TestGetPathFillsOnceThenServesFromCache`, `TestGetDenseLeafStampsShorterTTLClass` |
| A deep read prefers the owner over a stale local copy | ✓ | `TestGetPrefersOwnerOverStaleLocalCopy` |
| A miss asks the nearest peers to the key, and only α of them | ✓ | `TestClientReadFanoutIsBounded`, `TestZoneCandidatesOrderedByDistanceToKey` |
| Claims and dirty marks on the `owned` tree do not race | ✓ | `TestOwnedTreeSurvivesConcurrentClaimsAndDirtyMarks` (`-race`) |
| A closer ingress is adopted, a farther one ignored | ✓ | `network/ingressHint.test.mjs` |
| A handoff is accepted under any local pressure, and still placed by R3 | ✓ | `TestTransferAppliesEvenWhenIngressQueueIsFull`, `TestFullNodeStillAcceptsHandoffs`, `TestConvergence_LossyTransfer_RecoversWithoutLoss` |
| A handoff is durable before it is ACKed | ✓ | `TestReceiverCrashAfterAckReplaysTheBatch` |
| An unreachable owner reads as a miss, never as an empty zone | ✓ | `TestEmptyAnswerFromNearestIsAMissNotATruth`, `TestPartitionedReadIsAMissNotAnEmptyZone` |
| A stub holds its value when no authority answers | ✓ | `TestParentStubHoldsWhenNoAuthorityAnswers` |
| Holding costs nothing in practice: some authority does answer | ✓ 0 holds in 8 runs | `TestRootAggregateNeverContradictsItsChildrenDuringHandoff` |
| `@` agrees with the sum of its children once a handoff settles | ✓ | `TestRootAggregateNeverContradictsItsChildrenDuringHandoff` |
| A stranger's path cannot grow the traffic map without bound | ✓ | `TestUnknownPathsCannotGrowTheMapWithoutBound`, `TestDeclaredRoutesKeepTheirOwnCounterUnderAFlood` |
| Every letter of the alphabet is a usable location | ✓ | `TestItemsUnderEveryLetterAreCounted`, `TestSummaryPlaceholderCarriesNoReservedKey` |

## 5. Known defects

These are **not** accepted windows. They are properties the design intends to
have and the implementation does not. Each names the mechanism that would
establish it.

- **`Collection.Complete` materialises sets while holding the exclusive lock**
  and is called from the ownership path; it is the widest critical section left
  in `domain`. Establishing the property means splitting materialisation from
  the claim, so the lock covers the claim only.

Structural gaps large enough to be projects rather than defects are in
[roadmap.md](roadmap.md).

---

[← Lifecycle](lifecycle.md) · [Index](README.md) · [Roadmap →](roadmap.md)
