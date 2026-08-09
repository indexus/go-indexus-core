package p2p

import (
	"context"
	"encoding/base64"
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
	Name() string
	Ping(domain.Contact) (domain.Contact, error)
	Neighbors(domain.Peer) ([]domain.Contact, error)
	Random(domain.Peer) (domain.Contact, error)
	Transfer(domain.Peer, domain.Key, []*domain.Item) (string, error)
	Get(string, string, bool, domain.Visited, bool) (domain.Contact, *domain.Set, error)
	GetMultiple(string, []string, int, []func(*domain.Abelian) int, bool, domain.Visited, bool, bool) ([]byte, error)
	Children(string, string) (map[string]*domain.ChildEntry, error)
	New(*domain.Item, string, domain.Visited) error
	Handoff(*domain.Item, string, domain.Visited) error
	Delete(*domain.Item, string, domain.Visited) error
	Claim(domain.Peer, domain.ClaimPayload) error
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
	mux.HandleFunc("/claim", handler.Claim)

	mux.HandleFunc("/set", handler.Get)
	mux.HandleFunc("/sets", handler.GetMultiple)
	mux.HandleFunc("/children", handler.Children)
	mux.HandleFunc("/aggregates", handler.GetAggregates)
	mux.HandleFunc("/item", handler.New)
	mux.HandleFunc("/item/delete", handler.Delete)

	// Browser SDKs (axios arraybuffer /sets) preflight with Accept besides
	// Authorization. A narrow AllowedHeaders list makes rs/cors omit ACAO on
	// OPTIONS, which Chrome reports as "blocked by CORS". Credentials cannot
	// be true with AllowedOrigins "*" — Bearer auth does not need cookies.
	// Ingress hint response headers must be exposed or axios cannot read them.
	cors := cors.New(cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"*"},
		ExposedHeaders:   []string{"X-Indexus-Ingress-Name", "X-Indexus-Ingress-IP", "X-Indexus-Ingress-Port"},
		AllowCredentials: false,
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

	ackedPeer, err := h.Service.Transfer(origin, body.Key, body.Items)
	if err != nil {
		if errors.Is(err, domain.ErrLeaving) {

			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"peer": ackedPeer})
}

func (h *Handler) Claim(w http.ResponseWriter, r *http.Request) {
	if !h.requireClientScope(w, r, "write") {
		return
	}
	var body struct {
		Origin  string              `json:"origin"`
		Payload domain.ClaimPayload `json:"payload"`
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
	if err := h.Service.Claim(origin, body.Payload); err != nil {
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
	deep, via, refresh := parseGetFlags(r)

	contact, set, err := h.Service.Get(collection, location, deep, via, refresh)
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
	deep, via, refresh := parseGetFlags(r)
	envelope := parseEnvelopeFlag(r)

	precision := 6

	properties := [](func(*domain.Abelian) int){
		func(a *domain.Abelian) int {
			return a.Count()
		},
		func(a *domain.Abelian) int {
			return int(a.Metric(2))
		},
		func(a *domain.Abelian) int {
			return int(1_000_000 * a.Metric(3))
		},
		func(a *domain.Abelian) int {
			return int(1_000_000 * a.Metric(4))
		},
	}

	sets, err := h.Service.GetMultiple(collection, locations, precision, properties, deep, via, refresh, envelope)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}

	h.writeIngressHint(w, r)

	w.Header().Set("Content-Type", "application/octet-stream")
	if _, err := w.Write(sets); err != nil {
		slog.Warn("sets response truncated", "err", err)
	}
}

// writeIngressHint decodes the client session key and, when a closer peer
// exists, advertises it so sticky browsers can migrate without rediscovery.
func (h *Handler) writeIngressHint(w http.ResponseWriter, r *http.Request) {
	keyRaw := r.Header.Get("X-Indexus-Routing-Key")
	if keyRaw == "" {
		return
	}
	key, ok := decodeRoutingKey(keyRaw)
	if !ok {
		return
	}
	hintSvc, ok := h.Service.(interface {
		RoutingHint([]byte) domain.Contact
	})
	if !ok {
		return
	}
	hint := hintSvc.RoutingHint(key)
	if hint == nil {
		return
	}
	w.Header().Set("X-Indexus-Ingress-Name", hint.Name())
	w.Header().Set("X-Indexus-Ingress-IP", hint.IP())
	w.Header().Set("X-Indexus-Ingress-Port", strconv.Itoa(hint.Port()))
}

// decodeRoutingKey accepts the SDK's base64url(session bytes). Falls back to
// raw bytes only when the value is not valid base64url, so older probes that
// sent opaque strings keep a defined (if imperfect) behaviour.
func decodeRoutingKey(raw string) ([]byte, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, false
	}
	if key, err := base64.RawURLEncoding.DecodeString(raw); err == nil && len(key) > 0 {
		return key, true
	}
	padded := raw
	if m := len(raw) % 4; m != 0 {
		padded += strings.Repeat("=", 4-m)
	}
	if key, err := base64.URLEncoding.DecodeString(padded); err == nil && len(key) > 0 {
		return key, true
	}
	return []byte(raw), true
}

