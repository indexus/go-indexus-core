package domain

import (
	"strconv"
	"strings"
)

type Item struct {
	Collection string    `json:"collection"`
	Location   string    `json:"location"`
	Id         string    `json:"id"`
	Metrics    []float64 `json:"metrics"`
}

func (i Item) Content() string {
	return ItemContent(i.Collection, i.Location, i.Id, i.Metrics)
}

// ItemContent serialises an item to its WAL line form without allocating an
// intermediate Item struct. Hot path: called for every item snapshotted.
//
// 'g' with precision -1 emits the shortest decimal that round-trips back to
// the exact same float64, preserving full precision when the item is replayed
// from a snapshot or log file.
func ItemContent(collection, location, id string, metrics []float64) string {
	// Pre-size the builder to avoid re-allocs: prefix + 3 separators + worst
	// case 24 bytes per metric (sign + 17 digits + exponent).
	var b strings.Builder
	b.Grow(len(collection) + len(location) + len(id) + 3 + len(metrics)*24)
	b.WriteString(collection)
	b.WriteByte('|')
	b.WriteString(location)
	b.WriteByte('|')
	b.WriteString(id)
	b.WriteByte('|')
	for i, m := range metrics {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(strconv.FormatFloat(m, 'g', -1, 64))
	}
	return b.String()
}
