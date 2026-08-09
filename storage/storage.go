package storage

import (
	"bufio"
	"encoding/gob"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const (
	walBufferSize   = 64 << 10 // 64 KiB
	walFlushBytes   = 64 << 10
	walFlushEvery   = 10 * time.Millisecond
	walInputBuffer  = 4096
	walSyncGroupGap = 2 * time.Millisecond // coalesce fsync across concurrent SyncAppend
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
	// unflushed counts bytes sitting in writer since the last Flush.
	unflushed int
	// Group-commit fsync: waiters share one Sync covering all writes flushed
	// before the syncer ran.
	syncCond   *sync.Cond
	syncTicket uint64 // next ticket to issue
	syncedThru uint64 // highest ticket covered by a completed Sync
	syncing    bool
	// durableWindow optionally delays the group fsync so concurrent SyncAppend
	// callers share one Sync. Zero keeps today's immediate group-commit gap.
	// ACK still waits until syncedThru covers the ticket (never flush-only).
	durableWindow time.Duration
	// running tells Close whether a drain loop is live: waiting on a loop that
	// was never started would hang the shutdown path.
	running  atomic.Bool
	wg       sync.WaitGroup
	quit     chan struct{}
	quitOnce sync.Once
	// OnLogRotated is invoked with the absolute path of an archived WAL after
	// rotateLog moves it aside. Callers use it to push segments to object
	// storage. May be nil.
	OnLogRotated func(archivedPath string)
	// lastArchived is the path of the most recently rotated WAL (empty if none).
	lastArchived string
	// pendingSealed are WAL segments rotated without a matching Snapshot Save.
	// Stream replays them before the live log so recovery remains complete
	// across SealRotate under TransferBusy. Cleared on successful Save.
	pendingSealed []string
	// dirty is set on Append/SyncAppend (and when reopening a non-empty WAL)
	// and cleared after a successful Save. Checkpoint skips Save when false.
	dirty atomic.Bool
}

// NewStorage prepares the snapshot and log pair sitting at filename, filename
// being a path prefix rather than a file. The directory is created here so that
// a first run fails on its configuration instead of on its first write.
func NewStorage(archiveDir, filename string) (*Storage, error) {

	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		return nil, fmt.Errorf("create storage directory: %w", err)
	}

	storage := &Storage{
		archiveDir: archiveDir,
		filename:   filename,
		input:      make(chan string, walInputBuffer),
		quit:       make(chan struct{}),
	}
	storage.syncCond = sync.NewCond(&storage.mu)

	// Counted here rather than in Start: Start runs on its own goroutine, so an
	// Add there can land after Close has already called Wait.
	storage.wg.Add(1)

	return storage, nil
}

// SetDurableWindow sets how long the group-commit syncer may wait to coalesce
// concurrent SyncAppend callers. Zero (default) only uses the 2ms contention
// gap. ACK always waits for fsync covering its ticket.
func (s *Storage) SetDurableWindow(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d < 0 {
		d = 0
	}
	s.durableWindow = d
}

// The snapshot holds the state as of the last dump, the log holds everything
// that happened since.
func (s *Storage) snapshotPath() string { return s.filename + ".snapshot" }
func (s *Storage) logPath() string      { return s.filename + ".logs" }

func (s *Storage) Exist() bool {
	if _, err := os.Stat(s.snapshotPath()); err == nil {
		return true
	}
	// A node that crashes before its first Refresh has only the log: every
	// client ACK is already there. Pretending nothing exists would drop them.
	if _, err := os.Stat(s.logPath()); err == nil {
		return true
	}
	return false
}

