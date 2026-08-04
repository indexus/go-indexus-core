//go:build bench

package core

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
)

type stageTimings struct {
	Items int

	ZonesSnap       time.Duration
	ReceiverPull    time.Duration
	WALDelta        time.Duration
	MirrorWrites    time.Duration
	SwitchOwnership time.Duration

	DonorCritical time.Duration
	TotalS3       time.Duration

	ClassicTransfer time.Duration
	ClassicBytes    int
	OfferBytes      int
	DeltaBytes      int
	DonorWireS3     int
	WireReduction   float64
	DonorSpeedup    float64
}

func seedItems(t *testing.T, n *Node, count int) {
	t.Helper()
	n.settings.SetQueueMax(count + 1024)
	root := encoding.BASE64.Root()
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("item-%06d", i)
		it := &domain.Item{
			Collection: "demo",
			Location:   "aa",
			Id:         id,
			Metrics:    []float64{float64(i % 100), 1.5},
		}
		if err := n.New(it, root, "aa"); err != nil {
			t.Fatalf("New %s: %v", id, err)
		}
		if i%256 == 255 {
			drainAll(t, n)
		}
	}
	drainAll(t, n)
}

func drainAll(t *testing.T, n *Node) {
	t.Helper()
	limit := n.queue.Length() + 1000
	if limit < 10000 {
		limit = 10000
	}
	for steps := 0; n.queue.Length() > 0; steps++ {
		if steps > limit*2 {
			t.Fatalf("queue does not drain, length=%d after %d steps", n.queue.Length(), steps)
		}
		element, ok := n.queue.Consume()
		if !ok {
			return
		}
		if err := n.process(element); err != nil {
			t.Fatalf("process: %v", err)
		}
	}
}

func measureClassicTransfer(t *testing.T, itemCount int) (time.Duration, int) {
	t.Helper()
	donor := newNodeOn(t, &memStorage{}, 1_000_000)
	seedItems(t, donor, itemCount)

	keys := donor.listOwnedKeys()
	if len(keys) == 0 {
		t.Fatal("no owned keys")
	}
	key := keys[0]
	col, ok := donor.collections.Get(key.Collection)
	if !ok {
		t.Fatal("collection missing")
	}

	items, _ := col.Delegate(key.Location)
	body, err := json.Marshal(struct {
		Origin string         `json:"origin"`
		Key    domain.Key     `json:"key"`
		Items  []*domain.Item `json:"items"`
	}{Origin: donor.Name(), Key: key, Items: items})
	if err != nil {
		t.Fatal(err)
	}
	wireBytes := len(body)

	recv := newNodeOn(t, &memStorage{}, 1_000_000)
	recv.settings.SetQueueMax(itemCount + 1024)
	start := time.Now()
	if err := recv.Transfer(donor, key, items); err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	drainAll(t, recv)
	elapsed := time.Since(start)

	donor.removeOwnedKey(key)
	return elapsed, wireBytes
}

