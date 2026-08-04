package peer

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/indexus/go-indexus-core/domain"
)

type fakeOrigin struct {
	name string
	ips  map[string]any
	port int
}

func (f *fakeOrigin) ID() []byte                                             { return nil }
func (f *fakeOrigin) Name() string                                           { return f.name }
func (f *fakeOrigin) IPs() map[string]any                                    { return f.ips }
func (f *fakeOrigin) Port() int                                              { return f.port }
func (f *fakeOrigin) IP() string                                             { return "" }
func (f *fakeOrigin) Host() string                                           { return "" }
func (f *fakeOrigin) Ping(domain.Contact) (domain.Contact, error)            { return nil, nil }
func (f *fakeOrigin) Neighbors(domain.Peer) ([]domain.Contact, error)        { return nil, nil }
func (f *fakeOrigin) Random(domain.Peer) (domain.Contact, error)             { return nil, nil }
func (f *fakeOrigin) Transfer(domain.Peer, domain.Key, []*domain.Item) error { return nil }
func (f *fakeOrigin) Get(string, string, int) (domain.Contact, *domain.Set, error) {
	return nil, nil, nil
}
func (f *fakeOrigin) New(*domain.Item, string, string) error    { return nil }
func (f *fakeOrigin) Delete(*domain.Item, string, string) error { return nil }

func TestNewContactSetsPrimaryIP(t *testing.T) {
	c := NewContact("PeerAAAAAAAAAAAA", map[string]any{"127.0.0.1": nil}, 21010).(*Contact)
	if c.IP() != "127.0.0.1" {
		t.Fatalf("IP=%q want 127.0.0.1", c.IP())
	}
	host := c.Host()
	if host != "PeerAAAAAAAAAAAA@127.0.0.1|21010" {
		t.Fatalf("Host=%q", host)
	}
}

func newServerContact(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *Contact) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	u := srv.Listener.Addr().(interface{ String() string }).String()
	host, port := splitHostPort(t, u)

	c := &Contact{
		name: "peer",
		ips:  map[string]any{host: nil},
		ip:   host,
		port: port,
	}
	return srv, c
}

func splitHostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := splitAddr(addr)
	if err != nil {
		t.Fatalf("split addr %q: %v", addr, err)
	}
	port := 0
	for _, r := range portStr {
		port = port*10 + int(r-'0')
	}
	return host, port
}

func splitAddr(addr string) (string, string, error) {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[:i], addr[i+1:], nil
		}
	}
	return addr, "", nil
}

func TestPingPropagatesNon2xxAsError(t *testing.T) {
	_, c := newServerContact(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	origin := &fakeOrigin{name: "self", ips: map[string]any{"127.0.0.1": nil}, port: 1}
	got, err := c.Ping(origin)
	if err == nil {
		t.Fatalf("Ping must return an error on non-2xx response")
	}
	if got != nil {
		t.Fatalf("Ping must not return a contact on non-2xx response, got %#v", got)
	}
}

func TestPingHappyPathReturnsContact(t *testing.T) {
	var serverPort int
	srv, c := newServerContact(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(struct {
			Contact *Contact `json:"contact"`
		}{
			Contact: &Contact{name: "peer", ips: map[string]any{}, port: serverPort},
		})
	})
	_ = srv
	serverPort = c.port

	origin := &fakeOrigin{name: "self", ips: map[string]any{"127.0.0.1": nil}, port: 1}
	got, err := c.Ping(origin)
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got == nil || got.Name() != "peer" {
		t.Fatalf("Ping returned unexpected contact: %#v", got)
	}
}

func TestNewMaps503ToErrPeerBusy(t *testing.T) {
	_, c := newServerContact(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})

	item := &domain.Item{Collection: "c", Location: "a", Id: "x", Metrics: []float64{1}}
	err := c.New(item, "@", "a")
	if err == nil {
		t.Fatal("New must surface 503 as an error")
	}
	if !errors.Is(err, domain.ErrPeerBusy) {
		t.Fatalf("New: %v, want ErrPeerBusy", err)
	}
}