func (s *Storage) Reset() error {

	if err := os.MkdirAll(s.archiveDir, 0755); err != nil {
		return fmt.Errorf("failed to create archive directory: %w", err)
	}

	currentDate := time.Now().Format("20060102_150405")

	filesToArchive := []string{
		s.logPath(),
		s.snapshotPath(),
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

	s.dirty.Store(false)
	return nil
}

// Dirty reports WAL changes since the last successful Save. True when Append
// ran, or when a non-empty log is still on disk (e.g. crash before checkpoint).
func (s *Storage) Dirty() bool {
	if s.dirty.Load() {
		return true
	}
	st, err := os.Stat(s.logPath())
	return err == nil && st.Size() > 0
}

// Save writes a checkpoint atomically, then rotates the write-ahead log.
//
// The snapshot is the whole state as of this call; anything already in the log
// is either reflected in it or carried as a pending-ingress line. Leaving the
// old log in place made every restart replay the node's entire lifetime, and
// re-Append on apply grew the file without bound.
func (s *Storage) Save(commands []string) error {
	path := s.snapshotPath()
	tmp := path + ".tmp"

	file, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("error creating snapshot file: %v", err)
	}
	encoder := gob.NewEncoder(file)
	if err := encoder.Encode(&commands); err != nil {
		file.Close()
		os.Remove(tmp)
		return fmt.Errorf("error encoding snapshot: %v", err)
	}
	if err := file.Sync(); err != nil {
		file.Close()
		os.Remove(tmp)
		return fmt.Errorf("error syncing snapshot: %v", err)
	}
	if err := file.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("error closing snapshot: %v", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("error replacing snapshot: %v", err)
	}

	if archived, err := s.rotateLog(); err != nil {
		return fmt.Errorf("snapshot saved but log rotate failed: %w", err)
	} else if archived != "" && s.OnLogRotated != nil {
		s.OnLogRotated(archived)
	}
	// Snapshot covers prior sealed segments; drop replay list (files remain for S3).
	s.mu.Lock()
	s.pendingSealed = nil
	s.mu.Unlock()
	s.dirty.Store(false)
	return nil
}

// sealMinBytes is the live-WAL size that triggers SealRotate under TransferBusy.
var sealMinBytes int64 = 1 << 20 // 1 MiB

// SealRotate archives the live WAL without writing a snapshot. Used when
// TransferBusy blocks Checkpoint so the active log cannot grow without bound.
// Stream continues to replay sealed segments until the next Save.
func (s *Storage) SealRotate() (archived string, err error) {
	s.mu.Lock()
	path := s.logPath()
	var size int64
	if st, statErr := os.Stat(path); statErr == nil {
		size = st.Size()
	}
	s.mu.Unlock()
	if size < sealMinBytes {
		return "", nil
	}

	archived, err = s.rotateLog()
	if err != nil || archived == "" {
		return archived, err
	}
	s.mu.Lock()
	s.pendingSealed = append(s.pendingSealed, archived)
	s.mu.Unlock()
	// Still dirty: snapshot does not cover the seal yet.
	s.dirty.Store(true)
	if s.OnLogRotated != nil {
		s.OnLogRotated(archived)
	}
	return archived, nil
}

// rotateLog archives the current WAL and opens an empty one. Caller has just
// written a snapshot that covers everything the log held. Returns the archived
// path (empty when there was nothing to rotate) so the caller can upload it
// without holding s.mu.
func (s *Storage) rotateLog() (archived string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.writer != nil {
		if err := s.writer.Flush(); err != nil {
			return "", err
		}
		if err := s.logs.Sync(); err != nil {
			return "", err
		}
		_ = s.logs.Close()
		s.writer = nil
		s.logs = nil
		s.unflushed = 0
	}

	path := s.logPath()
	if _, err := os.Stat(path); err == nil {
		if err := os.MkdirAll(s.archiveDir, 0755); err != nil {
			return "", fmt.Errorf("create archive directory: %w", err)
		}
		name := fmt.Sprintf("%s_%s", time.Now().Format("20060102_150405"), filepath.Base(path))
		archived = filepath.Join(s.archiveDir, name)
		if err := os.Rename(path, archived); err != nil {
			return "", fmt.Errorf("archive log: %w", err)
		}
		s.lastArchived = archived
	} else if !os.IsNotExist(err) {
		return "", err
	}

	// Reopen so Append/SyncAppend after the checkpoint keep working. Start may
	// not be running yet (Restore happens before the drain loop).
	if err := s.ensureOpenLocked(); err != nil {
		return archived, err
	}
	return archived, nil
}

// LastArchivedWAL returns the path of the most recently rotated WAL segment.
func (s *Storage) LastArchivedWAL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastArchived
}

func (s *Storage) Load() ([]string, error) {
	path := s.snapshotPath()

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
	s.dirty.Store(true)
	s.input <- log
}

