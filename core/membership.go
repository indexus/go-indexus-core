package core

import (
	"log/slog"
	"math/bits"
	"math/rand"
	"sync"
	"time"

	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
)

func (n *Node) Observe() error {

	toIgnore := make([]domain.Contact, 0)
	toRegister := make([]domain.Contact, 0)
	toReject := make([]domain.Contact, 0)
	toProbe := make([]domain.Contact, 0)

	empty := true

	for _, contact := range n.traverseRegistered(false) {
		empty = false

		if !contactDialable(contact) {
			toReject = append(toReject, contact)
			continue
		}
		if _, err := contact.Ping(n); err != nil {
			n.suspend(contact.Name(), quarantineWindow)
			toReject = append(toReject, contact)
			toProbe = append(toProbe, contact)
		} else {
			// A successful round-trip is a stronger, newer membership fact
			// than the failure that opened quarantine.
			n.resume(contact.Name())
		}
	}
	for _, contact := range n.traverseAcknowledged(false) {
		empty = false

		node, err := contact.Ping(n)
		if node != nil {
			if err == nil && node.Name() == contact.Name() {
				n.resume(contact.Name())
			}
			toRegister = append(toRegister, node)
		}
		// Quarantined contacts remain probationary probe targets. Dropping the
		// last address on failure would make a healed partition undiscoverable
		// until the quarantine clock elapsed.
		if (err != nil || node == nil) && n.suspended(contact.Name()) {
			continue
		}
		if err != nil || node == nil || node.Name() != contact.Name() {
			toIgnore = append(toIgnore, contact)
		}
	}

	if empty {
		n.acknowledge(n.bootstraps)
	}

	n.ignore(toIgnore)
	n.reject(toReject)
	n.acknowledge(toProbe)

	registeredNew := n.register(toRegister)

	if registeredNew {
		n.subscribe(toRegister)
	}

	// Strangers go through the same ladder as anyone else: acknowledged now,
	// pinged on the next tick, registered only if they answer for themselves.
	// An introduction is a lead, not a credential.
	if !n.leaving.Load() {
		n.acknowledge(n.discover())
	}

	// Do not walk collections while ownership is moving — MeasureItems takes
	// Collection.RLock for the full tree and would stall applyZone / handoff.
	if !n.TransferBusy() {
		n.MeasureItems()
	}

	n.tryPublishClientReady()

	if registeredNew && !n.leaving.Load() && !n.TransferBusy() {
		go func() {
			if err := n.Refresh(); err != nil {
				slog.Warn("refresh-on-new-peer failed", "err", err)
			}
		}()
	}

	return nil
}

func (n *Node) Ping(origin domain.Contact) (domain.Contact, error) {

	if n.leaving.Load() {
		return nil, domain.ErrLeaving
	}

	if len(origin.Name()) > 0 {
		n.acknowledge([]domain.Contact{origin})
	}

	return n, nil
}

// gossipBucketWidth is how many contacts per bucket a Neighbors answer
// carries. One is all a *lookup* needs, since the head of each bucket is the
// nearest contact in its band and a single hop closer to the key is progress.
// Membership is learned through the same answer, though, and one per bucket
// cannot carry a mesh: bucket 0 always holds about half of it and names one.
// Measured, a saturated one-per-bucket channel reaches 20% of a 16-node mesh
// and 6% of a 64-node one — and no number of peers asked or rounds waited
// lifts that, because once every table is complete every answer is the same
// (`TestGossipChannelCeiling`). Twenty is Kademlia's k, and it takes a 16-node
// mesh to full knowledge in a single round.
//
// It raises a ceiling, it does not remove one: at a thousand nodes twenty per
// bucket still only reaches 12%. Completeness comes from M5, whose draw does
// not sort by distance; this is what makes it fast enough to matter.
const gossipBucketWidth = 20

func (n *Node) Neighbors(origin domain.Peer) ([]domain.Contact, error) {

	id, err := encoding.BASE64.Decode(origin.Name())
	if err != nil {
		return nil, err
	}

	buckets := &[160][]domain.Contact{}
	n.registered.ExtractK(0, id, gossipBucketWidth, buckets)

	result := []domain.Contact{}
	for _, bucket := range buckets {
		for _, neighbor := range bucket {
			if neighbor != nil {
				result = append(result, neighbor)
			}
		}
	}
	return result, nil
}

