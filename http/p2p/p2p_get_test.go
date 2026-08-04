package p2p

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/indexus/go-indexus-core/domain"
)

type getService struct {
	countingService
	set *domain.Set
}

func (s *getService) Get(string, string, int) (domain.Contact, *domain.Set, error) {
	return nil, s.set, nil
}

func TestGetSetJSONCompatibleWithMapShape(t *testing.T) {
	set := domain.NewSet()
	set.Put("aa:x", domain.NewAbelian(1, []float64{1, 2}))

	h := &Handler{Service: &getService{set: set}}
	req := httptest.NewRequest(http.MethodGet, "/set?collection=c&location=@&depth=0", nil)
	rr := httptest.NewRecorder()
	h.Get(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}

	var body struct {
		Set map[string]*domain.Abelian `json:"set"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	ab, ok := body.Set["aa:x"]
	if !ok || ab.Count() != 1 {
		t.Fatalf("wire shape broken: %#v", body.Set)
	}
}
