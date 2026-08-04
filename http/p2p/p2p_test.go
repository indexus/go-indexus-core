package p2p

import (
	"bytes"
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

func (s *countingService) Ping(domain.Contact) (domain.Contact, error) { return nil, nil }
func (s *countingService) Neighbors(domain.Peer) ([]domain.Contact, error) {
	return nil, nil
}
func (s *countingService) Random(domain.Peer) (domain.Contact, error) { return nil, nil }
func (s *countingService) Transfer(domain.Peer, domain.Key, []*domain.Item) error {
	s.transfers++
	return nil
}
func (s *countingService) Get(string, string, int) (domain.Contact, *domain.Set, error) {
	return nil, nil, nil
}
func (s *countingService) GetMultiple(string, []string, int, []func(*domain.Abelian) int) ([]byte, error) {
	return nil, nil
}
func (s *countingService) New(*domain.Item, string, string) error     { return nil }
func (s *countingService) Handoff(*domain.Item, string, string) error { return nil }
func (s *countingService) Delete(*domain.Item, string, string) error  { return nil }
func (s *countingService) DelegationOffer(domain.Peer, domain.DelegationOfferPayload) error {
	return nil
}
func (s *countingService) WALDelta(domain.Peer, domain.WALDeltaPayload) error { return nil }
func (s *countingService) CaughtUp(domain.Peer, domain.CaughtUpPayload) error { return nil }
func (s *countingService) SwitchAck(domain.Peer, domain.SwitchAckPayload) error {
	return nil
}

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
