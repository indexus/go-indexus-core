package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/indexus/go-indexus-core/app/simulation/mockup"
	"github.com/indexus/go-indexus-core/logging"
	"github.com/indexus/go-indexus-core/peer"
)

const asciiArt = `

██╗███╗   ██╗██████╗ ███████╗██╗  ██╗██╗   ██╗███████╗
██║████╗  ██║██╔══██╗██╔════╝╚██╗██╔╝██║   ██║██╔════╝
██║██╔██╗ ██║██║  ██║█████╗   ╚███╔╝ ██║   ██║███████╗
██║██║╚██╗██║██║  ██║██╔══╝   ██╔██╗ ██║   ██║╚════██║
██║██║ ╚████║██████╔╝███████╗██╔╝ ██╗╚██████╔╝███████║
╚═╝╚═╝  ╚═══╝╚═════╝ ╚══════╝╚═╝  ╚═╝ ╚═════╝ ╚══════╝


`

const appName = "Indexus Simulation"

// Build metadata, set at link time; see app/node.
var (
	version = "dev"
	commit  = "none"
)

func main() {
	logging.Setup("simulation")

	port := flag.Int("port", 2100, "Port of the simulation service")
	flag.Parse()

	fmt.Print(asciiArt)
	fmt.Printf("%s %s (%s)\n\n", appName, version, commit)

	if err := run(*port); err != nil {
		slog.Error("simulation stopped on error", "err", err)
		os.Exit(1)
	}

	slog.Info("simulation stopped")
}

func run(port int) error {
	failed := make(chan error, 1)
	handler := mockup.NewHttpHandler(failed, peer.NewContact, 0, 0)

	listener, err := net.Listen("tcp", fmt.Sprintf("localhost:%d", port))
	if err != nil {
		return fmt.Errorf("listener: %w", err)
	}
	defer listener.Close()

	go func() { failed <- handler.Serve(listener) }()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-failed:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case sig := <-signals:
		slog.Info("shutting down", "signal", sig.String())
		return nil
	}
}
