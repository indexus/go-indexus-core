package peer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
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

var TransferClient = &http.Client{
	Timeout: 30 * time.Second,
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

	var body struct {
		Contact *Contact `json:"contact"`
	}
	decoder := json.NewDecoder(resp.Body)
	if err := decoder.Decode(&body); err != nil {
		return nil, err
	}

	return body.Contact, nil
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

func (c *Contact) Transfer(origin domain.Peer, key domain.Key, items []*domain.Item) error {
	if c.port <= 0 {
		return fmt.Errorf("transfer: invalid port on peer %s", c.name)
	}
	targets := c.dialTargets()
	if len(targets) == 0 {
		return fmt.Errorf("transfer: no dialable IP on peer %s", c.name)
	}
	var lastErr error
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
			return err
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
			lastErr = nil
		}()
		if errors.Is(lastErr, domain.ErrLeaving) {
			return lastErr
		}
		if lastErr == nil {
			if c.ip == "" {
				c.ip = rawIP
			}
			return nil
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("transfer: all dial targets failed for %s", c.name)
	}
	return lastErr
}

func (c *Contact) Get(collection string, location string, depth int) (domain.Contact, *domain.Set, error) {
	ip, parsedIP := c.ip, net.ParseIP(c.ip)

	if parsedIP != nil && parsedIP.To4() == nil {
		ip = fmt.Sprintf("[%s]", ip)
	}

	url := fmt.Sprintf("http://%s:%d/set?collection=%s&location=%s&depth=%d", ip, c.port, collection, location, depth)
	req, err := http.NewRequest("GET", url, nil)
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

	var body struct {
		Contact *Contact                   `json:"contact"`
		Set     map[string]*domain.Abelian `json:"set"`
	}
	decoder := json.NewDecoder(resp.Body)
	if err := decoder.Decode(&body); err != nil {
		return nil, nil, err
	}

	if body.Set == nil {
		var contact domain.Contact
		if body.Contact != nil {
			contact = body.Contact
		}
		return contact, nil, nil
	}

	set := domain.NewSet()
	for key, value := range body.Set {
		set.Put(key, value)
	}

	var contact domain.Contact
	if body.Contact != nil {
		contact = body.Contact
	}
	return contact, set, nil
}

func (c *Contact) New(item *domain.Item, root string, current string) error {
	ip, parsedIP := c.ip, net.ParseIP(c.ip)

	if parsedIP != nil && parsedIP.To4() == nil {
		ip = fmt.Sprintf("[%s]", ip)
	}

	url := fmt.Sprintf("http://%s:%d/item", ip, c.port)
	body := struct {
		Item    *domain.Item `json:"item"`
		Root    string       `json:"root"`
		Current string       `json:"current"`
	}{
		Item:    item,
		Root:    root,
		Current: current,
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

func (c *Contact) Delete(item *domain.Item, root string, current string) error {
	ip, parsedIP := c.ip, net.ParseIP(c.ip)

	if parsedIP != nil && parsedIP.To4() == nil {
		ip = fmt.Sprintf("[%s]", ip)
	}

	url := fmt.Sprintf("http://%s:%d/item/delete", ip, c.port)
	body := struct {
		Item    *domain.Item `json:"item"`
		Root    string       `json:"root"`
		Current string       `json:"current"`
	}{
		Item:    item,
		Root:    root,
		Current: current,
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

func (c *Contact) DelegationOffer(origin domain.Peer, offer domain.DelegationOfferPayload) error {
	return c.postJSON("/delegation/offer", struct {
		Origin string                        `json:"origin"`
		Offer  domain.DelegationOfferPayload `json:"offer"`
	}{Origin: origin.Name(), Offer: offer})
}

func (c *Contact) WALDelta(origin domain.Peer, delta domain.WALDeltaPayload) error {
	return c.postJSON("/delegation/wal-delta", struct {
		Origin string                 `json:"origin"`
		Delta  domain.WALDeltaPayload `json:"delta"`
	}{Origin: origin.Name(), Delta: delta})
}

func (c *Contact) CaughtUp(origin domain.Peer, payload domain.CaughtUpPayload) error {
	return c.postJSON("/delegation/caught-up", struct {
		Origin  string                 `json:"origin"`
		Payload domain.CaughtUpPayload `json:"payload"`
	}{Origin: origin.Name(), Payload: payload})
}

func (c *Contact) SwitchAck(origin domain.Peer, payload domain.SwitchAckPayload) error {
	return c.postJSON("/delegation/switch-ack", struct {
		Origin  string                  `json:"origin"`
		Payload domain.SwitchAckPayload `json:"payload"`
	}{Origin: origin.Name(), Payload: payload})
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
