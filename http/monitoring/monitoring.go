package monitoring

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/logging"
)

// Service is the part of the node the monitoring endpoints report on.
type Service interface {
	Name() string
	Acknowledged() ([]domain.Contact, error)
	Registered() ([]domain.Contact, error)
	Routing() ([]domain.Contact, error)
	Ownership() (map[string]map[string]map[string]any, error)
	OwnershipStats() (collections, zones int)
	Queue() int
	IngressStats() map[string]any
	Items() int
	ItemsPreparing() int
	Leaving() bool
	Leave(timeoutSeconds int) any
	AutoscaleSnapshot() map[string]any
	ClientReady() bool
	Rebalancing() bool
	SnapshotInfo() map[string]any
	Checkpoint() error
	ListSnapshots(ctx context.Context) (map[string]any, error)
	ClearSnapshots(ctx context.Context) (map[string]any, error)
}

type Handler struct {
	service Service
	version string
	tlsDir  string
	started time.Time
	server  *http.Server
}

// NewHttpHandler wires the monitoring endpoints. A non-empty tlsDir must hold
// server.crt and server.key.
func NewHttpHandler(tlsDir string, service Service, version string) *Handler {
	handler := &Handler{
		service: service,
		version: version,
		tlsDir:  tlsDir,
		started: time.Now(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", handler.Health)
	mux.HandleFunc("/status", handler.Status)
	mux.HandleFunc("/acknowledged", hosts(service.Acknowledged))
	mux.HandleFunc("/registered", hosts(service.Registered))
	mux.HandleFunc("/routing", hosts(service.Routing))
	mux.HandleFunc("/ownership", handler.Ownership)
	mux.HandleFunc("/queue", handler.Queue)
	mux.HandleFunc("/count", handler.Count)
	mux.HandleFunc("/leave", handler.Leave)
	mux.HandleFunc("/autoscale", handler.Autoscale)
	mux.HandleFunc("/checkpoint", handler.Checkpoint)
	mux.HandleFunc("/snapshots", handler.Snapshots)
	mux.HandleFunc("/snapshots/clear", handler.SnapshotsClear)

	handler.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       time.Minute,
		ErrorLog:          logging.StdLogger(slog.LevelWarn),
	}

	return handler
}

// Serve runs until Shutdown is called, then returns http.ErrServerClosed.
func (h *Handler) Serve(lis net.Listener) error {
	slog.Info("monitoring server listening", "addr", lis.Addr().String(), "tls", h.tlsDir != "")

	if h.tlsDir != "" {
		return h.server.ServeTLS(lis, h.tlsDir+"/server.crt", h.tlsDir+"/server.key")
	}
	return h.server.Serve(lis)
}

// Shutdown stops accepting connections and waits for in-flight requests.
func (h *Handler) Shutdown(ctx context.Context) error {
	return h.server.Shutdown(ctx)
}

// Health reports whether the process can serve traffic. A draining node answers
// 503 so load balancers stop routing to it before it exits.
func (h *Handler) Health(w http.ResponseWriter, r *http.Request) {
	if h.service.Leaving() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "leaving"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Status is the single call an operator needs: what runs, since when, how much
// work is pending and how much of the ring this node holds. It reports the item
// total as last measured by the background pass rather than counting inline, so
// a loaded node still answers; /count is there for an exact figure.
//
// Zone/collection counts come from OwnershipStats (XOR owned index), not
// Ownership()/Browse, so /status never contends on Collection.mu with the
// protocol path (Get, Transfer, Count, applyZone).
func (h *Handler) Status(w http.ResponseWriter, r *http.Request) {
	collections, zones := h.service.OwnershipStats()
	peers, err := h.service.Registered()
	if err != nil {
		writeError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"name":         h.service.Name(),
		"version":      h.version,
		"uptime_s":     int64(time.Since(h.started).Seconds()),
		"queue":        h.service.Queue(),
		"peers":        len(peers),
		"collections":  collections,
		"zones":        zones,
		"items":        h.service.Items(),
		"items_prep":   h.service.ItemsPreparing(),
		"leaving":      h.service.Leaving(),
		"client_ready": h.service.ClientReady(),
		"rebalancing":  h.service.Rebalancing(),
		"autoscale":    h.service.AutoscaleSnapshot(),
		"snapshot":     h.service.SnapshotInfo(),
	})
}

// Ownership lists the zones this node answers for (cached snapshot; never
// waits on Collection.mu — may be briefly stale during exclusive writes).
func (h *Handler) Ownership(w http.ResponseWriter, r *http.Request) {
	body, err := h.service.Ownership()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// Queue reports the ingress backlog and, when it is not draining, the errors
// holding it there.
func (h *Handler) Queue(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.service.IngressStats())
}

// Count reports the last background MeasureItems totals (same atomics as
// /status). It does not walk the collection on the request path.
func (h *Handler) Count(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"count":      h.service.Items(),
		"items_prep": h.service.ItemsPreparing(),
	})
}

// Leave hands every owned zone over to peers, then snapshots. The autoscaler
// calls it before terminating an instance.
func (h *Handler) Leave(w http.ResponseWriter, r *http.Request) {
	timeout := 90
	if raw := r.URL.Query().Get("timeout_s"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "timeout_s must be a positive integer"})
			return
		}
		timeout = parsed
	}

	writeJSON(w, http.StatusOK, h.service.Leave(timeout))
}

// Autoscale exposes the insert window and the hysteresis state behind scaling.
func (h *Handler) Autoscale(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.service.AutoscaleSnapshot())
}

// Checkpoint flushes a local snapshot when the WAL is dirty, then zone snaps.
func (h *Handler) Checkpoint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := h.service.Checkpoint(); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"info": h.service.SnapshotInfo(),
	})
}

// Snapshots lists object-store keys under zones/, nodes/, and snapshots/.
func (h *Handler) Snapshots(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := h.service.ListSnapshots(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// SnapshotsClear deletes object-store keys under zones/, nodes/, and snapshots/.
func (h *Handler) SnapshotsClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := h.service.ClearSnapshots(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// hosts renders one of the routing tables as a list of hosts.
func hosts(list func() ([]domain.Contact, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		contacts, err := list()
		if err != nil {
			writeError(w, err)
			return
		}

		body := make([]string, 0, len(contacts))
		for _, contact := range contacts {
			body = append(body, contact.Host())
		}
		writeJSON(w, http.StatusOK, map[string][]string{"hosts": body})
	}
}

func writeJSON(w http.ResponseWriter, code int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(data)
}

func writeError(w http.ResponseWriter, err error) {
	writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
}
