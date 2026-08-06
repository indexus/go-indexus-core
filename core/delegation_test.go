package core

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
)

func TestZoneSnapshotRoundTrip(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	store := NewMemStore()
	n.SetStore(store)
	t.Setenv("INDEXUS_ZONE_SNAP_MIN", "1ns")

	root := encoding.BASE64.Root()
	if err := n.New(item("z1"), root, "aa"); err != nil {
		t.Fatalf("New: %v", err)
	}
	drain(t, n)

	keys := n.listOwnedKeys()
	if len(keys) == 0 {
		t.Fatal("expected owned keys")
	}
	if err := n.forceZones(context.Background(), keys); err != nil {
		t.Fatalf("forceZones: %v", err)
	}

	entry, ok := n.zoneRef(keys[0])
	if !ok {
		t.Fatal("expected manifest entry after checkpoint")
	}
	body, err := store.Get(context.Background(), entry.Key)
	if err != nil {
		t.Fatalf("Get zone: %v", err)
	}
	lines, err := gobDecodeStrings(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(lines) < 2 {
		t.Fatalf("zone snap too short: %v", lines)
	}

	man, err := pullManifest(context.Background(), store, n.Name())
	if err != nil {
		t.Fatalf("pullManifest: %v", err)
	}
	if len(man.Zones) == 0 {
		t.Fatal("manifest missing zones")
	}

	recv := newNodeOn(t, &memStorage{}, 64)
	if err := recv.applyZoneLines(lines); err != nil {
		t.Fatalf("applyZoneLines: %v", err)
	}
	count, err := recv.Count()
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if count != 1 {
		t.Fatalf("count=%d want 1", count)
	}
}

func TestDirtyTrackingMarksOwnedZone(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	root := encoding.BASE64.Root()
	if err := n.New(item("d1"), root, "aa"); err != nil {
		t.Fatalf("New: %v", err)
	}
	drain(t, n)

	n.zoneSnap.mu.Lock()
	dirty := len(n.zoneSnap.dirty)
	n.zoneSnap.mu.Unlock()
	if dirty == 0 {
		t.Fatal("expected dirty zone after insert")
	}
}

func TestWALLineKeyParsing(t *testing.T) {
	col, loc := walLineKey("demo|aa|x|1.000000")
	if col != "demo" || loc != "aa" {
		t.Fatalf("got %s/%s", col, loc)
	}
	col, loc = walLineKey("tombstone|demo|aa|gone|3")
	if col != "demo" || loc != "aa" {
		t.Fatalf("tombstone got %s/%s", col, loc)
	}
	col, loc = walLineKey("ingress|@|aa|demo|aa|x|1.000000")
	if col != "demo" || loc != "aa" {
		t.Fatalf("ingress got %s/%s", col, loc)
	}
}

func TestDelegationFallsBackWithoutStore(t *testing.T) {
	n := ownedNode(t, &memStorage{})
	n.SetDelegation(true)

	peer := nearestFailPeer(t, n, "demo", encoding.BASE64.Root())
	n.subscribe([]domain.Contact{peer})
	handed := n.offerZones(peer, n.listOwnedKeys())
	if handed != 0 {

		t.Logf("handed=%d (expected 0 on refuse)", handed)
	}
	after, _ := n.Count()
	if after == 0 {
		t.Fatal("items lost when S3 path fell back to Transfer failure")
	}
}

func TestDelegationOfferApplyAndSwitch(t *testing.T) {
	donor := newNodeOn(t, &memStorage{}, 64)
	recv := newNodeOn(t, &memStorage{}, 64)
	store := NewMemStore()
	donor.SetStore(store)
	recv.SetStore(store)
	donor.SetDelegation(true)
	recv.SetDelegation(true)
	t.Setenv("INDEXUS_ZONE_SNAP_MIN", "1ns")
	t.Setenv("INDEXUS_TRANSFER_THRESHOLD", "1")

	root := encoding.BASE64.Root()
	for i := 0; i < 3; i++ {
		if err := donor.New(item("d"+string(rune('a'+i))), root, "aa"); err != nil {
			t.Fatalf("New: %v", err)
		}
	}
	drain(t, donor)

	keys := donor.listOwnedKeys()
	if err := donor.forceZones(context.Background(), keys); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	zones := make([]domain.ZoneSnapshotRef, 0)
	for _, k := range keys {
		if e, ok := donor.zoneRef(k); ok {
			zones = append(zones, e)
		}
	}
	if len(zones) == 0 {
		t.Fatal("no zones uploaded")
	}

	offer := domain.DelegationOfferPayload{
		Donor:     donor.Name(),
		SessionID: "sess-1",
		Zones:     zones,
		WALSeq:    time.Now().UnixNano(),
	}
	if err := recv.DelegationOffer(donor, offer); err != nil {
		t.Fatalf("DelegationOffer: %v", err)
	}
	if recv.pendingInbound() != 1 {
		t.Fatalf("pending=%d want 1", recv.pendingInbound())
	}

	delta := domain.WALDeltaPayload{SessionID: "sess-1", Lines: nil, Done: true}
	if err := recv.WALDelta(donor, delta); err != nil {
		t.Fatalf("WALDelta: %v", err)
	}

	donor.delegMu.Lock()
	donor.delegOut[recv.Name()] = &delegationSession{
		ID:       "sess-1",
		PeerName: recv.Name(),
		Peer:     recv,
		Keys:     keys,
		State:    delegCatchingUp,
		Started:  time.Now(),
	}
	donor.delegMu.Unlock()

	if err := donor.CaughtUp(recv, domain.CaughtUpPayload{SessionID: "sess-1"}); err != nil {
		t.Fatalf("CaughtUp: %v", err)
	}
	if got := len(donor.listOwnedKeys()); got != 0 {
		t.Fatalf("donor still owns %d keys", got)
	}
	// SwitchAck is invoked inside CaughtUp (ACK-before-drop); a second call is idempotent.
	if err := recv.SwitchAck(donor, domain.SwitchAckPayload{SessionID: "sess-1", Keys: keys}); err != nil {
		t.Fatalf("SwitchAck: %v", err)
	}
	if recv.pendingInbound() != 0 {
		t.Fatal("inbound session should be closed after switch")
	}
}

func TestUploadWALRecordsManifest(t *testing.T) {
	n := newNodeOn(t, &memStorage{}, 64)
	store := NewMemStore()
	n.SetStore(store)

	dir := t.TempDir()
	path := dir + "/20260101_000000_backup.logs"
	if err := os.WriteFile(path, []byte("demo|aa|x|1.000000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := n.UploadWAL(context.Background(), path); err != nil {
		t.Fatalf("UploadWAL: %v", err)
	}
	man, err := pullManifest(context.Background(), store, n.Name())
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if len(man.WALSegs) != 1 {
		t.Fatalf("wal segs=%d want 1", len(man.WALSegs))
	}
}

// switchAckFailPeer wraps a node so SwitchAck fails (P0: donor must not drop).
type switchAckFailPeer struct {
	*Node
}

func (p *switchAckFailPeer) SwitchAck(domain.Peer, domain.SwitchAckPayload) error {
	return fmt.Errorf("injected switch-ack failure")
}

func TestCaughtUpAckBeforeDropKeepsOwnershipOnAckFail(t *testing.T) {
	donor := newNodeOn(t, &memStorage{}, 64)
	recv := newNodeOn(t, &memStorage{}, 64)
	store := NewMemStore()
	donor.SetStore(store)
	recv.SetStore(store)
	donor.SetDelegation(true)
	recv.SetDelegation(true)
	t.Setenv("INDEXUS_ZONE_SNAP_MIN", "1ns")
	t.Setenv("INDEXUS_TRANSFER_THRESHOLD", "1")

	root := encoding.BASE64.Root()
	if err := donor.New(item("keep"), root, "aa"); err != nil {
		t.Fatalf("New: %v", err)
	}
	drain(t, donor)
	keys := donor.listOwnedKeys()
	if err := donor.forceZones(context.Background(), keys); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	zones := make([]domain.ZoneSnapshotRef, 0)
	for _, k := range keys {
		if e, ok := donor.zoneRef(k); ok {
			zones = append(zones, e)
		}
	}
	offer := domain.DelegationOfferPayload{
		Donor: donor.Name(), SessionID: "sess-ack-fail", Zones: zones, WALSeq: 1,
	}
	if err := recv.DelegationOffer(donor, offer); err != nil {
		t.Fatalf("offer: %v", err)
	}
	if err := recv.WALDelta(donor, domain.WALDeltaPayload{SessionID: "sess-ack-fail", Done: true}); err != nil {
		t.Fatalf("delta: %v", err)
	}

	failPeer := &switchAckFailPeer{Node: recv}
	donor.delegMu.Lock()
	donor.delegOut[recv.Name()] = &delegationSession{
		ID: "sess-ack-fail", PeerName: recv.Name(), Peer: failPeer,
		Keys: keys, State: delegCatchingUp, Started: time.Now(),
	}
	donor.delegMu.Unlock()

	err := donor.CaughtUp(failPeer, domain.CaughtUpPayload{SessionID: "sess-ack-fail"})
	if err == nil {
		t.Fatal("CaughtUp should fail when SwitchAck fails")
	}
	if got := len(donor.listOwnedKeys()); got == 0 {
		t.Fatal("P0: donor must keep ownership when SwitchAck fails")
	}
	owned := donor.listOwnedKeys()
	loc := owned[0].Location
	contact, set, gerr := donor.Get(owned[0].Collection, loc, false, 0)
	if gerr != nil || set == nil || set.Count() == 0 {
		t.Fatalf("donor should still serve %s/%s: contact=%v set=%v err=%v owned=%v",
			owned[0].Collection, loc, contact, set, gerr, owned)
	}
	if contact.Name() != donor.Name() {
		t.Fatalf("P1: contact should be self while owned, got %s", contact.Name())
	}
}

func TestGetServesLocalWhileOwnedEvenIfNearerPeer(t *testing.T) {
	donor := newNodeOn(t, &memStorage{}, 64)
	nearer := newNodeOn(t, &memStorage{}, 64)
	root := encoding.BASE64.Root()
	if err := donor.New(item("local"), root, "aa"); err != nil {
		t.Fatalf("New: %v", err)
	}
	drain(t, donor)

	owned := donor.listOwnedKeys()
	if len(owned) == 0 {
		t.Fatal("expected owned keys")
	}
	// Register a peer so Nearest may prefer someone else; ownership stays on donor.
	donor.subscribe([]domain.Contact{nearer})

	contact, set, err := donor.Get(owned[0].Collection, owned[0].Location, true, DefaultDeepHops)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if set == nil || set.Count() == 0 {
		t.Fatalf("expected local set at %q", owned[0].Location)
	}
	if contact == nil || contact.Name() != donor.Name() {
		t.Fatalf("P1: want self as contact while owned, got %v", contact)
	}
}

func TestCaughtUpFlushesCutoverQueueBeforeDrop(t *testing.T) {
	donor := newNodeOn(t, &memStorage{}, 64)
	recv := newNodeOn(t, &memStorage{}, 64)
	store := NewMemStore()
	donor.SetStore(store)
	recv.SetStore(store)
	donor.SetDelegation(true)
	recv.SetDelegation(true)
	t.Setenv("INDEXUS_ZONE_SNAP_MIN", "1ns")
	t.Setenv("INDEXUS_TRANSFER_THRESHOLD", "1")

	root := encoding.BASE64.Root()
	for i := 0; i < 2; i++ {
		if err := donor.New(item("c"+string(rune('a'+i))), root, "aa"); err != nil {
			t.Fatalf("New: %v", err)
		}
	}
	drain(t, donor)
	keys := donor.listOwnedKeys()
	if err := donor.forceZones(context.Background(), keys); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	zones := make([]domain.ZoneSnapshotRef, 0)
	for _, k := range keys {
		if e, ok := donor.zoneRef(k); ok {
			zones = append(zones, e)
		}
	}
	offer := domain.DelegationOfferPayload{
		Donor: donor.Name(), SessionID: "sess-cutover", Zones: zones, WALSeq: 1,
	}
	if err := recv.DelegationOffer(donor, offer); err != nil {
		t.Fatalf("offer: %v", err)
	}
	if err := recv.WALDelta(donor, domain.WALDeltaPayload{SessionID: "sess-cutover", Done: true}); err != nil {
		t.Fatalf("delta: %v", err)
	}

	donor.delegMu.Lock()
	sess := &delegationSession{
		ID: "sess-cutover", PeerName: recv.Name(), Peer: recv,
		Keys: keys, State: delegCatchingUp, Started: time.Now(),
	}
	donor.delegOut[recv.Name()] = sess
	donor.delegMu.Unlock()

	// Simulate a write that lands after Done but before CaughtUp completes:
	// enter cutover and queue, then CaughtUp flushes.
	late := item("late-cutover")
	donor.delegMu.Lock()
	sess.cutover = true
	sess.State = delegReady
	sess.cutoverQ = append(sess.cutoverQ, late.Content())
	donor.delegMu.Unlock()
	// Also apply locally so donor still has it for the flush race window.
	_ = donor.add(late, false)

	beforeRecv, _ := recv.Count()
	if err := donor.CaughtUp(recv, domain.CaughtUpPayload{SessionID: "sess-cutover"}); err != nil {
		t.Fatalf("CaughtUp: %v", err)
	}
	if got := len(donor.listOwnedKeys()); got != 0 {
		t.Fatalf("donor still owns %d after successful cutover", got)
	}
	afterRecv, _ := recv.Count()
	if afterRecv <= beforeRecv {
		t.Fatalf("P2: receiver should gain flushed cutover item: before=%d after=%d", beforeRecv, afterRecv)
	}
	if recv.pendingInbound() != 0 {
		t.Fatal("inbound should be closed after SwitchAck inside CaughtUp")
	}
}
