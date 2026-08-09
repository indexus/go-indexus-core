package peer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/indexus/go-indexus-core/auth"
	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
)

var HttpClient = &http.Client{
	Timeout: 2 * time.Second,
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          512,
		MaxIdleConnsPerHost:   128,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   2 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	},
}

// TransferClient carries repeatable ownership handoff rounds. Large zones
// (~100k items) routinely exceed 30s on a single POST, so the default is
// DefaultTransferTimeout. Override with INDEXUS_TRANSFER_TIMEOUT (Go duration,
// e.g. 10m).
const DefaultTransferTimeout = 5 * time.Minute

var TransferClient = &http.Client{
	Timeout: transferTimeout(),
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	},
}

func transferTimeout() time.Duration {
	raw := os.Getenv("INDEXUS_TRANSFER_TIMEOUT")
	if raw == "" {
		return DefaultTransferTimeout
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return DefaultTransferTimeout
	}
	return d
}

var OutboundBearer string

func withAuth(req *http.Request) {
	if OutboundBearer != "" {
		req.Header.Set("Authorization", "Bearer "+OutboundBearer)
	}
}

type Contact struct {
	name string
	id   []byte
	ips  map[string]any
	ip   string
	port int
	cert *auth.NodeCert
}

func (c *Contact) MarshalJSON() ([]byte, error) {
	type Alias struct {
		Name string         `json:"name"`
		IPs  map[string]any `json:"ips"`
		IP   string         `json:"ip"`
		Port int            `json:"port"`
		Cert *auth.NodeCert `json:"cert,omitempty"`
	}
	return json.Marshal(&Alias{
		Name: c.name,
		IPs:  c.ips,
		IP:   c.ip,
		Port: c.port,
		Cert: c.cert,
	})
}

func (c *Contact) UnmarshalJSON(data []byte) error {
	type Alias struct {
		Name string         `json:"name"`
		IPs  map[string]any `json:"ips"`
		IP   string         `json:"ip"`
		Port int            `json:"port"`
		Cert *auth.NodeCert `json:"cert,omitempty"`
	}
	aux := &Alias{}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	c.name = aux.Name
	c.ips = aux.IPs
	c.ip = aux.IP
	c.port = aux.Port
	c.cert = aux.Cert
	c.id, _ = encoding.BASE64.Decode(aux.Name)
	return nil
}

func NewContact(name string, ips map[string]any, port int) domain.Contact {
	id, _ := encoding.BASE64.Decode(name)
	c := &Contact{
		name: name,
		id:   id,
		ips:  ips,
		port: port,
	}

	for ip := range ips {
		if ip != "" {
			c.ip = ip
			break
		}
	}
	return c
}

func NewContactWithCert(name string, ips map[string]any, port int, cert *auth.NodeCert) domain.Contact {
	c := NewContact(name, ips, port).(*Contact)
	c.cert = cert
	return c
}

func (c *Contact) Cert() *auth.NodeCert { return c.cert }

func (c *Contact) SetCert(cert *auth.NodeCert) { c.cert = cert }

func (c *Contact) ID() []byte {
	return c.id
}

func (c *Contact) Name() string {
	return c.name
}

func (c *Contact) IPs() map[string]any {
	return c.ips
}

func (c *Contact) Port() int {
	return c.port
}

func (c *Contact) IP() string {
	return c.ip
}

func (c *Contact) Host() string {
	ip := c.ip
	if ip == "" {
		for candidate := range c.ips {
			if candidate != "" {
				ip = candidate
				break
			}
		}
	}
	return fmt.Sprintf("%s@%s|%d", c.name, ip, c.port)
}

func (c *Contact) Ping(origin domain.Contact) (domain.Contact, error) {
	ips := c.ips
	if len(ips) == 0 && c.ip != "" {
		ips = map[string]any{c.ip: nil}
	}
	for ip := range ips {
		if ip == "" {
			continue
		}
		contact, err := c.ping(origin, ip)
		if err != nil {
			continue
		}
		return contact, nil
	}
	return nil, fmt.Errorf("no hosts found from ips and port provided")
}

