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
}

func (i Item) Content() string {
	metrics := make([]string, len(i.Metrics))
	for idx := range i.Metrics {
		metrics[idx] = fmt.Sprintf("%f", i.Metrics[idx])
	}
	return fmt.Sprintf("%s|%s|%s|%s", i.Collection, i.Location, i.Id, strings.Join(metrics, ":"))
}
