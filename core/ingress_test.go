package core

import (
	"github.com/indexus/go-indexus-core/storage"

	"strings"

	"path/filepath"

	"os"

	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
)

func TestWriteRejectsUnwalkableKey(t *testing.T) {
	node := newNodeOn(t, &memStorage{}, 64)
	item := &domain.Item{Collection: "demo", Location: "aa", Id: "x", Metrics: []float64{1}}
	for name, root := range map[string]string{"empty root": "", "root outside location": "zz"} {
		t.Run(name, func(t *testing.T) {
			if err := node.New(item, root, nil); err == nil {
				t.Fatalf("New accepted %s", name)
			}
		})
	}
	if err := node.New(item, encoding.BASE64.Root(), nil); err != nil {
		t.Fatalf("New under root: %v", err)
	}
}

func TestWriteRejectsItemWithoutLocation(t *testing.T) {
	node := newNodeOn(t, &memStorage{}, 64)

	if err := node.New(&domain.Item{Collection: "demo", Id: "x"}, encoding.BASE64.Root(), nil); err == nil {
		t.Fatal("New accepted an item without location")
	}
	if err := node.New(nil, encoding.BASE64.Root(), nil); err == nil {
		t.Fatal("New accepted a nil item")
	}
}

func TestWriteRejectsUnkeyableItem(t *testing.T) {
	node := newNodeOn(t, &memStorage{}, 64)

	location, err := encoding.BASE64.RandomName()
	if err != nil {
		t.Fatal(err)
	}

	short := &domain.Item{Collection: "demo", Location: location, Id: "x", Metrics: []float64{1}}
	if err := node.New(short, encoding.BASE64.Root(), nil); err == nil {
		t.Fatal("New accepted a location its collection cannot key")
	}

	collection, err := encoding.BASE64.RandomName()
	if err != nil {
		t.Fatal(err)
	}
	full := &domain.Item{Collection: collection, Location: location, Id: "x", Metrics: []float64{1}}
	if err := node.New(full, encoding.BASE64.Root(), nil); err != nil {
		t.Fatalf("New on a keyable item: %v", err)
	}

	wide := location + location
	over := &domain.Item{Collection: wide, Location: wide, Id: "x", Metrics: []float64{1}}
	if err := node.New(over, encoding.BASE64.Root(), nil); err == nil {
		t.Fatal("New accepted an identifier wider than the key space")
	}
}

func transferItems() []*domain.Item {
	return []*domain.Item{
		{Collection: "demo", Location: "aa", Id: "x", Metrics: []float64{1}},
		{Collection: "demo", Location: "ab", Id: "y", Metrics: []float64{1}},
	}
}

var transferKey = domain.Key{Collection: "demo", Location: "a"}

func TestTransferAppliesEvenWhenIngressQueueIsFull(t *testing.T) {
	node := newNodeOn(t, &memStorage{}, 64)

	node.settings.SetQueueMax(1)
	for i := 0; i < node.settings.peerQueueMax; i++ {
		if err := node.Handoff(item(fmt.Sprintf("filler-%d", i)), "@", nil); err != nil {
			t.Fatalf("filling the queue: %v", err)
		}
	}

	// Classic zone Transfer must not compete with the ingress queue — otherwise
	// PreferNear handoffs stall Items() and freeze client writes above queueMax.
	if _, err := node.Transfer(nil, transferKey, transferItems()); err != nil {
		t.Fatalf("Transfer refused while ingress queue full: %v", err)
	}
	count, err := node.Count()
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count < 2 {
		t.Fatalf("transferred items missing from Count: got %d", count)
	}
}

func TestHandoffQueuesAboveTheClientCeiling(t *testing.T) {
	node := newNodeOn(t, &memStorage{}, 64)
	node.settings.SetQueueMax(1)

	if err := node.New(item("client-1"), "@", nil); err != nil {
		t.Fatalf("first client write: %v", err)
	}
	if err := node.New(item("client-2"), "@", nil); err == nil {
		t.Fatal("client write accepted past the ingress ceiling")
	}
	if err := node.Handoff(item("peer-1"), "@", nil); err != nil {
		t.Fatalf("handoff refused on a node full of client writes: %v", err)
	}
}

func TestTransferAcceptsItemsWhenQueueHasRoom(t *testing.T) {
	node := newNodeOn(t, &memStorage{}, 64)

	if _, err := node.Transfer(nil, transferKey, transferItems()); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	// Applied synchronously — no Feed drain required for Count.
	count, err := node.Count()
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 2 {
		t.Fatalf("count after transfer: got %d want 2", count)
	}
}

