package domain

import "fmt"

type Item struct {
	Collection string `json:"collection"`
	Location   string `json:"location"`
	Id         string `json:"id"`
}

func (i Item) Content() string {
	return fmt.Sprintf("%s|%s|%s", i.Collection, i.Location, i.Id)
}
