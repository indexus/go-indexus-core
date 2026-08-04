package core

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

func (n *Node) Checkpoint() error {

	n.checkpointMu.Lock()
	defer n.checkpointMu.Unlock()

	if err := n.storage.Save(n.Snapshot()); err != nil {
		return err
	}
	n.uploadLatestSnapshot()
	if err := n.checkpointZones(context.Background()); err != nil {
		slog.Warn("zone checkpoint failed", "err", err)
	}
	return nil
}

func (n *Node) uploadLatestSnapshot() {
	bucket := os.Getenv("SNAPSHOT_BUCKET")
	if bucket == "" {
		return
	}
	path := storageSnapshotPath()
	if path == "" {
		return
	}
	if st, err := os.Stat(path); err != nil || st.Size() == 0 {
		return
	}
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "eu-west-3"
	}
	key := fmt.Sprintf("snapshots/%s/latest/%s", n.Name(), filepath.Base(path))
	dest := fmt.Sprintf("s3://%s/%s", bucket, key)
	out, err := exec.Command("aws", "s3", "cp", path, dest, "--region", region).CombinedOutput()
	if err != nil {
		slog.Warn("latest snapshot not uploaded", "dest", dest, "err", err, "output", string(out))
		return
	}
	slog.Info("latest snapshot uploaded", "dest", dest)
}

func storageSnapshotPath() string {
	base := os.Getenv("INDEXUS_STORAGE")
	if base == "" {
		base = storageFilename()
	}
	return base + ".snapshot"
}

func PullLatestSnapshot(fromNode, storagePrefix string) error {
	if err := pullManifestLayout(fromNode, storagePrefix); err == nil {
		return nil
	}
	return pullLegacyLatest(fromNode, storagePrefix)
}

func pullManifestLayout(fromNode, storagePrefix string) error {
	ctx := context.Background()
	store, err := NewS3Store(ctx)
	if err != nil || store == nil {
		return fmt.Errorf("no s3 store")
	}
	man, err := pullManifest(ctx, store, fromNode)
	if err != nil {
		return err
	}

	lines := make([]string, 0)
	for _, z := range man.Zones {
		body, err := store.Get(ctx, z.Key)
		if err != nil {
			slog.Warn("skip zone snap", "key", z.Key, "err", err)
			continue
		}
		zoneLines, err := gobDecodeStrings(body)
		if err != nil {
			continue
		}
		lines = append(lines, zoneLines...)
	}
	if len(lines) == 0 {
		return fmt.Errorf("manifest had no usable zones")
	}
	if err := os.MkdirAll(filepath.Dir(storagePrefix), 0o755); err != nil {
		return err
	}
	snapPath := storagePrefix + ".snapshot"
	encoded, err := gobEncodeStrings(lines)
	if err != nil {
		return err
	}
	if err := os.WriteFile(snapPath, encoded, 0o644); err != nil {
		return err
	}

	logPath := storagePrefix + ".logs"
	logFile, err := os.Create(logPath)
	if err != nil {
		return err
	}
	defer logFile.Close()
	for _, seg := range man.WALSegs {
		body, err := store.Get(ctx, seg)
		if err != nil {
			continue
		}
		_, _ = logFile.Write(body)
		if len(body) > 0 && body[len(body)-1] != '\n' {
			_, _ = logFile.Write([]byte("\n"))
		}
	}
	slog.Info("restored from zone manifest", "from", fromNode, "zones", len(man.Zones), "wal_segs", len(man.WALSegs))
	return nil
}

func pullLegacyLatest(fromNode, storagePrefix string) error {
	bucket := os.Getenv("SNAPSHOT_BUCKET")
	if bucket == "" || fromNode == "" || storagePrefix == "" {
		return nil
	}
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "eu-west-3"
	}
	if err := os.MkdirAll(filepath.Dir(storagePrefix), 0o755); err != nil {
		return err
	}
	src := fmt.Sprintf("s3://%s/snapshots/%s/latest/", bucket, fromNode)
	destDir := filepath.Dir(storagePrefix)
	base := filepath.Base(storagePrefix)
	tmp := filepath.Join(destDir, ".restore-"+base)
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	out, err := exec.Command("aws", "s3", "sync", src, tmp, "--region", region).CombinedOutput()
	if err != nil {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("pull %s: %w (%s)", src, err, string(out))
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	if len(entries) == 0 {
		_ = os.RemoveAll(tmp)
		return fmt.Errorf("no snapshot objects under %s", src)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		var target string
		switch {
		case name == "backup.snapshot" || filepath.Ext(name) == ".snapshot":
			target = storagePrefix + ".snapshot"
		case name == "backup.logs" || filepath.Ext(name) == ".logs":
			target = storagePrefix + ".logs"
		default:
			continue
		}
		if err := os.Rename(filepath.Join(tmp, name), target); err != nil {
			_ = os.RemoveAll(tmp)
			return err
		}
	}
	_ = os.RemoveAll(tmp)
	slog.Info("restored snapshot from peer", "from", fromNode, "prefix", storagePrefix, "at", time.Now().UTC())
	return nil
}
