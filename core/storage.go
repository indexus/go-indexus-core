package core

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
)

func (n *Node) Snapshot() []string {
	snapshot := make([]string, 0)

	n.routing.Traverse(0, encoding.BASE64.NewID(), func(i int, b []byte, p domain.Peer) {
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

	n.owned.Traverse(0, encoding.BASE64.NewID(), func(i int, b []byte, keys map[domain.Key]any) {
		for key := range keys {
			collection, exist := n.collections.Get(key.Collection)
			if !exist {
				continue
			}
			collection.Traverse(
				key.Location,
				func(string, *domain.Abelian) {},
				func(_ string, location, id string, abelian *domain.Abelian) {
					item := &domain.Item{
						Collection: key.Collection,
						Location:   location,
						Id:         id,
						Metrics:    abelian.Metrics(),
					}
					snapshot = append(snapshot, "item|"+item.Content())
				},
			)
			for _, tomb := range collection.TombstonesUnder(key.Location) {
				snapshot = append(snapshot, tomb.Content())
			}
		}
	})

	for _, el := range n.queue.Snapshot() {
		if el == nil || el.item == nil {
			continue
		}
		switch el.op {
		case OpDelete:
			snapshot = append(snapshot, fmt.Sprintf("pending-delete|%s|%s|%s", el.root, el.current, el.item.Content()))
		default:
			snapshot = append(snapshot, fmt.Sprintf("pending-ingress|%s|%s|%s", el.root, el.current, el.item.Content()))
		}
	}

	return snapshot
}

func (n *Node) Restore() error {
	if !n.storage.Exist() {
		return nil
	}

	commands, err := n.storage.Load()
	if err != nil {

		slog.Error("snapshot unreadable, replaying the write-ahead log alone", "err", err)
		commands = nil
	}

	var collection string

	for _, command := range commands {
		arr := strings.Split(command, "|")
		if len(arr) < 2 {
			slog.Warn("skipping corrupted snapshot line", "line", command)
			continue
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
			id, err := encoding.BASE64.Decode(name)
			if err != nil {
				continue
			}
			n.acknowledged.Insert(0, id, n.newContact(name, mIps, port))
		case "collection":
			collection = arr[1]
		case "ownership":
			n.create(collection, arr[1])
		case "delegation":
			current, ok := n.collections.Get(collection)
			if !ok {
				continue
			}
			if _, exist := current.Get(arr[1]); !exist {
				continue
			}
			current.Delegate(arr[1])
		case "item":
			item, ok := domain.ParseContent(command)
			if !ok || item.Tombstone {
				continue
			}
			if _, exist := n.collections.Get(item.Collection); !exist {
				continue
			}
			n.add(item, false)
		case "tombstone":
			item, ok := domain.ParseContent(command)
			if !ok || !item.Tombstone {
				continue
			}
			if c, ok := n.collections.Get(item.Collection); ok {
				c.ApplyTombstone(item.Location, item.Id, item.Gen)
			}
		case "pending-ingress":
			n.restorePending(command, false)
		case "pending-delete":
			n.restorePending(command, true)
		default:
			slog.Warn("unknown snapshot command, skipping", "line", command)
		}
	}

	stream := n.storage.Stream(0)
	for log := range stream {
		n.replayLogLine(log)
	}

	return nil
}

func (n *Node) restorePending(command string, del bool) {
	arr := strings.SplitN(command, "|", 4)
	if len(arr) < 4 {
		return
	}
	item, ok := domain.ParseContent(arr[3])
	if !ok {
		return
	}
	if del {
		n.queue.Add(NewDeleteElement(item, arr[1], arr[2]))
		return
	}
	n.queue.Add(NewElement(item, arr[1], arr[2]))
}

func (n *Node) replayLogLine(log string) {
	if strings.HasPrefix(log, "ingress|") {
		arr := strings.SplitN(log, "|", 4)
		if len(arr) < 4 {
			return
		}
		item, ok := domain.ParseContent(arr[3])
		if !ok {
			return
		}
		n.queue.Add(NewElement(item, arr[1], arr[2]))
		return
	}
	if strings.HasPrefix(log, "delete|") {
		arr := strings.SplitN(log, "|", 4)
		if len(arr) < 4 {
			return
		}
		item, ok := domain.ParseContent(arr[3])
		if !ok {
			return
		}
		n.queue.Add(NewDeleteElement(item, arr[1], arr[2]))
		return
	}
	if strings.HasPrefix(log, "tombstone|") {
		item, ok := domain.ParseContent(log)
		if !ok {
			return
		}
		if c, ok := n.collections.Get(item.Collection); ok {
			c.ApplyTombstone(item.Location, item.Id, item.Gen)
		}
		return
	}
	item, ok := domain.ParseContent(log)
	if !ok || item.Tombstone {
		return
	}
	if _, exist := n.collections.Get(item.Collection); exist {
		n.add(item, false)
	}
}
