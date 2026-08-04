package mockup

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/indexus/go-indexus-core/core"
	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
	"github.com/indexus/go-indexus-core/storage"
	"github.com/indexus/go-indexus-core/worker"
)

type Contact struct {
	Name string         `json:"name"`
	IPs  map[string]any `json:"ips"`
	Port int            `json:"port"`
	IP   string         `json:"ip"`
}

type Handler struct {
	errChan    chan error
	newContact func(string, map[string]any, int) domain.Contact
	minDelay   int          // Minimum delay in milliseconds
	maxDelay   int          // Maximum delay in milliseconds
	mu         sync.RWMutex // Mutex to protect minDelay and maxDelay
}

// NewHttpHandler creates a new HTTP handler with specified latency parameters
func NewHttpHandler(errChan chan error, newContact func(string, map[string]any, int) domain.Contact, minDelay, maxDelay int) *Handler {
	return &Handler{
		errChan:    errChan,
		newContact: newContact,
		minDelay:   minDelay,
		maxDelay:   maxDelay,
	}
}

// Serve runs the HTTP server
func (h *Handler) Serve(lis net.Listener) error {

	mux := http.NewServeMux()

	// Discovery
	mux.HandleFunc("/ping", h.Ping)

	// Peer
	/*
	   mux.HandleFunc("/neighbors", h.Neighbors)
	   mux.HandleFunc("/random", h.Random)
	   mux.HandleFunc("/transfer", h.Transfer)
	*/

	// Client
	mux.HandleFunc("/set", h.Get)
	mux.HandleFunc("/sets", h.GetMultiple)
	mux.HandleFunc("/item", h.New)

	// Monitoring
	mux.HandleFunc("/acknowledged", h.Acknowledged)
	mux.HandleFunc("/registered", h.Registered)
	mux.HandleFunc("/routing", h.Routing)

	// Network
	mux.HandleFunc("/all/ownership", h.AllOwnership)
	mux.HandleFunc("/all/count", h.AllCount)
	mux.HandleFunc("/all/check", h.AllCheck)
	mux.HandleFunc("/all/queue", h.AllQueue)

	// Latency
	mux.HandleFunc("/latency", h.SetLatency)

	// Feeds
	mux.HandleFunc("/feed/network", h.FeedNetwork)
	mux.HandleFunc("/feed/collection", h.FeedCollection)

	s := &http.Server{Handler: mux}

	slog.Info("simulation server listening", "addr", lis.Addr().String())

	return s.Serve(lis)
}

func writeJSON(w http.ResponseWriter, code int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(data)
}

// randomDelay introduces a random delay between minDelay and maxDelay milliseconds
func (h *Handler) randomDelay() {
	h.mu.RLock()
	defer h.mu.RUnlock()

	if h.maxDelay > h.minDelay {
		delay := h.minDelay + rand.Intn(h.maxDelay-h.minDelay+1)
		time.Sleep(time.Duration(delay) * time.Millisecond)
	} else {
		// If maxDelay is not greater than minDelay, use minDelay only
		time.Sleep(time.Duration(h.minDelay) * time.Millisecond)
	}
}

// SetLatency handles the POST /latency endpoint to update minDelay and maxDelay
func (h *Handler) SetLatency(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed"})
		return
	}

	var payload struct {
		MinDelay int `json:"minDelay"` // in milliseconds
		MaxDelay int `json:"maxDelay"` // in milliseconds
	}

	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid JSON payload"})
		return
	}

	// Validate the payload
	if payload.MinDelay < 0 || payload.MaxDelay < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Delays must be non-negative integers"})
		return
	}

	// Optionally, enforce that minDelay <= maxDelay
	if payload.MaxDelay < payload.MinDelay {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "maxDelay must be greater than or equal to minDelay"})
		return
	}

	// Update the delays with write lock
	h.mu.Lock()
	h.minDelay = payload.MinDelay
	h.maxDelay = payload.MaxDelay
	h.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]string{
		"message": fmt.Sprintf("Latency updated: minDelay=%dms, maxDelay=%dms", h.minDelay, h.maxDelay),
	})
}