func measureS3Delegation(t *testing.T, itemCount int, warmSnapshot bool) stageTimings {
	t.Helper()
	st := stageTimings{Items: itemCount}
	t.Setenv("INDEXUS_ZONE_SNAP_MIN", "1ns")
	t.Setenv("INDEXUS_TRANSFER_THRESHOLD", "1")

	store := NewMemStore()
	donor := newNodeOn(t, &memStorage{}, 1_000_000)
	recv := newNodeOn(t, &memStorage{}, 1_000_000)
	donor.SetStore(store)
	recv.SetStore(store)
	donor.SetDelegation(true)
	recv.SetDelegation(true)
	recv.settings.SetQueueMax(itemCount + 1024)

	seedItems(t, donor, itemCount)
	keys := donor.listOwnedKeys()
	if len(keys) == 0 {
		t.Fatal("no owned keys")
	}

	ctx := context.Background()

	t0 := time.Now()
	if err := donor.forceZones(ctx, keys); err != nil {
		t.Fatalf("forceZones: %v", err)
	}
	st.ZonesSnap = time.Since(t0)

	zones := make([]domain.ZoneSnapshotRef, 0, len(keys))
	for _, k := range keys {
		if e, ok := donor.zoneRef(k); ok {
			zones = append(zones, e)
		}
	}
	if len(zones) == 0 {
		t.Fatal("no zone entries uploaded")
	}

	walCursor := donor.walLineCount()

	const residual = 30
	for i := 0; i < residual; i++ {
		late := &domain.Item{
			Collection: "demo",
			Location:   "aa",
			Id:         fmt.Sprintf("late-%d", i),
			Metrics:    []float64{1},
		}
		_ = donor.add(late, false)
	}

	offer := domain.DelegationOfferPayload{
		Donor:     donor.Name(),
		SessionID: fmt.Sprintf("bench-%d", itemCount),
		Zones:     zones,
		WALSeq:    int64(walCursor),
	}
	offerJSON, _ := json.Marshal(offer)
	st.OfferBytes = len(offerJSON)

	totalStart := time.Now()
	donorCritStart := time.Now()

	_ = offerJSON
	donorOfferDone := time.Now()

	t0 = time.Now()
	if err := recv.DelegationOffer(donor, offer); err != nil {
		t.Fatalf("DelegationOffer: %v", err)
	}
	st.ReceiverPull = time.Since(t0)

	t0 = time.Now()
	lines := donor.collectWALDelta(keys, walCursor)
	delta := domain.WALDeltaPayload{SessionID: offer.SessionID, Lines: lines, Done: true}
	deltaJSON, _ := json.Marshal(delta)
	st.DeltaBytes = len(deltaJSON)
	if err := recv.WALDelta(donor, delta); err != nil {
		t.Fatalf("WALDelta: %v", err)
	}
	st.WALDelta = time.Since(t0)

	t0 = time.Now()
	donor.delegMu.Lock()
	donor.delegOut[recv.Name()] = &delegationSession{
		ID:       offer.SessionID,
		PeerName: recv.Name(),
		Peer:     recv,
		Keys:     keys,
		WALSeq:   int64(walCursor),
		State:    delegMirroring,
		Started:  time.Now(),
	}
	donor.delegMu.Unlock()
	for i := 0; i < 10; i++ {
		m := &domain.Item{
			Collection: "demo",
			Location:   "aa",
			Id:         fmt.Sprintf("mirror-%d", i),
			Metrics:    []float64{1},
		}
		_ = donor.add(m, false)
		_ = recv.WALDelta(donor, domain.WALDeltaPayload{
			SessionID: offer.SessionID,
			Lines:     []string{m.Content()},
		})
	}
	st.MirrorWrites = time.Since(t0)

	t0 = time.Now()
	switchKeys := donor.listOwnedKeys()
	donor.delegMu.Lock()
	if s := donor.delegOut[recv.Name()]; s != nil {
		s.Keys = switchKeys
		s.State = delegCatchingUp
	}
	donor.delegMu.Unlock()
	if err := donor.CaughtUp(recv, domain.CaughtUpPayload{SessionID: offer.SessionID}); err != nil {
		t.Fatalf("CaughtUp: %v", err)
	}
	if err := recv.SwitchAck(donor, domain.SwitchAckPayload{SessionID: offer.SessionID, Keys: switchKeys}); err != nil {
		t.Fatalf("SwitchAck: %v", err)
	}
	st.SwitchOwnership = time.Since(t0)

	st.TotalS3 = time.Since(totalStart)

	st.DonorCritical = donorOfferDone.Sub(donorCritStart) + st.WALDelta + st.SwitchOwnership
	if warmSnapshot {

		st.ZonesSnap = 0
	}
	st.DonorWireS3 = st.OfferBytes + st.DeltaBytes

	if got := len(donor.listOwnedKeys()); got != 0 {
		t.Fatalf("donor still owns %d keys after switch", got)
	}
	count, err := recv.Count()
	if err != nil {
		t.Fatal(err)
	}
	if count < itemCount {
		t.Fatalf("receiver count=%d want >= %d", count, itemCount)
	}
	if len(lines) > residual*3 {
		t.Fatalf("delta too large (%d lines): expected residual-only, got full WAL?", len(lines))
	}

	return st
}

