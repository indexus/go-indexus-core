package storage

import (
	"bufio"
	"encoding/gob"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Storage struct {
	archiveDir string
	filename   string
	logs       *os.File
	writer     *bufio.Writer
	input      chan string
	wg         sync.WaitGroup
	quit       chan struct{}
}

func NewStorage(archiveDir, filename string) *Storage {

	storage := &Storage{
		archiveDir: archiveDir,
		filename:   filename,
		input:      make(chan string, 100),
		quit:       make(chan struct{}),
	}

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
	return nil, fmt.Errorf("error when loading the file resetting the in any case")
}

func (s *Storage) Append(log string) {
	s.input <- log
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

	logs, err := os.OpenFile(fmt.Sprintf("%s.logs", s.filename), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}

	s.logs = logs
	s.writer = bufio.NewWriter(logs)

	s.wg.Add(1)
	defer s.wg.Done()

	for {
		select {
		case log := <-s.input:
			if _, err := s.writer.WriteString(log + "\n"); err != nil {
				return fmt.Errorf("failed to write data: %v", err)
			}
			if err := s.writer.Flush(); err != nil {
				return fmt.Errorf("failed to flush buffer: %v", err)
			}
		case <-s.quit:
			if err := s.writer.Flush(); err != nil {
				return fmt.Errorf("failed to flush buffer on shutdown: %v", err)
			}
			return nil
		}
	}
}

func (s *Storage) Close() {
	close(s.quit)
	s.wg.Wait()
	if err := s.logs.Close(); err != nil {
		fmt.Printf("Failed to close file: %v\n", err)
	}
}
