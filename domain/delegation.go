package domain

type Key struct {
	Collection string
	Location   string
}

type Delegation map[string]any
type Ownership map[string]Delegation

const delegation = 1_000

func DelegationTreshold() int {
	return delegation
}