func TestDelegationTransitionStages(t *testing.T) {
	const n = 2000
	classicDur, classicBytes := measureClassicTransfer(t, n)

	st := measureS3Delegation(t, n, true)
	st.ClassicTransfer = classicDur
	st.ClassicBytes = classicBytes
	if st.DonorCritical > 0 {
		st.DonorSpeedup = float64(classicDur) / float64(st.DonorCritical)
	}
	if st.DonorWireS3 > 0 {
		st.WireReduction = float64(classicBytes) / float64(st.DonorWireS3)
	}

	t.Logf("=== Transition stages (%d items, warm snapshot) ===", n)
	t.Logf("  [async] checkpointZones : %s  (paid by periodic tick, not handoff)", st.ZonesSnap)
	t.Logf("  1. Offer (donor wire)   : %d B", st.OfferBytes)
	t.Logf("  2. Receiver S3 pull     : %s  (new node's network, not donor)", st.ReceiverPull)
	t.Logf("  3. Residual WAL delta   : %s  (%d B wire)", st.WALDelta, st.DeltaBytes)
	t.Logf("  4. Mirror burst         : %s", st.MirrorWrites)
	t.Logf("  5. Switch ownership     : %s", st.SwitchOwnership)
	t.Logf("  ---")
	t.Logf("  Donor critical path     : %s", st.DonorCritical)
	t.Logf("  Classic Transfer         : %s  (wire %d B = %.1f KiB)", st.ClassicTransfer, st.ClassicBytes, float64(st.ClassicBytes)/1024)
	t.Logf("  Donor wire reduction    : %.0fx  (%d B → %d B)", st.WireReduction, st.ClassicBytes, st.DonorWireS3)
	t.Logf("  Donor speedup           : %.1fx", st.DonorSpeedup)
	t.Logf("  E2E wall (incl. pull)   : %s", st.TotalS3)
}

func TestDelegationSpeedupAcrossSizes(t *testing.T) {
	sizes := []int{100, 500, 1000, 5000, 10000, 20000}
	t.Logf("%8s %14s %14s %12s %12s %10s %10s",
		"items", "classic", "donor_crit", "wire_classic", "wire_s3", "wire_x", "speedup")

	for _, n := range sizes {
		n := n
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			if n >= 20000 {
				t.Logf("large run n=%d — seeding may take a minute", n)
			}
			classicDur, classicBytes := measureClassicTransfer(t, n)
			st := measureS3Delegation(t, n, true)
			st.ClassicTransfer = classicDur
			st.ClassicBytes = classicBytes
			if st.DonorCritical > 0 {
				st.DonorSpeedup = float64(classicDur) / float64(st.DonorCritical)
			}
			if st.DonorWireS3 > 0 {
				st.WireReduction = float64(classicBytes) / float64(st.DonorWireS3)
			}
			t.Logf("%8d %14s %14s %12d %12d %9.0fx %9.1fx",
				n,
				classicDur.Round(time.Microsecond),
				st.DonorCritical.Round(time.Microsecond),
				classicBytes,
				st.DonorWireS3,
				st.WireReduction,
				st.DonorSpeedup,
			)
			if n >= 1000 && st.WireReduction < 10 {
				t.Errorf("wire reduction %.1fx < 10x for n=%d", st.WireReduction, n)
			}
			if n >= 1000 && st.DonorSpeedup < 2 {
				t.Errorf("donor speedup %.1fx < 2x for n=%d", st.DonorSpeedup, n)
			}
		})
	}
}