// Ping handles the /ping endpoint
func (h *Handler) Ping(w http.ResponseWriter, r *http.Request) {
	h.randomDelay() // Introduce latency

	destination := r.Header.Get("Destination")

	node, ok := network.Get(destination)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	var bodyReq = struct {
		Origin Contact `json:"origin"`
	}{}
	if err := json.NewDecoder(r.Body).Decode(&bodyReq); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	origin := h.newContact(bodyReq.Origin.Name, bodyReq.Origin.IPs, bodyReq.Origin.Port)

	contact, err := node.Ping(origin)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}

	if contact == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	var bodyResp = struct {
		Contact Contact `json:"contact"`
	}{
		Contact: Contact{
			Name: contact.Name(),
			IPs:  contact.IPs(),
			Port: contact.Port(),
		},
	}
	writeJSON(w, http.StatusOK, bodyResp)
}

// Get handles the /set endpoint
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	h.randomDelay() // Introduce latency

	destination := r.Header.Get("Destination")

	node, ok := network.Get(destination)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	collection := r.URL.Query().Get("collection")
	location := r.URL.Query().Get("location")

	contact, set, err := node.Get(collection, location, 2)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}

	var body = struct {
		Contact Contact     `json:"contact"`
		Set     *domain.Set `json:"set"`
	}{
		Set: set,
	}

	if contact != nil {
		body.Contact = Contact{
			Name: contact.Name(),
			IPs:  contact.IPs(),
			Port: contact.Port(),
			IP:   contact.IP(),
		}
	}

	writeJSON(w, http.StatusOK, body)
}

// Get handles the /sets endpoint
func (h *Handler) GetMultiple(w http.ResponseWriter, r *http.Request) {
	h.randomDelay() // Introduce latency

	destination := r.Header.Get("Destination")

	node, ok := network.Get(destination)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// We expect a single collection
	collection := r.URL.Query().Get("collection")
	if collection == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "collection parameter is required"})
		return
	}

	// Potentially multiple locations (comma-separated)
	locationsParam := r.URL.Query().Get("locations")
	if locationsParam == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "location parameter is required"})
		return
	}

	// Split the location(s) on commas
	locations := strings.Split(locationsParam, ",")

	precision := 6

	properties := [](func(*domain.Abelian) int){
		func(a *domain.Abelian) int {
			return a.Count()
		},
		func(a *domain.Abelian) int {
			return int(a.Metrics()[2])
		},
		func(a *domain.Abelian) int {
			return int(1_000_000 * a.Metrics()[3])
		},
		func(a *domain.Abelian) int {
			return int(1_000_000 * a.Metrics()[4])
		},
	}

	sets, err := node.GetMultiple(collection, locations, precision, properties)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	if _, err := w.Write(sets); err != nil {
		slog.Warn("sets response truncated", "err", err)
	}
}

