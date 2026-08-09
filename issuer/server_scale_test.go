package issuer

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/auth"
)

func writeLocalStubs(t *testing.T, dir string, countOut string) {
	t.Helper()
	mustWrite := func(name, body string) {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	mustWrite("count.sh", "#!/bin/sh\necho "+countOut+"\n")
	mustWrite("spawn.sh", "#!/bin/sh\necho local-0\n")
	mustWrite("terminate.sh", "#!/bin/sh\nexit 0\n")
}

func TestScaleReconcilesGhostScaleCount(t *testing.T) {
	dir := t.TempDir()
	writeLocalStubs(t, dir, "0")

	key, err := auth.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	s := NewServer(auth.NewIssuer("test-net", key))
	s.ScaleEnabled = true
	s.LaunchTemplateID = "local"
	s.LocalDir = dir
	s.SpawnMaxExtra = 2
	s.ScaleCooldown = time.Millisecond
	// Simulate prior launches that were terminated outside SoftLeave.
	s.scaleCount = 2

	body, _ := json.Marshal(map[string]any{
		"requester_id":  "lab",
		"local_inserts": 9999,
		"spawn_count":   1,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/scale", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.scale(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s want 200 (ghost scaleCount must not block)", rec.Code, rec.Body.String())
	}
	if s.scaleCount != 1 {
		t.Fatalf("scaleCount=%d want 1 after successful spawn", s.scaleCount)
	}
}

func TestDrainLockRespectsDownCooldown(t *testing.T) {
	key, err := auth.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	s := NewServer(auth.NewIssuer("test-net", key))
	s.DownCooldown = time.Hour
	s.lastDownscale = time.Now()

	body, _ := json.Marshal(map[string]any{
		"requester_id": "lab",
		"instance_id":  "local-1",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/drain-lock", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.drainLock(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d body=%s want 429 during down cooldown", rec.Code, rec.Body.String())
	}

	// Holder renewing mid-drain must not be blocked.
	s.drainingID = "local-1"
	s.drainingSince = time.Now()
	req = httptest.NewRequest(http.MethodPost, "/v1/drain-lock", bytes.NewReader(body))
	rec = httptest.NewRecorder()
	s.drainLock(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("renew status=%d body=%s want 200", rec.Code, rec.Body.String())
	}
}

func TestScaleParallelRequestersReserveSlots(t *testing.T) {
	dir := t.TempDir()
	// Slow spawn so overlapping requests both see reserved capacity.
	mustWrite := func(name, body string) {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	mustWrite("count.sh", "#!/bin/sh\necho 0\n")
	mustWrite("spawn.sh", "#!/bin/sh\nsleep 0.2\necho local-$$\n")
	mustWrite("terminate.sh", "#!/bin/sh\nexit 0\n")

	key, err := auth.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	s := NewServer(auth.NewIssuer("test-net", key))
	s.ScaleEnabled = true
	s.LaunchTemplateID = "local"
	s.LocalDir = dir
	s.SpawnMaxExtra = 2
	s.ScaleCooldown = time.Millisecond
	s.GlobalAPICooldown = time.Millisecond

	// First request reserves 2; second should 409.
	done := make(chan int, 2)
	go func() {
		body, _ := json.Marshal(map[string]any{
			"requester_id": "a", "spawn_count": 2, "prefer_near": "aaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		})
		req := httptest.NewRequest(http.MethodPost, "/v1/scale", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		s.scale(rec, req)
		done <- rec.Code
	}()
	time.Sleep(50 * time.Millisecond)
	body, _ := json.Marshal(map[string]any{
		"requester_id": "b", "spawn_count": 1,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/scale", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.scale(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("second status=%d body=%s want 409 while slots reserved", rec.Code, rec.Body.String())
	}
	code := <-done
	if code != http.StatusOK {
		t.Fatalf("first status=%d want 200", code)
	}
}

func TestScaleMaxWhenReallyFull(t *testing.T) {
	dir := t.TempDir()
	writeLocalStubs(t, dir, "2")

	key, err := auth.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair: %v", err)
	}
	s := NewServer(auth.NewIssuer("test-net", key))
	s.ScaleEnabled = true
	s.LaunchTemplateID = "local"
	s.LocalDir = dir
	s.SpawnMaxExtra = 2
	s.ScaleCooldown = time.Millisecond
	s.scaleCount = 2

	body, _ := json.Marshal(map[string]any{
		"requester_id":  "lab",
		"local_inserts": 9999,
		"spawn_count":   1,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/scale", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.scale(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s want 409 when live inventory is full", rec.Code, rec.Body.String())
	}
}
