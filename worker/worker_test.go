package worker

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// failingService fails every job, every tick, and signals once every job has
// run enough times to prove the loops survive.
type failingService struct {
	mu        sync.Mutex
	observe   int
	refresh   int
	update    int
	autoscale int
	target    int
	closed    bool
	reached   chan struct{}
}

func newFailingService(target int) *failingService {
	return &failingService{target: target, reached: make(chan struct{})}
}

func (s *failingService) Delay() time.Duration { return time.Millisecond }

// signal closes reached once every job has hit the target. Each loop runs on
// its own cadence, so no single job can stand in for the others.
func (s *failingService) signal() {
	if s.closed || s.observe < s.target || s.refresh < s.target ||
		s.update < s.target || s.autoscale < s.target {
		return
	}
	s.closed = true
	close(s.reached)
}

func (s *failingService) Observe() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.observe++
	s.signal()
	return errors.New("peer unreachable")
}

func (s *failingService) Refresh() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.refresh++
	s.signal()
	return errors.New("save failed")
}

func (s *failingService) Update() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.update++
	s.signal()
	return errors.New("cache refresh failed")
}

func (s *failingService) Feed() error { return nil }

func (s *failingService) AutoscaleTick() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.autoscale++
	s.signal()
}

func (s *failingService) counts() (int, int, int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.observe, s.refresh, s.update, s.autoscale
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

	// A failing job must not stop any of the others, including the loops that
	// run beside them.
	observe, refresh, update, autoscale := service.counts()
	if observe < 3 || refresh < 3 || update < 3 || autoscale < 3 {
		t.Fatalf("jobs ran observe=%d refresh=%d update=%d autoscale=%d, want at least 3 each",
			observe, refresh, update, autoscale)
	}
}

// slowService holds Refresh for as long as the test wants, the way handing
// zones to a loaded peer does.
type slowService struct {
	release   chan struct{}
	ticked    chan struct{}
	updated   chan struct{}
	autoscale int
	update    int
	mu        sync.Mutex
}

func (s *slowService) Delay() time.Duration { return time.Millisecond }
func (s *slowService) Observe() error       { return nil }
func (s *slowService) Feed() error          { return nil }

func (s *slowService) Refresh() error {
	<-s.release
	return nil
}

func (s *slowService) Update() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.update++
	if s.update == 3 && s.updated != nil {
		close(s.updated)
		s.updated = nil
	}
	return nil
}

func (s *slowService) AutoscaleTick() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.autoscale++
	if s.autoscale == 3 {
		close(s.ticked)
	}
}

// A rebalance that takes minutes must not hold the scaling decision with it:
// the node reads its own pressure on that tick, and a reading from before the
// handover is what it would act on next.
func TestAutoscaleTicksDuringASlowRebalance(t *testing.T) {
	service := &slowService{release: make(chan struct{}), ticked: make(chan struct{})}
	worker := NewWorker(service)
	defer worker.Close()

	go func() { _ = worker.Start() }()

	select {
	case <-service.ticked:
	case <-time.After(2 * time.Second):
		t.Fatal("autoscale never ticked while refresh was busy")
	}
	close(service.release)
}

// Soft-cache refresh must keep advancing while Refresh is blocked on a slow
// peer transfer — Update used to share Refresh's tick and froze with it.
func TestUpdateTicksDuringASlowRebalance(t *testing.T) {
	service := &slowService{
		release: make(chan struct{}),
		ticked:  make(chan struct{}),
		updated: make(chan struct{}),
	}
	worker := NewWorker(service)
	defer worker.Close()

	go func() { _ = worker.Start() }()

	select {
	case <-service.updated:
	case <-time.After(2 * time.Second):
		t.Fatal("update never ticked while refresh was busy")
	}
	close(service.release)
}