// New handles the /item endpoint
func (h *Handler) New(w http.ResponseWriter, r *http.Request) {
	h.randomDelay() // Introduce latency

	destination := r.Header.Get("Destination")

	node, ok := network.Get(destination)
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	var body struct {
		Item    *domain.Item `json:"item"`
		Root    string       `json:"root"`
		Current string       `json:"current"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if err := node.New(body.Item, body.Root, body.Current); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// Acknowledged handles the /acknowledged endpoint
func (h *Handler) Acknowledged(w http.ResponseWriter, r *http.Request) {
	h.randomDelay() // Introduce latency

	destination := r.Header.Get("Destination")

	node, ok := network.Get(destination)
	if !ok {
		node, ok = network.unreachable[destination]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
	}

	contacts, err := node.Acknowledged()
	if err != nil {
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
	h.randomDelay() // Introduce latency

	destination := r.Header.Get("Destination")

	node, ok := network.Get(destination)
	if !ok {
		node, ok = network.unreachable[destination]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
	}

	contacts, err := node.Registered()
	if err != nil {
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
	h.randomDelay() // Introduce latency

	destination := r.Header.Get("Destination")

	node, ok := network.Get(destination)
	if !ok {
		node, ok = network.unreachable[destination]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
	}

	contacts, err := node.Routing()
	if err != nil {
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

// AllOwnership handles the /all/ownership endpoint
func (h *Handler) AllOwnership(w http.ResponseWriter, r *http.Request) {
	h.randomDelay() // Introduce latency

	body := map[string]any{}

	for _, node := range network.Nodes() {
		arr, err := node.Ownership()
		if err != nil || len(arr) == 0 {
			continue
		}
		body[node.Name()] = arr
	}

	writeJSON(w, http.StatusOK, body)
}

// AllCount handles the /all/count endpoint
func (h *Handler) AllCount(w http.ResponseWriter, r *http.Request) {
	h.randomDelay() // Introduce latency

	body := map[string]any{}
	total := 0

	for _, node := range network.Nodes() {
		count, err := node.Count()
		if err != nil {
			continue
		}
		total += count
		body[node.Name()] = count
		body["-"] = total
	}

	writeJSON(w, http.StatusOK, body)
}

// AllCheck handles the /all/check endpoint
func (h *Handler) AllCheck(w http.ResponseWriter, r *http.Request) {
	h.randomDelay() // Introduce latency

	body := map[string]any{}

	for _, node := range network.Nodes() {
		body[node.Name()] = node.Check()
	}

	writeJSON(w, http.StatusOK, body)
}

// AllQueue handles the /all/queue endpoint
func (h *Handler) AllQueue(w http.ResponseWriter, r *http.Request) {
	h.randomDelay() // Introduce latency

	body := map[string]any{}

	for _, node := range network.Nodes() {
		body[node.Name()] = node.Queue()
	}

	writeJSON(w, http.StatusOK, body)
}

// FeedNetwork handles the /feed/network endpoint
func (h *Handler) FeedNetwork(w http.ResponseWriter, r *http.Request) {
	h.randomDelay() // Introduce latency

	c, err := strconv.Atoi(r.URL.Query().Get("count"))

	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}

	for i := 0; i < c; i++ {

		var bootstraps = []domain.Contact{}
		if network.Length() > 0 {
			random := network.Random()
			bootstraps = append(bootstraps, NewContact(random.Name(), random.IPs(), random.Port()))
		}

		name, err := encoding.BASE64.RandomName()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}

		settings, err := core.NewSettings(name, network.Length(), 1*time.Second, 5*time.Minute, domain.DelegationTreshold())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}

		node, err := core.NewNode(settings, NewContact, bootstraps, storage.NewMemory())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}

		workerInstance := worker.NewWorker(node)
		go func() {
			defer workerInstance.Close()
			h.errChan <- workerInstance.Feed()
		}()
		go func() {
			defer workerInstance.Close()
			h.errChan <- workerInstance.Start()
		}()

		if network.Length() == 0 || rand.Intn(10) < 3 || true {
			network.Join(node)
		} else {
			network.Unreachable(node)
		}

		slog.Info("simulated node started", "name", node.Name())
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": "FeedNetwork completed"})
}

// FeedCollection handles the /feed/collection endpoint
func (h *Handler) FeedCollection(w http.ResponseWriter, r *http.Request) {
	h.randomDelay() // Introduce latency

	collectionId := r.URL.Query().Get("collectionId")
	c, err := strconv.Atoi(r.URL.Query().Get("count"))

	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}

	go func() {
		for i := 0; i < c; i++ {

			location, err := encoding.BASE64.RandomName()
			if err != nil {
				slog.Error("simulation feed stopped", "err", err)
				return
			}

			item := &domain.Item{
				Collection: collectionId,
				Location:   location,
				Id:         fmt.Sprintf("%d", i),
				Metrics:    []float64{rand.Float64(), rand.Float64(), rand.Float64(), rand.Float64(), rand.Float64()},
			}

			if err := network.Random().New(item, encoding.BASE64.Root(), item.Location); err != nil {
				slog.Debug("simulated insert rejected", "err", err)
			}
		}
	}()

	writeJSON(w, http.StatusOK, map[string]string{"message": "FeedCollection started"})
}
