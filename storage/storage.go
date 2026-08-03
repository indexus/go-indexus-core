package storage

import (
	"bufio"
	"encoding/gob"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

type Storage struct {
	archiveDir string
	filename   string
	// mu guards logs and writer: Start publishes them from the background
	// goroutine while Close tears them down from the caller's.
	mu     sync.Mutex
	logs   *os.File
	writer *bufio.Writer
	input  chan string
	// running tells Close whether a drain loop is live: waiting on a loop that
	// was never started would hang the shutdown path.
	running  atomic.Bool
	wg       sync.WaitGroup
	quit     chan struct{}
	quitOnce sync.Once
}

func NewStorage(archiveDir, filename string) *Storage {

	storage := &Storage{
		archiveDir: archiveDir,
		filename:   filename,
		input:      make(chan string, 100),
		quit:       make(chan struct{}),
	}

	// Counted here rather than in Start: Start runs on its own goroutine, so an
	// Add there can land after Close has already called Wait.
	storage.wg.Add(1)

	return storage
}

func (s *Storage) Exist() bool {
	_, err := os.Stat(fmt.Sprintf("%s.snapshot", s.filename))
	if os.IsNotExist(err) {
		return false
	}
	return err == nil
}

func (s *Storage) Reset() error {

	if err := os.MkdirAll(s.archiveDir, 0755); err != nil {
		return fmt.Errorf("failed to create archive directory: %w", err)
	}

	currentDate := time.Now().Format("20060102_150405")

	filesToArchive := []string{
		fmt.Sprintf("%s.logs", s.filename),
		fmt.Sprintf("%s.snapshot", s.filename),
	}

	for _, file := range filesToArchive {
		if _, err := os.Stat(file); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return fmt.Errorf("failed to stat file %s: %w", file, err)
		}

		newFilename := fmt.Sprintf("%s_%s", currentDate, filepath.Base(file))
		archivePath := filepath.Join(s.archiveDir, newFilename)

		if err := os.Rename(file, archivePath); err != nil {
			return fmt.Errorf("failed to archive file %s: %w", file, err)
		}
	}

	return nil
}

func (s *Storage) Save(commands []string) error {
	file, err := os.Create(fmt.Sprintf("%s.snapshot", s.filename))
	if err != nil {
		return fmt.Errorf("error creating snapshot file: %v", err)
	}
	defer file.Close()

	encoder := gob.NewEncoder(file)
	if err := encoder.Encode(&commands); err != nil {
		return fmt.Errorf("error encoding snapshot: %v", err)
	}
	return nil
}

func (s *Storage) Load() ([]string, error) {
	path := fmt.Sprintf("%s.snapshot", s.filename)

	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("error opening snapshot file %q: %w", path, err)
	}
	defer file.Close()

	var commands []string
	if err := gob.NewDecoder(file).Decode(&commands); err != nil {
		return nil, fmt.Errorf("error decoding snapshot %q: %w", path, err)
	}
	return commands, nil
}

func (s *Storage) Append(log string) {
	s.input <- log
}

// SyncAppend durable-writes one line (append + fsync) for ingress ACK.
// Prefer this over Append when the caller must not lose a client-visible accept.
func (s *Storage) SyncAppend(log string) error {
	path := fmt.Sprintf("%s.logs", s.filename)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("sync append open: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteString(log + "\n"); err != nil {
		return fmt.Errorf("sync append write: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync append fsync: %w", err)
	}
	return nil
}

func (s *Storage) Stream(start int) <-chan string {
	stream := make(chan string)
	go func() {
		defer close(stream)

		file, err := os.Open(fmt.Sprintf("%s.logs", s.filename))
		if err != nil {
			fmt.Printf("Error opening file for streaming: %v\n", err)
			return
		}
		defer file.Close()

		fileInfo, err := file.Stat()
		if err != nil {
			fmt.Printf("Error getting file info: %v\n", err)
			return
		}
		if fileInfo.Size() == 0 {
			fmt.Println("File is empty, no data to stream.")
			return
		}

		scanner := bufio.NewScanner(file)

		for current := 0; current < start && scanner.Scan(); current++ {
		}

		for scanner.Scan() {
			stream <- scanner.Text()
		}

		if err := scanner.Err(); err != nil {
			fmt.Printf("Error reading from file: %v\n", err)
		}
	}()
	return stream
}

func (s *Storage) Start() error {

	if !s.running.CompareAndSwap(false, true) {
		return fmt.Errorf("storage is already started")
	}
	defer s.wg.Done()

	logs, err := os.OpenFile(fmt.Sprintf("%s.logs", s.filename), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.logs, s.writer = logs, bufio.NewWriter(logs)
	s.mu.Unlock()

	for {
		select {
		case log := <-s.input:
			if err := s.write(log); err != nil {
				return err
			}
		case <-s.quit:
			// Drain what producers already handed over. Returning here would
			// discard the tail of the log on every clean shutdown, which is
			// exactly when a node is draining.
			for {
				select {
				case log := <-s.input:
					if err := s.write(log); err != nil {
						return err
					}
				default:
					return s.flush()
				}
			}
		}
	}
}

func (s *Storage) write(log string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.writer == nil {
		return fmt.Errorf("failed to write data: storage is closed")
	}
	if _, err := s.writer.WriteString(log + "\n"); err != nil {
		return fmt.Errorf("failed to write data: %v", err)
	}
	if err := s.writer.Flush(); err != nil {
		return fmt.Errorf("failed to flush buffer: %v", err)
	}
	return nil
}

func (s *Storage) flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.writer == nil {
		return nil
	}
	if err := s.writer.Flush(); err != nil {
		return fmt.Errorf("failed to flush buffer on shutdown: %v", err)
	}
	return nil
}

// Close stops the writer and releases the log file. It is safe to call more
// than once, and safe to call when Start was never run.
func (s *Storage) Close() {
	s.quitOnce.Do(func() { close(s.quit) })
	if s.running.Load() {
		s.wg.Wait()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.logs == nil {
		return
	}
	if err := s.logs.Close(); err != nil {
		fmt.Printf("Failed to close file: %v\n", err)
	}
	s.logs, s.writer = nil, nil
}