func (c *Contact) ping(origin domain.Contact, ip string) (domain.Contact, error) {
	parsedIP := net.ParseIP(c.ip)

	if parsedIP != nil && parsedIP.To4() == nil {
		ip = fmt.Sprintf("[%s]", ip)
	}

	url := fmt.Sprintf("http://%s:%d/ping", ip, c.port)
	originPeer := &Contact{
		name: origin.Name(),
		ips:  origin.IPs(),
		port: origin.Port(),
	}
	if oc, ok := origin.(interface{ Cert() *auth.NodeCert }); ok {
		originPeer.cert = oc.Cert()
	}
	reqBody := struct {
		Origin *Contact `json:"origin"`
	}{
		Origin: originPeer,
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	withAuth(req)

	resp, err := HttpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ping %s: unexpected status %d", url, resp.StatusCode)
	}

	var respBody struct {
		Contact *Contact `json:"contact"`
	}
	decoder := json.NewDecoder(resp.Body)
	if err := decoder.Decode(&respBody); err != nil {
		return nil, err
	}

	contact := respBody.Contact
	contact.ips[ip] = nil
	contact.ip = ip

	return contact, nil
}

func (c *Contact) Neighbors(origin domain.Peer) ([]domain.Contact, error) {
	ip, parsedIP := c.ip, net.ParseIP(c.ip)

	if parsedIP != nil && parsedIP.To4() == nil {
		ip = fmt.Sprintf("[%s]", ip)
	}

	url := fmt.Sprintf("http://%s:%d/neighbors?origin=%s", ip, c.port, origin.Name())

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("error creating request: %s", err.Error())
	}
	withAuth(req)

	resp, err := HttpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error making request: %s", err.Error())
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("error code: %d", resp.StatusCode)
	}

	var body struct {
		Neighbors []*Contact `json:"neighbors"`
	}
	decoder := json.NewDecoder(resp.Body)
	if err := decoder.Decode(&body); err != nil {
		return nil, err
	}

	for _, neighbor := range body.Neighbors {
		if neighbor.Name() == c.Name() {
			neighbor.ip = c.ip
		}
	}

	return domain.ConvertToContactSlice(body.Neighbors), nil
}

func (c *Contact) Random(origin domain.Peer) (domain.Contact, error) {
	ip, parsedIP := c.ip, net.ParseIP(c.ip)

	if parsedIP != nil && parsedIP.To4() == nil {
		ip = fmt.Sprintf("[%s]", ip)
	}

	url := fmt.Sprintf("http://%s:%d/random?origin=%s", ip, c.port, origin.Name())

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("error creating request: %s", err.Error())
	}
	withAuth(req)

	resp, err := HttpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error making request: %s", err.Error())
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("error code: %d", resp.StatusCode)
	}

	// Handler writes {"random": {...}}; a missing/null body must be a true
	// nil interface, not a typed (*Contact)(nil), or callers panic on .Name().
	var body struct {
		Random *Contact `json:"random"`
	}
	decoder := json.NewDecoder(resp.Body)
	if err := decoder.Decode(&body); err != nil {
		return nil, err
	}
	if body.Random == nil {
		return nil, nil
	}
	return body.Random, nil
}

func (c *Contact) dialTargets() []string {
	seen := map[string]bool{}
	out := make([]string, 0, 1+len(c.ips))
	add := func(ip string) {
		if ip == "" || seen[ip] {
			return
		}
		seen[ip] = true
		out = append(out, ip)
	}
	add(c.ip)
	for ip := range c.ips {
		add(ip)
	}
	return out
}

