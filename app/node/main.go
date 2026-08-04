package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/indexus/go-indexus-core/auth"
	"github.com/indexus/go-indexus-core/core"
	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
	"github.com/indexus/go-indexus-core/http/monitoring"
	"github.com/indexus/go-indexus-core/http/p2p"
	"github.com/indexus/go-indexus-core/logging"
	"github.com/indexus/go-indexus-core/peer"
	"github.com/indexus/go-indexus-core/storage"
	"github.com/indexus/go-indexus-core/worker"
)

const asciiArt = `

██╗███╗   ██╗██████╗ ███████╗██╗  ██╗██╗   ██╗███████╗
██║████╗  ██║██╔══██╗██╔════╝╚██╗██╔╝██║   ██║██╔════╝
██║██╔██╗ ██║██║  ██║█████╗   ╚███╔╝ ██║   ██║███████╗
██║██║╚██╗██║██║  ██║██╔══╝   ██╔██╗ ██║   ██║╚════██║
██║██║ ╚████║██████╔╝███████╗██╔╝ ██╗╚██████╔╝███████║
╚═╝╚═╝  ╚═══╝╚═════╝ ╚══════╝╚═╝  ╚═╝ ╚═════╝ ╚══════╝

`

const appName = "Indexus"

var (
	version = "dev"
	commit  = "none"
)

const (
	jobInterval     = 10 * time.Second
	cacheExpiration = 5 * time.Minute

	shutdownTimeout = 15 * time.Second
)

type Config struct {
	Name           string
	Bootstraps     []domain.Contact
	MonitoringPort int
	P2pPort        int
	Storage        string
	Archive        string
	TLSDir         string
	Advertise      []string
	LeaveTimeout   time.Duration

	IssuerURL     string
	NetworkID     string
	NetworkPubKey string
	NodeKeyPath   string
	CertPath      string
	RequireAuth   bool

	Delegation int
	Autoscale  core.AutoscaleConfig
}

func main() {
	logging.Setup("node")
	core.CapHeapToHost()

	config, err := parseFlags()
	if err != nil {
		slog.Error("invalid configuration", "err", err)
		os.Exit(2)
	}

	banner(config)

	if err := run(config); err != nil {
		slog.Error("node stopped on error", "err", err)
		os.Exit(1)
	}

	slog.Info("node stopped")
}

