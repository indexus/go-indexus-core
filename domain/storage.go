package domain

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Operation types for write-ahead logging
const (
	OpAddItem = iota
	OpDeleteItem
)

func (c *Collections) periodicFlush() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := c.flushBuffer(); err != nil {
				log.Printf("Error flushing operation buffer: %v", err)
			}
		case <-c.stopFlusher:
			// Final flush before stopping
			if err := c.flushBuffer(); err != nil {
				log.Printf("Error during final flush: %v", err)
			}
			return
		}
	}
}

func (c *Collections) flushBuffer() error {
	c.bufferMu.Lock()
	if len(c.operationBuffer) == 0 {
		c.bufferMu.Unlock()
		return nil
	}

	// Get the current buffer and create a new one
	ops := c.operationBuffer
	c.operationBuffer = make([]*CollectionOperation, 0, 1000)
	c.bufferMu.Unlock()

	logsDir := c.settings.dataDir + "/collections/logs"

	// Create logs directory if it doesn't exist
	if err := os.MkdirAll(logsDir, 0755); err != nil {
		return fmt.Errorf("failed to create logs directory: %w", err)
	}

	// Open log file in append mode
	logFile, err := os.OpenFile(logsDir+"/operations.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to open log file: %w", err)
	}
	defer logFile.Close()

	// Write all operations
	for _, op := range ops {
		if err := c.writeOperationToFile(logFile, op); err != nil {
			return err
		}
	}

	// Ensure all operations are written to disk
	if err := logFile.Sync(); err != nil {
		return fmt.Errorf("failed to sync log file: %w", err)
	}

	return nil
}

func (c *Collections) writeOperationToFile(file *os.File, op *CollectionOperation) error {
	if err := binary.Write(file, binary.LittleEndian, int32(op.Type)); err != nil {
		return fmt.Errorf("failed to write operation type: %w", err)
	}

	// Write collection name
	nameLen := uint32(len(op.Collection))
	if err := binary.Write(file, binary.LittleEndian, nameLen); err != nil {
		return fmt.Errorf("failed to write collection name length: %w", err)
	}
	if _, err := file.Write([]byte(op.Collection)); err != nil {
		return fmt.Errorf("failed to write collection name: %w", err)
	}

	// Write location
	locLen := uint32(len(op.Location))
	if err := binary.Write(file, binary.LittleEndian, locLen); err != nil {
		return fmt.Errorf("failed to write location length: %w", err)
	}
	if _, err := file.Write([]byte(op.Location)); err != nil {
		return fmt.Errorf("failed to write location: %w", err)
	}

	// Write ID
	idLen := uint32(len(op.ID))
	if err := binary.Write(file, binary.LittleEndian, idLen); err != nil {
		return fmt.Errorf("failed to write ID length: %w", err)
	}
	if _, err := file.Write([]byte(op.ID)); err != nil {
		return fmt.Errorf("failed to write ID: %w", err)
	}

	// Write count
	if err := binary.Write(file, binary.LittleEndian, int32(op.Count)); err != nil {
		return fmt.Errorf("failed to write count: %w", err)
	}

	// Write metrics
	metricsLen := uint32(len(op.Metrics))
	if err := binary.Write(file, binary.LittleEndian, metricsLen); err != nil {
		return fmt.Errorf("failed to write metrics length: %w", err)
	}
	for _, metric := range op.Metrics {
		if err := binary.Write(file, binary.LittleEndian, metric); err != nil {
			return fmt.Errorf("failed to write metric: %w", err)
		}
	}

	// Write timestamp
	if err := binary.Write(file, binary.LittleEndian, op.Timestamp); err != nil {
		return fmt.Errorf("failed to write timestamp: %w", err)
	}

	return nil
}

func (c *Collections) WriteOperation(op *CollectionOperation) error {
	c.bufferMu.Lock()
	defer c.bufferMu.Unlock()

	c.operationBuffer = append(c.operationBuffer, op)

	// If buffer is full, trigger an immediate flush
	if len(c.operationBuffer) >= 1000 {
		go func() {
			if err := c.flushBuffer(); err != nil {
				log.Printf("Error flushing full buffer: %v", err)
			}
		}()
	}

	return nil
}

