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
	quit        chan struct{}
	wg          sync.WaitGroup
}

type shardWriter struct {
	file   *os.File
	writer *bufio.Writer
}

func NewStorage(archiveDir, root string) *Storage {
	return &Storage{
		archiveDir:  archiveDir,
		root:        root,
		shardWrites: make(map[string]*shardWriter),
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

func readSnapshotFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var lines []string
	if err := gob.NewDecoder(f).Decode(&lines); err != nil {
		return nil, err
	}
	return lines, nil
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

func (s *Storage) LoadShard(k domain.Key) (snapshot []string, logs []string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	snapPath := s.shardSnapshotPath(k)
	logPath := s.shardLogPath(k)

	snapshot, err = readSnapshotFile(snapPath)
	if err != nil {
		return nil, nil, fmt.Errorf("load shard snapshot %v: %w", k, err)
	}
	logs, err = readLogLines(logPath)
	if err != nil {
		return nil, nil, fmt.Errorf("load shard log %v: %w", k, err)
	}
	return snapshot, logs, nil
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

func (s *Storage) AppendShard(k domain.Key, line string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	sw, err := s.getShardWriterLocked(k)
	if err != nil {
		fmt.Printf("storage AppendShard %v: %v\n", k, err)
		return
	}
	if _, err := sw.writer.WriteString(line + "\n"); err != nil {
		fmt.Printf("storage AppendShard write %v: %v\n", k, err)
		return
	}
	if err := sw.writer.Flush(); err != nil {
		fmt.Printf("storage AppendShard flush %v: %v\n", k, err)
	}
}

func (s *Storage) SnapshotShard(k domain.Key, items []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(s.shardSnapshotPath(k)), 0755); err != nil {
		return err
	}

	snapPath := s.shardSnapshotPath(k)
	tmp := snapPath + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("snapshot shard create temp: %w", err)
	}
	if err := gob.NewEncoder(f).Encode(&items); err != nil {
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

// Start blocks until Close; on shutdown it flushes and closes open shard log writers.
func (s *Storage) Start() error {
	s.wg.Add(1)
	defer s.wg.Done()

	<-s.quit

	s.mu.Lock()
	defer s.mu.Unlock()

	for sk := range s.shardWrites {
		sw := s.shardWrites[sk]
		_ = sw.writer.Flush()
		_ = sw.file.Close()
		delete(s.shardWrites, sk)
	}
	return nil
}

func (s *Storage) Close() {
	close(s.quit)
	s.wg.Wait()
}
