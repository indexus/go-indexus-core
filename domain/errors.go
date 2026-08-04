package domain

import "errors"

// ErrLeaving is returned while a node drains. Ingress and inbound transfers
// must be refused so SoftLeave can converge: accepting a write re-owns the
// zone that was just handed off.
var ErrLeaving = errors.New("node is leaving")

// ErrPeerBusy is returned when a forward target answers 503 (queue/memory
// full). Callers should fail fast and back off rather than burn the local
// forwardRate budget on a peer that cannot accept work.
var ErrPeerBusy = errors.New("peer ingress full")

// ErrOwnerUnavailable is returned when the XOR-nearest owner is quarantined
// and no live peer can take the write. Callers should hold/requeue rather than
// fall back to self and re-own a zone that still belongs elsewhere.
var ErrOwnerUnavailable = errors.New("nearest owner unavailable")

// HandoffHeader marks a write on /item as a forward from another node rather
// than a client write, so the receiver can tell load already inside the mesh
// from load arriving at its door.
const HandoffHeader = "X-Indexus-Handoff"