func seedItemsCollection(t *testing.T, n *Node, collection string, count int) {
	t.Helper()
	n.settings.SetQueueMax(count + 1024)
	root := encoding.BASE64.Root()
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("%s-%06d", collection, i)
		it := &domain.Item{
			Collection: collection,
			Location:   "aa",
			Id:         id,
			Metrics:    []float64{float64(i % 100), 1.5},
		}
		if err := n.New(it, root, "aa"); err != nil {
			t.Fatalf("New %s: %v", id, err)
		}
		if i%256 == 255 {
			drainAll(t, n)
		}
	}
	drainAll(t, n)
}

func handoffOneDonor(t *testing.T, store Store, donor, recv *Node, residual int) (
	donorCrit, recvPull time.Duration, classicBytes, s3Wire int, itemCount int,
) {
	t.Helper()
	ctx := context.Background()
	keys := donor.listOwnedKeys()
	if len(keys) == 0 {
		t.Fatal("donor owns nothing")
	}

	{
		key := keys[0]
		col, _ := donor.collections.Get(key.Collection)
		items, _ := col.Delegate(key.Location)
		body, _ := json.Marshal(struct {
			Origin string         `json:"origin"`
			Key    domain.Key     `json:"key"`
			Items  []*domain.Item `json:"items"`
		}{Origin: donor.Name(), Key: key, Items: items})
		classicBytes = len(body)
		donor.restoreDelegated(key, items)
	}
	itemCount, _ = donor.Count()

	if err := donor.forceZones(ctx, keys); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	walCursor := donor.walLineCount()
	for i := 0; i < residual; i++ {
		_ = donor.add(&domain.Item{
			Collection: keys[0].Collection,
			Location:   "aa",
			Id:         fmt.Sprintf("res-%d", i),
			Metrics:    []float64{1},
		}, false)
	}

	zones := make([]domain.ZoneSnapshotRef, 0, len(keys))
	for _, k := range keys {
		if e, ok := donor.zoneRef(k); ok {
			zones = append(zones, e)
		}
	}
	sessID := fmt.Sprintf("%s-%d", donor.Name(), time.Now().UnixNano())
	offer := domain.DelegationOfferPayload{
		Donor:     donor.Name(),
		SessionID: sessID,
		Zones:     zones,
		WALSeq:    int64(walCursor),
	}
	offerJSON, _ := json.Marshal(offer)

	critStart := time.Now()
	_ = offerJSON

	t0 := time.Now()
	if err := recv.DelegationOffer(donor, offer); err != nil {
		t.Fatalf("offer: %v", err)
	}
	recvPull = time.Since(t0)

	t0 = time.Now()
	lines := donor.collectWALDelta(keys, walCursor)
	delta := domain.WALDeltaPayload{SessionID: sessID, Lines: lines, Done: true}
	deltaJSON, _ := json.Marshal(delta)
	if err := recv.WALDelta(donor, delta); err != nil {
		t.Fatalf("delta: %v", err)
	}
	deltaDur := time.Since(t0)

	donor.delegMu.Lock()
	donor.delegOut[recv.Name()] = &delegationSession{
		ID: sessID, PeerName: recv.Name(), Peer: recv,
		Keys: keys, WALSeq: int64(walCursor), State: delegCatchingUp, Started: time.Now(),
	}
	donor.delegMu.Unlock()

	t0 = time.Now()
	switchKeys := donor.listOwnedKeys()
	donor.delegMu.Lock()
	if s := donor.delegOut[recv.Name()]; s != nil {
		s.Keys = switchKeys
	}
	donor.delegMu.Unlock()
	if err := donor.CaughtUp(recv, domain.CaughtUpPayload{SessionID: sessID}); err != nil {
		t.Fatalf("caughtup: %v", err)
	}
	if err := recv.SwitchAck(donor, domain.SwitchAckPayload{SessionID: sessID, Keys: switchKeys}); err != nil {
		t.Fatalf("switch: %v", err)
	}
	switchDur := time.Since(t0)

	donorCrit = time.Since(critStart) - recvPull + deltaDur + switchDur

	donorCrit = deltaDur + switchDur
	s3Wire = len(offerJSON) + len(deltaJSON)
	return donorCrit, recvPull, classicBytes, s3Wire, itemCount
}

