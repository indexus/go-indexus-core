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
	for {
		select {
		case <-w.ctx.Done():
			return nil
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
