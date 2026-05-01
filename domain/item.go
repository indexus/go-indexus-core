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
}

func (i Item) Content() string {
	metrics := make([]string, len(i.Metrics))
	for idx := range i.Metrics {
		// 'g' with precision -1 emits the shortest decimal that round-trips
		// back to the exact same float64, preserving full precision when the
		// item is replayed from a snapshot or log file.
		metrics[idx] = strconv.FormatFloat(i.Metrics[idx], 'g', -1, 64)
	}
	return fmt.Sprintf("%s|%s|%s|%s", i.Collection, i.Location, i.Id, strings.Join(metrics, ":"))
}
