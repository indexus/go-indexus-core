package p2p

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"

	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/peer"

	"github.com/rs/cors"
)

type Contact struct {
	Name     string         `json:"name"`
	IPs      map[string]any `json:"ips"`
	Port     int            `json:"port"`
	IP       string         `json:"ip"`
	Location string         `json:"location,omitempty"`
}

type Peer struct {
	id   []byte
	name string
}

func NewPeer(name string) (*Peer, error) {

	id, err := domain.BASE64.Decode(name)
	if err != nil {
		return nil, err
	}

	return &Peer{
		id:   id,
		name: name,
	}, nil
}

func (p *Peer) ID() []byte {
	return p.id
}

func (p *Peer) Name() string {
	return p.name
}

type Service interface {
	Ping(domain.Contact) (domain.Contact, error)
	Neighbors(domain.Peer) ([]domain.Contact, error)
	Random(domain.Peer) (domain.Contact, error)
	Transfer(domain.Peer, domain.Key, domain.Delegation, []*domain.Item, int) error
	Get(string, string) (domain.Contact, *domain.Set, error)
	GetMultiple(string, []string, int, []func(*domain.Abelian) int) (*domain.MultiGetResponse, error)
	New(*domain.Item, string, string) error
}

type Handler struct {
	SSLStorage string
	Service    Service
	NewContact func(string, map[string]any, int) domain.Contact
}

// New - Create a HTTP handler
func NewHttpHandler(sslStorage string, service Service, newContact func(string, map[string]any, int) domain.Contact) *Handler {
	// Set HTTPS flag based on SSL storage configuration
	peer.SetHTTPS(len(sslStorage) > 0)

	return &Handler{
		SSLStorage: sslStorage,
		Service:    service,
		NewContact: newContact,
	}
}

// Serve - Run the HTTP server
func (h *Handler) Serve(lis net.Listener) error {

	mux := http.NewServeMux()

	// Discovery
	mux.HandleFunc("/ping", h.Ping)

	// Peer
	mux.HandleFunc("/neighbors", h.Neighbors)
	mux.HandleFunc("/random", h.Random)
	mux.HandleFunc("/transfer", h.Transfer)

	// Client
	mux.HandleFunc("/set", h.Get)
	mux.HandleFunc("/sets", h.GetMultiple)
	mux.HandleFunc("/item", h.New)

	// Configure CORS
	c := cors.New(cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Content-Type", "Authorization"},
		AllowCredentials: true,
	})

	handler := c.Handler(mux)

	s := &http.Server{Handler: handler}

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

