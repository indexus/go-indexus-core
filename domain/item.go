package domain

import (
	"fmt"
	"strings"
)

type Item struct {
	Collection string    `json:"collection"`
	Location   string    `json:"location"`
	Id         string    `json:"id"`
	Metrics    []float64 `json:"metrics"`
	// Tombstone marks a delete record carried across Transfer / WAL replay.
	Tombstone bool   `json:"tombstone,omitempty"`
	Gen       uint64 `json:"gen,omitempty"`
}

func (i Item) Content() string {
	if i.Tombstone {
		return fmt.Sprintf("tombstone|%s|%s|%s|%d", i.Collection, i.Location, i.Id, i.Gen)
	}
	metrics := make([]string, len(i.Metrics))
	for idx := range i.Metrics {
		metrics[idx] = fmt.Sprintf("%f", i.Metrics[idx])
	}
	return fmt.Sprintf("%s|%s|%s|%s", i.Collection, i.Location, i.Id, strings.Join(metrics, ":"))
}
