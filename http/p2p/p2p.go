package p2p

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/indexus/go-indexus-core/auth"
	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
	"github.com/indexus/go-indexus-core/logging"

	"github.com/rs/cors"
)

type Contact struct {
	Name string         `json:"name"`
	IPs  map[string]any `json:"ips"`
	Port int            `json:"port"`
	IP   string         `json:"ip"`
	Cert *auth.NodeCert `json:"cert,omitempty"`
}

type Peer struct {
	id   []byte
	name string
}

func NewPeer(name string) (*Peer, error) {

	id, err := encoding.BASE64.Decode(name)
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
	Get(string, string, bool, int) (domain.Contact, *domain.Set, error)
	GetMultiple(string, []string, int, []func(*domain.Abelian) int, bool, int) ([]byte, error)
	New(*domain.Item, string, string) error
	Handoff(*domain.Item, string, string) error
	Delete(*domain.Item, string, string) error
	DelegationOffer(domain.Peer, domain.DelegationOfferPayload) error
	WALDelta(domain.Peer, domain.WALDeltaPayload) error
	CaughtUp(domain.Peer, domain.CaughtUpPayload) error
	SwitchAck(domain.Peer, domain.SwitchAckPayload) error
}

type Handler struct {
	Service    Service
	NewContact func(string, map[string]any, int) domain.Contact

	Verifier    *auth.Verifier
	RequireAuth bool
	SelfCert    *auth.NodeCert

	tlsDir string
	server *http.Server
}

func NewHttpHandler(tlsDir string, service Service, newContact func(string, map[string]any, int) domain.Contact) *Handler {
	handler := &Handler{
		Service:    service,
		NewContact: newContact,
		tlsDir:     tlsDir,
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/ping", handler.Ping)

	mux.HandleFunc("/neighbors", handler.Neighbors)
	mux.HandleFunc("/random", handler.Random)
	mux.HandleFunc("/transfer", handler.Transfer)
	mux.HandleFunc("/delegation/offer", handler.DelegationOffer)
	mux.HandleFunc("/delegation/wal-delta", handler.WALDelta)
	mux.HandleFunc("/delegation/caught-up", handler.CaughtUp)
	mux.HandleFunc("/delegation/switch-ack", handler.SwitchAck)

	mux.HandleFunc("/set", handler.Get)
	mux.HandleFunc("/sets", handler.GetMultiple)
	mux.HandleFunc("/item", handler.New)
	mux.HandleFunc("/item/delete", handler.Delete)

	cors := cors.New(cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Content-Type", "Authorization"},
		AllowCredentials: true,
	})

	handler.server = &http.Server{
		Handler:           cors.Handler(mux),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       time.Minute,
		ErrorLog:          logging.StdLogger(slog.LevelWarn),
	}

	return handler
}

func (h *Handler) Serve(lis net.Listener) error {
	slog.Info("p2p server listening", "addr", lis.Addr().String(), "tls", h.tlsDir != "", "auth", h.RequireAuth)

	if h.tlsDir != "" {
		return h.server.ServeTLS(lis, h.tlsDir+"/server.crt", h.tlsDir+"/server.key")
	}
	return h.server.Serve(lis)
}

func (h *Handler) Shutdown(ctx context.Context) error {
	return h.server.Shutdown(ctx)
}

func (h *Handler) clientTokenOK(r *http.Request, scope string) bool {
	if h.Verifier == nil {
		return false
	}
	raw := r.Header.Get("Authorization")
	if !strings.HasPrefix(raw, "Bearer ") {
		return false
	}
	tok, err := auth.DecodeToken(strings.TrimPrefix(raw, "Bearer "))
	if err != nil {
		return false
	}
	if err := h.Verifier.VerifyClientToken(tok); err != nil {
		return false
	}
	return tok.HasScope(scope)
}

func (h *Handler) requireClientScope(w http.ResponseWriter, r *http.Request, scope string) bool {
	if !h.RequireAuth || h.Verifier == nil {
		return true
	}
	if !h.clientTokenOK(r, scope) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing or invalid bearer token"})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(data)
}

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

	if h.RequireAuth && h.Verifier != nil {

		if bodyReq.Origin.Cert != nil {
			if bodyReq.Origin.Cert.NodeID != "" && bodyReq.Origin.Name != "" && bodyReq.Origin.Cert.NodeID != bodyReq.Origin.Name {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "cert node_id mismatch"})
				return
			}
			if err := h.Verifier.VerifyNodeCertForAddr(bodyReq.Origin.Cert, ips); err != nil {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
				return
			}
		} else if !h.clientTokenOK(r, "read") {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing node cert or client token"})
			return
		} else {

			bodyReq.Origin.Name = ""
		}
	}

	origin := h.NewContact(bodyReq.Origin.Name, bodyReq.Origin.IPs, bodyReq.Origin.Port)
	if c, ok := origin.(interface{ SetCert(*auth.NodeCert) }); ok && bodyReq.Origin.Cert != nil {
		c.SetCert(bodyReq.Origin.Cert)
	}

	contact, err := h.Service.Ping(origin)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}

	if contact == nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	respCert := h.SelfCert
	if respCert == nil {
		if cc, ok := contact.(interface{ Cert() *auth.NodeCert }); ok {
			respCert = cc.Cert()
		}
	}

	var bodyResp = struct {
		Contact     Contact `json:"contact"`
		ClientReady bool    `json:"client_ready"`
	}{
		Contact: Contact{
			Name: contact.Name(),
			IPs:  contact.IPs(),
			Port: contact.Port(),
			Cert: respCert,
		},
		ClientReady: true,
	}
	if cr, ok := h.Service.(interface{ ClientReady() bool }); ok {
		bodyResp.ClientReady = cr.ClientReady()
	}
	writeJSON(w, http.StatusOK, bodyResp)
}

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