// ClearOperationsLog stops the flusher, clears the log, and restarts the flusher
func (c *Collections) ClearOperationsLog() error {
	// Stop the flusher (which will do a final flush)
	close(c.stopFlusher)

	logPath := c.settings.dataDir + "/collections/logs/operations.log"

	// Open the file with truncate flag
	file, err := os.OpenFile(logPath, os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		if os.IsNotExist(err) {
			// If file doesn't exist, create an empty one
			if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
				return fmt.Errorf("failed to create logs directory: %w", err)
			}
			file, err = os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE, 0644)
			if err != nil {
				return fmt.Errorf("failed to create operations log: %w", err)
			}
		} else {
			return fmt.Errorf("failed to open operations log: %w", err)
		}
	}
	defer file.Close()

	// Create new stopFlusher channel and start new flusher goroutine
	c.stopFlusher = make(chan struct{})
	go c.periodicFlush()

	return nil
}

func (c *Collections) ReplayOperations() error {
	logsDir := c.settings.dataDir + "/collections/logs"
	if err := os.Remove(logsDir + "/operations.log"); os.IsNotExist(err) {
		return nil // No log file to replay
	}

	file, err := os.Open(logsDir + "/operations.log")
	if err != nil {
		return fmt.Errorf("failed to open log file: %w", err)
	}
	defer file.Close()

	reader := bufio.NewReader(file)

	for {
		// Read operation type
		var opType int32
		if err := binary.Read(reader, binary.LittleEndian, &opType); err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("failed to read operation type: %w", err)
		}

		// Read collection name
		var nameLen uint32
		if err := binary.Read(reader, binary.LittleEndian, &nameLen); err != nil {
			return fmt.Errorf("failed to read collection name length: %w", err)
		}
		nameBuf := make([]byte, nameLen)
		if _, err := io.ReadFull(reader, nameBuf); err != nil {
			return fmt.Errorf("failed to read collection name: %w", err)
		}
		collectionName := string(nameBuf)

		// Read location
		var locLen uint32
		if err := binary.Read(reader, binary.LittleEndian, &locLen); err != nil {
			return fmt.Errorf("failed to read location length: %w", err)
		}
		locBuf := make([]byte, locLen)
		if _, err := io.ReadFull(reader, locBuf); err != nil {
			return fmt.Errorf("failed to read location: %w", err)
		}
		location := string(locBuf)

		// Read ID
		var idLen uint32
		if err := binary.Read(reader, binary.LittleEndian, &idLen); err != nil {
			return fmt.Errorf("failed to read ID length: %w", err)
		}
		idBuf := make([]byte, idLen)
		if _, err := io.ReadFull(reader, idBuf); err != nil {
			return fmt.Errorf("failed to read ID: %w", err)
		}
		id := string(idBuf)

		// Read count
		var count int32
		if err := binary.Read(reader, binary.LittleEndian, &count); err != nil {
			return fmt.Errorf("failed to read count: %w", err)
		}

		// Read metrics
		var metricsLen uint32
		if err := binary.Read(reader, binary.LittleEndian, &metricsLen); err != nil {
			return fmt.Errorf("failed to read metrics length: %w", err)
		}
		metrics := make([]float64, metricsLen)
		for i := uint32(0); i < metricsLen; i++ {
			if err := binary.Read(reader, binary.LittleEndian, &metrics[i]); err != nil {
				return fmt.Errorf("failed to read metric: %w", err)
			}
		}

		// Read timestamp
		var timestamp int64
		if err := binary.Read(reader, binary.LittleEndian, &timestamp); err != nil {
			return fmt.Errorf("failed to read timestamp: %w", err)
		}

		// Apply the operation
		collection, exists := c.Get(collectionName)
		if !exists {
			collection = NewCollection(collectionName, collection.Base().Root(), BASE64)
			c.Set(collection)
		}

		switch opType {
		case OpAddItem:
			collection.Add(location, id, metrics, c.settings.delegation)
		case OpDeleteItem:
			if set, exists := collection.Get(location); exists {
				delete(set.list, id)
			}
		}
	}

	return nil
}

