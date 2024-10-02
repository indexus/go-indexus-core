package p2p

import (
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"strings"

	"gitlab.com/indexus/node/domain"
)

type Contact struct {
	Name string         `json:"name"`
	IPs  map[string]any `json:"ips"`
	Port int            `json:"port"`
	IP   string         `json:"ip"`
}

type Peer struct {
	id   []byte
	name string
}

func NewPeer(name string) (*Peer, error) {
	id, err := domain.DecodeName(name)
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
	Transfer(domain.Peer, domain.Key, []*domain.Item) error
	Get(string, string) (domain.Contact, *domain.Set, error)
	New(*domain.Item, string, string) error
}

type Handler struct {
	Service    Service
	NewContact func(string, map[string]any, int) domain.Contact
}

// New - Create a HTTP handler
func NewHttpHandler(service Service, newContact func(string, map[string]any, int) domain.Contact) *Handler {
	return &Handler{
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
	mux.HandleFunc("/item", h.New)

	s := &http.Server{Handler: mux}

	log.Println("P2P HTTP Server started")

	return s.Serve(lis)
}

func writeJSON(w http.ResponseWriter, code int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(data)
}

// Ping handles the /ping endpoint
func (h *Handler) Ping(w http.ResponseWriter, r *http.Request) {

	var bodyReq = struct {
		Origin Contact `json:"origin"`
	}{}
	if err := json.NewDecoder(r.Body).Decode(&bodyReq); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	ips, err := getClientIPs(r)
	if err != nil {
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

// Neighbors handles the /neighbors endpoint
func (h *Handler) Neighbors(w http.ResponseWriter, r *http.Request) {

	origin, err := NewPeer(r.URL.Query().Get("origin"))
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}

	contacts, err := h.Service.Neighbors(origin)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}

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

	origin, err := NewPeer(r.URL.Query().Get("origin"))
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}

	random, err := h.Service.Random(origin)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}

	if random == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

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

	var body struct {
		Origin string         `json:"origin"`
		Key    domain.Key     `json:"key"`
		Items  []*domain.Item `json:"items"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	origin, err := NewPeer(body.Origin)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}

	if err := h.Service.Transfer(origin, body.Key, body.Items); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusCreated)
}

// Get handles the /set endpoint
func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	collection := r.URL.Query().Get("collection")
	location := r.URL.Query().Get("location")

	contact, set, err := h.Service.Get(collection, location)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}

	var list map[string]int
	if set != nil {
		list = set.List()
	}

	var body = struct {
		Contact Contact        `json:"contact"`
		Set     map[string]int `json:"set"`
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
