package monitoring

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"

	"github.com/indexus/go-indexus-core/domain"
)

type Service interface {
	Acknowledged() ([]domain.Contact, error)
	Registered() ([]domain.Contact, error)
	Routing() ([]domain.Contact, error)
	Ownership() (map[string]map[string]map[string]any, error)
	Queue() int
}

type Handler struct {
	SSLStorage string
	Service    Service
}

// New - Create a HTTP handler
func NewHttpHandler(sslStorage string, service Service) *Handler {
	return &Handler{
		SSLStorage: sslStorage,
		Service:    service,
	}
}

// Serve - Run the HTTP server
func (h *Handler) Serve(lis net.Listener) error {

	mux := http.NewServeMux()

	// Monitoring
	mux.HandleFunc("/acknowledged", h.Acknowledged)
	mux.HandleFunc("/registered", h.Registered)
	mux.HandleFunc("/routing", h.Routing)
	mux.HandleFunc("/ownership", h.Ownership)
	mux.HandleFunc("/queue", h.Queue)

	s := &http.Server{Handler: mux}

	if len(h.SSLStorage) > 0 {

		log.Println("Monitoring HTTPS Server started")
		return s.ServeTLS(lis, fmt.Sprintf("%s/server.crt", h.SSLStorage), fmt.Sprintf("%s/server.key", h.SSLStorage))
	}

	log.Println("Monitoring HTTP Server started")
	return s.Serve(lis)
}

func writeJSON(w http.ResponseWriter, code int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(data)
}

// Acknowledged handles the /acknowledged endpoint
func (h *Handler) Acknowledged(w http.ResponseWriter, r *http.Request) {
	log.Printf("[Monitoring] GET /acknowledged from %s", r.RemoteAddr)
	contacts, err := h.Service.Acknowledged()
	if err != nil {
		log.Printf("[Monitoring] Error in /acknowledged: %v", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}

	var body = struct {
		Hosts []string `json:"hosts"`
	}{
		[]string{},
	}
	for _, contact := range contacts {
		body.Hosts = append(body.Hosts, contact.Host())
	}
	writeJSON(w, http.StatusOK, body)
}

// Registered handles the /registered endpoint
func (h *Handler) Registered(w http.ResponseWriter, r *http.Request) {
	log.Printf("[Monitoring] GET /registered from %s", r.RemoteAddr)
	contacts, err := h.Service.Registered()
	if err != nil {
		log.Printf("[Monitoring] Error in /registered: %v", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}

	var body = struct {
		Hosts []string `json:"hosts"`
	}{
		[]string{},
	}
	for _, contact := range contacts {
		body.Hosts = append(body.Hosts, contact.Host())
	}
	writeJSON(w, http.StatusOK, body)
}

// Routing handles the /routing endpoint
func (h *Handler) Routing(w http.ResponseWriter, r *http.Request) {
	log.Printf("[Monitoring] GET /routing from %s", r.RemoteAddr)
	contacts, err := h.Service.Routing()
	if err != nil {
		log.Printf("[Monitoring] Error in /routing: %v", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}

	var body = struct {
		Hosts []string `json:"hosts"`
	}{
		[]string{},
	}
	for _, contact := range contacts {
		body.Hosts = append(body.Hosts, contact.Host())
	}
	writeJSON(w, http.StatusOK, body)
}

// Ownership handles the /ownership endpoint
func (h *Handler) Ownership(w http.ResponseWriter, r *http.Request) {
	log.Printf("[Monitoring] GET /ownership from %s", r.RemoteAddr)
	body, err := h.Service.Ownership()
	if err != nil {
		log.Printf("[Monitoring] Error in /ownership: %v", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, body)
}

// Queue handles the /queue endpoint
func (h *Handler) Queue(w http.ResponseWriter, r *http.Request) {
	log.Printf("[Monitoring] GET /queue from %s", r.RemoteAddr)
	writeJSON(w, http.StatusOK, struct {
		Pending int `json:"pending"`
	}{
		h.Service.Queue(),
	})
}
