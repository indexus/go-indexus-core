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

// DefaultTransferThreshold is the item count at/above which a zone uses
// snapshot delegation instead of classic item Transfer. Override with
// INDEXUS_TRANSFER_THRESHOLD.
const DefaultTransferThreshold = 200

// DefaultDelegationTimeout is the max lifetime of a snapshot-delegation
// session. Override with INDEXUS_DELEGATION_TIMEOUT (Go duration).
const DefaultDelegationTimeout = 2 * time.Minute

// DefaultTransferTimeout is the HTTP client timeout for classic /transfer.
// Override with INDEXUS_TRANSFER_TIMEOUT (Go duration).
const DefaultTransferTimeout = 5 * time.Minute

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
	// cutover freezes live mirror: writes covering Keys are queued and flushed
	// before SwitchAck so the receiver is fully caught up before ownership drops.
	cutover  bool
	cutoverQ []string
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
	return DefaultTransferThreshold
}

// TransferThreshold is the configured snap-vs-Transfer cutoff (env or default).
func TransferThreshold() int {
	return transferThreshold()
}

func sessionTimeout() time.Duration {
	if raw := os.Getenv("INDEXUS_DELEGATION_TIMEOUT"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return DefaultDelegationTimeout
}

// DelegationTimeout is the configured snapshot-delegation session timeout.
func DelegationTimeout() time.Duration {
	return sessionTimeout()
}

func classicTransferTimeout() time.Duration {
	if raw := os.Getenv("INDEXUS_TRANSFER_TIMEOUT"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return DefaultTransferTimeout
}

// ClassicTransferTimeout is the configured HTTP timeout for item Transfer.
func ClassicTransferTimeout() time.Duration {
	return classicTransferTimeout()
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

	n.delegMu.Lock()
	if existing := n.delegOut[peer.Name()]; existing != nil &&
		existing.State != delegSwitched && existing.State != delegCancelled {
		n.delegMu.Unlock()
		slog.Info("skipping duplicate outbound offer; session already open",
			"peer", peer.Name(), "session", existing.ID, "state", int(existing.State))
		return handed
	}
	n.delegMu.Unlock()

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
	// Snapshot cutover targets under the same lock so enqueue is atomic.
	type cutoverTarget struct {
		sess *delegationSession
	}
	var cutovers []cutoverTarget
	for _, s := range sessions {
		if s.cutover && sessionCovers(s, item.Collection, item.Location) {
			cutovers = append(cutovers, cutoverTarget{sess: s})
		}
	}
	line := item.Content()
	for _, c := range cutovers {
		c.sess.cutoverQ = append(c.sess.cutoverQ, line)
	}
	n.delegMu.Unlock()

	for _, sess := range sessions {
		if sess.cutover {
			continue // already queued
		}
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
		Peer:     n.findRegisteredByName(offer.Donor), // may be nil; sendCaughtUp also tries bootstraps
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
	go n.watchInboundTimeout(sess)
	n.MeasureItems()
	n.tryPublishClientReady()
	return nil
}

func (n *Node) watchInboundTimeout(sess *delegationSession) {
	time.Sleep(sessionTimeout())
	n.delegMu.Lock()
	s := n.delegIn[sess.PeerName]
	if s == nil || s.ID != sess.ID || s.State == delegSwitched || s.State == delegCancelled {
		n.delegMu.Unlock()
		return
	}
	s.State = delegCancelled
	delete(n.delegIn, sess.PeerName)
	n.delegMu.Unlock()
	slog.Warn("inbound delegation timed out (clearing stuck TRANSFER)",
		"donor", sess.PeerName, "session", sess.ID, "zones", len(sess.Zones))
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
	tryOnce := func() bool {
		donor := n.findRegisteredByName(sess.PeerName)
		if donor == nil && sess.Peer != nil {
			donor = sess.Peer
		}
		if donor == nil {
			for _, b := range n.bootstraps {
				if b != nil && b.Name() == sess.PeerName {
					donor = b
					break
				}
			}
		}
		if donor == nil {
			return false
		}
		sender, ok := donor.(interface {
			CaughtUp(domain.Peer, domain.CaughtUpPayload) error
		})
		if !ok {
			return false
		}
		if err := sender.CaughtUp(n, domain.CaughtUpPayload{SessionID: sess.ID, WALSeq: sess.WALSeq}); err != nil {
			slog.Warn("CaughtUp failed", "donor", sess.PeerName, "err", err)
			return false
		}
		return true
	}
	if tryOnce() {
		return
	}
	slog.Warn("CaughtUp deferred: donor not registered", "donor", sess.PeerName, "session", sess.ID)
	deadline := time.Now().Add(sessionTimeout())
	interval := caughtUpRetryInterval()
	go func() {
		for time.Now().Before(deadline) {
			time.Sleep(interval)
			n.delegMu.Lock()
			s := n.delegIn[sess.PeerName]
			alive := s != nil && s.ID == sess.ID && s.State != delegSwitched && s.State != delegCancelled
			n.delegMu.Unlock()
			if !alive {
				return
			}
			if tryOnce() {
				return
			}
		}
		slog.Warn("CaughtUp retries exhausted", "donor", sess.PeerName, "session", sess.ID)
	}()
}

func caughtUpRetryInterval() time.Duration {
	if raw := os.Getenv("INDEXUS_CAUGHTUP_RETRY"); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return 2 * time.Second
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
	// Fence: new covering writes queue briefly; we flush once then flip.
	sess.cutover = true
	peer := sess.Peer
	n.delegMu.Unlock()

	// HTTP origin is a name-only Peer — RPC must go through the Contact stored
	// at offer time (sess.Peer), else SwitchAck/WALDelta type-assert fails → 503 loop.
	if peer == nil || peer.Name() != origin.Name() {
		peer = n.findRegisteredByName(origin.Name())
	}
	if peer == nil {
		n.delegMu.Lock()
		if s := n.delegOut[origin.Name()]; s != nil && s.ID == payload.SessionID {
			s.cutover = false
			s.State = delegCatchingUp
		}
		n.delegMu.Unlock()
		return fmt.Errorf("no dialable contact for receiver %s", origin.Name())
	}

	n.rebalancing.Store(true)
	defer func() {
		if n.pendingOutbound() == 0 {
			n.rebalancing.Store(false)
		} else {
			// Keep Feed moving during any remaining S3 mirrors.
			n.rebalancing.Store(false)
		}
	}()

	// Fast cutover: one flush of whatever is queued, then ACK+drop. Residual
	// ingress after Delegate forwards via XOR to the new owner — do not wait
	// for a quiet period (that never comes under load).
	if err := n.flushCutoverQueue(sess, peer); err != nil {
		slog.Warn("cutover flush failed, keeping ownership", "peer", peer.Name(), "session", sess.ID, "err", err)
		n.delegMu.Lock()
		if s := n.delegOut[origin.Name()]; s != nil && s.ID == payload.SessionID {
			s.cutover = false
			s.State = delegCatchingUp
		}
		n.delegMu.Unlock()
		return fmt.Errorf("cutover flush: %w", err)
	}

	ack := domain.SwitchAckPayload{SessionID: payload.SessionID, Keys: keys}
	acker, ok := peer.(interface {
		SwitchAck(domain.Peer, domain.SwitchAckPayload) error
	})
	if !ok {
		slog.Warn("peer lacks SwitchAck, keeping ownership", "peer", peer.Name())
		n.delegMu.Lock()
		if s := n.delegOut[origin.Name()]; s != nil && s.ID == payload.SessionID {
			s.cutover = false
			s.State = delegCatchingUp
		}
		n.delegMu.Unlock()
		return fmt.Errorf("peer %s lacks SwitchAck", peer.Name())
	}
	if err := acker.SwitchAck(n, ack); err != nil {
		slog.Warn("switch ack failed, keeping ownership (no drop)", "peer", peer.Name(), "err", err)
		n.delegMu.Lock()
		if s := n.delegOut[origin.Name()]; s != nil && s.ID == payload.SessionID {
			s.cutover = false
			s.State = delegCatchingUp
		}
		n.delegMu.Unlock()
		return fmt.Errorf("switch ack: %w", err)
	}

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

	n.delegMu.Lock()
	if s := n.delegOut[origin.Name()]; s != nil && s.ID == payload.SessionID {
		s.State = delegSwitched
		s.cutover = false
		s.cutoverQ = nil
		delete(n.delegOut, origin.Name())
	}
	n.delegMu.Unlock()

	n.MeasureItems()
	n.syncParentsAfterHandoff(keys, peer)

	if n.autoscale != nil {
		n.autoscale.MarkReliefArrived()
	}
	slog.Info("delegation switched (ack-before-drop)",
		"peer", peer.Name(), "session", payload.SessionID, "keys", len(keys))
	return nil
}

// flushCutoverQueue sends a single snapshot of queued write lines (Done=false).
// We deliberately do not wait for a quiet period — cut fast; later writes forward.
func (n *Node) flushCutoverQueue(sess *delegationSession, peer domain.Contact) error {
	sender, ok := peer.(interface {
		WALDelta(domain.Peer, domain.WALDeltaPayload) error
	})
	if !ok {
		return nil
	}
	n.delegMu.Lock()
	lines := sess.cutoverQ
	sess.cutoverQ = nil
	n.delegMu.Unlock()
	if len(lines) == 0 {
		return nil
	}
	if err := sender.WALDelta(n, domain.WALDeltaPayload{
		SessionID: sess.ID,
		Lines:     lines,
		Done:      false,
	}); err != nil {
		n.delegMu.Lock()
		sess.cutoverQ = append(lines, sess.cutoverQ...)
		n.delegMu.Unlock()
		return err
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
	n.MeasureItems()
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

func (n *Node) pendingOutbound() int {
	n.delegMu.Lock()
	defer n.delegMu.Unlock()
	count := 0
	for _, s := range n.delegOut {
		if s.State != delegSwitched && s.State != delegCancelled {
			count++
		}
	}
	return count
}

func (n *Node) pendingInboundKeySet() map[domain.Key]struct{} {
	return n.pendingKeySet(n.delegIn)
}

func (n *Node) pendingOutboundKeySet() map[domain.Key]struct{} {
	return n.pendingKeySet(n.delegOut)
}

func (n *Node) pendingKeySet(sessions map[string]*delegationSession) map[domain.Key]struct{} {
	n.delegMu.Lock()
	defer n.delegMu.Unlock()
	out := make(map[domain.Key]struct{})
	for _, s := range sessions {
		if s.State == delegSwitched || s.State == delegCancelled {
			continue
		}
		for _, k := range s.Keys {
			out[k] = struct{}{}
		}
	}
	return out
}

func keyInPendingSet(pending map[domain.Key]struct{}, key domain.Key) bool {
	if len(pending) == 0 {
		return false
	}
	if _, ok := pending[key]; ok {
		return true
	}
	for pk := range pending {
		if pk.Collection != key.Collection {
			continue
		}
		if key.Location == pk.Location {
			return true
		}
		if pk.Location != "" && strings.HasPrefix(key.Location, pk.Location) {
			return true
		}
		if key.Location != "" && strings.HasPrefix(pk.Location, key.Location) {
			return true
		}
	}
	return false
}

// keyInPendingInbound is kept as a thin alias for call sites that name inbound.
func keyInPendingInbound(pending map[domain.Key]struct{}, key domain.Key) bool {
	return keyInPendingSet(pending, key)
}
