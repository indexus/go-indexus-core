package storage

import (
	"bufio"
	"encoding/gob"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/indexus/go-indexus-core/domain"
)

// encName / decName ensure on-disk names are case-insensitive-safe and
// portable across filesystems (APFS default, NTFS, etc.). Both BASE64
// collection ids and ownership locations are mixed-case strings whose
// distinct values would otherwise collide on a case-insensitive FS — see
// the case-collision bug where `1uA.snapshot` and `1ua.snapshot` shared
// the same inode and silently overwrote each other.
func encName(s string) string {
	return hex.EncodeToString([]byte(s))
}

func decName(s string) (string, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Storage persists cluster metadata and per-shard (collection, ownership) WAL + snapshots.
// root is the storage directory (e.g. ./data); layout:
//
//	<root>/cluster.snapshot
//	<root>/shards/<hex(collection)>/<hex(location)>.snapshot
//	<root>/shards/<hex(collection)>/<hex(location)>.log
//
// The hex encoding for both segments avoids collisions on case-insensitive
// filesystems (the BASE64 alphabet is mixed-case).
type Storage struct {
	archiveDir string
	root       string

	mu          sync.Mutex
	shardWrites map[string]*shardWriter // key: collection\x00location
	flushEvery  time.Duration           // background flush interval; 0 disables
	quit        chan struct{}
	wg          sync.WaitGroup
}

type shardWriter struct {
	file   *os.File
	writer *bufio.Writer
}

// flushDefaultInterval bounds how long an item written via AppendShard can
// stay in the bufio buffer before being durably written to the OS page cache.
// Trade-off: larger = fewer syscalls / better throughput; smaller = lower
// data-loss window on hard kill.
const flushDefaultInterval = 200 * time.Millisecond

func NewStorage(archiveDir, root string) *Storage {
	return &Storage{
		archiveDir:  archiveDir,
		root:        root,
		shardWrites: make(map[string]*shardWriter),
		flushEvery:  flushDefaultInterval,
		quit:        make(chan struct{}),
	}
}

func shardMapKey(k domain.Key) string {
	return k.Collection + "\x00" + k.Location
}

func (s *Storage) clusterSnapshotPath() string {
	return filepath.Join(s.root, "cluster.snapshot")
}

func (s *Storage) shardsDir() string {
	return filepath.Join(s.root, "shards")
}

func (s *Storage) shardSnapshotPath(k domain.Key) string {
	return filepath.Join(s.shardsDir(), encName(k.Collection), encName(k.Location)+".snapshot")
}

func (s *Storage) shardLogPath(k domain.Key) string {
	return filepath.Join(s.shardsDir(), encName(k.Collection), encName(k.Location)+".log")
}

func (s *Storage) Exist() bool {
	_, err := os.Stat(s.clusterSnapshotPath())
	if os.IsNotExist(err) {
		return false
	}
	return err == nil
}

// Reset archives cluster.snapshot, the entire shards tree, and legacy monolithic backup files.
func (s *Storage) Reset() error {
	if err := os.MkdirAll(s.archiveDir, 0755); err != nil {
		return fmt.Errorf("failed to create archive directory: %w", err)
	}

	ts := time.Now().Format("20060102_150405")
	batch := filepath.Join(s.archiveDir, ts+"_reset")
	if err := os.MkdirAll(batch, 0755); err != nil {
		return fmt.Errorf("failed to create archive batch: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, sw := range s.shardWrites {
		_ = sw.writer.Flush()
		_ = sw.file.Close()
	}
	s.shardWrites = make(map[string]*shardWriter)

	moveInto := func(name string, src string) error {
		if _, err := os.Stat(src); os.IsNotExist(err) {
			return nil
		} else if err != nil {
			return err
		}
		dst := filepath.Join(batch, name)
		return os.Rename(src, dst)
	}

	_ = moveInto("cluster.snapshot", s.clusterSnapshotPath())
	_ = moveInto("shards", s.shardsDir())

	// Legacy single-file layout (pre per-shard).
	legacySnap := s.root + ".snapshot"
	legacyLogs := s.root + ".logs"
	_ = moveInto(filepath.Base(legacySnap), legacySnap)
	_ = moveInto(filepath.Base(legacyLogs), legacyLogs)

	return nil
}

func (s *Storage) SaveCluster(commands []string) error {
	if err := os.MkdirAll(s.root, 0755); err != nil {
		return fmt.Errorf("failed to create storage root: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	path := s.clusterSnapshotPath()
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("error creating cluster snapshot temp: %w", err)
	}
	if err := gob.NewEncoder(f).Encode(&commands); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("error encoding cluster snapshot: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("error syncing cluster snapshot: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("error renaming cluster snapshot: %w", err)
	}
	return nil
}

func (s *Storage) LoadCluster() ([]string, error) {
	path := s.clusterSnapshotPath()
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("error opening cluster snapshot %q: %w", path, err)
	}
	defer file.Close()

	var commands []string
	if err := gob.NewDecoder(file).Decode(&commands); err != nil {
		return nil, fmt.Errorf("error decoding cluster snapshot %q: %w", path, err)
	}
	return commands, nil
}

func (s *Storage) Shards() ([]domain.Key, error) {
	shardsDir := s.shardsDir()
	if _, err := os.Stat(shardsDir); os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}

	seen := make(map[string]domain.Key)
	err := filepath.WalkDir(shardsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		base := filepath.Base(path)
		collDirName := filepath.Base(filepath.Dir(path))
		coll, decErr := decName(collDirName)
		if decErr != nil {
			// Skip any file whose parent dir is not a hex-encoded
			// collection name (legacy/unknown layout).
			return nil
		}

		var locName string
		switch {
		case strings.HasSuffix(base, ".snapshot"):
			locName = strings.TrimSuffix(base, ".snapshot")
		case strings.HasSuffix(base, ".log"):
			locName = strings.TrimSuffix(base, ".log")
		default:
			return nil
		}
		loc, decErr := decName(locName)
		if decErr != nil {
			return nil
		}
		k := domain.Key{Collection: coll, Location: loc}
		seen[shardMapKey(k)] = k
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]domain.Key, 0, len(seen))
	for _, k := range seen {
		out = append(out, k)
	}
	return out, nil
}

