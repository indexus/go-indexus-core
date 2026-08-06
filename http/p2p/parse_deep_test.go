package p2p

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseDeepHop(t *testing.T) {
	cases := []struct {
		query    string
		wantDeep bool
		wantHop  int
	}{
		{"", true, 8},
		{"deep=true", true, 8},
		{"deep=false", false, 0},
		{"deep=0", false, 0},
		{"depth=0", false, 0},
		{"depth=2", true, 8},
		{"deep=true&hop=3", true, 3},
		{"deep=true&hop=0", true, 0},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, "/set?"+tc.query, nil)
		deep, hop := parseDeepHop(req)
		if deep != tc.wantDeep || hop != tc.wantHop {
			t.Fatalf("%q → deep=%v hop=%d want deep=%v hop=%d",
				tc.query, deep, hop, tc.wantDeep, tc.wantHop)
		}
	}
}
