package core

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/indexus/go-indexus-core/domain"
)

const delegationTransferThreshold = 500

type delegationState int

const (
	delegMirroring delegationState = iota
	delegCatchingUp
	delegReady
	delegSwitched
	delegCancelled
)

type delegationSession struct {
	ID       string
	PeerName string
	Peer     domain.Contact
	Keys     []domain.Key
	Zones    []domain.ZoneSnapshotRef
	WALSeq   int64
	State    delegationState
	Started  time.Time
	inbound  bool
}

func envDelegation() bool {
	v := os.Getenv("INDEXUS_DELEGATION_S3")
	return v == "1" || strings.EqualFold(v, "true")
}

func (n *Node) Delegation() bool {
	return n != nil && (n.delegEnabled || envDelegation())
}

func (n *Node) SetDelegation(v bool) {
	if n != nil {
		n.delegEnabled = v
	}
}

func transferThreshold() int {
	if raw := os.Getenv("INDEXUS_TRANSFER_THRESHOLD"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return n
		}
	}
	return delegationTransferThreshold
}

func sessionTimeout() time.Duration {
	if raw := os.Getenv("INDEXUS_DELEGATION_TIMEOUT"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return 2 * time.Minute
}

func (n *Node) offerZones(peer domain.Contact, keys []domain.Key) int {
	if !n.Delegation() || n.Store() == nil {
		return n.transferToPeer(peer, keys)
	}

	small := make([]domain.Key, 0)
	large := make([]domain.Key, 0)
	thresh := transferThreshold()
	for _, key := range keys {
		_, count, err := n.zoneLines(key)
		if err != nil || count < thresh {
			small = append(small, key)
			continue
		}
		large = append(large, key)
	}

	handed := 0
	if len(small) > 0 {
		handed += n.transferToPeer(peer, small)
	}
	if len(large) == 0 {
		return handed
	}

	ctx := context.Background()
	if err := n.forceZones(ctx, large); err != nil {
		slog.Warn("force zone checkpoint before offer failed, falling back to Transfer", "err", err)
		return handed + n.transferToPeer(peer, large)
	}

	zones := make([]domain.ZoneSnapshotRef, 0, len(large))
	fallback := make([]domain.Key, 0)
	for _, key := range large {
		if ref, ok := n.zoneRef(key); ok {
			zones = append(zones, ref)
		} else {
			fallback = append(fallback, key)
		}
	}
	if len(fallback) > 0 {
		handed += n.transferToPeer(peer, fallback)
	}
	if len(zones) == 0 {
		return handed
	}

	sessionID := fmt.Sprintf("%s-%d", n.Name(), time.Now().UnixNano())
	offer := domain.DelegationOfferPayload{
		Donor:     n.Name(),
		SessionID: sessionID,
		Zones:     zones,
		WALSeq:    int64(n.walLineCount()),
	}

	offerer, ok := peer.(interface {
		DelegationOffer(domain.Peer, domain.DelegationOfferPayload) error
	})
	if !ok {
		return handed + n.transferToPeer(peer, keysFromZones(zones))
	}
	if err := offerer.DelegationOffer(n, offer); err != nil {
		slog.Warn("delegation offer failed, falling back to Transfer", "peer", peer.Name(), "err", err)
		return handed + n.transferToPeer(peer, keysFromZones(zones))
	}

	keysOffered := keysFromZones(zones)
	sess := &delegationSession{
		ID:       sessionID,
		PeerName: peer.Name(),
		Peer:     peer,
		Keys:     keysOffered,
		Zones:    zones,
		WALSeq:   offer.WALSeq,
		State:    delegMirroring,
		Started:  time.Now(),
	}
	n.delegMu.Lock()
	n.delegOut[peer.Name()] = sess
	n.delegMu.Unlock()

	go n.runOutboundDelta(sess)
	go n.watchOutboundTimeout(sess)
	return handed
}

func keysFromZones(zones []domain.ZoneSnapshotRef) []domain.Key {
	out := make([]domain.Key, 0, len(zones))
	for _, z := range zones {
		out = append(out, domain.Key{Collection: z.Collection, Location: z.Location})
	}
	return out
}

func (n *Node) runOutboundDelta(sess *delegationSession) {
	lines := n.collectWALDelta(sess.Keys, int(sess.WALSeq))
	delta := domain.WALDeltaPayload{SessionID: sess.ID, Lines: lines, Done: true}
	sender, ok := sess.Peer.(interface {
		WALDelta(domain.Peer, domain.WALDeltaPayload) error
	})
	if !ok {
		n.cancelOutbound(sess, "peer lacks WALDelta")
		return
	}
	if err := sender.WALDelta(n, delta); err != nil {
		n.cancelOutbound(sess, err.Error())
		return
	}
	n.delegMu.Lock()
	if s := n.delegOut[sess.PeerName]; s != nil && s.ID == sess.ID {
		s.State = delegCatchingUp
	}
	n.delegMu.Unlock()
}

func (n *Node) watchOutboundTimeout(sess *delegationSession) {
	time.Sleep(sessionTimeout())
	n.delegMu.Lock()
	s := n.delegOut[sess.PeerName]
	if s == nil || s.ID != sess.ID || s.State == delegSwitched || s.State == delegCancelled {
		n.delegMu.Unlock()
		return
	}
	n.delegMu.Unlock()
	n.cancelOutbound(sess, "timeout waiting for CaughtUp")
}

func (n *Node) cancelOutbound(sess *delegationSession, reason string) {
	n.delegMu.Lock()
	if s := n.delegOut[sess.PeerName]; s != nil && s.ID == sess.ID {
		s.State = delegCancelled
		delete(n.delegOut, sess.PeerName)
	}
	n.delegMu.Unlock()
	slog.Info("delegation cancelled", "peer", sess.PeerName, "session", sess.ID, "reason", reason)
}

func (n *Node) collectWALDelta(keys []domain.Key, startLine int) []string {
	keySet := make(map[domain.Key]struct{}, len(keys))
	for _, k := range keys {
		keySet[k] = struct{}{}
	}
	matches := func(collection, location string) bool {
		for k := range keySet {
			if k.Collection != collection {
				continue
			}
			if k.Location == location || strings.HasPrefix(location, k.Location) ||
				(k.Location != "" && strings.HasPrefix(k.Location, location)) {
				return true
			}
			if k.Location == "" || k.Location == "@" {
				return true
			}
		}
		return false
	}

	out := make([]string, 0)
	for log := range n.storage.Stream(startLine) {
		col, loc := walLineKey(log)
		if col == "" {
			continue
		}
		if matches(col, loc) {
			out = append(out, log)
		}
	}
	return out
}

func (n *Node) walLineCount() int {
	count := 0
	for range n.storage.Stream(0) {
		count++
	}
	return count
}

func walLineKey(line string) (collection, location string) {
	switch {
	case strings.HasPrefix(line, "ingress|"):
		arr := strings.SplitN(line, "|", 4)
		if len(arr) < 4 {
			return "", ""
		}
		item, ok := domain.ParseContent(arr[3])
		if !ok {
			return "", ""
		}
		return item.Collection, item.Location
	case strings.HasPrefix(line, "delete|"):
		arr := strings.SplitN(line, "|", 4)
		if len(arr) < 4 {
			return "", ""
		}
		item, ok := domain.ParseContent(arr[3])
		if !ok {
			return "", ""
		}
		return item.Collection, item.Location
	case strings.HasPrefix(line, "tombstone|"):
		item, ok := domain.ParseContent(line)
		if !ok {
			return "", ""
		}
		return item.Collection, item.Location
	default:
		item, ok := domain.ParseContent(line)
		if !ok {
			return "", ""
		}
		return item.Collection, item.Location
	}
}

func (n *Node) mirrorWrite(item *domain.Item) {
	if n == nil || item == nil || !n.Delegation() {
		return
	}
	n.delegMu.Lock()
	sessions := make([]*delegationSession, 0, len(n.delegOut))
	for _, s := range n.delegOut {
		if s.State == delegMirroring || s.State == delegCatchingUp || s.State == delegReady {
			sessions = append(sessions, s)
		}
	}
	n.delegMu.Unlock()

	line := item.Content()
	for _, sess := range sessions {
		if !sessionCovers(sess, item.Collection, item.Location) {
			continue
		}
		sender, ok := sess.Peer.(interface {
			WALDelta(domain.Peer, domain.WALDeltaPayload) error
		})
		if !ok {
			continue
		}
		if err := sender.WALDelta(n, domain.WALDeltaPayload{SessionID: sess.ID, Lines: []string{line}}); err != nil {
			n.cancelOutbound(sess, err.Error())
		}
	}
}

func sessionCovers(sess *delegationSession, collection, location string) bool {
	for _, k := range sess.Keys {
		if k.Collection != collection {
			continue
		}
		if k.Location == location || k.Location == "@" || k.Location == "" ||
			strings.HasPrefix(location, k.Location) {
			return true
		}
	}
	return false
}

func (n *Node) findRegisteredByName(name string) domain.Contact {
	for _, c := range n.traverseRegistered(false) {
		if c.Name() == name {
			return c
		}
	}
	return nil
}

func (n *Node) DelegationOffer(origin domain.Peer, offer domain.DelegationOfferPayload) error {
	if n.leaving.Load() {
		return domain.ErrLeaving
	}
	store := n.Store()
	if store == nil {
		return fmt.Errorf("no object store to pull zone snapshots")
	}
	ctx := context.Background()
	keys := make([]domain.Key, 0, len(offer.Zones))
	for _, z := range offer.Zones {
		if err := n.applyZone(ctx, store, z); err != nil {
			return fmt.Errorf("pull zone %s/%s: %w", z.Collection, z.Location, err)
		}
		keys = append(keys, domain.Key{Collection: z.Collection, Location: z.Location})
	}
	sess := &delegationSession{
		ID:       offer.SessionID,
		PeerName: offer.Donor,
		Keys:     keys,
		Zones:    offer.Zones,
		WALSeq:   offer.WALSeq,
		State:    delegCatchingUp,
		Started:  time.Now(),
		inbound:  true,
	}
	n.delegMu.Lock()
	n.delegIn[offer.Donor] = sess
	n.delegMu.Unlock()
	n.tryPublishClientReady()
	return nil
}

func (n *Node) WALDelta(origin domain.Peer, delta domain.WALDeltaPayload) error {
	n.delegMu.Lock()
	var sess *delegationSession
	for _, s := range n.delegIn {
		if s.ID == delta.SessionID {
			sess = s
			break
		}
	}
	n.delegMu.Unlock()
	if sess == nil {
		return fmt.Errorf("unknown delegation session %s", delta.SessionID)
	}
	for _, line := range delta.Lines {
		n.replayLogLine(line)
	}
	if delta.Done {
		n.delegMu.Lock()
		sess.State = delegReady
		n.delegMu.Unlock()
		go n.sendCaughtUp(sess)
	}
	return nil
}

func (n *Node) sendCaughtUp(sess *delegationSession) {
	donor := n.findRegisteredByName(sess.PeerName)
	if donor == nil {
		return
	}
	sender, ok := donor.(interface {
		CaughtUp(domain.Peer, domain.CaughtUpPayload) error
	})
	if !ok {
		return
	}
	_ = sender.CaughtUp(n, domain.CaughtUpPayload{SessionID: sess.ID, WALSeq: sess.WALSeq})
}

func (n *Node) CaughtUp(origin domain.Peer, payload domain.CaughtUpPayload) error {
	n.delegMu.Lock()
	sess := n.delegOut[origin.Name()]
	if sess == nil || sess.ID != payload.SessionID {
		n.delegMu.Unlock()
		return fmt.Errorf("no outbound session %s for %s", payload.SessionID, origin.Name())
	}
	if sess.State == delegSwitched || sess.State == delegCancelled {
		n.delegMu.Unlock()
		return nil
	}
	keys := append([]domain.Key(nil), sess.Keys...)
	sess.State = delegReady
	n.delegMu.Unlock()

	n.rebalancing.Store(true)
	defer n.rebalancing.Store(false)

	for _, key := range keys {
		if collection, ok := n.collections.Get(key.Collection); ok {
			_, empty := collection.Delegate(key.Location)
			n.removeOwnedKey(key)
			if empty {
				n.collections.Delete(key.Collection)
			}
			forget := key.Location
			if forget == collection.Base().Root() {
				forget = ""
			}
			n.cache.ForgetUnder(key.Collection, forget)
		} else {
			n.removeOwnedKey(key)
		}
	}

	ack := domain.SwitchAckPayload{SessionID: payload.SessionID, Keys: keys}
	if acker, ok := origin.(interface {
		SwitchAck(domain.Peer, domain.SwitchAckPayload) error
	}); ok {
		if err := acker.SwitchAck(n, ack); err != nil {
			slog.Warn("switch ack RPC failed (ownership already dropped)", "err", err)
		}
	}

	n.delegMu.Lock()
	if s := n.delegOut[origin.Name()]; s != nil && s.ID == payload.SessionID {
		s.State = delegSwitched
		delete(n.delegOut, origin.Name())
	}
	n.delegMu.Unlock()

	if n.autoscale != nil {
		n.autoscale.MarkReliefArrived()
	}
	return nil
}

func (n *Node) SwitchAck(origin domain.Peer, payload domain.SwitchAckPayload) error {
	n.delegMu.Lock()
	sess := n.delegIn[origin.Name()]
	if sess != nil && sess.ID == payload.SessionID {
		sess.State = delegSwitched
		delete(n.delegIn, origin.Name())
	}
	n.delegMu.Unlock()
	for _, key := range payload.Keys {
		n.create(key.Collection, key.Location)
	}
	n.tryPublishClientReady()
	return nil
}

func (n *Node) pendingInbound() int {
	n.delegMu.Lock()
	defer n.delegMu.Unlock()
	count := 0
	for _, s := range n.delegIn {
		if s.State != delegSwitched && s.State != delegCancelled {
			count++
		}
	}
	return count
}
