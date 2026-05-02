package peer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/indexus/go-indexus-core/domain"
	"github.com/indexus/go-indexus-core/encoding"
)

// HttpClient is the default client used for short, idempotent P2P RPCs
// (Ping, Neighbors, Random, Get). The 2-second default is conservative
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

// ItemClient is used for forwarding individual items via /item. Even
// though the receiver only enqueues and returns 201, the response can
// still be delayed by GC pauses, lock contention on a busy queue, or
// kernel-level socket backpressure when the loader saturates the
// destination with concurrent POSTs. A 2-second budget proved too
// tight: the origin spuriously timed out, re-queued, and the receiver
// ended up adding the same entry twice — silently inflating parent
// aggregates while sometimes losing the item entirely if the retry
// also raced. Bump the budget enough to absorb that backpressure
// without giving up the ability to detect a truly dead peer.
var ItemClient = &http.Client{
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

func (c *Contact) Get(collection string, location string) (domain.Contact, *domain.Set, error) {
	ip, parsedIP := c.ip, net.ParseIP(c.ip)

	if parsedIP != nil && parsedIP.To4() == nil {
		ip = fmt.Sprintf("[%s]", ip)
	}

	// `local=1` tells the receiving peer to answer from its own state only
	// instead of fanning the query out to other peers. Without it, two
	// nodes that both lack the shard could ping-pong forever asking each
	// other for it.
	url := fmt.Sprintf("http://%s:%d/set?collection=%s&location=%s&local=1", ip, c.port, collection, location)
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

	// Distinguish "peer doesn't have this set" (body.Set is nil) from
	// "peer returned an empty set (zero entries)". The former must not
	// overwrite a locally-cached aggregate with zero, otherwise the
	// Update job silently rolls back items every refresh tick.
	if body.Set == nil {
		return body.Contact, nil, nil
	}
	set := domain.NewSet()
	for key, value := range body.Set {
		set.Put(key, value)
	}

	return body.Contact, set, nil
}

func (c *Contact) GetMultiple(collection string, locations []string, precision int, properties []func(*domain.Abelian) int) ([]byte, error) {
	ip, parsedIP := c.ip, net.ParseIP(c.ip)

	if parsedIP != nil && parsedIP.To4() == nil {
		ip = fmt.Sprintf("[%s]", ip)
	}

	query := url.Values{}
	query.Set("collection", collection)
	query.Set("location", strings.Join(locations, ","))

	url := fmt.Sprintf("http://%s:%d/sets?%s", ip, c.port, query.Encode())
	resp, err := HttpClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("error code: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	return body, nil
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

	resp, err := ItemClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("error code: %d", resp.StatusCode)
	}

	return nil
}
