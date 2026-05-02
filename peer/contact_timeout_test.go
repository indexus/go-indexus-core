package peer

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/domain"
)

func TestContactNewItemClientAllowsSlowEnqueue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(1500 * time.Millisecond)
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)

	host, port := splitHostPort(t, srv.Listener.Addr().String())
	c := &Contact{
		name: "dst",
		ips:  map[string]any{host: nil},
		ip:   host,
		port: port,
	}
	item := &domain.Item{Collection: "c", Location: "a", Id: "1", Metrics: []float64{1}}

	old := ItemClient
	ItemClient = &http.Client{Timeout: 5 * time.Second}
	defer func() { ItemClient = old }()

	if err := c.New(item, "@", "a"); err != nil {
		t.Fatalf("New with ItemClient 5s budget: %v", err)
	}
}

func TestContactNewShortClientTimesOut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(srv.Close)

	host, port := splitHostPort(t, srv.Listener.Addr().String())
	c := &Contact{
		name: "dst",
		ips:  map[string]any{host: nil},
		ip:   host,
		port: port,
	}
	item := &domain.Item{Collection: "c", Location: "a", Id: "1", Metrics: []float64{1}}

	old := ItemClient
	ItemClient = &http.Client{Timeout: 300 * time.Millisecond}
	defer func() { ItemClient = old }()

	err := c.New(item, "@", "a")
	if err == nil {
		t.Fatal("expected timeout error with 300ms client")
	}
}