// SaveToBinary saves the collection to a binary file
func (c *Collection) SaveToBinary(filePath string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Create collections directory if it doesn't exist
	if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
		return fmt.Errorf("failed to create collections directory: %w", err)
	}

	// Validate collection data before saving
	if c.name == "" {
		return fmt.Errorf("invalid collection: empty name")
	}
	if c.sets == nil {
		return fmt.Errorf("invalid collection: nil sets")
	}

	// Determine the metrics size for this collection
	var collectionMetricsSize int
	for _, set := range c.sets {
		if set == nil {
			continue
		}
		for _, abelian := range set.list {
			if abelian != nil && abelian.metrics != nil {
				collectionMetricsSize = len(abelian.metrics)
				break
			}
		}
		if collectionMetricsSize > 0 {
			break
		}
	}
	if collectionMetricsSize == 0 {
		collectionMetricsSize = 5 // Default size if no metrics found
	}

	// Pre-validate all data before writing
	for location, set := range c.sets {
		if set == nil {
			return fmt.Errorf("invalid set at location %s: nil set", location)
		}
		for key, abelian := range set.list {
			if abelian == nil {
				return fmt.Errorf("invalid abelian at %s/%s: nil abelian", location, key)
			}
			if abelian.metrics == nil {
				return fmt.Errorf("invalid metrics at %s/%s: nil metrics", location, key)
			}
			if len(abelian.metrics) != collectionMetricsSize {
				// Try to fix the metrics if possible
				if len(abelian.metrics) < collectionMetricsSize {
					newMetrics := make([]float64, collectionMetricsSize)
					copy(newMetrics, abelian.metrics)
					abelian.metrics = newMetrics
					// Log that we fixed the metrics
					log.Printf("Fixed metrics array size for %s/%s from %d to %d", location, key, len(abelian.metrics), collectionMetricsSize)
				} else {
					return fmt.Errorf("inconsistent metrics length at %s/%s: got %d, expected %d (collection standard)",
						location, key, len(abelian.metrics), collectionMetricsSize)
				}
			}
			for i, metric := range abelian.metrics {
				if math.IsNaN(metric) || math.IsInf(metric, 0) {
					return fmt.Errorf("invalid metric value at %s/%s index %d: %f", location, key, i, metric)
				}
			}
		}
	}

	// Create a temporary file first
	tempFile := filePath + ".tmp"
	file, err := os.Create(tempFile)
	if err != nil {
		return fmt.Errorf("failed to create temporary file: %w", err)
	}
	defer func() {
		file.Close()
		if err != nil {
			os.Remove(tempFile) // Clean up the temp file if there was an error
		}
	}()

	// Write collection name with validation
	nameLen := uint32(len(c.name))
	if nameLen == 0 || nameLen > 1024 { // Add reasonable size limit
		return fmt.Errorf("invalid name length: %d", nameLen)
	}
	if err := binary.Write(file, binary.LittleEndian, nameLen); err != nil {
		return fmt.Errorf("failed to write name length: %w", err)
	}
	if _, err := file.Write([]byte(c.name)); err != nil {
		return fmt.Errorf("failed to write name: %w", err)
	}

	// Write metrics size for this collection
	if err := binary.Write(file, binary.LittleEndian, uint32(collectionMetricsSize)); err != nil {
		return fmt.Errorf("failed to write metrics size: %w", err)
	}

	// Write sets count with validation
	setsCount := uint32(len(c.sets))
	if setsCount > 1000000 { // Add reasonable limit
		return fmt.Errorf("too many sets: %d", setsCount)
	}
	if err := binary.Write(file, binary.LittleEndian, setsCount); err != nil {
		return fmt.Errorf("failed to write sets count: %w", err)
	}

	// Write each set
	for location, set := range c.sets {
		// Write location with validation
		locLen := uint32(len(location))
		if locLen == 0 || locLen > 1024 {
			return fmt.Errorf("invalid location length at %s: %d", location, locLen)
		}
		if err := binary.Write(file, binary.LittleEndian, locLen); err != nil {
			return fmt.Errorf("failed to write location length: %w", err)
		}
		if _, err := file.Write([]byte(location)); err != nil {
			return fmt.Errorf("failed to write location: %w", err)
		}

		// Write set items count with validation
		itemsCount := uint32(len(set.list))
		if itemsCount > 1000000 { // Add reasonable limit
			return fmt.Errorf("too many items in set %s: %d", location, itemsCount)
		}
		if err := binary.Write(file, binary.LittleEndian, itemsCount); err != nil {
			return fmt.Errorf("failed to write items count: %w", err)
		}

		// Write each item in the set
		for key, abelian := range set.list {
			// Write key with validation
			keyLen := uint32(len(key))
			if keyLen == 0 || keyLen > 1024 {
				return fmt.Errorf("invalid key length at %s/%s: %d", location, key, keyLen)
			}
			if err := binary.Write(file, binary.LittleEndian, keyLen); err != nil {
				return fmt.Errorf("failed to write key length: %w", err)
			}
			if _, err := file.Write([]byte(key)); err != nil {
				return fmt.Errorf("failed to write key: %w", err)
			}

			// Write Abelian count
			if err := binary.Write(file, binary.LittleEndian, int32(abelian.count)); err != nil {
				return fmt.Errorf("failed to write abelian count: %w", err)
			}

			// Write Abelian metrics
			for i, metric := range abelian.metrics {
				if err := binary.Write(file, binary.LittleEndian, metric); err != nil {
					return fmt.Errorf("failed to write metric %d at %s/%s: %w", i, location, key, err)
				}
			}
		}
	}

	// Write ownership with validation
	ownedCount := uint32(len(c.owned))
	if ownedCount > 1000000 { // Add reasonable limit
		return fmt.Errorf("too many ownerships: %d", ownedCount)
	}
	if err := binary.Write(file, binary.LittleEndian, ownedCount); err != nil {
		return fmt.Errorf("failed to write owned count: %w", err)
	}

	for location, delegation := range c.owned {
		if delegation == nil {
			return fmt.Errorf("invalid delegation at %s: nil delegation", location)
		}

		// Write location
		locLen := uint32(len(location))
		if locLen == 0 || locLen > 1024 {
			return fmt.Errorf("invalid owned location length at %s: %d", location, locLen)
		}
		if err := binary.Write(file, binary.LittleEndian, locLen); err != nil {
			return fmt.Errorf("failed to write owned location length: %w", err)
		}
		if _, err := file.Write([]byte(location)); err != nil {
			return fmt.Errorf("failed to write owned location: %w", err)
		}

		// Write delegation count
		delegationCount := uint32(len(delegation))
		if delegationCount > 1000000 { // Add reasonable limit
			return fmt.Errorf("too many delegations at %s: %d", location, delegationCount)
		}
		if err := binary.Write(file, binary.LittleEndian, delegationCount); err != nil {
			return fmt.Errorf("failed to write delegation count: %w", err)
		}

		// Write each delegation key
		for key := range delegation {
			keyLen := uint32(len(key))
			if keyLen == 0 || keyLen > 1024 {
				return fmt.Errorf("invalid delegation key length at %s: %d", location, keyLen)
			}
			if err := binary.Write(file, binary.LittleEndian, keyLen); err != nil {
				return fmt.Errorf("failed to write delegation key length: %w", err)
			}
			if _, err := file.Write([]byte(key)); err != nil {
				return fmt.Errorf("failed to write delegation key: %w", err)
			}
		}
	}

	// Ensure all data is written to disk
	if err := file.Sync(); err != nil {
		return fmt.Errorf("failed to sync file: %w", err)
	}

	// Close the file before renaming
	if err := file.Close(); err != nil {
		return fmt.Errorf("failed to close file: %w", err)
	}

	// Atomically replace the old file with the new one
	if err := os.Rename(tempFile, filePath); err != nil {
		return fmt.Errorf("failed to rename temporary file: %w", err)
	}

	return nil
}

