package worker

import (
	"context"
	"log"
	"time"
)

type Service interface {
	Delay() time.Duration
	Observe() error
	Refresh() error
	Update() error
	Feed() error
	SaveCollections() error
	ReplayOperations() error
	ClearOperationsLog() error
}

type Worker struct {
	Service Service
	ctx     context.Context
	cancel  context.CancelFunc
}

func NewWorker(service Service) *Worker {
	ctx, cancel := context.WithCancel(context.Background())
	return &Worker{
		Service: service,
		ctx:     ctx,
		cancel:  cancel,
	}
}

func (w *Worker) Feed() error {
	log.Println("Queuing system started")
	return w.Service.Feed()
}

func (w *Worker) Start() error {
	log.Println("Recurring jobs started")

	// First, replay any operations from the write-ahead log
	if err := w.Service.ReplayOperations(); err != nil {
		log.Printf("Error replaying operations: %v", err)
	}

	saveInterval := 5 * time.Minute // Save collections every 5 minutes
	saveTicker := time.NewTicker(saveInterval)
	defer saveTicker.Stop()

	for {
		select {
		case <-w.ctx.Done():
			// Save collections one last time before shutting down
			if err := w.Service.SaveCollections(); err != nil {
				log.Printf("Error saving collections during shutdown: %v", err)
			}
			return nil
		case <-saveTicker.C:
			// Save collections and clear the operations log if successful
			if err := w.Service.SaveCollections(); err != nil {
				log.Printf("Error saving collections: %v", err)
			} else {
				if err := w.Service.ClearOperationsLog(); err != nil {
					log.Printf("Error clearing operations log: %v", err)
				}
			}
		case <-time.After(w.Service.Delay()):
			if err := w.Service.Observe(); err != nil {
				return err
			}
			if err := w.Service.Refresh(); err != nil {
				return err
			}
			if err := w.Service.Update(); err != nil {
				return err
			}
		}
	}
}

func (w *Worker) Close() {
	w.cancel()
}
