package peer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
)

// HttpClient is the default client used for short P2P RPCs (Ping,
// Neighbors, Random, Get, New). The 2-second default is conservative
// enough to absorb WAN jitter while still failing peers fast.
var HttpClient = &http.Client{
	Timeout: 2 * time.Second,
}

// TransferClient is used for /transfer, which can carry up to
// `delegation` items in a single batch and therefore needs a much
// larger budget than the discovery RPCs.
var TransferClient = &http.Client{
	Timeout: 30 * time.Second,
}

type Contact struct {
	name string
	id   []byte
	ips  map[string]any
	ip   string
	port int
}

// Implementing json.Marshaler interface
func (c *Contact) MarshalJSON() ([]byte, error) {
	type Alias struct {
		Name string         `json:"name"`
		IPs  map[string]any `json:"ips"`
		IP   string         `json:"ip"`
		Port int            `json:"port"`
	}
	return json.Marshal(&Alias{
		Name: c.name,
		IPs:  c.ips,
		IP:   c.ip,
		Port: c.port,
	})
}

// Implementing json.Unmarshaler interface
func (c *Contact) UnmarshalJSON(data []byte) error {
	type Alias struct {
		Name string         `json:"name"`
		IPs  map[string]any `json:"ips"`
		IP   string         `json:"ip"`
		Port int            `json:"port"`
	}
	aux := &Alias{}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	c.name = aux.Name
	c.ips = aux.IPs
	c.ip = aux.IP
	c.port = aux.Port
	c.id, _ = encoding.BASE64.Decode(aux.Name)
	return nil
}

func NewContact(name string, ips map[string]any, port int) domain.Contact {
	id, _ := encoding.BASE64.Decode(name)
	return &Contact{
		name: name,
		id:   id,
		ips:  ips,
		port: port,
	}
}

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
	return fmt.Sprintf("%s@%s|%d", c.name, c.ip, c.port)
}

func (c *Contact) Ping(origin domain.Contact) (domain.Contact, error) {
	for ip := range c.ips {
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
	reqBody := struct {
		Origin *Contact `json:"origin"`
	}{
		Origin: &Contact{
			name: origin.Name(),
			ips:  origin.IPs(),
			port: origin.Port(),
		},
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

func (c *Contact) Transfer(origin domain.Peer, key domain.Key, items []*domain.Item) error {
	ip, parsedIP := c.ip, net.ParseIP(c.ip)

	if parsedIP != nil && parsedIP.To4() == nil {
		ip = fmt.Sprintf("[%s]", ip)
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
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := TransferClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("error code: %d", resp.StatusCode)
	}

	return nil
}

func (c *Contact) Get(collection string, location string, depth int) (domain.Contact, *domain.Set, error) {
	ip, parsedIP := c.ip, net.ParseIP(c.ip)

	if parsedIP != nil && parsedIP.To4() == nil {
		ip = fmt.Sprintf("[%s]", ip)
	}

	url := fmt.Sprintf("http://%s:%d/set?collection=%s&location=%s&depth=%d", ip, c.port, collection, location, depth)
	resp, err := HttpClient.Get(url)
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

	resp, err := HttpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusServiceUnavailable {
		return fmt.Errorf("ingress queue full")
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

	resp, err := HttpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusServiceUnavailable {
		return fmt.Errorf("ingress queue full")
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("error code: %d", resp.StatusCode)
	}

	return nil
}