func parseEnvelopeFlag(r *http.Request) bool {
	raw := r.URL.Query().Get("envelope")
	if raw == "" {
		return false
	}
	switch strings.ToLower(raw) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

func parseGetFlags(r *http.Request) (deep bool, via domain.Visited, refresh bool) {
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
	via = domain.ParseVisited(r.URL.Query().Get("via"))
	if raw := r.URL.Query().Get("refresh"); raw != "" {
		switch strings.ToLower(raw) {
		case "1", "true", "yes":
			refresh = true
		}
	}
	return deep, via, refresh
}

func childWire(entry *domain.ChildEntry) map[string]any {
	if entry == nil {
		return map[string]any{}
	}
	out := map[string]any{}
	if entry.Abelian != nil {
		out["count"] = entry.Abelian.Count()
	}
	if entry.RedirectName != "" {
		out["redirect_name"] = entry.RedirectName
	}
	if entry.RedirectIP != "" {
		out["redirect_ip"] = entry.RedirectIP
	}
	if entry.RedirectPort > 0 {
		out["redirect_port"] = entry.RedirectPort
	}
	if len(entry.RedirectIPs) > 0 {
		out["redirect_ips"] = entry.RedirectIPs
	}
	return out
}

func (h *Handler) Children(w http.ResponseWriter, r *http.Request) {
	if !h.requireClientScope(w, r, "read") {
		return
	}
	collection := r.URL.Query().Get("collection")
	parent := r.URL.Query().Get("location")
	entries, err := h.Service.Children(collection, parent)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	if entries == nil {
		entries = map[string]*domain.ChildEntry{}
	}
	wire := make(map[string]any, len(entries))
	for loc, entry := range entries {
		wire[loc] = childWire(entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"parent": parent, "children": wire})
}

func (h *Handler) GetAggregates(w http.ResponseWriter, r *http.Request) {
	if !h.requireClientScope(w, r, "read") {
		return
	}
	collection := r.URL.Query().Get("collection")
	locParam := r.URL.Query().Get("location")
	if collection == "" || locParam == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "collection and location are required"})
		return
	}
	aggSvc, ok := h.Service.(interface {
		GetAggregates(string, []string) (map[string]*domain.Abelian, error)
	})
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "aggregates not supported"})
		return
	}
	locations := strings.Split(locParam, ",")
	aggs, err := aggSvc.GetAggregates(collection, locations)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"aggregates": aggs})
}

func (h *Handler) New(w http.ResponseWriter, r *http.Request) {
	if !h.requireClientScope(w, r, "write") {
		return
	}
	var body struct {
		Item    *domain.Item `json:"item"`
		Root    string       `json:"root"`
		Current string       `json:"current"` // legacy SDK field; ignored
		Via     string       `json:"via"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	via := domain.ParseVisited(body.Via)
	accept := h.Service.New
	if r.Header.Get(domain.HandoffHeader) != "" {
		accept = h.Service.Handoff
	}
	if err := accept(body.Item, body.Root, via); err != nil {

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
		Current string       `json:"current"` // legacy SDK field; ignored
		Via     string       `json:"via"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if err := h.Service.Delete(body.Item, body.Root, domain.ParseVisited(body.Via)); err != nil {
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
