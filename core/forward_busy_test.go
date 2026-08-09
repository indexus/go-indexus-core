package core

import (
	"errors"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
)

// A peer that answers 503 must not keep consuming forwardRate tokens on every
// subsequent Feed attempt — markPeerBusy fail-fasts until the short window elapses.
func TestForwardFailFastSkipsBusyPeer(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	n.settings.SetForwardRate(0) // unlimited — isolate busy-peer short-circuit
	root := encoding.BASE64.Root()

	id, err := encoding.MergeEncodings(encoding.BASE64, encoding.BASE64, root, "demo")
	if err != nil {
		t.Fatalf("MergeEncodings: %v", err)
	}
	peer := &busyPeer{
		forwardFailPeer: &forwardFailPeer{dialPeer: newDialPeer("PeerAAAAAAAAAAAA", "10.0.0.1", 21000)},
		err:             domain.ErrPeerBusy,
	}
	peer.id = id

	item := &domain.Item{Collection: "demo", Location: "aa", Id: "x", Metrics: []float64{1}}
	if err := n.forwardNew(peer, item, root, nil); !errors.Is(err, domain.ErrPeerBusy) {
		t.Fatalf("first forward: %v want ErrPeerBusy", err)
	}
	firstCalls := peer.calls.Load()
	if firstCalls != 1 {
		t.Fatalf("first forward calls=%d want 1", firstCalls)
	}

	// Immediate retry must short-circuit without another RPC or another token.
	if err := n.forwardNew(peer, item, root, nil); !errors.Is(err, domain.ErrPeerBusy) {
		t.Fatalf("busy forward: %v want ErrPeerBusy", err)
	}
	if got := peer.calls.Load(); got != firstCalls {
		t.Fatalf("busy peer still dialed: calls=%d want %d", got, firstCalls)
	}

	time.Sleep(peerBusyBase + 20*time.Millisecond)
	_ = n.forwardNew(peer, item, root, nil)
	if got := peer.calls.Load(); got != firstCalls+1 {
		t.Fatalf("after busy window calls=%d want %d", got, firstCalls+1)
	}
}

type busyPeer struct {
	*forwardFailPeer
	err error
}

func (p *busyPeer) New(*domain.Item, string, domain.Visited) error {
	p.calls.Add(1)
	return p.err
}
