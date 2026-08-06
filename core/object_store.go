package core

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// ObjectInfo is a lightweight listing entry for an object-store key.
type ObjectInfo struct {
	Key          string    `json:"key"`
	Size         int64     `json:"size"`
	LastModified time.Time `json:"last_modified,omitempty"`
}

type Store interface {
	Put(ctx context.Context, key string, body []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
	List(ctx context.Context, prefix string) ([]ObjectInfo, error)
	DeletePrefix(ctx context.Context, prefix string) error
}

type MemStore struct {
	mu   sync.Mutex
	data map[string][]byte
}

func NewMemStore() *MemStore {
	return &MemStore{data: make(map[string][]byte)}
}

func (m *MemStore) Put(_ context.Context, key string, body []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]byte, len(body))
	copy(cp, body)
	m.data[key] = cp
	return nil
}

func (m *MemStore) Get(_ context.Context, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	body, ok := m.data[key]
	if !ok {
		return nil, fmt.Errorf("object not found: %s", key)
	}
	cp := make([]byte, len(body))
	copy(cp, body)
	return cp, nil
}

func (m *MemStore) List(_ context.Context, prefix string) ([]ObjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ObjectInfo, 0)
	for k, v := range m.data {
		if prefix == "" || strings.HasPrefix(k, prefix) {
			out = append(out, ObjectInfo{Key: k, Size: int64(len(v))})
		}
	}
	return out, nil
}

func (m *MemStore) DeletePrefix(_ context.Context, prefix string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range m.data {
		if prefix == "" || strings.HasPrefix(k, prefix) {
			delete(m.data, k)
		}
	}
	return nil
}

type S3Store struct {
	client *s3.Client
	bucket string
}

func NewS3Store(ctx context.Context) (*S3Store, error) {
	bucket := os.Getenv("SNAPSHOT_BUCKET")
	if bucket == "" {
		return nil, nil
	}
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "eu-west-3"
	}
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}
	return &S3Store{client: s3.NewFromConfig(cfg), bucket: bucket}, nil
}

func (s *S3Store) Put(ctx context.Context, key string, body []byte) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   bytes.NewReader(body),
	})
	return err
}

func (s *S3Store) Get(ctx context.Context, key string) ([]byte, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	defer out.Body.Close()
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(out.Body); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (s *S3Store) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	var out []ObjectInfo
	var token *string
	for {
		resp, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: token,
		})
		if err != nil {
			return nil, err
		}
		for _, obj := range resp.Contents {
			info := ObjectInfo{Key: aws.ToString(obj.Key), Size: aws.ToInt64(obj.Size)}
			if obj.LastModified != nil {
				info.LastModified = *obj.LastModified
			}
			out = append(out, info)
		}
		if !aws.ToBool(resp.IsTruncated) {
			break
		}
		token = resp.NextContinuationToken
	}
	return out, nil
}

func (s *S3Store) DeletePrefix(ctx context.Context, prefix string) error {
	objs, err := s.List(ctx, prefix)
	if err != nil {
		return err
	}
	for i := 0; i < len(objs); i += 1000 {
		end := i + 1000
		if end > len(objs) {
			end = len(objs)
		}
		batch := make([]types.ObjectIdentifier, 0, end-i)
		for _, o := range objs[i:end] {
			batch = append(batch, types.ObjectIdentifier{Key: aws.String(o.Key)})
		}
		if len(batch) == 0 {
			continue
		}
		_, err := s.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String(s.bucket),
			Delete: &types.Delete{Objects: batch, Quiet: aws.Bool(true)},
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func (n *Node) SetStore(store Store) {
	if n == nil {
		return
	}
	n.storeMu.Lock()
	defer n.storeMu.Unlock()
	n.objectStore = store
	if store != nil {
		slog.Info("zone object store attached")
	}
}

func (n *Node) Store() Store {
	if n == nil {
		return nil
	}
	n.storeMu.Lock()
	defer n.storeMu.Unlock()
	return n.objectStore
}

func putFile(ctx context.Context, store Store, key, path string) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return store.Put(ctx, key, body)
}

// DirStore is a filesystem-backed ObjectStore. All mesh nodes that share the
// same SNAPSHOT_DIR (or INDEXUS_SNAPSHOT_DIR) can hand off zone snapshots via
// the S3 delegation path without crossing the network for the bulk payload.
type DirStore struct {
	root string
}

// NewDirStore returns a DirStore when SNAPSHOT_DIR or INDEXUS_SNAPSHOT_DIR is
// set; otherwise (nil, nil). Keys are stored as files under root/<key>.
func NewDirStore() (*DirStore, error) {
	root := os.Getenv("SNAPSHOT_DIR")
	if root == "" {
		root = os.Getenv("INDEXUS_SNAPSHOT_DIR")
	}
	if root == "" {
		return nil, nil
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("snapshot dir %q: %w", root, err)
	}
	return &DirStore{root: root}, nil
}

func (d *DirStore) pathFor(key string) (string, error) {
	cleaned := filepath.Clean("/" + key)
	cleaned = strings.TrimPrefix(cleaned, "/")
	if cleaned == "" || cleaned == "." || strings.HasPrefix(cleaned, "..") {
		return "", fmt.Errorf("invalid object key %q", key)
	}
	full := filepath.Join(d.root, filepath.FromSlash(cleaned))
	// Stay inside root even if key had ".." segments after FromSlash.
	rel, err := filepath.Rel(d.root, full)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("object key escapes root: %q", key)
	}
	return full, nil
}

func (d *DirStore) Put(_ context.Context, key string, body []byte) error {
	full, err := d.pathFor(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	tmp := full + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, full)
}

func (d *DirStore) Get(_ context.Context, key string) ([]byte, error) {
	full, err := d.pathFor(key)
	if err != nil {
		return nil, err
	}
	body, err := os.ReadFile(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("object not found: %s", key)
		}
		return nil, err
	}
	return body, nil
}

func (d *DirStore) List(_ context.Context, prefix string) ([]ObjectInfo, error) {
	var out []ObjectInfo
	err := filepath.WalkDir(d.root, func(path string, de os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if de.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(d.root, path)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(rel)
		if strings.HasSuffix(key, ".tmp") {
			return nil
		}
		if prefix != "" && !strings.HasPrefix(key, prefix) {
			return nil
		}
		info, err := de.Info()
		if err != nil {
			return nil
		}
		out = append(out, ObjectInfo{
			Key:          key,
			Size:         info.Size(),
			LastModified: info.ModTime(),
		})
		return nil
	})
	return out, err
}

func (d *DirStore) DeletePrefix(_ context.Context, prefix string) error {
	objs, err := d.List(context.Background(), prefix)
	if err != nil {
		return err
	}
	for _, o := range objs {
		full, err := d.pathFor(o.Key)
		if err != nil {
			continue
		}
		_ = os.Remove(full)
	}
	return nil
}

func (d *DirStore) Root() string {
	if d == nil {
		return ""
	}
	return d.root
}
