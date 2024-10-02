package mockup

type Storage struct {
}

func NewStorage() *Storage {

	storage := &Storage{}

	return storage
}

func (s *Storage) Exist() bool {
	return true
}

func (s *Storage) Reset() error {
	return nil
}

func (s *Storage) Save(commands []string) error {
	return nil
}

func (s *Storage) Load() ([]string, error) {
	return []string{}, nil
}

func (s *Storage) Append(log string) {
}

func (s *Storage) Stream(start int) <-chan string {
	stream := make(chan string)
	go func() {
		defer close(stream)
	}()
	return stream
}