func (c *Contact) Transfer(origin domain.Peer, key domain.Key, items []*domain.Item) (string, error) {
	if c.port <= 0 {
		return "", fmt.Errorf("transfer: invalid port on peer %s", c.name)
	}
	targets := c.dialTargets()
	if len(targets) == 0 {
		return "", fmt.Errorf("transfer: no dialable IP on peer %s", c.name)
	}
	var lastErr error
	var ackedPeer string
	for _, rawIP := range targets {
		ip := rawIP
		if parsed := net.ParseIP(rawIP); parsed != nil && parsed.To4() == nil {
			ip = fmt.Sprintf("[%s]", rawIP)
		}
		url := fmt.Sprintf("http://%s:%d/transfer", ip, c.port)
		body := struct {
			Origin string         `json:"origin"`
			Key    domain.Key     `json:"key"`
			Items  []*domain.Item `json:"items"`
		}{
			Origin: origin.Name(),
			Key:    key,
			Items:  items,
		}
		jsonData, err := json.Marshal(body)
		if err != nil {
			return "", err
		}
		req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		withAuth(req)
		resp, err := TransferClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		func() {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusConflict {

				lastErr = domain.ErrLeaving
				return
			}
			if resp.StatusCode != http.StatusCreated {
				lastErr = fmt.Errorf("transfer %s: status %d", url, resp.StatusCode)
				return
			}
			var ack struct {
				Peer string `json:"peer"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&ack); err != nil {
				lastErr = fmt.Errorf("transfer %s: invalid ACK: %w", url, err)
				return
			}
			if ack.Peer != c.name {
				// A repeatable handoff may be relayed to the holder selected
				// by the receiver's newer membership view.
				if ack.Peer == "" {
					lastErr = fmt.Errorf("transfer %s: empty nominative ACK", url)
					return
				}
			}
			ackedPeer = ack.Peer
			lastErr = nil
		}()
		if errors.Is(lastErr, domain.ErrLeaving) {
			return "", lastErr
		}
		if lastErr == nil {
			if c.ip == "" {
				c.ip = rawIP
			}
			return ackedPeer, nil
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("transfer: all dial targets failed for %s", c.name)
	}
	return "", lastErr
}

func (c *Contact) Get(collection string, location string, deep bool, via domain.Visited, refresh bool) (domain.Contact, *domain.Set, error) {
	ip, parsedIP := c.ip, net.ParseIP(c.ip)

	if parsedIP != nil && parsedIP.To4() == nil {
		ip = fmt.Sprintf("[%s]", ip)
	}

	// Inter-node reads use the same /sets protocol as browser clients so the
	// two paths cannot diverge. envelope=1 carries owner redirects for
	// deep=false; /set remains a JSON adapter for older probes.
	rawURL := fmt.Sprintf("http://%s:%d/sets?collection=%s&location=%s&deep=%t&envelope=1",
		ip, c.port, url.QueryEscape(collection), url.QueryEscape(location), deep)
	if s := via.String(); s != "" {
		rawURL += "&via=" + url.QueryEscape(s)
	}
	if refresh {
		rawURL += "&refresh=true"
	}
	req, err := http.NewRequest("GET", rawURL, nil)
	if err != nil {
		return nil, nil, err
	}
	withAuth(req)
	resp, err := HttpClient.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("error code: %d", resp.StatusCode)
	}

	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return nil, nil, err
	}
	payload := buf.Bytes()

	redirects, body, isEnv, err := domain.DecodeSetsEnvelope(payload)
	if err != nil {
		return nil, nil, err
	}
	if !isEnv {
		body = payload
	}

	var contact domain.Contact
	for _, r := range redirects {
		if r.Location != location || r.Name == "" || r.Port <= 0 {
			continue
		}
		contact = NewContact(r.Name, map[string]any{r.IP: nil}, r.Port)
		break
	}

	if len(body) == 0 {
		return contact, nil, nil
	}
	entries, err := domain.DecodeSetsBinary(body, 4)
	if err != nil {
		return nil, nil, err
	}
	if len(entries) == 0 {
		return contact, nil, nil
	}
	set := domain.NewSet()
	for key, value := range entries {
		set.Put(key, value)
	}
	return contact, set, nil
}

func (c *Contact) Children(collection, parent string) (map[string]*domain.ChildEntry, error) {
	ip, parsedIP := c.ip, net.ParseIP(c.ip)
	if parsedIP != nil && parsedIP.To4() == nil {
		ip = fmt.Sprintf("[%s]", ip)
	}
	rawURL := fmt.Sprintf("http://%s:%d/children?collection=%s&location=%s",
		ip, c.port, url.QueryEscape(collection), url.QueryEscape(parent))
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	withAuth(req)
	resp, err := HttpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("error code: %d", resp.StatusCode)
	}
	var body struct {
		Children map[string]json.RawMessage `json:"children"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	out := make(map[string]*domain.ChildEntry, len(body.Children))
	for loc, raw := range body.Children {
		entry := &domain.ChildEntry{}
		var stub struct {
			Count        int            `json:"count"`
			RedirectName string         `json:"redirect_name"`
			RedirectIP   string         `json:"redirect_ip"`
			RedirectPort int            `json:"redirect_port"`
			RedirectIPs  map[string]any `json:"redirect_ips"`
		}
		if err := json.Unmarshal(raw, &stub); err != nil {
			return nil, err
		}
		entry.Abelian = domain.NewAbelian(stub.Count, nil)
		entry.RedirectName = stub.RedirectName
		entry.RedirectIP = stub.RedirectIP
		entry.RedirectPort = stub.RedirectPort
		entry.RedirectIPs = stub.RedirectIPs
		out[loc] = entry
	}
	return out, nil
}

// GetAggregates asks a peer for SoT summaries of locations it owns. Answering
// is a claim of ownership — the only source allowed to write a parent stub.
func (c *Contact) GetAggregates(collection string, locations []string) (map[string]*domain.Abelian, error) {
	if len(locations) == 0 {
		return map[string]*domain.Abelian{}, nil
	}
	ip, parsedIP := c.ip, net.ParseIP(c.ip)
	if parsedIP != nil && parsedIP.To4() == nil {
		ip = fmt.Sprintf("[%s]", ip)
	}
	rawURL := fmt.Sprintf("http://%s:%d/aggregates?collection=%s&location=%s",
		ip, c.port, url.QueryEscape(collection), url.QueryEscape(strings.Join(locations, ",")))
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	withAuth(req)
	resp, err := HttpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("error code: %d", resp.StatusCode)
	}
	var body struct {
		Aggregates map[string]*domain.Abelian `json:"aggregates"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	if body.Aggregates == nil {
		return map[string]*domain.Abelian{}, nil
	}
	return body.Aggregates, nil
}

func (c *Contact) New(item *domain.Item, root string, via domain.Visited) error {
	ip, parsedIP := c.ip, net.ParseIP(c.ip)

	if parsedIP != nil && parsedIP.To4() == nil {
		ip = fmt.Sprintf("[%s]", ip)
	}

	url := fmt.Sprintf("http://%s:%d/item", ip, c.port)
	body := struct {
		Item *domain.Item `json:"item"`
		Root string       `json:"root"`
		Via  string       `json:"via,omitempty"`
	}{
		Item: item,
		Root: root,
		Via:  via.String(),
	}

	jsonData, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	req.Header.Set(domain.HandoffHeader, "1")
	withAuth(req)

	resp, err := HttpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusServiceUnavailable {
		return domain.ErrPeerBusy
	}
	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("error code: %d", resp.StatusCode)
	}

	return nil
}

func (c *Contact) Delete(item *domain.Item, root string, via domain.Visited) error {
	ip, parsedIP := c.ip, net.ParseIP(c.ip)

	if parsedIP != nil && parsedIP.To4() == nil {
		ip = fmt.Sprintf("[%s]", ip)
	}

	url := fmt.Sprintf("http://%s:%d/item/delete", ip, c.port)
	body := struct {
		Item *domain.Item `json:"item"`
		Root string       `json:"root"`
		Via  string       `json:"via,omitempty"`
	}{
		Item: item,
		Root: root,
		Via:  via.String(),
	}

	jsonData, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	withAuth(req)

	resp, err := HttpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusServiceUnavailable {
		return domain.ErrPeerBusy
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("error code: %d", resp.StatusCode)
	}

	return nil
}

// Claim announces local ownership of a child zone to the holder of its parent.
// The payload already carries the claimant peer name; origin on the wire is
// the same identity so the receiver can authenticate the announcement.
func (c *Contact) Claim(payload domain.ClaimPayload) error {
	origin := payload.Peer
	if origin == "" {
		origin = c.name
	}
	return c.postJSON("/claim", struct {
		Origin  string              `json:"origin"`
		Payload domain.ClaimPayload `json:"payload"`
	}{Origin: origin, Payload: payload})
}

func (c *Contact) postJSON(path string, body any) error {
	if c.port <= 0 {
		return fmt.Errorf("%s: invalid port on peer %s", path, c.name)
	}
	targets := c.dialTargets()
	if len(targets) == 0 {
		return fmt.Errorf("%s: no dialable IP on peer %s", path, c.name)
	}
	jsonData, err := json.Marshal(body)
	if err != nil {
		return err
	}
	var lastErr error
	for _, rawIP := range targets {
		ip := rawIP
		if parsed := net.ParseIP(rawIP); parsed != nil && parsed.To4() == nil {
			ip = fmt.Sprintf("[%s]", rawIP)
		}
		url := fmt.Sprintf("http://%s:%d%s", ip, c.port, path)
		req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		withAuth(req)
		resp, err := TransferClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		func() {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusConflict {
				lastErr = domain.ErrLeaving
				return
			}
			if resp.StatusCode >= 300 {
				lastErr = fmt.Errorf("%s: status %d", path, resp.StatusCode)
				return
			}
			lastErr = nil
		}()
		if errors.Is(lastErr, domain.ErrLeaving) {
			return lastErr
		}
		if lastErr == nil {
			return nil
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("%s: all dial targets failed for %s", path, c.name)
	}
	return lastErr
}