func parseFlags() (Config, error) {
	name, err := encoding.BASE64.RandomName()
	if err != nil {
		return Config{}, fmt.Errorf("generate node name: %w", err)
	}

	var config Config
	flag.StringVar(&config.Name, "name", name, "Node name, a base64 identifier")
	flag.IntVar(&config.MonitoringPort, "monitoringPort", 19000, "Port of the monitoring service")
	flag.IntVar(&config.P2pPort, "p2pPort", 21000, "Port of the peer-to-peer service")
	flag.StringVar(&config.Storage, "storage", ".data/backup", "Snapshot and write-ahead log path, empty to stay in memory")
	flag.StringVar(&config.Archive, "archive", ".data/archive", "Directory holding rotated logs")
	flag.StringVar(&config.TLSDir, "sslStorage", "", "Directory holding server.crt and server.key")
	flag.DurationVar(&config.LeaveTimeout, "leaveTimeout", 90*time.Second, "Budget for handing zones over on shutdown")
	flag.StringVar(&config.IssuerURL, "issuer", "", "Issuer base URL, for example http://127.0.0.1:22000")
	flag.StringVar(&config.NetworkID, "network", "indexus-aws", "Network id")
	flag.StringVar(&config.NetworkPubKey, "networkPub", "", "Network issuer public key, base64; fetched from the issuer if empty")
	flag.StringVar(&config.NodeKeyPath, "nodeKey", "", "Path to the node ed25519 private key")
	flag.StringVar(&config.CertPath, "cert", "", "Path to the cached node certificate")
	flag.BoolVar(&config.RequireAuth, "requireAuth", false, "Require node certificates and client tokens")
	flag.IntVar(&config.Delegation, "delegation", domain.DelegationTreshold(), "Items a zone holds before it is owned")

	bootstrap := flag.String("bootstrap", "", "Bootstrap peers, as ip|port,ip|port")
	advertise := flag.String("advertise", "", "Comma-separated IPs to advertise, defaults to 127.0.0.1")
	showVersion := flag.Bool("version", false, "Print the version and exit")

	autoscale := &config.Autoscale
	flag.BoolVar(&autoscale.Enabled, "autoscale", false, "Let this node request scale-ups and scale-downs")
	flag.StringVar(&autoscale.Role, "autoscaleRole", envOr("INDEXUS_ROLE", "bootstrap"), "bootstrap or spawned")
	flag.DurationVar(&autoscale.Window, "scaleWindow", 2*time.Minute, "Sliding window used to count inserts")
	flag.Int64Var(&autoscale.DownThreshold, "scaleDownThreshold", 200, "Scale down when inserts in the window stay below this")
	flag.DurationVar(&autoscale.DownHold, "scaleDownHold", 8*time.Minute, "How long the scale-down condition must hold")
	flag.DurationVar(&autoscale.Cooldown, "scaleCooldown", 3*time.Minute, "Quiet period after a scale-up")
	flag.IntVar(&autoscale.QueueAbsThreshold, "queuePressure", 0, "Pending work that blocks scale-down (0 keeps the default; not a scale-up trigger)")
	flag.DurationVar(&autoscale.PressureHold, "pressureHold", 0, "How long a resource signal must last before asking for a node (0 keeps the default)")

	flag.Parse()

	if *showVersion {
		fmt.Printf("%s %s\n", appName, versionString())
		os.Exit(0)
	}

	explicitName := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "name" {
			explicitName = true
		}
	})

	config.Bootstraps, err = parseBootstraps(*bootstrap)
	if err != nil {
		return Config{}, err
	}

	config.Advertise = []string{"127.0.0.1"}
	if *advertise != "" {
		config.Advertise = strings.Split(*advertise, ",")
	}

	config.IssuerURL = strings.TrimRight(config.IssuerURL, "/")
	config.Autoscale.IssuerURL = config.IssuerURL

	if config.Delegation <= 0 {
		return Config{}, fmt.Errorf("delegation must be positive, got %d", config.Delegation)
	}
	if config.RequireAuth && config.NetworkPubKey == "" && config.IssuerURL == "" {
		return Config{}, errors.New("requireAuth needs -networkPub or -issuer")
	}

	config.Name = stickyNodeName(config)

	if !explicitName && os.Getenv("INDEXUS_PREFER_NEAR") != "" {
		if cert, ok := readCert(config.CertPath); !ok || cert.NodeID == "" {
			if near, err := nameNearEnv(); err == nil && near != "" {
				config.Name = near
			}
		}
	}

	return config, nil
}

func nameNearEnv() (string, error) {
	raw := os.Getenv("INDEXUS_PREFER_NEAR")
	if raw == "" {
		return "", nil
	}
	target, err := encoding.BASE64.Decode(raw)
	if err != nil {
		return "", err
	}
	return encoding.BASE64.RandomNameNear(target, 20)
}

func parseBootstraps(raw string) ([]domain.Contact, error) {
	contacts := make([]domain.Contact, 0)
	if raw == "" {
		return contacts, nil
	}

	for _, entry := range strings.Split(raw, ",") {
		ip, rawPort, found := strings.Cut(entry, "|")
		if !found {
			return nil, fmt.Errorf("bootstrap %q: expected ip|port", entry)
		}
		port, err := strconv.Atoi(rawPort)
		if err != nil {
			return nil, fmt.Errorf("bootstrap %q: %w", entry, err)
		}
		name, err := encoding.BASE64.RandomName()
		if err != nil {
			return nil, err
		}
		contacts = append(contacts, peer.NewContact(name, map[string]any{ip: nil}, port))
	}

	return contacts, nil
}

func banner(config Config) {
	fmt.Print(asciiArt)
	fmt.Printf("%s %s\n\n", appName, versionString())

	slog.Info("starting node",
		"name", config.Name,
		"monitoring_port", config.MonitoringPort,
		"p2p_port", config.P2pPort,
		"bootstraps", strings.Join(hostsOf(config.Bootstraps), ","),
		"advertise", strings.Join(config.Advertise, ","),
		"storage", config.Storage,
		"delegation", config.Delegation,
		"issuer", config.IssuerURL,
		"require_auth", config.RequireAuth,
	)

	if config.Autoscale.Enabled {
		slog.Info("autoscale enabled",
			"role", config.Autoscale.Role,
			"queue_pressure", config.Autoscale.QueueAbsThreshold,
			"pressure_hold", config.Autoscale.PressureHold,
			"cooldown", config.Autoscale.Cooldown,
			"down_threshold", config.Autoscale.DownThreshold,
			"window", config.Autoscale.Window,
			"down_hold", config.Autoscale.DownHold,
		)
	}
}

