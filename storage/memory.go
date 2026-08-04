package storage

// Memory is a storage that keeps nothing. It lets a node run without a
// write-ahead log, which is what simulations and throwaway instances want.
type Memory struct{}

func NewMemory() *Memory {
	return &Memory{}
}

func (m *Memory) Exist() bool { return true }

func (m *Memory) Reset() error { return nil }

func (m *Memory) Save([]string) error { return nil }

func (m *Memory) Load() ([]string, error) { return []string{}, nil }

func (m *Memory) Append(string) {}

func (m *Memory) SyncAppend(string) error { return nil }

func (m *Memory) Stream(int) <-chan string {
	stream := make(chan string)
	close(stream)
	return stream
}
