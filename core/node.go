package core

import (
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/indexus/go-indexus-core/auth"
	"github.com/indexus/go-indexus-core/domain"
)

type Node struct {
	settings     *Settings
	newContact   func(string, map[string]any, int) domain.Contact
	bootstraps   []domain.Contact
	routing      *domain.BST[domain.Peer]
	registered   *domain.BST[domain.Contact]
	acknowledged *domain.BST[domain.Contact]
	collections  *domain.Collections
	owned        *domain.BST[map[domain.Key]any]
	cache        *domain.Cache
	queue        *domain.Queue[*Element]
	storage      domain.Storage
	ready        bool
	cert         *auth.NodeCert
	autoscale    *AutoscaleController
	leaving      atomic.Bool

	transferMu sync.Mutex

	refreshBusy  atomic.Bool
	checkpointMu sync.Mutex
	fwdMu        sync.Mutex
	fwdTokens    float64
	fwdLast      time.Time

	peerBusyMu sync.Mutex
	peerBusy   map[string]time.Time

	suspectMu sync.Mutex
	suspect   map[string]time.Time

	stallMu       sync.Mutex
	stalls        map[string]int64
	requeues      int64
	stallLoggedAt time.Time

	full atomic.Bool

	rebalancing atomic.Bool

	clientPublished atomic.Bool

	joinFirstOwn atomic.Int64

	items       atomic.Int64
	lastCountAt atomic.Int64

	storeMu     sync.Mutex
	objectStore Store
	zoneSnap    *zoneSnapState

	delegMu      sync.Mutex
	delegOut     map[string]*delegationSession
	delegIn      map[string]*delegationSession
	delegEnabled bool
}

func NewNode(settings *Settings, newContact func(string, map[string]any, int) domain.Contact, bootstraps []domain.Contact, storage domain.Storage) (*Node, error) {
	node := &Node{
		settings:     settings,
		newContact:   newContact,
		bootstraps:   bootstraps,
		routing:      domain.NewBST[domain.Peer](),
		registered:   domain.NewBST[domain.Contact](),
		acknowledged: domain.NewBST[domain.Contact](),
		collections:  domain.NewCollections(),
		owned:        domain.NewBST[map[domain.Key]any](),
		cache:        domain.NewCache(),
		queue:        domain.NewQueue[*Element](),
		storage:      storage,
		fwdLast:      time.Now(),
		fwdTokens:    float64(settings.forwardRate),
		peerBusy:     make(map[string]time.Time),
		suspect:      make(map[string]time.Time),
		stalls:       make(map[string]int64),
		zoneSnap:     newZoneSnapState(),
		delegOut:     make(map[string]*delegationSession),
		delegIn:      make(map[string]*delegationSession),
		delegEnabled: os.Getenv("INDEXUS_DELEGATION_S3") == "1" || os.Getenv("INDEXUS_DELEGATION_S3") == "true",
	}

	node.register([]domain.Contact{node})
	node.acknowledge(bootstraps)

	if err := node.Restore(); err != nil {

		slog.Error("cannot restore from backup, archiving it and starting empty", "err", err)

		if err := node.storage.Reset(); err != nil {
			return nil, fmt.Errorf("reset storage: %w", err)
		}
	}

	node.ready = true

	return node, nil
}

func (n *Node) ID() []byte {
	return n.settings.id
}

func (n *Node) Name() string {
	return n.settings.name
}

func (n *Node) IPs() map[string]any {
	return n.settings.ips
}

func (n *Node) Port() int {
	return n.settings.port
}

func (n *Node) IP() string {
	return n.settings.ip
}

func (n *Node) Host() string {
	return fmt.Sprintf("%s@%s|%d", n.settings.name, n.settings.ip, n.settings.port)
}

func (n *Node) Cert() *auth.NodeCert {
	return n.cert
}

func (n *Node) SetCert(cert *auth.NodeCert) {
	n.cert = cert
}

func (n *Node) Delay() time.Duration {

	if n.autoscale != nil && n.autoscale.cfg.Role == "spawned" && !n.ClientReady() {
		return 2 * time.Second
	}
	return n.settings.delay
}
