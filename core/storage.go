package core

import (
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/indexus/go-indexus-core/domain"
)

func (n *Node) SaveCollections() error {
	collectionsDir := n.settings.dataDir + "/collections"

	// Create collections directory if it doesn't exist
	if err := os.MkdirAll(collectionsDir, 0755); err != nil {
		return fmt.Errorf("failed to create collections directory: %w", err)
	}

	// Save each collection to a binary file
	for _, collection := range n.collections.List() {
		filename := fmt.Sprintf("%s/%s.bin", collectionsDir, collection.Name())
		if err := collection.SaveToBinary(filename); err != nil {
			log.Printf("Error saving collection %s: %v", collection.Name(), err)
			continue
		}
	}

	return nil
}

func (n *Node) LoadCollections() error {
	collectionsDir := n.settings.dataDir + "/collections"

	// Check if collections directory exists
	if _, err := os.Stat(collectionsDir); os.IsNotExist(err) {
		log.Println("No collections directory found, starting with empty collections")
		return nil
	}

	// Read directory entries
	entries, err := os.ReadDir(collectionsDir)
	if err != nil {
		return fmt.Errorf("failed to read collections directory: %w", err)
	}

	// Load each collection file
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".bin") {
			filename := fmt.Sprintf("%s/%s", collectionsDir, entry.Name())

			// Try to load the collection
			collection, err := domain.LoadFromBinary(filename)
			if err != nil {
				return fmt.Errorf("error loading collection from %s: %w", filename, err)
			}
			n.collections.Set(collection)
		}
	}

	return nil
}

func (n *Node) ClearOperationsLog() error {
	return n.collections.ClearOperationsLog()
}

func (n *Node) ReplayOperations() error {
	return n.collections.ReplayOperations()
}

func (n *Node) CompleteOwnership() error {
	// Iterate through all collections
	for _, collection := range n.collections.List() {
		// Get all areas that need to be owned
		areas := collection.Complete(collection.Base().Root())

		// Update the node's owned BST
		n.own(collection, areas)
	}
	return nil
}