// ShardSnapshot is the on-disk format for a shard .snapshot file.
// Header holds the Set.list at the owner level (map from child key or
// "location:id" to *Abelian), enabling fast cold-restore of aggregates
// without replaying every item. Items holds the flat Item.Content() lines
// so that a full reload can rebuild the deep-set hierarchy.
type ShardSnapshot struct {
	Header map[string]*domain.Abelian
	Items  []string
}

func readShardSnapshot(path string) (*ShardSnapshot, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var ss ShardSnapshot
	if err := gob.NewDecoder(f).Decode(&ss); err != nil {
		return nil, err
	}
	return &ss, nil
}

func readLogLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines, sc.Err()
}

func (s *Storage) LoadShardHeader(k domain.Key) (map[string]*domain.Abelian, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	ss, err := readShardSnapshot(s.shardSnapshotPath(k))
	if err != nil {
		return nil, fmt.Errorf("load shard header %v: %w", k, err)
	}
	if ss == nil {
		return nil, nil
	}
	return ss.Header, nil
}

func (s *Storage) LoadShard(k domain.Key) (header map[string]*domain.Abelian, items []string, logs []string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Make sure any in-flight buffered WAL writes for this shard are visible
	// to the reader; otherwise EnsureLoaded would replay a stale tail.
	s.flushShardLocked(k)

	ss, err := readShardSnapshot(s.shardSnapshotPath(k))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load shard snapshot %v: %w", k, err)
	}
	if ss != nil {
		header = ss.Header
		items = ss.Items
	}
	logs, err = readLogLines(s.shardLogPath(k))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load shard log %v: %w", k, err)
	}
	return header, items, logs, nil
}

func (s *Storage) closeShardWriterLocked(sk string) {
	sw, ok := s.shardWrites[sk]
	if !ok {
		return
	}
	_ = sw.writer.Flush()
	_ = sw.file.Close()
	delete(s.shardWrites, sk)
}

func (s *Storage) getShardWriterLocked(k domain.Key) (*shardWriter, error) {
	sk := shardMapKey(k)
	if sw, ok := s.shardWrites[sk]; ok {
		return sw, nil
	}
	if err := os.MkdirAll(filepath.Dir(s.shardLogPath(k)), 0755); err != nil {
		return nil, err
	}
	logPath := s.shardLogPath(k)
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	sw := &shardWriter{file: f, writer: bufio.NewWriter(f)}
	s.shardWrites[sk] = sw
	return sw, nil
}

