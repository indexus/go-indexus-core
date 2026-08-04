package core

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type Store interface {
	Put(ctx context.Context, key string, body []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
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
