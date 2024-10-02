package storage

import (
	"bufio"
	"encoding/gob"
	"fmt"
	"os"
	"sync"
)

type Storage struct {
	filename string
	logs     *os.File
	writer   *bufio.Writer
	input    chan string
	wg       sync.WaitGroup
	quit     chan struct{}
}

func NewStorage(filename string) *Storage {

	storage := &Storage{
		filename: filename,
		input:    make(chan string, 100),
		quit:     make(chan struct{}),
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
	err := os.Remove(fmt.Sprintf("%s.logs", s.filename))
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	err = os.Remove(fmt.Sprintf("%s.snapshot", s.filename))
	if err != nil && !os.IsNotExist(err) {
		return err
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
	logs, err := os.OpenFile(fmt.Sprintf("%s.logs", s.filename), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("error opening the logs file: %v", err)
	}

	s.logs = logs
	s.writer = bufio.NewWriter(logs)

	file, err := os.Open(fmt.Sprintf("%s.snapshot", s.filename))
	if err != nil {
		return nil, fmt.Errorf("error opening the snapshot file: %v", err)
	}
	defer file.Close()

	var commands []string
	decoder := gob.NewDecoder(file)
	if err := decoder.Decode(&commands); err != nil {
		return nil, fmt.Errorf("error decoding snapshot: %v", err)
	}

	return commands, nil
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