// LoadFromBinary loads a collection from a binary file
func LoadFromBinary(filePath string) (*Collection, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	// Read file size for validation
	fileInfo, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("failed to get file info: %w", err)
	}
	if fileInfo.Size() < 12 { // Minimum size (4 bytes name length + at least 1 byte name + 4 bytes metrics size + 4 bytes sets count)
		return nil, fmt.Errorf("file too small to be valid: %d bytes", fileInfo.Size())
	}

	// Create a buffered reader for better performance
	reader := bufio.NewReader(file)

	// Read collection name
	var nameLen uint32
	if err := binary.Read(reader, binary.LittleEndian, &nameLen); err != nil {
		return nil, fmt.Errorf("failed to read name length: %w", err)
	}
	if nameLen == 0 || nameLen > 1024 {
		return nil, fmt.Errorf("invalid name length: %d", nameLen)
	}

	nameBuf := make([]byte, nameLen)
	if _, err := io.ReadFull(reader, nameBuf); err != nil {
		return nil, fmt.Errorf("failed to read name: %w", err)
	}
	name := string(nameBuf)

	// Read metrics size for this collection
	var metricsSize uint32
	if err := binary.Read(reader, binary.LittleEndian, &metricsSize); err != nil {
		return nil, fmt.Errorf("failed to read metrics size: %w", err)
	}
	if metricsSize == 0 || metricsSize > 1000 { // Add reasonable limit
		return nil, fmt.Errorf("invalid metrics size: %d", metricsSize)
	}

	// Create new collection
	collection := &Collection{
		name:  name,
		sets:  make(map[string]*Set),
		owned: make(Ownership),
		mu:    &sync.Mutex{},
	}

	// Read sets count
	var setsCount uint32
	if err := binary.Read(reader, binary.LittleEndian, &setsCount); err != nil {
		return nil, fmt.Errorf("failed to read sets count: %w", err)
	}
	if setsCount > 1000000 {
		return nil, fmt.Errorf("too many sets: %d", setsCount)
	}

	// Read each set
	for i := uint32(0); i < setsCount; i++ {
		// Read location
		var locLen uint32
		if err := binary.Read(reader, binary.LittleEndian, &locLen); err != nil {
			return nil, fmt.Errorf("failed to read location length for set %d: %w", i, err)
		}
		if locLen == 0 || locLen > 1024 {
			return nil, fmt.Errorf("invalid location length for set %d: %d", i, locLen)
		}

		locBuf := make([]byte, locLen)
		if _, err := io.ReadFull(reader, locBuf); err != nil {
			return nil, fmt.Errorf("failed to read location for set %d: %w", i, err)
		}
		location := string(locBuf)

		// Create new set
		set := NewSet()
		collection.sets[location] = set

		// Read items count
		var itemsCount uint32
		if err := binary.Read(reader, binary.LittleEndian, &itemsCount); err != nil {
			return nil, fmt.Errorf("failed to read items count for set %d: %w", i, err)
		}
		if itemsCount > 1000000 {
			return nil, fmt.Errorf("too many items in set %d: %d", i, itemsCount)
		}

		// Read each item
		for j := uint32(0); j < itemsCount; j++ {
			// Read key
			var keyLen uint32
			if err := binary.Read(reader, binary.LittleEndian, &keyLen); err != nil {
				return nil, fmt.Errorf("failed to read key length for item %d in set %d: %w", j, i, err)
			}
			if keyLen == 0 || keyLen > 1024 {
				return nil, fmt.Errorf("invalid key length for item %d in set %d: %d", j, i, keyLen)
			}

			keyBuf := make([]byte, keyLen)
			if _, err := io.ReadFull(reader, keyBuf); err != nil {
				return nil, fmt.Errorf("failed to read key for item %d in set %d: %w", j, i, err)
			}
			key := string(keyBuf)

			// Read Abelian count
			var count int32
			if err := binary.Read(reader, binary.LittleEndian, &count); err != nil {
				return nil, fmt.Errorf("failed to read count for item %d in set %d: %w", j, i, err)
			}

			// Read Abelian metrics
			metrics := make([]float64, metricsSize)
			for k := uint32(0); k < metricsSize; k++ {
				if err := binary.Read(reader, binary.LittleEndian, &metrics[k]); err != nil {
					return nil, fmt.Errorf("failed to read metric %d for item %d in set %d: %w", k, j, i, err)
				}
				if math.IsNaN(metrics[k]) || math.IsInf(metrics[k], 0) {
					return nil, fmt.Errorf("invalid metric value %f at index %d for item %d in set %d", metrics[k], k, j, i)
				}
			}

			// Create and add Abelian
			abelian := NewAbelian(int(count), metrics)
			set.list[key] = abelian
		}
	}

	// Read ownership
	var ownedCount uint32
	if err := binary.Read(reader, binary.LittleEndian, &ownedCount); err != nil {
		return nil, fmt.Errorf("failed to read owned count: %w", err)
	}
	if ownedCount > 1000000 {
		return nil, fmt.Errorf("too many ownerships: %d", ownedCount)
	}

	for i := uint32(0); i < ownedCount; i++ {
		// Read location
		var locLen uint32
		if err := binary.Read(reader, binary.LittleEndian, &locLen); err != nil {
			return nil, fmt.Errorf("failed to read owned location length for ownership %d: %w", i, err)
		}
		if locLen == 0 || locLen > 1024 {
			return nil, fmt.Errorf("invalid owned location length for ownership %d: %d", i, locLen)
		}

		locBuf := make([]byte, locLen)
		if _, err := io.ReadFull(reader, locBuf); err != nil {
			return nil, fmt.Errorf("failed to read owned location for ownership %d: %w", i, err)
		}
		location := string(locBuf)

		// Read delegation count
		var delegationCount uint32
		if err := binary.Read(reader, binary.LittleEndian, &delegationCount); err != nil {
			return nil, fmt.Errorf("failed to read delegation count for ownership %d: %w", i, err)
		}
		if delegationCount > 1000000 {
			return nil, fmt.Errorf("too many delegations for ownership %d: %d", i, delegationCount)
		}

		// Create delegation map
		delegation := make(Delegation)
		collection.owned[location] = delegation

		// Read each delegation key
		for j := uint32(0); j < delegationCount; j++ {
			var keyLen uint32
			if err := binary.Read(reader, binary.LittleEndian, &keyLen); err != nil {
				return nil, fmt.Errorf("failed to read delegation key length for ownership %d, delegation %d: %w", i, j, err)
			}
			if keyLen == 0 || keyLen > 1024 {
				return nil, fmt.Errorf("invalid delegation key length for ownership %d, delegation %d: %d", i, j, keyLen)
			}

			keyBuf := make([]byte, keyLen)
			if _, err := io.ReadFull(reader, keyBuf); err != nil {
				return nil, fmt.Errorf("failed to read delegation key for ownership %d, delegation %d: %w", i, j, err)
			}
			key := string(keyBuf)
			delegation[key] = nil
		}
	}

	// Check for unexpected EOF by trying to read one more byte
	extraByte := make([]byte, 1)
	n, err := reader.Read(extraByte)
	if err != io.EOF {
		return nil, fmt.Errorf("file contains extra data or is corrupted")
	}
	if n > 0 {
		return nil, fmt.Errorf("file contains %d extra bytes", n)
	}

	return collection, nil
}

// CollectionOperation represents a single operation on a collection
type CollectionOperation struct {
	Type       int       // Operation type (OpAddItem, OpUpdateItem, OpDeleteItem)
	Collection string    // Collection name
	Location   string    // Location in the collection
	ID         string    // Item ID
	Count      int       // Abelian count
	Metrics    []float64 // Metrics array
	Timestamp  int64     // Operation timestamp
}
