package worker

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// failingService fails every job, every tick, and signals once it has been
// ticked enough times to prove the loop survives.
type failingService struct {
	mu      sync.Mutex
	observe int
	refresh int
	update  int
	ticks   int
	target  int
	reached chan struct{}
}

func newFailingService(target int) *failingService {
	return &failingService{target: target, reached: make(chan struct{})}
}

func (s *failingService) Delay() time.Duration { return time.Millisecond }

func (s *failingService) Observe() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.observe++
	return errors.New("peer unreachable")
}

func (s *failingService) Refresh() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.refresh++
	return errors.New("save failed")
}

func (s *failingService) Update() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.update++
	s.ticks++
	if s.ticks == s.target {
		close(s.reached)
	}
	return errors.New("cache refresh failed")
}

func (s *failingService) Feed() error { return nil }

func (s *failingService) counts() (int, int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.observe, s.refresh, s.update
}

// Job errors are transient in practice: an unreachable peer, a full queue.
// Stopping the loop on the first one silently freezes rebalancing and cache
// refresh while the node keeps serving requests.
func TestStartSurvivesFailingJobs(t *testing.T) {
	service := newFailingService(3)
	worker := NewWorker(service)

	stopped := make(chan error, 1)
	go func() { stopped <- worker.Start() }()

	select {
	case <-service.reached:
	case err := <-stopped:
		t.Fatalf("worker stopped after a job error: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not keep ticking")
	}

	worker.Close()

	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("Start returned %v, want nil on Close", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Start did not return after Close")
	}

	// A failing Observe must not skip the rest of the tick.
	observe, refresh, update := service.counts()
	if observe < 3 || refresh < 3 || update < 3 {
		t.Fatalf("jobs ran observe=%d refresh=%d update=%d, want at least 3 each",
			observe, refresh, update)
	}
}
