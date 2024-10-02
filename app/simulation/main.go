package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gitlab.com/indexus/node/app/simulation/mockup"
	"gitlab.com/indexus/node/peer"
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
	appName    = "Indexus Simulation"
	appVersion = "1.0.0"
	buildDate  = "2023-10-03"
	commitHash = "abcdef1234567890"
)

// Config holds the configuration parsed from command-line flags
type Config struct {
	PortFlag int
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
	portFlagPtr := flag.Int("port", 2100, "Port number of the node")

	flag.Parse()

	return Config{
		PortFlag: *portFlagPtr,
	}
}

// displayMessages displays the startup messages using the parsed configuration
func displayMessages(config Config) {

	fmt.Print(asciiArt)

	fmt.Printf("%s Version %s | Build Date: %s | Commit Hash: %s\n", appName, appVersion, buildDate, commitHash)
	fmt.Println()

	startTime := time.Now().Format("2006-01-02 15:04:05")

	fmt.Println("Start Time:", startTime)
	fmt.Println("Port:", config.PortFlag)
	fmt.Println()
}

// startProcess initializes and starts the core components of the application
func startProcess(config Config) {

	var errChan = make(chan error, 1)

	httpHandler := mockup.NewHttpHandler(errChan, peer.NewContact, 100, 200)

	listener, err := net.Listen("tcp", fmt.Sprintf("localhost:%d", config.PortFlag))
	if err != nil {
		log.Fatal(err)
	}

	go func() {
		defer listener.Close()
		errChan <- httpHandler.Serve(listener)
	}()

	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-errChan:
		if err != nil {
			log.Fatalf("Error occurred: %v", err)
		}
	case sig := <-signalChan:
		log.Printf("Received signal: %s", sig)
	}
}
