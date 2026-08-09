[← Writes](writes.md) · [Index](README.md) · [Reads →](reads.md)

# Ownership: repeatable handoff and repair

Ownership moves in `core/rebalance.go`. There is one production handoff
protocol: synchronous, repeatable `/transfer` rounds. The former S3
mirror/cutover/CaughtUp/SwitchAck state machine and its session maps have been
removed.

## 1. Rebalance

```
Refresh():
  if !refreshBusy.CAS → return
  gossip
  if !leaving && transferMu.TryLock():
      plan := control()
      rebalancing = true
      for (peer, keys) in plan, in parallel:
          offerZones(peer, keys)
      rebalancing = false
      transferMu.Unlock()
  clean()
  Checkpoint()

offerZones(peer, keys):
  return transferToPeer(peer, keys)

transferToPeer(peer, keys):
  for key in keys:
      items := collection.Delegate(key)
      if peer.Transfer(key, items) fails:
          restoreDelegated(key, items)
          leave this and all untouched keys as residue
      else:
          verify the HTTP ACK names peer
          name the parent mark Handoff{Peer: peer, At: now}
          drop only this ACKed owned key
```

`control()` is read-only. `transferMu` is the single ownership-mover gate.
Every failed or untouched key remains eligible for the next `Refresh` tick;
there is no session that can remain wedged between states.

## 2. Guarantees

**R1 — ACK before drop.** The donor drops one zone only after `/transfer`
returns HTTP 201 with a nominative ACK naming the expected receiver. A refused,
invalid, or mismatched ACK restores the delegated items and tombstones locally.
A lost response may leave two copies, but never zero; placement convergence
resolves duplicate ownership.

Tests: `TestRefreshKeepsOwnershipUntilTransferAck`,
`TestSoftLeaveRestoresOnTransferFailure`,
`TestRestoreDelegatedKeepsTombstones`,
`TestTransferRequiresNominativeAck`.

**R2 — one mover.** `transferMu` serializes Refresh and leave. Two movers cannot
Delegate the same zone concurrently.

**R3 — convergent placement.** `control()` donates only keys in the XOR
half-space closer to a candidate. With stable membership, every accepted move
strictly decreases owner-to-key distance.

**R4 — bounded critical section.** `rebalancing` is true only for the
synchronous transfer round. There are no asynchronous inbound/outbound session
maps and `TransferBusy()` is exactly `rebalancing`.

**R5 — no autoscale feedback.** Transferred writes use `meter=false`; only
client writes count as growth.

**R6 — repeatable residue.** One key is one independently acknowledged round.
Successful keys are dropped; failed and not-yet-visited keys are residue for a
later tick. This replaces mirroring, WAL deltas, cutover queues and five session
states.

**R7 — receiver placement.** The receiver validates the complete payload before
installing ownership, then applies every item synchronously through the
memo-less placement walk. It does not depend on ingress queue capacity or
client-ready admission.

Tests: `TestTransferAppliesEvenWhenIngressQueueIsFull`,
`TestFullNodeStillAcceptsHandoffs`,
`TestSpawnedNodeAcceptsPeerTransferWhileJoining`.

**R8 — no half-mounted session.** The receiver has no persistent "preparing"
state. Invalid payloads fail before ownership installation. Once the synchronous
request succeeds, the receiver is the installed owner and its named ACK lets
the donor drop exactly that zone.

**R9 — crossed handoffs cannot interlock.** There are no callback RPCs inside a
handoff. Transfer is one request/response and ownership movement remains under
the local single-mover lock.

## 3. R10 — marks, parking and Claim

A delegation edge is `Handoff{Peer, At}`.

- A named active mark blocks placement into an ancestor. The write is parked by
  `(collection, child)`, outside aggregates and outside the hot queue.
- The garage is present in snapshots as `parked-ingress` / `parked-delete`.
  Restore preserves operation, root and item and clears `via`.
- `IngressStats` exposes total `parked`, `parked_by_collection`, and samples.
- A pre-named local mark carries `At` and remains structural. A legacy snapshot
  mark with both `Peer == ""` and `At == 0` is unresolved and cannot block a
  write.
- Silence alone never changes authority.

`repair()` runs every Update tick:

1. Every locally owned non-root zone sends `Claim(parent, child, me)` to the
   parent holder.
2. Claim verifies that payload peer equals the authenticated origin, validates
   the direct-child relation, names the mark, wakes parked writes and pulls the
   authoritative `/aggregates` value.
3. Named marks are checked only against their named holder. A successful answer
   wakes parked writes; no answer changes nothing.
4. Explicit membership death/quarantine of the named holder allows local Own
   and wakes the garage.

Tests: `TestParkedWriteWakesOnClaim`, `TestClaimCannotNameAnotherPeer`,
`TestAnonymousMarkNeverBlocksProgress`,
`TestCheckpointKeepsParkedIngressOutsideHotQueue`.

## 4. Duplicate ownership and partitions

Duplicate ownership can result from a lost ACK, partition remerge, or a stale
snapshot. It is a recoverable state: the ordinary XOR rebalance relation picks
the closer owner and moves the farther copy. Transfer and CRDT application are
idempotent, so retrying an already received batch does not multiply leaves.

The protocol deliberately does not infer death from an unanswered data RPC.
Membership supplies the explicit death/quarantine event used by repair.

---

[← Writes](writes.md) · [Index](README.md) · [Reads →](reads.md)
