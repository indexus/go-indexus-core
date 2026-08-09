package p2p

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/auth"
	"github.com/indexus/go-indexus-core/domain"
)

// countingService records whether the handler reached the node.
type countingService struct {
	transfers int
}

func (s *countingService) Name() string                                { return "receiver" }
func (s *countingService) Ping(domain.Contact) (domain.Contact, error) { return nil, nil }
func (s *countingService) Neighbors(domain.Peer) ([]domain.Contact, error) {
	return nil, nil
}
func (s *countingService) Random(domain.Peer) (domain.Contact, error) { return nil, nil }
func (s *countingService) Transfer(domain.Peer, domain.Key, []*domain.Item) (string, error) {
	s.transfers++
	return s.Name(), nil
}
func (s *countingService) Get(string, string, bool, domain.Visited, bool) (domain.Contact, *domain.Set, error) {
	return nil, nil, nil
}
func (s *countingService) GetMultiple(string, []string, int, []func(*domain.Abelian) int, bool, domain.Visited, bool, bool) ([]byte, error) {
	return nil, nil
}
func (s *countingService) Children(string, string) (map[string]*domain.ChildEntry, error) {
	return nil, nil
}
func (s *countingService) New(*domain.Item, string, domain.Visited) error     { return nil }
func (s *countingService) Handoff(*domain.Item, string, domain.Visited) error { return nil }
func (s *countingService) Delete(*domain.Item, string, domain.Visited) error  { return nil }
func (s *countingService) Claim(domain.Peer, domain.ClaimPayload) error       { return nil }

func transferRequest(t *testing.T, bearer string) *http.Request {
	t.Helper()

	body, err := json.Marshal(map[string]any{
		"origin": "peer",
		"key":    domain.Key{Collection: "demo", Location: "a"},
		"items":  []*domain.Item{{Collection: "demo", Location: "aa", Id: "x"}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/transfer", bytes.NewReader(body))
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	return request
}

// writeToken returns a bearer the verifier accepts, with the scopes a node's
// service token carries.
func writeToken(t *testing.T, issuer *auth.Issuer, scopes ...string) string {
	t.Helper()

	token, err := issuer.SignClientToken("node", scopes, time.Hour)
	if err != nil {
		t.Fatalf("SignClientToken: %v", err)
	}
	encoded, err := auth.EncodeToken(token)
	if err != nil {
		t.Fatalf("EncodeToken: %v", err)
	}
	return encoded
}

// /transfer writes items into the node, so it has to be gated like /item.
// Leaving it open lets anyone inject or overwrite data on an authenticated mesh.
func TestTransferRequiresWriteScope(t *testing.T) {
	keys, err := auth.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	issuer := auth.NewIssuer("test-net", keys)

	cases := map[string]struct {
		bearer string
		status int
		calls  int
	}{
		"no token":   {bearer: "", status: http.StatusUnauthorized, calls: 0},
		"read only":  {bearer: writeToken(t, issuer, "read"), status: http.StatusUnauthorized, calls: 0},
		"junk token": {bearer: "not-a-token", status: http.StatusUnauthorized, calls: 0},
		"read write": {bearer: writeToken(t, issuer, "read", "write"), status: http.StatusCreated, calls: 1},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			service := &countingService{}
			handler := &Handler{
				Service:     service,
				Verifier:    auth.NewVerifier("test-net", keys.Public),
				RequireAuth: true,
			}

			recorder := httptest.NewRecorder()
			handler.Transfer(recorder, transferRequest(t, tc.bearer))

			if recorder.Code != tc.status {
				t.Fatalf("status: got %d want %d", recorder.Code, tc.status)
			}
			if service.transfers != tc.calls {
				t.Fatalf("service reached %d times, want %d", service.transfers, tc.calls)
			}
		})
	}
}

func TestParseGetFlags(t *testing.T) {
	cases := []struct {
		query       string
		wantDeep    bool
		wantVia     string
		wantRefresh bool
	}{
		{"", true, "", false},
		{"deep=true", true, "", false},
		{"deep=false", false, "", false},
		{"deep=0", false, "", false},
		{"depth=0", false, "", false},
		{"depth=2", true, "", false},
		{"deep=true&via=a,b", true, "a,b", false},
		{"refresh=true", true, "", true},
		{"refresh=1&deep=false&via=peer", false, "peer", true},
		{"refresh=false", true, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/set?"+tc.query, nil)
			deep, via, refresh := parseGetFlags(req)
			if deep != tc.wantDeep || via.String() != tc.wantVia || refresh != tc.wantRefresh {
				t.Fatalf("parseGetFlags(%q)=(%v,%q,%v) want (%v,%q,%v)",
					tc.query, deep, via.String(), refresh, tc.wantDeep, tc.wantVia, tc.wantRefresh)
			}
		})
	}
}

func TestDecodeRoutingKeyBase64URL(t *testing.T) {
	key := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	raw := base64.RawURLEncoding.EncodeToString(key)
	got, ok := decodeRoutingKey(raw)
	if !ok || !bytes.Equal(got, key) {
		t.Fatalf("decodeRoutingKey(%q)=(%v,%v)", raw, got, ok)
	}
	padded := base64.URLEncoding.EncodeToString(key)
	got, ok = decodeRoutingKey(padded)
	if !ok || !bytes.Equal(got, key) {
		t.Fatalf("padded decodeRoutingKey(%q)=(%v,%v)", padded, got, ok)
	}
}

