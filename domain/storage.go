package domain

type Storage interface {
	Exist() bool
	Reset() error
	Save([]string) error
	Load() ([]string, error)
	Append(string)
	Stream(int) <-chan string
}