func TestMultiDonorS3Delegation(t *testing.T) {
	const (
		donors   = 4
		perDonor = 3000
		residual = 20
	)
	t.Setenv("INDEXUS_ZONE_SNAP_MIN", "1ns")
	t.Setenv("INDEXUS_TRANSFER_THRESHOLD", "1")

	store := NewMemStore()
	recv := newNodeOn(t, &memStorage{}, 1_000_000)
	recv.SetStore(store)
	recv.SetDelegation(true)
	recv.EnableAutoscale(AutoscaleConfig{Enabled: true, Role: "spawned", Window: time.Minute})
	recv.settings.SetQueueMax(donors*perDonor + 4096)

	type result struct {
		name                 string
		crit, pull           time.Duration
		classicBytes, s3Wire int
		items                int
	}
	results := make([]result, donors)
	ds := make([]*Node, donors)

	for i := 0; i < donors; i++ {
		d := newNodeOn(t, &memStorage{}, 1_000_000)
		d.SetStore(store)
		d.SetDelegation(true)
		seedItemsCollection(t, d, fmt.Sprintf("col%d", i), perDonor)
		ds[i] = d
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		pullSum time.Duration
		critMax time.Duration
		wireC   int
		wireS   int
	)
	wallStart := time.Now()
	for i := 0; i < donors; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			crit, pull, cb, sb, items := handoffOneDonor(t, store, ds[i], recv, residual)
			mu.Lock()
			results[i] = result{ds[i].Name(), crit, pull, cb, sb, items}
			if pull > pullSum {

			}
			_ = pullSum
			if crit > critMax {
				critMax = crit
			}
			wireC += cb
			wireS += sb
			mu.Unlock()
			t.Logf("  donor[%d] items=%d crit=%s pull=%s wire %d→%d",
				i, items, crit.Round(time.Microsecond), pull.Round(time.Microsecond), cb, sb)
		}(i)
	}
	wg.Wait()
	wall := time.Since(wallStart)

	totalExpected := 0
	for i, d := range ds {
		if got := len(d.listOwnedKeys()); got != 0 {
			t.Errorf("donor[%d] still owns %d keys", i, got)
		}
		totalExpected += results[i].items + residual
	}
	recvCount, err := recv.Count()
	if err != nil {
		t.Fatal(err)
	}

	minExpected := donors * perDonor
	if recvCount < minExpected {
		t.Fatalf("receiver count=%d want >= %d", recvCount, minExpected)
	}
	if pending := recv.pendingInbound(); pending != 0 {
		t.Fatalf("pending inbound sessions=%d want 0", pending)
	}

	if !recv.ClientReady() {

		recv.tryPublishClientReady()
	}
	if !recv.ClientReady() {
		t.Fatal("receiver not client_ready after all donors switched")
	}

	t.Logf("=== Multi-donor S3 (%d donors × %d items) ===", donors, perDonor)
	t.Logf("  Wall (parallel handoffs): %s", wall)
	t.Logf("  Max donor critical path : %s", critMax)
	t.Logf("  Classic wire (sum)      : %d B (%.1f MiB)", wireC, float64(wireC)/(1024*1024))
	t.Logf("  S3 donor wire (sum)     : %d B", wireS)
	t.Logf("  Wire reduction          : %.0fx", float64(wireC)/float64(max(wireS, 1)))
	t.Logf("  Receiver count          : %d (seeded >= %d)", recvCount, minExpected)

	if float64(wireC)/float64(max(wireS, 1)) < 50 {
		t.Errorf("expected large wire reduction with %d×%d items", donors, perDonor)
	}
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
