package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gitlab.com/indexus/node/app/simulation/mockup"
	"gitlab.com/indexus/node/core"
	"gitlab.com/indexus/node/domain"
	"gitlab.com/indexus/node/http/monitoring"
	"gitlab.com/indexus/node/http/p2p"
	"gitlab.com/indexus/node/peer"
	"gitlab.com/indexus/node/worker"
)

const asciiArt = `

██╗███╗   ██╗██████╗ ███████╗██╗  ██╗██╗   ██╗███████╗
██║████╗  ██║██╔══██╗██╔════╝╚██╗██╔╝██║   ██║██╔════╝
██║██╔██╗ ██║██║  ██║█████╗   ╚███╔╝ ██║   ██║███████╗
██║██║╚██╗██║██║  ██║██╔══╝   ██╔██╗ ██║   ██║╚════██║
██║██║ ╚████║██████╔╝███████╗██╔╝ ██╗╚██████╔╝███████║
╚═╝╚═╝  ╚═══╝╚═════╝ ╚══════╝╚═╝  ╚═╝ ╚═════╝ ╚══════╝


`

const (
	appName    = "Indexus"
	appVersion = "1.0.0"
	buildDate  = "2023-10-03"
	commitHash = "abcdef1234567890"
)

// Config holds the configuration parsed from command-line flags
type Config struct {
	BootstrapFlag      string
	NameFlag           string
	MonitoringPortFlag int
	P2pPortFlag        int
	ClientPortFlag     int
	StorageFlag        string
	Bootstraps         []domain.Contact
}

func displayContacts(contacts []domain.Contact) string {

	hosts := make([]string, 0)
	for _, contact := range contacts {
		for ip := range contact.IPs() {
			hosts = append(hosts, fmt.Sprintf("%s:%d", ip, contact.Port()))
		}
	}

	return strings.Join(hosts, ", ")
}

func main() {

	// 1. Flag Section
	config := parseFlags()

	// 2. Display Message Section
	displayMessages(config)

	// 3. Starting Process Section
	startProcess(config)
}

// parseFlags handles command-line flag parsing and returns a Config struct
func parseFlags() Config {
	bootstrapFlagPtr := flag.String("bootstrap", "", "Host of the bootstrap peer")
	nameFlagPtr := flag.String("name", domain.EncodeId(domain.RandomId()), "Name of the node")
	monitoringPortFlagPtr := flag.Int("monitoringPort", 19000, "Port number of the node for the monitoring service")
	p2pPortFlagPtr := flag.Int("p2pPort", 21000, "Port number of the node for the peer to peer network")
	storageFlagPtr := flag.String("storage", ".data/backup", "Path to the backup file")

	flag.Parse()

	bootstraps := make([]domain.Contact, 0)

	if len(*bootstrapFlagPtr) > 0 {
		arr := strings.Split(*bootstrapFlagPtr, "|")
		ip := arr[0]
		port, err := strconv.Atoi(arr[1])
		if err != nil {
			log.Fatal(err)
		}

		bootstraps = append(bootstraps, peer.NewContact(domain.EncodeId(make([]byte, domain.IdLength())), map[string]any{ip: nil}, port))
	}

	return Config{
		BootstrapFlag:      *bootstrapFlagPtr,
		NameFlag:           *nameFlagPtr,
		MonitoringPortFlag: *monitoringPortFlagPtr,
		P2pPortFlag:        *p2pPortFlagPtr,
		StorageFlag:        *storageFlagPtr,
		Bootstraps:         bootstraps,
	}
}

// displayMessages displays the startup messages using the parsed configuration
func displayMessages(config Config) {

	fmt.Print(asciiArt)

	fmt.Printf("%s Version %s | Build Date: %s | Commit Hash: %s\n", appName, appVersion, buildDate, commitHash)
	fmt.Println()

	startTime := time.Now().Format("2006-01-02 15:04:05")

	fmt.Println("Start Time:", startTime)
	fmt.Println("Name:", config.NameFlag)
	fmt.Println("Monitoring, P2P Ports:", config.MonitoringPortFlag, config.P2pPortFlag)
	fmt.Println("Bootstrap Nodes:", displayContacts(config.Bootstraps))
	fmt.Println("Storage Path:", config.StorageFlag)
	fmt.Println()
}

// startProcess initializes and starts the core components of the application
func startProcess(config Config) {
	settings, err := core.NewSettings(config.NameFlag, config.P2pPortFlag, 10*time.Second, 5*time.Minute, domain.DelegationTreshold(), domain.IdLength())
	if err != nil {
		log.Fatal(err)
	}

	storageInstance := mockup.NewStorage() // storage.NewStorage(config.StorageFlag)
	node, err := core.NewNode(settings, peer.NewContact, config.Bootstraps, storageInstance)
	if err != nil {
		log.Fatal(err)
	}

	monitoringHttpHandler := monitoring.NewHttpHandler(node)
	monitoringListener, err := net.Listen("tcp", fmt.Sprintf(":%d", config.MonitoringPortFlag))
	if err != nil {
		log.Fatal(err)
	}

	p2pHttpHandler := p2p.NewHttpHandler(node, peer.NewContact)
	p2pListener, err := net.Listen("tcp", fmt.Sprintf(":%d", config.P2pPortFlag))
	if err != nil {
		log.Fatal(err)
	}

	workerInstance := worker.NewWorker(node)

	errChan := make(chan error, 3)

	go func() {
		// errChan <- storageInstance.Start()
	}()
	go func() {
		errChan <- monitoringHttpHandler.Serve(monitoringListener)
	}()
	go func() {
		errChan <- p2pHttpHandler.Serve(p2pListener)
	}()
	go func() {
		errChan <- workerInstance.Feed()
	}()
	go func() {
		errChan <- workerInstance.Start()
	}()

	// Wait for interrupt signal to gracefully shutdown
	signalChan := make(chan os.Signal, 5)
	signal.Notify(signalChan, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-errChan:
		if err != nil {
			log.Fatalf("Error occurred: %v", err)
		}
	case sig := <-signalChan:
		log.Printf("Received signal: %s", sig)
	}

	// Clean up resources
	// storageInstance.Close()
	monitoringListener.Close()
	p2pListener.Close()
	workerInstance.Close()

	log.Println("Node gracefully stopped")
}
