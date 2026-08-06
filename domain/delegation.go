package domain

import (
	"os"
	"strconv"
)

type Key struct {
	Collection string
	Location   string
}

type Delegation map[string]any
type Ownership map[string]Delegation

// DefaultDelegationSize is the default soft item count before a location
// range is owned / may split (-delegation). Override via flag or
// INDEXUS_DELEGATION.
const DefaultDelegationSize = 5_000

func DelegationTreshold() int {
	if raw := os.Getenv("INDEXUS_DELEGATION"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return n
		}
	}
	return DefaultDelegationSize
}
