package core

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/indexus/go-indexus-core/domain"
)

func (n *Node) Snapshot() []string {
	snapshot := make([]string, 0)

	n.routing.Traverse(0, make([]byte, domain.IdLength()), func(i int, b []byte, p domain.Peer) {
		c, exist := n.registered.Get(0, p.ID())
		if exist {
			arr := make([]string, 0)
			for ip := range c.IPs() {
				arr = append(arr, ip)
			}
			snapshot = append(snapshot, fmt.Sprintf("contact|%s|%s|%d", c.Name(), strings.Join(arr, ","), c.Port()))
		}
	})

	for _, collection := range n.collections.List() {
		snapshot = append(snapshot, fmt.Sprintf("collection|%s", collection.Name()))

		collection.Browse(
			func(ownership string) {
				snapshot = append(snapshot, fmt.Sprintf("ownership|%s", ownership))
			},
			func(ownership, delegation string) {
				snapshot = append(snapshot, fmt.Sprintf("delegation|%s", delegation))
			},
		)
	}
	return snapshot
}

func (n *Node) Restore() error {
	if !n.storage.Exist() {
		return nil
	}

	commands, err := n.storage.Load()
	if err != nil {
		return err
	}

	var collection, ownership, delegation string
	for _, command := range commands {
		arr := strings.Split(command, "|")
		if len(arr) == 0 {
			return errors.New("backup file is corrupted and cannot be restored")
		}

		switch arr[0] {
		case "contact":
			if len(arr) != 4 {
				continue
			}
			name := arr[1]
			ips := strings.Split(arr[2], ",")
			mIps := make(map[string]any)
			for _, ip := range ips {
				mIps[ip] = nil
			}
			port, err := strconv.Atoi(arr[3])
			if err != nil {
				continue
			}
			n.acknowledged.Insert(0, make([]byte, domain.IdLength()), n.newContact(name, mIps, port))
		case "collection":
			collection = arr[1]
		case "ownership":
			ownership = arr[1]
			n.create(collection, ownership)
		case "delegation":
			delegation = arr[1]
			c, _ := n.collections.Get(collection)
			c.Delegate(delegation)
		default:
			return errors.New("backup file is corrupted and cannot be restored")
		}
	}

	stream := n.storage.Stream(0)
	for log := range stream {
		arr := strings.Split(log, "|")
		item := &domain.Item{
			Collection: arr[0],
			Location:   arr[1],
			Id:         arr[2],
		}
		if _, exist := n.collections.Get(item.Collection); exist {
			n.add(item)
		}
	}

	return nil
}
