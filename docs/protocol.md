# Indexus Mesh Protocol — moved

The specification now lives in **[`docs/protocol/`](protocol/README.md)**, split
into one page per aspect. Start at [`protocol/README.md`](protocol/README.md),
which recalls the network's properties and its trade-offs before pointing at the
rest.

This file is kept because code comments across the repository cite sections of
the old single-file layout. The table below maps every one of those citations to
its new home. New comments should cite the phase page directly.

## Where each section went

| Old reference | New location |
|---|---|
| §1 Model | [protocol/model.md §1](protocol/model.md#1-definitions) |
| §1b The two orders | [protocol/model.md §2](protocol/model.md#2-the-two-orders) |
| §1c What the XOR metric buys | [protocol/model.md §3](protocol/model.md#3-what-the-xor-metric-buys-and-what-it-does-not) |
| §2 Aggregate algebra, authority | [protocol/aggregates.md](protocol/aggregates.md) |
| §3 Node state, `TransferBusy` | [protocol/README.md §7](protocol/README.md#7-node-state) |
| §4/M Membership | [protocol/membership.md](protocol/membership.md) |
| §4/G Gossip | [protocol/membership.md §4](protocol/membership.md#4-g--gossip) |
| §4/R Rebalance, R1–R9 | [protocol/ownership.md](protocol/ownership.md) |
| §4/R6 Repeatable handoff | [protocol/ownership.md §2](protocol/ownership.md#2-guarantees) |
| §4/R10 Named marks, parking, Claim | [protocol/ownership.md §3](protocol/ownership.md#3-r10--marks-parking-and-claim) |
| §4/I Ingress | [protocol/writes.md §1](protocol/writes.md#1-i--ingress) |
| §4/P Placement, P1–P5 | [protocol/writes.md §2](protocol/writes.md#2-p--placement) |
| §4/J Join | [protocol/lifecycle.md §3](protocol/lifecycle.md#3-j--join-bluegreen) |
| §4/L Leave | [protocol/lifecycle.md §4](protocol/lifecycle.md#4-l--leave) |
| §4/C Recovery | [protocol/durability.md](protocol/durability.md) |
| §4/D Child discovery | [protocol/convergence.md §1](protocol/convergence.md#1-d--child-discovery) |
| §4/Q Duplicate ownership | [protocol/ownership.md §4](protocol/ownership.md#4-q--duplicate-ownership-and-partition-remerge) |
| §4/U Background convergence | [protocol/convergence.md §2](protocol/convergence.md#2-u--background-convergence) |
| §4/K Reads | [protocol/reads.md](protocol/reads.md) |
| §4/X Client ingress hint | [protocol/reads.md §3](protocol/reads.md#3-x--client-ingress-hint) |
| §5 Progression of the mesh | [protocol/lifecycle.md](protocol/lifecycle.md) |
| §6 Crash matrix | [protocol/durability.md §3](protocol/durability.md#3-the-crash-matrix) |
| §7 Accepted windows | [protocol/guarantees.md §3](protocol/guarantees.md#3-accepted-windows) |
| §8 CRDT scorecard | [protocol/guarantees.md §4a](protocol/guarantees.md#4a-crdt-scorecard) |
| §9 Routing scorecard | [protocol/guarantees.md §4b](protocol/guarantees.md#4b-routing-and-transition-scorecard) |
| §10 Known defects | [protocol/guarantees.md §5](protocol/guarantees.md#5-known-defects) |
| §11 Code map | [protocol/README.md §6](protocol/README.md#6-code-map) |

Things that are new rather than moved:

- [protocol/roadmap.md](protocol/roadmap.md) — the open work: zone merging and
  network scale-down, replication inside the mesh, and the membership ceiling.
- [protocol/glossary.md](protocol/glossary.md) — every academic term the
  specification borrows, what it means, what it refers to here.
- [protocol/bibliography.md](protocol/bibliography.md) — related papers checked
  against their content; analogies only (no lineage); the nearby / incremental /
  linear-feed ambition that organises the design.
- [protocol/pressures.md](protocol/pressures.md) — beside the protocol:
  advantage / cost / evidence / known exit for each structural decision.