// AppendShard writes a WAL line to the shard's bufio buffer. The buffer is
// flushed to the OS by either: the periodic flusher (Start), the next read
// of the same shard (LoadShard / LoadShardHeader), or a snapshot/drop/close.
// We deliberately do NOT flush per-line — that turned every item insert into
// a syscall and saturated I/O under load.
func (s *Storage) AppendShard(k domain.Key, line string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sw, err := s.getShardWriterLocked(k)
	if err != nil {
		fmt.Printf("storage AppendShard %v: %v\n", k, err)
		return
	}
	if _, err := sw.writer.WriteString(line); err != nil {
		fmt.Printf("storage AppendShard write %v: %v\n", k, err)
		return
	}
	if err := sw.writer.WriteByte('\n'); err != nil {
		fmt.Printf("storage AppendShard write %v: %v\n", k, err)
	}
}

// flushShardLocked flushes any buffered WAL data for the given shard so that
// a subsequent reader observes a consistent view. Called by load helpers.
func (s *Storage) flushShardLocked(k domain.Key) {
	sw, ok := s.shardWrites[shardMapKey(k)]
	if !ok {
		return
	}
	_ = sw.writer.Flush()
}

// flushAllLocked flushes every open shard writer; used by the periodic
// flusher to bound the in-buffer data-loss window.
func (s *Storage) flushAllLocked() {
	for _, sw := range s.shardWrites {
		_ = sw.writer.Flush()
	}
}

func (s *Storage) SnapshotShard(k domain.Key, header map[string]*domain.Abelian, items []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(s.shardSnapshotPath(k)), 0755); err != nil {
		return err
	}

	ss := ShardSnapshot{Header: header, Items: items}
	snapPath := s.shardSnapshotPath(k)
	tmp := snapPath + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("snapshot shard create temp: %w", err)
	}
	if err := gob.NewEncoder(f).Encode(&ss); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("snapshot shard encode: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, snapPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("snapshot shard rename: %w", err)
	}

	sk := shardMapKey(k)
	s.closeShardWriterLocked(sk)

	logPath := s.shardLogPath(k)
	if err := os.Remove(logPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("snapshot shard remove log: %w", err)
	}
	return nil
}

func (s *Storage) DropShard(k domain.Key) error {
	if err := os.MkdirAll(s.archiveDir, 0755); err != nil {
		return err
	}
	ts := time.Now().Format("20060102_150405")
	destDir := filepath.Join(s.archiveDir, fmt.Sprintf("%s_drop_%s_%s", ts, k.Collection, k.Location))
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	sk := shardMapKey(k)
	s.closeShardWriterLocked(sk)

	snapPath := s.shardSnapshotPath(k)
	logPath := s.shardLogPath(k)

	if err := moveFileIfExists(snapPath, filepath.Join(destDir, filepath.Base(snapPath))); err != nil {
		return err
	}
	if err := moveFileIfExists(logPath, filepath.Join(destDir, filepath.Base(logPath))); err != nil {
		return err
	}

	// Remove empty collection directory if possible.
	_ = os.Remove(filepath.Dir(snapPath))
	return nil
}

func moveFileIfExists(src, dst string) error {
	if _, err := os.Stat(src); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	return os.Rename(src, dst)
}

// Start runs the periodic flusher and blocks until Close. On shutdown it
// flushes and closes every open shard log writer.
func (s *Storage) Start() error {
	s.wg.Add(1)
	defer s.wg.Done()

	var ticker *time.Ticker
	var tick <-chan time.Time
	if s.flushEvery > 0 {
		ticker = time.NewTicker(s.flushEvery)
		defer ticker.Stop()
		tick = ticker.C
	}

	for {
		select {
		case <-tick:
			s.mu.Lock()
			s.flushAllLocked()
			s.mu.Unlock()
		case <-s.quit:
			s.mu.Lock()
			for sk, sw := range s.shardWrites {
				_ = sw.writer.Flush()
				_ = sw.file.Close()
				delete(s.shardWrites, sk)
			}
			s.mu.Unlock()
			return nil
		}
	}
}

func (s *Storage) Close() {
	close(s.quit)
	s.wg.Wait()
}