// routingNeighbors is what clean() rebuilds the routing table from: one
// contact per bucket, the narrow extract. Routing stays deliberately lean —
// Refresh asks every routing peer for its neighbours and control() plans a
// donation against each of them, so widening it multiplies both costs for a
// table whose only job is to name the next hop. Learning the mesh is the
// gossip answer's work, not the routing table's.
func (n *Node) routingNeighbors() []domain.Contact {
	neighbors := &[160]domain.Contact{}
	n.registered.Extract(0, n.ID(), neighbors)

	result := []domain.Contact{}
	for _, neighbor := range neighbors {
		if neighbor != nil {
			result = append(result, neighbor)
		}
	}
	return result
}

func (n *Node) Random(origin domain.Peer) (domain.Contact, error) {

	contacts := n.traverseRegistered(false)

	// Naming the asker back is the one answer that teaches it nothing: it is
	// sampling precisely for peers outside its own view.
	if name := origin.Name(); name != "" {
		kept := contacts[:0]
		for _, contact := range contacts {
			if contact.Name() != name {
				kept = append(kept, contact)
			}
		}
		contacts = kept
	}

	if len(contacts) == 0 {
		return nil, nil
	}

	return contacts[rand.Intn(len(contacts))], nil
}

// sampleFanout is how many peers to ask for an introduction on one tick: log2
// of what we already know. Below that, an epidemic protocol leaves the
// knowledge graph in pieces; above it, the extra round trips buy almost
// nothing. It is also the figure the validation scenarios settled on — twelve
// nodes fan out to four, two hundred to eight, a thousand to ten.
func sampleFanout(known int) int {
	if known <= 0 {
		return 0
	}
	return bits.Len(uint(known))
}

// discover asks a random sample of known peers to name a peer of their own
// choosing.
//
// Refresh already walks the mesh, but everything it can learn there is an
// extract around our own identifier: Neighbors answers with the peers closest
// to the asker, so a view keeps being sharpened in the region it already
// covers and never reaches the rest. That is enough to route a write, which
// only ever needs a next hop closer to the target and is safe because XOR
// distance falls at every step. It is not enough for anything that concludes
// from silence — an unanswered pull means "nobody I know owns this", which is
// evidence only if what we know is a fair sample of the mesh. Under a biased
// view a node reads its own ignorance as an absent owner.
//
// The sample therefore has to come from outside our neighbourhood, and it
// does: each peer answers Random out of its own registered pool, which is
// centred on it rather than on us. Composed over ticks this is a random walk
// on the knowledge graph, and it is what keeps that graph connected across
// partitions and churn.
//
// One contact comes back per call, not a table. The simulator that validated
// this exchanged whole routing tables, which converges just as well and costs
// O(N) of state per message — fine in a model that does not weigh its
// messages, not something to put on a real mesh.
func (n *Node) discover() []domain.Contact {
	known := n.traverseRegistered(false)
	fanout := sampleFanout(len(known))
	if fanout == 0 {
		return nil
	}

	// Partial Fisher-Yates: an unbiased sample of `fanout` without shuffling a
	// slice the size of the mesh.
	for i := 0; i < fanout; i++ {
		j := i + rand.Intn(len(known)-i)
		known[i], known[j] = known[j], known[i]
	}

	var (
		mu    sync.Mutex
		found []domain.Contact
		wg    sync.WaitGroup
	)
	for _, contact := range known[:fanout] {
		if n.suspended(contact.Name()) {
			continue
		}
		contact := contact
		wg.Add(1)
		go func() {
			defer wg.Done()
			introduced, err := contact.Random(n)
			// Random must return a true nil interface on miss — a typed
			// (*Contact)(nil) would pass this check and panic on .Name().
			if err != nil || introduced == nil {
				return
			}
			if introduced.Name() == n.Name() || !contactDialable(introduced) {
				return
			}
			if n.suspended(introduced.Name()) {
				// An introduction is not enough to clear quarantine, but a
				// direct successful ping is. This lets healed partitions
				// converge immediately without trusting silence or waiting for
				// the quarantine clock.
				confirmed, pingErr := introduced.Ping(n)
				if pingErr != nil || confirmed == nil || confirmed.Name() != introduced.Name() {
					return
				}
				n.resume(introduced.Name())
				introduced = confirmed
			}
			mu.Lock()
			found = append(found, introduced)
			mu.Unlock()
		}()
	}
	wg.Wait()

	return found
}

func (n *Node) acknowledge(candidates []domain.Contact) {
	if len(candidates) == 0 {
		return
	}
	for _, candidate := range candidates {
		_, exist := n.registered.Get(0, candidate.ID())
		if !exist {
			n.acknowledged.Insert(0, candidate.ID(), candidate)
		}
	}
}

func (n *Node) ignore(candidates []domain.Contact) {
	if len(candidates) == 0 {
		return
	}
	for _, candidate := range candidates {
		n.acknowledged.Remove(0, candidate.ID())
	}
}