// ensureOpenLocked opens the WAL if needed. Caller must hold s.mu.
func (s *Storage) ensureOpenLocked() error {
	if s.writer != nil {
		return nil
	}
	logs, err := os.OpenFile(s.logPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	s.logs = logs
	s.writer = bufio.NewWriterSize(logs, walBufferSize)
	s.unflushed = 0
	return nil
}

// SyncAppend durable-writes one line for ingress ACK. Reuses the open WAL fd
// and coalesces fsync across concurrent callers (group commit).
func (s *Storage) SyncAppend(log string) error {
	s.dirty.Store(true)
	s.mu.Lock()
	if err := s.ensureOpenLocked(); err != nil {
		s.mu.Unlock()
		return fmt.Errorf("sync append open: %w", err)
	}
	if _, err := s.writer.WriteString(log + "\n"); err != nil {
		s.mu.Unlock()
		return fmt.Errorf("sync append write: %w", err)
	}
	if err := s.writer.Flush(); err != nil {
		s.mu.Unlock()
		return fmt.Errorf("sync append flush: %w", err)
	}
	s.unflushed = 0
	ticket := s.syncTicket
	s.syncTicket++
	s.mu.Unlock()

	return s.waitDurable(ticket)
}

// waitDurable blocks until an fsync has covered ticket. Concurrent waiters
// share a single Sync when possible.
func (s *Storage) waitDurable(ticket uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for s.syncedThru <= ticket {
		if s.logs == nil {
			return fmt.Errorf("sync append: storage closed")
		}
		if s.syncing {
			s.syncCond.Wait()
			continue
		}
		s.syncing = true
		gap := time.Duration(0)
		if s.durableWindow > 0 {
			gap = s.durableWindow
		}
		// Coalesce when other writers already queued tickets, or when an
		// explicit durable window is configured.
		if s.syncTicket-ticket > 1 || gap > 0 {
			if gap < walSyncGroupGap {
				gap = walSyncGroupGap
			}
			s.mu.Unlock()
			time.Sleep(gap)
			s.mu.Lock()
			if s.logs == nil {
				s.syncing = false
				s.syncCond.Broadcast()
				return fmt.Errorf("sync append: storage closed")
			}
		}
		// Cover every ticket issued so far (writes already flushed above).
		cover := s.syncTicket
		err := s.logs.Sync()
		if err == nil {
			s.syncedThru = cover
		}
		s.syncing = false
		s.syncCond.Broadcast()
		if err != nil {
			return fmt.Errorf("sync append fsync: %w", err)
		}
	}
	return nil
}

func (s *Storage) Stream(start int) <-chan string {
	stream := make(chan string)
	go func() {
		defer close(stream)

		s.mu.Lock()
		sealed := append([]string(nil), s.pendingSealed...)
		s.mu.Unlock()

		current := 0
		scanFile := func(path string) bool {
			file, err := os.Open(path)
			if err != nil {
				if !os.IsNotExist(err) {
					slog.Error("cannot read write-ahead log", "path", path, "err", err)
				}
				return true
			}
			defer file.Close()
			scanner := bufio.NewScanner(file)
			// Raise limit for long ingress lines.
			scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
			for scanner.Scan() {
				if current < start {
					current++
					continue
				}
				stream <- scanner.Text()
				current++
			}
			if err := scanner.Err(); err != nil {
				slog.Error("write-ahead log truncated on read", "path", path, "err", err)
				return false
			}
			return true
		}

		for _, path := range sealed {
			if !scanFile(path) {
				return
			}
		}
		_ = scanFile(s.logPath())
	}()
	return stream
}

func (s *Storage) Start() error {

	if !s.running.CompareAndSwap(false, true) {
		return fmt.Errorf("storage is already started")
	}
	defer s.wg.Done()

	s.mu.Lock()
	err := s.ensureOpenLocked()
	s.mu.Unlock()
	if err != nil {
		return err
	}

	ticker := time.NewTicker(walFlushEvery)
	defer ticker.Stop()

	for {
		select {
		case log := <-s.input:
			if err := s.write(log); err != nil {
				return err
			}
		case <-ticker.C:
			if err := s.flush(); err != nil {
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

	if err := s.ensureOpenLocked(); err != nil {
		return fmt.Errorf("failed to write data: %v", err)
	}
	n, err := s.writer.WriteString(log + "\n")
	if err != nil {
		return fmt.Errorf("failed to write data: %v", err)
	}
	s.unflushed += n
	if s.unflushed >= walFlushBytes {
		return s.flushLocked()
	}
	return nil
}

func (s *Storage) flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushLocked()
}

func (s *Storage) flushLocked() error {
	if s.writer == nil {
		return nil
	}
	if s.unflushed == 0 && s.writer.Buffered() == 0 {
		return nil
	}
	if err := s.writer.Flush(); err != nil {
		return fmt.Errorf("failed to flush buffer: %v", err)
	}
	s.unflushed = 0
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
	if s.writer != nil {
		_ = s.writer.Flush()
	}
	if err := s.logs.Close(); err != nil {
		slog.Warn("closing write-ahead log", "err", err)
	}
	s.logs, s.writer = nil, nil
	s.syncCond.Broadcast()
}
