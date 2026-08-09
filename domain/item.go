package domain

import (
	"fmt"
	"strconv"
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

// ParseContent rebuilds an item from Content() or from a snapshot "item|…" line.
// Metrics used to be dropped on every WAL replay because Restore only copied
// collection/location/id; a crash then woke the node with empty aggregates.
func ParseContent(line string) (*Item, bool) {
	line = strings.TrimPrefix(line, "item|")
	if strings.HasPrefix(line, "tombstone|") {
		arr := strings.Split(line, "|")
		if len(arr) < 5 {
			return nil, false
		}
		var gen uint64
		fmt.Sscanf(arr[4], "%d", &gen)
		return &Item{
			Collection: arr[1],
			Location:   arr[2],
			Id:         arr[3],
			Tombstone:  true,
			Gen:        gen,
		}, true
	}
	arr := strings.Split(line, "|")
	if len(arr) < 3 {
		return nil, false
	}
	item := &Item{
		Collection: arr[0],
		Location:   arr[1],
		Id:         arr[2],
	}
	if len(arr) >= 4 && arr[3] != "" {
		parts := strings.Split(arr[3], ":")
		item.Metrics = make([]float64, 0, len(parts))
		for _, part := range parts {
			v, err := strconv.ParseFloat(part, 64)
			if err != nil {
				continue
			}
			item.Metrics = append(item.Metrics, v)
		}
	}
	return item, true
}
