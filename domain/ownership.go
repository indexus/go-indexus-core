package domain

import (
	"os"
	"strconv"
)

// Key names a zone: a location range inside a collection. It is the unit every
// phase agrees on — what a node owns, transfers, snapshots and delegates.
type Key struct {
	Collection string
	Location   string
}

// Handoff records that a direct child zone is served elsewhere.
type Handoff struct {
	Peer string // holder name; "" = anonymous/legacy
	At   int64  // unix tick when marked
}

// Blocks reports whether the mark names an authority. Anonymous marks are
// topology hints only and never block a write; Claim or a transfer ACK must
// name the holder first.
func (h Handoff) Blocks() bool {
	return h.Peer != ""
}

// Delegation is what a node publishes about one zone it owns; Ownership is that
// map for a whole collection, keyed by location.
type Delegation map[string]Handoff
type Ownership map[string]Delegation

// DefaultDelegationSize is the default soft item count before a location
// range is owned / may split (-delegation). Override via flag or
// INDEXUS_DELEGATION.
const DefaultDelegationSize = 5_000

func DelegationSize() int {
	if raw := os.Getenv("INDEXUS_DELEGATION"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return n
		}
	}
	return DefaultDelegationSize
}
