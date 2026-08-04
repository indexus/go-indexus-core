package domain

type DelegationOfferPayload struct {
	Donor     string            `json:"donor"`
	SessionID string            `json:"session_id"`
	Zones     []ZoneSnapshotRef `json:"zones"`
	WALSeq    int64             `json:"wal_seq"`
}

type ZoneSnapshotRef struct {
	Collection string `json:"collection"`
	Location   string `json:"location"`
	Seq        int64  `json:"seq"`
	Key        string `json:"key"`
	ItemCount  int    `json:"item_count"`
}

type WALDeltaPayload struct {
	SessionID string   `json:"session_id"`
	Lines     []string `json:"lines"`
	Done      bool     `json:"done"`
}

type CaughtUpPayload struct {
	SessionID string `json:"session_id"`
	WALSeq    int64  `json:"wal_seq"`
}

type SwitchAckPayload struct {
	SessionID string `json:"session_id"`
	Keys      []Key  `json:"keys"`
}