func (h *Handler) Transfer(w http.ResponseWriter, r *http.Request) {

	if !h.requireClientScope(w, r, "write") {
		return
	}

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
		if errors.Is(err, domain.ErrLeaving) {

			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (h *Handler) DelegationOffer(w http.ResponseWriter, r *http.Request) {
	if !h.requireClientScope(w, r, "write") {
		return
	}
	var body struct {
		Origin string                        `json:"origin"`
		Offer  domain.DelegationOfferPayload `json:"offer"`
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
	if err := h.Service.DelegationOffer(origin, body.Offer); err != nil {
		if errors.Is(err, domain.ErrLeaving) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) WALDelta(w http.ResponseWriter, r *http.Request) {
	if !h.requireClientScope(w, r, "write") {
		return
	}
	var body struct {
		Origin string                 `json:"origin"`
		Delta  domain.WALDeltaPayload `json:"delta"`
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
	if err := h.Service.WALDelta(origin, body.Delta); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) CaughtUp(w http.ResponseWriter, r *http.Request) {
	if !h.requireClientScope(w, r, "write") {
		return
	}
	var body struct {
		Origin  string                 `json:"origin"`
		Payload domain.CaughtUpPayload `json:"payload"`
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
	if err := h.Service.CaughtUp(origin, body.Payload); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) SwitchAck(w http.ResponseWriter, r *http.Request) {
	if !h.requireClientScope(w, r, "write") {
		return
	}
	var body struct {
		Origin  string                  `json:"origin"`
		Payload domain.SwitchAckPayload `json:"payload"`
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
	if err := h.Service.SwitchAck(origin, body.Payload); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	if !h.requireClientScope(w, r, "read") {
		return
	}
	collection := r.URL.Query().Get("collection")
	location := r.URL.Query().Get("location")
	deep, hop := parseDeepHop(r)

	contact, set, err := h.Service.Get(collection, location, deep, hop)
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

func (h *Handler) GetMultiple(w http.ResponseWriter, r *http.Request) {
	if !h.requireClientScope(w, r, "read") {
		return
	}

	collection := r.URL.Query().Get("collection")
	if collection == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "collection parameter is required"})
		return
	}

	locationsParam := r.URL.Query().Get("location")
	if locationsParam == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "location parameter is required"})
		return
	}

	locations := strings.Split(locationsParam, ",")
	deep, hop := parseDeepHop(r)

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

	sets, err := h.Service.GetMultiple(collection, locations, precision, properties, deep, hop)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	if _, err := w.Write(sets); err != nil {
		slog.Warn("sets response truncated", "err", err)
	}
}

// parseDeepHop: deep defaults true. hop defaults 8 when deep. Legacy depth:
// depth=0 → deep=false; depth>0 → deep=true (hop still default unless set).
func parseDeepHop(r *http.Request) (deep bool, hop int) {
	deep = true
	if raw := r.URL.Query().Get("deep"); raw != "" {
		switch strings.ToLower(raw) {
		case "0", "false", "no":
			deep = false
		default:
			deep = true
		}
	} else if d := r.URL.Query().Get("depth"); d != "" {
		if v, err := strconv.Atoi(d); err == nil && v <= 0 {
			deep = false
		}
	}
	hop = 0
	if deep {
		hop = 8
		if h := r.URL.Query().Get("hop"); h != "" {
			if v, err := strconv.Atoi(h); err == nil {
				hop = v
			}
		}
	}
	return deep, hop
}

func (h *Handler) New(w http.ResponseWriter, r *http.Request) {
	if !h.requireClientScope(w, r, "write") {
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
	accept := h.Service.New
	if r.Header.Get(domain.HandoffHeader) != "" {
		accept = h.Service.Handoff
	}
	if err := accept(body.Item, body.Root, body.Current); err != nil {

		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}

	w.WriteHeader(http.StatusCreated)
}

func (h *Handler) Delete(w http.ResponseWriter, r *http.Request) {
	if !h.requireClientScope(w, r, "write") {
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
	if err := h.Service.Delete(body.Item, body.Root, body.Current); err != nil {
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func getClientIPs(r *http.Request) ([]string, error) {
	var ips []string

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

	if ip := r.Header.Get("X-Real-IP"); ip != "" {
		parseAndAppendIPs(ip)
	}

	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parseAndAppendIPs(xff)
	}

	remoteIP := r.RemoteAddr
	if remoteIP != "" {

		host, _, err := net.SplitHostPort(remoteIP)
		if err != nil {

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