func run(config Config) error {
	settings, err := core.NewSettings(config.Name, config.P2pPort, jobInterval, cacheExpiration, config.Delegation)
	if err != nil {
		return fmt.Errorf("settings: %w", err)
	}
	settings.SetAdvertise(config.Advertise...)

	var wal *storage.Storage
	var store domain.Storage = storage.NewMemory()
	if config.Storage != "" {

		if from := os.Getenv("INDEXUS_RESTORE_FROM"); from != "" {
			if err := core.PullLatestSnapshot(from, config.Storage); err != nil {
				slog.Warn("could not pull peer snapshot, starting from local storage", "from", from, "err", err)
			}
		}
		wal, err = storage.NewStorage(config.Archive, config.Storage)
		if err != nil {
			return err
		}

		if raw := os.Getenv("INDEXUS_DURABLE_WINDOW"); raw != "" {
			if d, err := time.ParseDuration(raw); err == nil {
				wal.SetDurableWindow(d)
				slog.Info("durable window set", "window", d)
			} else {
				slog.Warn("ignoring invalid INDEXUS_DURABLE_WINDOW", "value", raw, "err", err)
			}
		}
		store = wal
	}

	node, err := core.NewNode(settings, peer.NewContact, config.Bootstraps, store)
	if err != nil {
		return fmt.Errorf("node: %w", err)
	}

	if obj, err := core.NewS3Store(context.Background()); err != nil {
		slog.Warn("s3 object store unavailable", "err", err)
	} else if obj != nil {
		node.SetStore(obj)
		if wal != nil {
			n := node
			wal.OnLogRotated = func(path string) {
				if err := n.UploadWAL(context.Background(), path); err != nil {
					slog.Warn("wal segment upload failed", "path", path, "err", err)
				}
			}
		}
	}

	verifier, cert, err := setupAuth(config, node)
	if err != nil {
		return fmt.Errorf("authentication: %w", err)
	}
	if cert != nil {
		node.SetCert(cert)
	}
	if config.Autoscale.Enabled {
		node.EnableAutoscale(config.Autoscale)
	}

	monitoringServer := monitoring.NewHttpHandler(config.TLSDir, node, versionString())
	monitoringListener, err := net.Listen("tcp", fmt.Sprintf(":%d", config.MonitoringPort))
	if err != nil {
		return fmt.Errorf("monitoring listener: %w", err)
	}

	p2pServer := p2p.NewHttpHandler(config.TLSDir, node, peer.NewContact)
	p2pServer.Verifier = verifier
	p2pServer.RequireAuth = config.RequireAuth
	p2pServer.SelfCert = cert
	p2pListener, err := net.Listen("tcp", fmt.Sprintf(":%d", config.P2pPort))
	if err != nil {
		return fmt.Errorf("p2p listener: %w", err)
	}

	jobs := worker.NewWorker(node)

	failed := make(chan error, 1)
	watch := func(component string, err error) {
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return
		}
		select {
		case failed <- fmt.Errorf("%s: %w", component, err):
		default:
		}
	}

	guard, stopGuard := context.WithCancel(context.Background())
	defer stopGuard()
	go node.GuardMemory(guard)

	if wal != nil {
		go func() { watch("storage", wal.Start()) }()
	}
	go func() { watch("monitoring", monitoringServer.Serve(monitoringListener)) }()
	go func() { watch("p2p", p2pServer.Serve(p2pListener)) }()
	go func() { watch("feed", jobs.Feed()) }()
	go func() { watch("jobs", jobs.Start()) }()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	var runErr error
	select {
	case runErr = <-failed:
	case sig := <-signals:
		slog.Info("shutting down, handing zones over first", "signal", sig.String())
		logLeave(node.SoftLeave(config.LeaveTimeout))
	}

	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := p2pServer.Shutdown(ctx); err != nil {
		slog.Warn("p2p shutdown", "err", err)
	}
	if err := monitoringServer.Shutdown(ctx); err != nil {
		slog.Warn("monitoring shutdown", "err", err)
	}
	jobs.Close()
	if wal != nil {
		wal.Close()
	}

	return runErr
}

func logLeave(result core.LeaveResult) {
	attrs := []any{
		"ok", result.OK,
		"transferred_keys", result.TransferredKeys,
		"transferred_items", result.TransferredItems,
		"remaining_owned", result.RemainingOwned,
		"remaining_queue", result.RemainingQueue,
		"elapsed_ms", result.ElapsedMS,
	}
	if result.OK {
		slog.Info("soft-leave complete", attrs...)
		return
	}
	slog.Warn("soft-leave incomplete", append(attrs, "err", result.Error)...)
}

