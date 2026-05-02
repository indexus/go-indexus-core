package peer

import (
	"encoding/json"
	"fmt"
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
	urlStr := c.httpBaseURL(ip) + "/ping"
	reqBody := struct {
		Origin *Contact `json:"origin"`
	}{
		Origin: &Contact{
			name: origin.Name(),
			ips:  origin.IPs(),
			port: origin.Port(),
		},
	}
	var respBody struct {
		Contact *Contact `json:"contact"`
	}
	st, err := jsonRoundTrip(HttpClient, "POST", urlStr, reqBody, &respBody)
	if err != nil {
		return nil, err
	}
	if st != http.StatusOK {
		return nil, fmt.Errorf("ping %s: unexpected status %d", urlStr, st)
	}
	contact := respBody.Contact
	contact.ips[ip] = nil
	contact.ip = ip
	return contact, nil
}

func (c *Contact) Neighbors(origin domain.Peer) ([]domain.Contact, error) {
	q := url.Values{}
	q.Set("origin", origin.Name())
	urlStr := c.url("/neighbors", q)
	var body struct {
		Neighbors []*Contact `json:"neighbors"`
	}
	st, err := jsonRoundTrip(HttpClient, "GET", urlStr, nil, &body)
	if err != nil {
		return nil, fmt.Errorf("error making request: %s", err.Error())
	}
	if st != http.StatusOK {
		return nil, fmt.Errorf("error code: %d", st)
	}
	for _, neighbor := range body.Neighbors {
		if neighbor.Name() == c.Name() {
			neighbor.ip = c.ip
		}
	}
	return domain.ConvertToContactSlice(body.Neighbors), nil
}

func (c *Contact) Random(origin domain.Peer) (domain.Contact, error) {
	q := url.Values{}
	q.Set("origin", origin.Name())
	urlStr := c.url("/random", q)
	var body struct {
		Contact *Contact `json:"contact"`
	}
	st, err := jsonRoundTrip(HttpClient, "GET", urlStr, nil, &body)
	if err != nil {
		return nil, fmt.Errorf("error making request: %s", err.Error())
	}
	if st != http.StatusOK {
		return nil, fmt.Errorf("error code: %d", st)
	}
	return body.Contact, nil
}

func (c *Contact) Transfer(origin domain.Peer, key domain.Key, items []*domain.Item) error {
	urlStr := c.url("/transfer", nil)
	reqBody := struct {
		Origin string         `json:"origin"`
		Key    domain.Key     `json:"key"`
		Items  []*domain.Item `json:"items"`
	}{
		Origin: origin.Name(),
		Key:    key,
		Items:  items,
	}
	st, err := jsonRoundTrip(TransferClient, "POST", urlStr, reqBody, nil)
	if err != nil {
		return err
	}
	if st != http.StatusCreated {
		return fmt.Errorf("error code: %d", st)
	}
	return nil
}

func (c *Contact) Get(collection string, location string) (domain.Contact, *domain.Set, error) {
	// `local=1` tells the receiving peer to answer from its own state only
	// instead of fanning the query out to other peers. Without it, two
	// nodes that both lack the shard could ping-pong forever asking each
	// other for it.
	q := url.Values{}
	q.Set("collection", collection)
	q.Set("location", location)
	q.Set("local", "1")
	urlStr := c.url("/set", q)
	var body struct {
		Contact *Contact                   `json:"contact"`
		Set     map[string]*domain.Abelian `json:"set"`
	}
	st, err := jsonRoundTrip(HttpClient, "GET", urlStr, nil, &body)
	if err != nil {
		return nil, nil, err
	}
	if st != http.StatusOK {
		return nil, nil, fmt.Errorf("error code: %d", st)
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
	_ = precision
	_ = properties
	q := url.Values{}
	q.Set("collection", collection)
	q.Set("location", strings.Join(locations, ","))
	urlStr := c.url("/sets", q)
	body, st, err := doRaw(HttpClient, "GET", urlStr)
	if err != nil {
		return nil, err
	}
	if st != http.StatusOK {
		return nil, fmt.Errorf("error code: %d", st)
	}
	return body, nil
}

func (c *Contact) New(item *domain.Item, root string, current string) error {
	urlStr := c.url("/item", nil)
	reqBody := struct {
		Item    *domain.Item `json:"item"`
		Root    string       `json:"root"`
		Current string       `json:"current"`
	}{
		Item:    item,
		Root:    root,
		Current: current,
	}
	st, err := jsonRoundTrip(ItemClient, "POST", urlStr, reqBody, nil)
	if err != nil {
		return err
	}
	if st != http.StatusCreated {
		return fmt.Errorf("error code: %d", st)
	}
	return nil
}
