package core

import (
	"errors"
	"github.com/indexus/go-indexus-core/domain"
	"testing"
	"time"
)

func TestSpawnedNodeJoiningRefusesClientWrites(t *testing.T) {
	node := newNodeOn(t, &memStorage{}, 4)
	node.EnableAutoscale(AutoscaleConfig{
		Enabled: true,
		Role:    "spawned",
	})

	if node.ClientReady() {
		t.Fatal("spawned node without zones must not be client-ready")
	}
	err := node.New(item("i1"), "@", "aa")
	if !errors.Is(err, ErrJoining) {
		t.Fatalf("New err=%v want ErrJoining", err)
	}
}

func TestSpawnedNodeAcceptsPeerTransferWhileJoining(t *testing.T) {
	node := newNodeOn(t, &memStorage{}, 4)
	node.EnableAutoscale(AutoscaleConfig{
		Enabled: true,
		Role:    "spawned",
	})

	key := domain.Key{Collection: "demo", Location: "aa"}
	if err := node.Transfer(node, key, []*domain.Item{item("peer")}); err != nil {
		t.Fatalf("Transfer while joining: %v", err)
	}
	if got := node.queue.Length(); got != 1 {
		t.Fatalf("transfer not queued: length=%d", got)
	}
}

func TestSpawnedNodePublishesAfterOwnershipSettles(t *testing.T) {
	node := newNodeOn(t, &memStorage{}, 4)
	node.EnableAutoscale(AutoscaleConfig{
		Enabled: true,
		Role:    "spawned",
	})
	node.settings.peerQueueMax = 1000
	node.settings.queueMax = 1000

	node.create("demo", "aa")
	if node.ownedZoneCount() == 0 {
		t.Fatal("expected owned zone after create")
	}

	if !node.ClientReady() {
		t.Fatal("expected client_ready after ownership with empty queue")
	}
	if !node.clientPublished.Load() {
		t.Fatal("expected clientPublished latch")
	}

	if !node.ClientReady() {
		t.Fatal("client_ready must stay latched")
	}
}

func TestBootstrapAlwaysClientReady(t *testing.T) {
	node := newNodeOn(t, &memStorage{}, 4)
	node.EnableAutoscale(AutoscaleConfig{
		Enabled: true,
		Role:    "bootstrap",
	})
	if !node.ClientReady() {
		t.Fatal("bootstrap must be client-ready without owned zones")
	}
}

func TestClientReadyLatchSurvivesBacklogRegrowth(t *testing.T) {
	node := newNodeOn(t, &memStorage{}, 4)
	node.EnableAutoscale(AutoscaleConfig{
		Enabled: true,
		Role:    "spawned",
	})
	node.settings.peerQueueMax = 8
	node.settings.queueMax = 8

	node.create("demo", "aa")
	if !node.ClientReady() {
		t.Fatal("precondition: node never published")
	}

	for i := 0; i < 8; i++ {
		_ = node.queue.TryAdd(NewElement(item("q"), "@", "aa"), 8)
	}
	if !node.ClientReady() {
		t.Fatal("a full queue flipped client_ready off — routing flap")
	}
}

func TestLeavingNodeIsNotClientReady(t *testing.T) {
	node := newNodeOn(t, &memStorage{}, 4)
	if !node.ClientReady() {
		t.Fatal("precondition: healthy bootstrap node not client-ready")
	}

	node.leaving.Store(true)
	if node.ClientReady() {
		t.Fatal("leaving node still advertises client_ready")
	}
}

func TestJoinPublishGraceOverridesBacklog(t *testing.T) {
	node := newNodeOn(t, &memStorage{}, 4)
	node.EnableAutoscale(AutoscaleConfig{
		Enabled: true,
		Role:    "spawned",
	})
	node.settings.peerQueueMax = 8
	node.settings.queueMax = 8

	node.create("demo", "aa")

	for i := 0; i < 6; i++ {
		_ = node.queue.TryAdd(NewElement(item("q"), "@", "aa"), 8)
	}
	node.joinFirstOwn.Store(time.Now().Add(-joinPublishGrace - time.Second).UnixNano())
	if !node.ClientReady() {
		t.Fatal("grace should publish even with a Transfer backlog")
	}
}