func setupAuth(config Config, node *core.Node) (*auth.Verifier, *auth.NodeCert, error) {
	if !config.RequireAuth && config.IssuerURL == "" {
		return nil, nil, nil
	}

	networkPub := config.NetworkPubKey
	if networkPub == "" {
		fetched, err := fetchNetworkPub(config.IssuerURL)
		if err != nil {
			return nil, nil, fmt.Errorf("fetch network public key: %w", err)
		}
		networkPub = fetched
	}
	networkKey, err := auth.ParsePublicKeyBase64(networkPub)
	if err != nil {
		return nil, nil, fmt.Errorf("network public key: %w", err)
	}

	keyPath := config.NodeKeyPath
	if keyPath == "" {
		keyPath = fmt.Sprintf(".keys/node-%s.ed25519", config.Name)
	}
	keyPair, err := auth.LoadOrCreateKeyPair(keyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("node key: %w", err)
	}

	cert, err := loadOrIssueCert(config, node, auth.PublicKeyBase64(keyPair.Public))
	if err != nil {
		return nil, nil, fmt.Errorf("node certificate: %w", err)
	}

	if config.IssuerURL != "" {
		token, err := issueServiceToken(config.IssuerURL, node.Name())
		if err != nil {
			slog.Warn("no service token, peer forwarding will be rejected on an auth-gated mesh", "err", err)
		} else {
			peer.OutboundBearer = token
			os.Setenv("INDEXUS_BEARER", token)
		}
	}

	return auth.NewVerifier(config.NetworkID, networkKey), cert, nil
}

func fetchNetworkPub(issuerURL string) (string, error) {
	resp, err := http.Get(issuerURL + "/v1/network/pubkey")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var body struct {
		PublicKey string `json:"public_key"`
		NetworkID string `json:"network_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	if body.PublicKey == "" {
		return "", errors.New("issuer returned no public key")
	}
	return body.PublicKey, nil
}

func loadOrIssueCert(config Config, node *core.Node, nodePub string) (*auth.NodeCert, error) {
	if cert, ok := readCert(config.CertPath); ok && cert.Signature != "" {
		return cert, nil
	}
	if config.IssuerURL == "" {
		return nil, errors.New("no certificate file and no issuer URL")
	}

	payload, err := json.Marshal(map[string]any{
		"node_id": node.Name(),
		"pub_key": nodePub,
		"ip":      node.IP(),
		"port":    node.Port(),
	})
	if err != nil {
		return nil, err
	}

	resp, err := http.Post(config.IssuerURL+"/v1/issue/node", "application/json", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("issuer status %d: %s", resp.StatusCode, string(raw))
	}

	var body struct {
		Cert *auth.NodeCert `json:"cert"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	if body.Cert == nil {
		return nil, errors.New("issuer returned an empty certificate")
	}

	if config.CertPath != "" {
		if err := writeCert(config.CertPath, body.Cert); err != nil {
			slog.Warn("certificate not cached, the next restart will get a new identity", "path", config.CertPath, "err", err)
		}
	}

	return body.Cert, nil
}

func issueServiceToken(issuerURL, nodeID string) (string, error) {
	payload, err := json.Marshal(map[string]any{
		"client_id": "node-service:" + nodeID,
		"scopes":    []string{"read", "write"},
	})
	if err != nil {
		return "", err
	}

	resp, err := http.Post(issuerURL+"/v1/issue/token", "application/json", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("issuer status %d: %s", resp.StatusCode, string(raw))
	}

	var body struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", err
	}
	return body.Token, nil
}

func stickyNodeName(config Config) string {
	if cert, ok := readCert(config.CertPath); ok && cert.NodeID != "" {
		return cert.NodeID
	}
	return config.Name
}

func readCert(path string) (*auth.NodeCert, bool) {
	if path == "" {
		return nil, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var cert auth.NodeCert
	if err := json.Unmarshal(raw, &cert); err != nil {
		return nil, false
	}
	return &cert, true
}

func writeCert(path string, cert *auth.NodeCert) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(cert, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

func versionString() string {
	return fmt.Sprintf("%s (%s, %s)", version, commit, runtime.Version())
}

func hostsOf(contacts []domain.Contact) []string {
	hosts := make([]string, 0, len(contacts))
	for _, contact := range contacts {
		for ip := range contact.IPs() {
			hosts = append(hosts, fmt.Sprintf("%s:%d", ip, contact.Port()))
		}
	}
	return hosts
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
