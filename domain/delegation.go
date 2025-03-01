package domain

type Key struct {
	Collection string
	Location   string
}

type Delegation map[string]any
type Ownership map[string]Delegation

// PendingTransfer represents a transfer that needs to be processed
type PendingTransfer struct {
	Key      Key
	Receiver Contact
}

const delegation = 1_000

func DelegationTreshold() int {
	return delegation
}
