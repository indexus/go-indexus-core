package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/indexus/go-indexus-core/auth"
	"github.com/indexus/go-indexus-core/issuer"
	"github.com/indexus/go-indexus-core/logging"
)

const appName = "Indexus Issuer"

// Build metadata, set at link time; see app/node.
var (
	version = "dev"
	commit  = "none"
)

func main() {
	logging.Setup("issuer")

	if err := run(); err != nil {
		slog.Error("issuer stopped on error", "err", err)
		os.Exit(1)
	}

	slog.Info("issuer stopped")
}

func run() error {
	addr := flag.String("addr", ":22000", "Listen address")
	networkID := flag.String("network", "indexus-aws", "Network id")
	keyPath := flag.String("key", ".keys/issuer.ed25519", "Path to the issuer ed25519 private key")
	bootstrap := flag.String("bootstrap", "", "Comma-separated bootstrap IPs handed to issued nodes")
	p2pPort := flag.Int("p2pPort", 21000, "Peer-to-peer port written into certificates")
	scaleEnabled := flag.Bool("scale", false, "Serve /v1/scale, spawning EC2 instances or local processes")
	launchTemplate := flag.String("launchTemplate", os.Getenv("LAUNCH_TEMPLATE_ID"), "EC2 launch template id")
	localDir := flag.String("localDir", os.Getenv("INDEXUS_LOCAL_DIR"), "Directory of local spawn scripts, which replaces EC2")
	region := flag.String("region", envOr("AWS_REGION", "eu-west-3"), "AWS region")
	spawnMax := flag.Int("spawnMax", 1, "Instances that may run at once")
	cooldown := flag.Duration("scaleCooldown", 3*time.Minute, "Quiet period between spawns")
	downCooldown := flag.Duration("downCooldown", 2*time.Minute, "Quiet period after a terminate before the next SoftLeave may lock")
	showVersion := flag.Bool("version", false, "Print the version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("%s %s (%s, %s)\n", appName, version, commit, runtime.Version())
		return nil
	}

	keys, err := auth.LoadOrCreateKeyPair(*keyPath)
	if err != nil {
		return fmt.Errorf("issuer key: %w", err)
	}

	server := issuer.NewServer(auth.NewIssuer(*networkID, keys))
	server.P2PPort = *p2pPort
	server.ScaleEnabled = *scaleEnabled
	server.LaunchTemplateID = *launchTemplate
	server.LocalDir = *localDir
	server.Region = *region
	server.SpawnMaxExtra = *spawnMax
	server.ScaleCooldown = *cooldown
	server.DownCooldown = *downCooldown
	if *bootstrap != "" {
		server.BootstrapIPs = strings.Split(*bootstrap, ",")
	}
	// "local" is the launch template that spawns processes instead of instances.
	if server.LocalDir != "" && server.LaunchTemplateID == "" {
		server.LaunchTemplateID = "local"
	}

	slog.Info("starting issuer",
		"version", version,
		"network", *networkID,
		"public_key", auth.PublicKeyBase64(keys.Public),
		"scale", *scaleEnabled,
		"local_dir", server.LocalDir,
		"region", *region,
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return server.Serve(ctx, *addr)
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
