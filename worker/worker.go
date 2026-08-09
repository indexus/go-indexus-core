package worker

import (
	"context"
	"log/slog"
	"time"
)

type Service interface {
	Delay() time.Duration
	Observe() error
	Refresh() error
	Update() error
	Feed() error
}

// OptionalAutoscale is implemented by nodes that decide on scaling themselves.
type OptionalAutoscale interface {
	AutoscaleTick()
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
	slog.Info("ingress worker started")
	return w.Service.Feed()
}

func (w *Worker) Start() error {
	slog.Info("background jobs started", "interval", w.Service.Delay())

	// Refresh hands zones to peers over the network and can spend minutes on
	// ones that stopped answering. Anything sharing its tick stops for as long
	// as it runs: liveness would go on routing writes to peers it should have
	// dropped, the soft-cache would freeze mid-rebalance, and the node would
	// hold a pressure reading from before the handover while memory keeps
	// climbing — blind exactly when it has to decide whether to ask for help.
	go w.every("observe", w.Service.Observe)
	go w.every("update", w.Service.Update)
	if autoscale, ok := w.Service.(OptionalAutoscale); ok {
		go w.every("autoscale", func() error {
			autoscale.AutoscaleTick()
			return nil
		})
	}

	// A job failure is almost always transient — an unreachable peer, a full
	// queue. Returning here would stop rebalancing for good while the node
	// keeps answering requests.
	w.every("rebalance", w.Service.Refresh)
	return nil
}

// every runs job on the service cadence until the worker is closed, logging
// failures instead of giving up on the loop.
//
// The first pass runs immediately: joiners must Ping their bootstrap without
// waiting a full jobInterval, or the bootstrap stays at peers=1 until the
// first Observe tick (and /routing stays empty until clean()).
func (w *Worker) every(name string, job func() error) {
	run := func() {
		if err := job(); err != nil {
			slog.Warn(name+" failed", "err", err)
		}
	}
	run()
	for {
		select {
		case <-w.ctx.Done():
			return
		case <-time.After(w.Service.Delay()):
			run()
		}
	}
}

func (w *Worker) Close() {
	w.cancel()
}
