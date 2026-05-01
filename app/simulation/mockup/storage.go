package mockup

import "github.com/indexus/go-indexus-core/domain"

type Storage struct {
}

func NewStorage() *Storage {

	storage := &Storage{}

	return storage
}

func (s *Storage) Exist() bool {
	return false
}

func (s *Storage) Reset() error {
	return nil
}

func (s *Storage) SaveCluster([]string) error {
	return nil
}

func (s *Storage) LoadCluster() ([]string, error) {
	return nil, nil
}

func (s *Storage) Shards() ([]domain.Key, error) {
	return nil, nil
}

func (s *Storage) LoadShardHeader(domain.Key) (map[string]*domain.Abelian, error) {
	return nil, nil
}

func (s *Storage) LoadShard(domain.Key) (map[string]*domain.Abelian, []string, []string, error) {
	return nil, nil, nil, nil
}

func (s *Storage) SnapshotShard(domain.Key, map[string]*domain.Abelian, []string) error {
	return nil
}

func (s *Storage) AppendShard(domain.Key, string) {
}

func (s *Storage) DropShard(domain.Key) error {
	return nil
}