// Ping handles the /ping endpoint
func (h *Handler) Ping(w http.ResponseWriter, r *http.Request) {
	log.Printf("[P2P] POST /ping from %s", r.RemoteAddr)

	var bodyReq = struct {
		Origin Contact `json:"origin"`
	}{}
	if err := json.NewDecoder(r.Body).Decode(&bodyReq); err != nil {
		log.Printf("[P2P] Error decoding ping request: %v", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	ips, err := getClientIPs(r)
	if err != nil {
		log.Printf("[P2P] Error getting client IPs: %v", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}

	if bodyReq.Origin.IPs == nil {
		bodyReq.Origin.IPs = make(map[string]any)
	}
	for _, ip := range ips {
		bodyReq.Origin.IPs[ip] = nil
	}

	origin := h.NewContact(bodyReq.Origin.Name, bodyReq.Origin.IPs, bodyReq.Origin.Port)

	contact, err := h.Service.Ping(origin)
	if err != nil {
		log.Printf("[P2P] Error processing ping: %v", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}

	if contact == nil {
		log.Printf("[P2P] No contact found for ping from %s", origin.Name())
		w.WriteHeader(http.StatusNotFound)
		return
	}

	log.Printf("[P2P] Successful ping from %s to %s", origin.Name(), contact.Name())

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

// Neighbors handles the /neighbors endpoint
func (h *Handler) Neighbors(w http.ResponseWriter, r *http.Request) {
	log.Printf("[P2P] GET /neighbors from %s", r.RemoteAddr)

	origin, err := NewPeer(r.URL.Query().Get("origin"))
	if err != nil {
		log.Printf("[P2P] Error creating peer: %v", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}

	contacts, err := h.Service.Neighbors(origin)
	if err != nil {
		log.Printf("[P2P] Error getting neighbors: %v", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}

	log.Printf("[P2P] Found %d neighbors for %s", len(contacts), origin.Name())

	var body = struct {
		Neighbors []Contact `json:"neighbors"`
	}{
		Neighbors: []Contact{},
	}
	for _, contact := range contacts {
		body.Neighbors = append(body.Neighbors, Contact{
			Name: contact.Name(),
			IPs:  contact.IPs(),
			Port: contact.Port(),
			IP:   contact.IP(),
		})
	}
	writeJSON(w, http.StatusOK, body)
}

// Random handles the /random endpoint
func (h *Handler) Random(w http.ResponseWriter, r *http.Request) {
	log.Printf("[P2P] GET /random from %s", r.RemoteAddr)

	origin, err := NewPeer(r.URL.Query().Get("origin"))
	if err != nil {
		log.Printf("[P2P] Error creating peer: %v", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}

	random, err := h.Service.Random(origin)
	if err != nil {
		log.Printf("[P2P] Error getting random peer: %v", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}

	if random == nil {
		log.Printf("[P2P] No random peer found for %s", origin.Name())
		w.WriteHeader(http.StatusNotFound)
		return
	}

	log.Printf("[P2P] Found random peer %s for %s", random.Name(), origin.Name())

	var body = struct {
		Random Contact `json:"random"`
	}{
		Random: Contact{
			Name: random.Name(),
			IPs:  random.IPs(),
			Port: random.Port(),
			IP:   random.IP(),
		},
	}
	writeJSON(w, http.StatusOK, body)
}

// Transfer handles the /transfer endpoint
func (h *Handler) Transfer(w http.ResponseWriter, r *http.Request) {
	log.Printf("[P2P] POST /transfer from %s", r.RemoteAddr)

	var body struct {
		Origin     string            `json:"origin"`
		Key        domain.Key        `json:"key"`
		Ownership  domain.Delegation `json:"ownership"`
		Items      []*domain.Item    `json:"items"`
		MetricSize int               `json:"metricSize"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		log.Printf("[P2P] Error decoding transfer request: %v", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	origin, err := NewPeer(body.Origin)
	if err != nil {
		log.Printf("[P2P] Error creating peer: %v", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}

	log.Printf("[P2P] Transferring %d items from %s for collection %s at location %s",
		len(body.Items), origin.Name(), body.Key.Collection, body.Key.Location)

	if err := h.Service.Transfer(origin, body.Key, body.Ownership, body.Items, body.MetricSize); err != nil {
		log.Printf("[P2P] Error during transfer: %v", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// Get handles the /set endpoint
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	collection := r.URL.Query().Get("collection")
	location := r.URL.Query().Get("location")
	log.Printf("[P2P] GET /set from %s for collection %s at location %s", r.RemoteAddr, collection, location)

	contact, set, err := h.Service.Get(collection, location)
	if err != nil {
		log.Printf("[P2P] Error getting set: %v", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}

	var list map[string]*domain.Abelian
	if set != nil {
		list = set.List()
		log.Printf("[P2P] Found set with %d items", len(list))
	} else {
		log.Printf("[P2P] No set found")
	}

	var body = struct {
		Contact Contact                    `json:"contact"`
		Set     map[string]*domain.Abelian `json:"set"`
	}{
		Contact: Contact{
			Name: contact.Name(),
			IPs:  contact.IPs(),
			Port: contact.Port(),
			IP:   contact.IP(),
		},
		Set: list,
	}

	writeJSON(w, http.StatusOK, body)
}

// Get handles the /sets endpoint -- ADAPTED for one collection, multiple locations
func (h *Handler) GetMultiple(w http.ResponseWriter, r *http.Request) {
	// We expect a single collection
	collection := r.URL.Query().Get("collection")
	if collection == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "collection parameter is required"})
		return
	}

	// Potentially multiple locations (comma-separated)
	locationsParam := r.URL.Query().Get("location")
	if locationsParam == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "location parameter is required"})
		return
	}

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

	response, err := h.Service.GetMultiple(collection, locations, precision, properties)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	remoteContacts := make([]Contact, len(response.RemoteLocations))
	for i, remote := range response.RemoteLocations {
		remoteContacts[i] = Contact{
			Name:     remote.Contact.Name(),
			IPs:      remote.Contact.IPs(),
			Port:     remote.Contact.Port(),
			IP:       remote.Contact.IP(),
			Location: remote.Location,
		}
	}

	writeJSON(w, http.StatusOK, struct {
		LocalData      []byte    `json:"local_data"`
		RemoteContacts []Contact `json:"remote_contacts"`
	}{
		LocalData:      response.LocalData,
		RemoteContacts: remoteContacts,
	})
}

// New handles the /item endpoint
func (h *Handler) New(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Item    *domain.Item `json:"item"`
		Root    string       `json:"root"`
		Current string       `json:"current"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if err := h.Service.New(body.Item, body.Root, body.Current); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// getClientIPs extracts all IPv4 and IPv6 addresses from the HTTP request.
func getClientIPs(r *http.Request) ([]string, error) {
	var ips []string

	// Helper function to parse and append IPs from a comma-separated string.
	parseAndAppendIPs := func(ipStr string) {
		ipList := strings.Split(ipStr, ",")
		for _, ip := range ipList {
			ip = strings.TrimSpace(ip)
			if ip == "" {
				continue
			}
			parsedIP := net.ParseIP(ip)
			if parsedIP != nil {
				ips = append(ips, parsedIP.String())
			}
		}
	}

	// Check the X-Real-IP header.
	if ip := r.Header.Get("X-Real-IP"); ip != "" {
		parseAndAppendIPs(ip)
	}

	// Check the X-Forwarded-For header.
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parseAndAppendIPs(xff)
	}

	// Fall back to r.RemoteAddr.
	remoteIP := r.RemoteAddr
	if remoteIP != "" {
		// Attempt to split the host and port.
		host, _, err := net.SplitHostPort(remoteIP)
		if err != nil {
			// If splitting fails, use the entire RemoteAddr.
			host = remoteIP
		}
		host = strings.TrimSpace(host)
		parsedIP := net.ParseIP(host)
		if parsedIP != nil {
			ips = append(ips, parsedIP.String())
		}
	}

	if len(ips) == 0 {
		return nil, errors.New("no valid IPs found")
	}

	return ips, nil
}