func TestParseEnvelopeFlag(t *testing.T) {
	cases := map[string]bool{
		"":           false,
		"envelope=1": true,
		"envelope=true": true,
		"envelope=0": false,
		"envelope=false": false,
	}
	for query, want := range cases {
		req := httptest.NewRequest(http.MethodGet, "/sets?"+query, nil)
		if got := parseEnvelopeFlag(req); got != want {
			t.Fatalf("parseEnvelopeFlag(%q)=%v want %v", query, got, want)
		}
	}
}

type hintService struct {
	countingService
	hint domain.Contact
	last []byte
}

func (s *hintService) RoutingHint(key []byte) domain.Contact {
	s.last = append([]byte(nil), key...)
	return s.hint
}

func (s *hintService) GetMultiple(string, []string, int, []func(*domain.Abelian) int, bool, domain.Visited, bool, bool) ([]byte, error) {
	return []byte{0x00}, nil
}

type stubHintContact struct {
	name string
	ip   string
	port int
}

func (c *stubHintContact) ID() []byte                                              { return nil }
func (c *stubHintContact) Name() string                                            { return c.name }
func (c *stubHintContact) IPs() map[string]any                                     { return map[string]any{c.ip: nil} }
func (c *stubHintContact) Port() int                                               { return c.port }
func (c *stubHintContact) IP() string                                              { return c.ip }
func (c *stubHintContact) Host() string                                            { return c.ip }
func (c *stubHintContact) Ping(domain.Contact) (domain.Contact, error)             { return nil, nil }
func (c *stubHintContact) Neighbors(domain.Peer) ([]domain.Contact, error)         { return nil, nil }
func (c *stubHintContact) Random(domain.Peer) (domain.Contact, error)              { return nil, nil }
func (c *stubHintContact) Transfer(domain.Peer, domain.Key, []*domain.Item) (string, error) {
	return "", nil
}
func (c *stubHintContact) Get(string, string, bool, domain.Visited, bool) (domain.Contact, *domain.Set, error) {
	return nil, nil, nil
}
func (c *stubHintContact) Children(string, string) (map[string]*domain.ChildEntry, error) {
	return nil, nil
}
func (c *stubHintContact) New(*domain.Item, string, domain.Visited) error    { return nil }
func (c *stubHintContact) Delete(*domain.Item, string, domain.Visited) error { return nil }

func TestGetMultipleExposesIngressHintHeaders(t *testing.T) {
	hint := &stubHintContact{name: "AAAAAAAAAAAAAAAAAAAAAA", ip: "10.0.0.2", port: 21002}
	service := &hintService{hint: hint}
	handler := &Handler{Service: service}

	key := make([]byte, 16)
	req := httptest.NewRequest(http.MethodGet, "/sets?collection=demo&location=qs", nil)
	req.Header.Set("X-Indexus-Routing-Key", base64.RawURLEncoding.EncodeToString(key))

	recorder := httptest.NewRecorder()
	handler.GetMultiple(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("X-Indexus-Ingress-Name"); got != hint.Name() {
		t.Fatalf("ingress name=%q want=%q", got, hint.Name())
	}
	if got := recorder.Header().Get("X-Indexus-Ingress-IP"); got != "10.0.0.2" {
		t.Fatalf("ingress ip=%q", got)
	}
	if got := recorder.Header().Get("X-Indexus-Ingress-Port"); got != "21002" {
		t.Fatalf("ingress port=%q", got)
	}
	if !bytes.Equal(service.last, key) {
		t.Fatalf("RoutingHint key=%v want=%v", service.last, key)
	}
}

func TestGetMultipleForwardsEnvelopeFlag(t *testing.T) {
	service := &envelopeFlagService{}
	handler := &Handler{Service: service}
	req := httptest.NewRequest(http.MethodGet, "/sets?collection=demo&location=qs&envelope=1&deep=false", nil)
	recorder := httptest.NewRecorder()
	handler.GetMultiple(recorder, req)
	if !service.envelope || service.deep {
		t.Fatalf("flags envelope=%v deep=%v", service.envelope, service.deep)
	}
}

type envelopeFlagService struct {
	countingService
	envelope bool
	deep     bool
}

func (s *envelopeFlagService) GetMultiple(_ string, _ []string, _ int, _ []func(*domain.Abelian) int, deep bool, _ domain.Visited, _ bool, envelope bool) ([]byte, error) {
	s.deep = deep
	s.envelope = envelope
	return []byte{0x00}, nil
}

// Auth is opt-in: a mesh running without a verifier must keep working.
func TestTransferOpenWhenAuthDisabled(t *testing.T) {
	service := &countingService{}
	handler := &Handler{Service: service}

	recorder := httptest.NewRecorder()
	handler.Transfer(recorder, transferRequest(t, ""))

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status: got %d want %d", recorder.Code, http.StatusCreated)
	}
	if service.transfers != 1 {
		t.Fatalf("service reached %d times, want 1", service.transfers)
	}
}