func (n *Node) register(contacts []domain.Contact) bool {
	if len(contacts) == 0 {
		return false
	}
	inserted := false
	for _, contact := range contacts {
		if !contactDialable(contact) || n.suspended(contact.Name()) {
			continue
		}
		_ = n.acknowledged.Remove(0, contact.ID())
		existing, exist := n.registered.Get(0, contact.ID())
		if !exist {
			n.registered.Insert(0, contact.ID(), contact)
			inserted = true
			continue
		}

		if !contactDialable(existing) || (existing.IP() == "" && contact.IP() != "") {
			n.registered.Remove(0, contact.ID())
			n.registered.Insert(0, contact.ID(), contact)
		}
	}
	return inserted
}

func (n *Node) reject(contacts []domain.Contact) {
	if len(contacts) == 0 {
		return
	}
	for _, contact := range contacts {
		n.routing.Remove(0, contact.ID())
	}
	for _, contact := range contacts {
		n.registered.Remove(0, contact.ID())
	}
}

func (n *Node) subscribe(contacts []domain.Contact) {
	if len(contacts) == 0 {
		return
	}
	for _, contact := range contacts {
		n.routing.Insert(0, contact.ID(), contact)
	}
}

func (n *Node) traverseAcknowledged(self bool) []domain.Contact {
	contacts := make([]domain.Contact, 0)
	n.acknowledged.Traverse(0, encoding.BASE64.NewID(), func(i int, b []byte, c domain.Contact) {
		if !self && n.Name() == c.Name() {
			return
		}
		contacts = append(contacts, c)
	})
	return contacts
}

func (n *Node) traverseRegistered(self bool) []domain.Contact {
	contacts := make([]domain.Contact, 0)
	n.registered.Traverse(0, encoding.BASE64.NewID(), func(i int, b []byte, c domain.Contact) {
		if !self && n.Name() == c.Name() {
			return
		}
		contacts = append(contacts, c)
	})
	return contacts
}

func (n *Node) traverseRouting(self bool) []domain.Contact {
	contacts := make([]domain.Contact, 0)
	n.routing.Traverse(0, encoding.BASE64.NewID(), func(i int, b []byte, p domain.Peer) {
		if !self && n.Name() == p.Name() {
			return
		}
		c, exist := n.registered.Get(0, p.ID())
		if exist {
			contacts = append(contacts, c)
		}
	})
	return contacts
}

func (n *Node) clean() error {

	neighbors := n.routingNeighbors()

	n.routing.Reset()

	n.subscribe(append(neighbors, n))
	return nil
}

const quarantineWindow = 90 * time.Second

func (n *Node) suspend(name string, window time.Duration) {
	if name == "" || name == n.Name() {
		return
	}

	n.suspectMu.Lock()
	defer n.suspectMu.Unlock()

	until := time.Now().Add(window)
	if current, exist := n.suspect[name]; exist && current.After(until) {
		return
	}
	n.suspect[name] = until
	n.ghosted[name] = struct{}{}
}

func (n *Node) resume(name string) {
	if name == "" {
		return
	}
	n.suspectMu.Lock()
	delete(n.suspect, name)
	delete(n.ghosted, name)
	n.suspectMu.Unlock()
}

func (n *Node) suspended(name string) bool {
	n.suspectMu.Lock()
	defer n.suspectMu.Unlock()

	until, exist := n.suspect[name]
	if !exist {
		return false
	}
	if time.Now().After(until) {
		delete(n.suspect, name)
		return false
	}
	return true
}

// ghostedPeer reports that name entered quarantine and has not resumed since.
// Combined with absence from membership tables, this is a reclaim verdict.
func (n *Node) ghostedPeer(name string) bool {
	if name == "" {
		return false
	}
	n.suspectMu.Lock()
	defer n.suspectMu.Unlock()
	_, ok := n.ghosted[name]
	return ok
}

// contactByName returns the dialable contact known under name, registered
// before acknowledged. A peer we did not create ourselves only has an address
// through membership, and a freshly spawned one is often still acknowledged
// when we already need to reach it — hence both pools.
func (n *Node) contactByName(name string) domain.Contact {
	for _, pool := range [][]domain.Contact{n.traverseRegistered(false), n.traverseAcknowledged(false)} {
		for _, c := range pool {
			if c.Name() == name && contactDialable(c) {
				return c
			}
		}
	}
	return nil
}

func contactDialable(c domain.Contact) bool {
	if c == nil || c.Port() <= 0 {
		return false
	}
	if c.IP() != "" {
		return true
	}
	for ip := range c.IPs() {
		if ip != "" {
			return true
		}
	}
	return false
}
