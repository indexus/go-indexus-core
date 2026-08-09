package core

import (
	"sync/atomic"
	"testing"

	"github.com/indexus/go-indexus-core/domain"
)

// introducer answers Random with a peer of the test's choosing and counts how
// often it was asked, which is what separates "the sample never ran" from "the
// sample ran and found nothing".
type introducer struct {
	*dialPeer
	introduce domain.Contact
	asked     atomic.Int64
}

func newIntroducer(name, ip string, port int, introduce domain.Contact) *introducer {
	return &introducer{dialPeer: newDialPeer(name, ip, port), introduce: introduce}
}

func (p *introducer) Random(domain.Peer) (domain.Contact, error) {
	p.asked.Add(1)
	if p.introduce == nil {
		return nil, nil
	}
	return p.introduce, nil
}

func TestSampleFanoutTracksTheLogOfTheMesh(t *testing.T) {
	cases := []struct {
		known int
		want  int
	}{
		{0, 0},
		{1, 1},
		{4, 3},
		{12, 4},
		{200, 8},
		{1000, 10},
	}
	for _, tc := range cases {
		if got := sampleFanout(tc.known); got != tc.want {
			t.Fatalf("sampleFanout(%d)=%d want %d", tc.known, got, tc.want)
		}
	}
}

// A node's own neighbourhood is all Refresh can teach it. Observe now asks the
// peers it has for someone it does not, which is the only channel through which
// a distant region of the mesh can reach a view that never touches it.
func TestObserveAsksKnownPeersToNameAStranger(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	stranger := newDialPeer("StrangerAAAAAAAA", "10.0.0.3", 21003)
	known := newIntroducer("KnownPeerAAAAAAA", "10.0.0.2", 21002, stranger)
	n.register([]domain.Contact{known})

	if err := n.Observe(); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if known.asked.Load() == 0 {
		t.Fatal("Observe never asked the one peer it knows for an introduction")
	}

	// An introduction is a lead, not a credential: the stranger waits in
	// acknowledged until it answers a ping for itself.
	if !holds(n.traverseAcknowledged(false), stranger.Name()) {
		t.Fatal("the introduced peer was not acknowledged")
	}
	if holds(n.traverseRegistered(false), stranger.Name()) {
		t.Fatal("the introduced peer was registered on hearsay, without a ping of its own")
	}

	if err := n.Observe(); err != nil {
		t.Fatalf("second Observe: %v", err)
	}
	if !holds(n.traverseRegistered(false), stranger.Name()) {
		t.Fatal("the introduced peer answered a ping but never made it into registered")
	}
}

// Quarantine exists because a peer stopped answering. Spending a round trip
// asking it for introductions is the one thing we already know will not work.
func TestDiscoveryLeavesQuarantinedPeersAlone(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	stranger := newDialPeer("StrangerAAAAAAAA", "10.0.0.3", 21003)
	known := newIntroducer("KnownPeerAAAAAAA", "10.0.0.2", 21002, stranger)
	n.register([]domain.Contact{known})
	n.suspend(known.Name(), quarantineWindow)

	if found := n.discover(); len(found) != 0 {
		t.Fatalf("discover returned %d contact(s) from a quarantined peer", len(found))
	}
	if got := known.asked.Load(); got != 0 {
		t.Fatalf("asked a quarantined peer %d time(s)", got)
	}
}

// The caller is sampling for peers outside its own view, so naming it back is
// the one answer that carries no information.
func TestRandomNeverNamesTheAsker(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	asker := newDialPeer("AskerAAAAAAAAAAA", "10.0.0.4", 21004)
	n.register([]domain.Contact{asker})

	introduced, err := n.Random(asker)
	if err != nil {
		t.Fatalf("Random: %v", err)
	}
	if introduced != nil {
		t.Fatalf("Random named the asker back: %s", introduced.Name())
	}

	other := newDialPeer("OtherPeerAAAAAAA", "10.0.0.5", 21005)
	n.register([]domain.Contact{other})
	for i := 0; i < 20; i++ {
		introduced, err := n.Random(asker)
		if err != nil {
			t.Fatalf("Random: %v", err)
		}
		if introduced == nil || introduced.Name() != other.Name() {
			t.Fatalf("Random returned %v, want the only peer that is not the asker", introduced)
		}
	}
}

func holds(contacts []domain.Contact, name string) bool {
	for _, contact := range contacts {
		if contact.Name() == name {
			return true
		}
	}
	return false
}