func TestHandoffDoesNotMeterAutoscale(t *testing.T) {
	node := newNodeOn(t, &memStorage{}, 64)
	node.EnableAutoscale(AutoscaleConfig{
		Enabled: true,
		Role:    "bootstrap",
		Window:  time.Minute,
	})

	root := encoding.BASE64.Root()
	if err := node.Handoff(item("peer-meter"), root, nil); err != nil {
		t.Fatalf("Handoff: %v", err)
	}
	drain(t, node)

	if got := node.autoscale.Snapshot()["inserts_window"].(int64); got != 0 {
		t.Fatalf("Handoff metered inserts_window=%d want 0", got)
	}

	if err := node.New(item("client-meter"), root, nil); err != nil {
		t.Fatalf("New: %v", err)
	}
	drain(t, node)

	if got := node.autoscale.Snapshot()["inserts_window"].(int64); got == 0 {
		t.Fatal("client New should meter inserts_window")
	}
}

func TestFullNodeRefusesClientWrites(t *testing.T) {
	node := newNodeOn(t, &memStorage{}, 4)
	node.full.Store(true)

	err := node.New(item("i1"), "@", nil)
	if !errors.Is(err, ErrNodeFull) {
		t.Fatalf("New err=%v want ErrNodeFull", err)
	}
	if got := node.queue.Length(); got != 0 {
		t.Fatalf("refused write still queued: length=%d", got)
	}
}

func TestFullNodeStillAcceptsHandoffs(t *testing.T) {
	node := newNodeOn(t, &memStorage{}, 4)
	node.full.Store(true)

	key := domain.Key{Collection: "demo", Location: "a"}
	it := &domain.Item{Collection: "demo", Location: "aa", Id: "i1", Metrics: []float64{1}}
	if _, err := node.Transfer(node, key, []*domain.Item{it}); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	if got := node.Items(); got != 1 {
		t.Fatalf("transfer not applied under mem-full: items=%d", got)
	}
}

func TestNodeAcceptsAgainWithHeadroom(t *testing.T) {
	node := newNodeOn(t, &memStorage{}, 4)
	node.full.Store(true)
	node.full.Store(false)

	if err := node.New(item("i1"), "@", nil); err != nil {
		t.Fatalf("New: %v", err)
	}
}

func TestClientRefusedWhileScaleUpInFlight(t *testing.T) {
	node := newNodeOn(t, &memStorage{}, 4)
	node.EnableAutoscale(AutoscaleConfig{
		Enabled:        true,
		Role:           "bootstrap",
		Cooldown:       time.Hour,
		PressureHold:   time.Millisecond,
		MemLimitPct:    50,
		DiskMinFreePct: 1,
		CPULimitPct:    99,
		MemRisePct:     1000,
		RiseHold:       time.Hour,
	})

	release := make(chan struct{})
	started := make(chan struct{})
	go node.autoscale.Tick(
		PressureInput{
			SelfName:  node.Name(),
			Resources: ResourceSample{MemPct: 80, DiskFreePct: 80, DiskOK: true},
		},
		func(ScaleUpRequest) error {
			close(started)
			<-release
			return nil
		},
		nil,
	)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("scale-up never started")
	}

	if err := node.New(item("client"), "@", nil); !errors.Is(err, ErrNodeFull) {
		t.Fatalf("New err=%v want ErrNodeFull while up_in_flight", err)
	}
	key := domain.Key{Collection: "demo", Location: "a"}
	it := &domain.Item{Collection: "demo", Location: "aa", Id: "peer", Metrics: []float64{1}}
	if _, err := node.Transfer(node, key, []*domain.Item{it}); err != nil {
		t.Fatalf("Transfer during up_in_flight: %v", err)
	}
	if got := node.Items(); got != 1 {
		t.Fatalf("peer transfer not applied: items=%d", got)
	}
	close(release)
}

func TestIngressWALBeforeAck(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "backup")
	store, err := storage.NewStorage(dir, base)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = store.Start() }()
	t.Cleanup(store.Close)

	name, err := encoding.BASE64.RandomName()
	if err != nil {
		t.Fatal(err)
	}
	settings, err := NewSettings(name, 21200, time.Second, time.Minute, 64)
	if err != nil {
		t.Fatal(err)
	}
	settings.SetQueueMax(10)
	n, err := NewNode(settings, func(string, map[string]any, int) domain.Contact {
		return nil
	}, nil, store)
	if err != nil {
		t.Fatal(err)
	}

	item := &domain.Item{Collection: "demo", Location: "aa", Id: "i1", Metrics: []float64{1}}
	if err := n.New(item, "@", nil); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(base + ".logs")
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(line, "ingress|@|aa|demo|aa|i1|") {
		t.Fatalf("unexpected wal line: %q", line)
	}
}
