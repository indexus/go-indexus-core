package core

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/indexus/go-indexus-core/encoding"
)

type AutoscaleConfig struct {
	Enabled bool

	Role      string
	IssuerURL string

	Window        time.Duration
	DownThreshold int64
	DownHold      time.Duration
	Cooldown      time.Duration

	QueueAbsThreshold int
	PressureHold      time.Duration

	MemLimitPct float64
	// DiskMinFreePct: free-disk floor that forces scale-up. 0 disables
	// (INDEXUS_DISK_MIN_FREE_PCT=0). Unset defaults to 35.
	DiskMinFreePct float64
	CPULimitPct    float64
	MemLead        time.Duration
	MemFloorPct    float64
	CPUFloorPct    float64
	MemRisePct     float64
	CPURisePct     float64
	RiseHold       time.Duration
	// MemRefusePct: emergency instant scale-up + same env as Settings write refuse
	// (INDEXUS_MEM_REFUSE_PCT). Soft mem scale uses MemLimitPct + hold instead.
	MemRefusePct float64
	// ItemsLimit: owned official item count that forces scale-up even without
	// mem/CPU pressure (0 disables). Env: INDEXUS_ITEMS_LIMIT.
	ItemsLimit int
}

type InsertWindow struct {
	mu      sync.Mutex
	buckets []int64
	start   time.Time
	size    int
	bucket  time.Duration
}

func NewInsertWindow(window time.Duration) *InsertWindow {
	bucket := time.Minute
	if window > 0 && window < 2*time.Minute {
		bucket = time.Second
	}
	if window < bucket {
		window = bucket
	}
	n := int(window / bucket)
	if n < 1 {
		n = 1
	}
	if n > 120 {
		n = 120
	}
	return &InsertWindow{
		buckets: make([]int64, n),
		start:   time.Now().UTC().Truncate(bucket),
		size:    n,
		bucket:  bucket,
	}
}

func (w *InsertWindow) Record(n int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.rotateLocked()
	w.buckets[w.size-1] += n
}

func (w *InsertWindow) Sum() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.rotateLocked()
	var s int64
	for _, v := range w.buckets {
		s += v
	}
	return s
}

func (w *InsertWindow) rotateLocked() {
	now := time.Now().UTC().Truncate(w.bucket)
	elapsed := int(now.Sub(w.start) / w.bucket)
	if elapsed <= 0 {
		return
	}
	if elapsed >= w.size {
		for i := range w.buckets {
			w.buckets[i] = 0
		}
		w.start = now
		return
	}
	copy(w.buckets, w.buckets[elapsed:])
	for i := w.size - elapsed; i < w.size; i++ {
		w.buckets[i] = 0
	}
	w.start = w.start.Add(time.Duration(elapsed) * w.bucket)
}

type ScaleUpRequest struct {
	PreferNear string

	PreferNears []string
	Reason      string

	SpawnCount int
}

type PressureInput struct {
	Queue      int
	OwnedZones int
	OwnedItems int
	Resources  ResourceSample
	SelfName   string
}

type AutoscaleController struct {
	cfg AutoscaleConfig

	window *InsertWindow

	mu           sync.Mutex
	lastUpAt     time.Time
	lastDownAt   time.Time
	downSince    time.Time
	joinedAt     time.Time
	upInFlight   atomic.Bool
	downInFlight atomic.Bool
	scaleUpsDone int

	wasHot bool

	downBackoffUntil time.Time

	effectiveDownHold time.Duration

	lastQueue      int
	lastQueueAt    time.Time
	lastMem        float64
	lastMemAt      time.Time
	lastCPU        float64
	lastCPUAt      time.Time
	queueHotSince  time.Time
	riseHotSince   time.Time
	risingFast     bool
	lastReason     string
	lastPreferNear string
	lastPressure   map[string]any

	reliefArrived bool

	// nearBuilder partitions retained owned load into PreferNear tags (~1/N each).
	nearBuilder func(spawnN int, fallback string) []string
}

func NewAutoscaleController(cfg AutoscaleConfig) *AutoscaleController {
	if cfg.Window <= 0 {
		cfg.Window = time.Minute
	}
	if cfg.DownThreshold <= 0 {
		cfg.DownThreshold = 100
	}
	if cfg.DownHold <= 0 {

		cfg.DownHold = 15 * time.Minute
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = 15 * time.Second
	}
	if cfg.Role == "" {
		cfg.Role = "bootstrap"
	}
	if cfg.QueueAbsThreshold <= 0 {
		cfg.QueueAbsThreshold = 50000
	}
	if cfg.PressureHold <= 0 {

		cfg.PressureHold = 30 * time.Second
	}

	if cfg.MemLimitPct <= 0 {
		cfg.MemLimitPct = 40
	}

	if cfg.DiskMinFreePct <= 0 {
		cfg.DiskMinFreePct = 35
	}
	if cfg.CPULimitPct <= 0 {
		cfg.CPULimitPct = 70
	}
	if cfg.MemLead <= 0 {
		cfg.MemLead = 90 * time.Second
	}
	if cfg.MemFloorPct <= 0 {
		cfg.MemFloorPct = 25
	}
	if cfg.CPUFloorPct <= 0 {
		cfg.CPUFloorPct = 25
	}

	if cfg.MemRisePct <= 0 {
		cfg.MemRisePct = 2.0
	}
	if cfg.CPURisePct <= 0 {
		cfg.CPURisePct = 2.0
	}
	if cfg.RiseHold <= 0 {
		cfg.RiseHold = 12 * time.Second
	}

	if v := envInt("INDEXUS_QUEUE_ABS", 0); v > 0 {
		cfg.QueueAbsThreshold = v
	}
	if raw := os.Getenv("INDEXUS_PRESSURE_HOLD"); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
			cfg.PressureHold = parsed
		}
	}
	if raw := os.Getenv("INDEXUS_RISE_HOLD"); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
			cfg.RiseHold = parsed
		}
	}
	if raw := os.Getenv("INDEXUS_MEM_LEAD"); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
			cfg.MemLead = parsed
		}
	}
	if v := envFloat("INDEXUS_MEM_LIMIT_PCT", 0); v > 0 {
		cfg.MemLimitPct = v
	}
	if v := envFloat("INDEXUS_MEM_FLOOR_PCT", 0); v > 0 {
		cfg.MemFloorPct = v
	}
	if v := envFloat("INDEXUS_CPU_FLOOR_PCT", 0); v > 0 {
		cfg.CPUFloorPct = v
	}
	// Explicit 0 disables the disk signal; only an unset env keeps the default.
	if raw := os.Getenv("INDEXUS_DISK_MIN_FREE_PCT"); raw != "" {
		if v := envFloat("INDEXUS_DISK_MIN_FREE_PCT", -1); v >= 0 {
			cfg.DiskMinFreePct = v
		}
	}
	if v := envFloat("INDEXUS_CPU_LIMIT_PCT", 0); v > 0 {
		cfg.CPULimitPct = v
	}
	if v := envFloat("INDEXUS_MEM_RISE", 0); v > 0 {
		cfg.MemRisePct = v
	}
	if v := envFloat("INDEXUS_CPU_RISE", 0); v > 0 {
		cfg.CPURisePct = v
	}
	if cfg.MemRefusePct <= 0 {
		cfg.MemRefusePct = 60
	}
	if v := envFloat("INDEXUS_MEM_REFUSE_PCT", 0); v > 0 {
		cfg.MemRefusePct = v
	}
	if v := envInt("INDEXUS_ITEMS_LIMIT", 0); v > 0 {
		cfg.ItemsLimit = v
	}
	hold := cfg.DownHold
	if cfg.Role == "spawned" && hold > 0 {

		hold = hold + time.Duration(rand.Int63n(int64(hold)/2+1))
	}
	return &AutoscaleController{
		cfg:               cfg,
		window:            NewInsertWindow(cfg.Window),
		joinedAt:          time.Now(),
		effectiveDownHold: hold,
		lastPressure:      map[string]any{},
	}
}

func (a *AutoscaleController) RecordInsert() {
	a.RecordInsertAt("", "")
}

// RecordInsertAt counts an insert in the pressure window. Location is ignored
// for placement — PreferNear comes from weighted zone split only.
func (a *AutoscaleController) RecordInsertAt(collection, location string) {
	if a == nil || !a.cfg.Enabled {
		return
	}
	_ = collection
	_ = location
	a.window.Record(1)
}

func (a *AutoscaleController) Snapshot() map[string]any {
	if a == nil {
		return map[string]any{"enabled": false}
	}
	sum := a.window.Sum()
	a.mu.Lock()
	defer a.mu.Unlock()
	out := map[string]any{
		"enabled":          a.cfg.Enabled,
		"role":             a.cfg.Role,
		"inserts_window":   sum,
		"down_threshold":   a.cfg.DownThreshold,
		"window":           a.cfg.Window.String(),
		"down_hold":        a.cfg.DownHold.String(),
		"down_hold_eff":    a.effectiveDownHold.String(),
		"cooldown":         a.cfg.Cooldown.String(),
		"scale_ups_done":   a.scaleUpsDone,
		"up_in_flight":     a.upInFlight.Load(),
		"down_in_flight":   a.downInFlight.Load(),
		"last_up_at":       a.lastUpAt,
		"last_down_at":     a.lastDownAt,
		"queue_abs":        a.cfg.QueueAbsThreshold,
		"pressure_hold":    a.cfg.PressureHold.String(),
		"mem_limit_pct":    a.cfg.MemLimitPct,
		"mem_floor_pct":    a.cfg.MemFloorPct,
		"cpu_floor_pct":    a.cfg.CPUFloorPct,
		"disk_min_free":    a.cfg.DiskMinFreePct,
		"cpu_limit_pct":    a.cfg.CPULimitPct,
		"mem_lead":         a.cfg.MemLead.String(),
		"mem_rise_pct":     a.cfg.MemRisePct,
		"cpu_rise_pct":     a.cfg.CPURisePct,
		"rise_hold":        a.cfg.RiseHold.String(),
		"mem_refuse_pct":   a.cfg.MemRefusePct,
		"items_limit":      a.cfg.ItemsLimit,
		"rising_fast":      a.risingFast,
		"admit_blocked":    a.clientWritesBlockedLocked(),
		"last_reason":      a.lastReason,
		"last_prefer_near": a.lastPreferNear,
		"pressure":         a.lastPressure,
	}
	return out
}

func (a *AutoscaleController) SetNearBuilder(fn func(spawnN int, fallback string) []string) {
	if a == nil {
		return
	}
	a.nearBuilder = fn
}

func (a *AutoscaleController) buildPreferNears(spawnN int, fallback string) []string {
	if a != nil && a.nearBuilder != nil {
		// Empty means "no stable placement" (e.g. no exclusive load) — do not
		// invent a PreferNear that would collide with the local mesh.
		return a.nearBuilder(spawnN, fallback)
	}
	nears, _ := encoding.BASE64.PreferNearTargets(fallback, spawnN)
	return nears
}

func locationToNearKey(location string) string {
	idLen := encoding.BASE64.IDLength()
	if idLen <= 0 {
		return ""
	}
	key := location
	if key == encoding.BASE64.Root() {
		key = ""
	}
	for len(key) < idLen {
		key += "0"
	}
	if len(key) > idLen {
		key = key[:idLen]
	}
	if _, err := encoding.BASE64.Decode(key); err != nil {
		return ""
	}
	return key
}

func (a *AutoscaleController) evaluatePressure(in PressureInput) (fire bool, reason string) {
	sum := a.window.Sum()
	res := in.Resources
	now := time.Now()

	a.mu.Lock()
	queueDelta := in.Queue - a.lastQueue
	if a.lastQueueAt.IsZero() {
		queueDelta = 0
	}
	a.lastQueue = in.Queue
	a.lastQueueAt = now

	memRise := a.memRiseRateLocked(res.MemPct, now)
	cpuRise := a.cpuRiseRateLocked(res.CPUPct, res.CPUOK, now)
	projected := a.projectMemoryLocked(res.MemPct, now)

	rise := ""
	switch {
	case a.cfg.MemRisePct > 0 && memRise >= a.cfg.MemRisePct:
		rise = "mem_rise"
	case a.cfg.CPURisePct > 0 && res.CPUOK && res.CPUPct >= a.cfg.CPUFloorPct && cpuRise >= a.cfg.CPURisePct:
		rise = "cpu_rise"
	}

	level := ""
	switch {
	case a.cfg.MemLimitPct > 0 && res.MemPct >= a.cfg.MemFloorPct && projected >= a.cfg.MemLimitPct:
		level = "mem_filling"
	case a.cfg.CPULimitPct > 0 && res.CPUOK && res.CPUPct >= a.cfg.CPUFloorPct && res.CPUPct >= a.cfg.CPULimitPct:
		level = "cpu"
	}

	sustained := level
	if rise != "" {
		sustained = rise
	}
	a.risingFast = rise != ""

	holdNeed := a.cfg.PressureHold
	var held time.Duration
	if rise != "" {
		holdNeed = a.cfg.RiseHold
		a.queueHotSince = time.Time{}
		if a.riseHotSince.IsZero() {
			a.riseHotSince = now
		}
		held = now.Sub(a.riseHotSince)
	} else {
		a.riseHotSince = time.Time{}
		if level == "" {
			a.queueHotSince = time.Time{}
		} else if a.queueHotSince.IsZero() {
			a.queueHotSince = now
		}
		if !a.queueHotSince.IsZero() {
			held = now.Sub(a.queueHotSince)
		}
	}

	a.lastPressure = map[string]any{
		"queue":          in.Queue,
		"queue_delta":    queueDelta,
		"hot_signal":     sustained,
		"hot_for":        held.Round(time.Second).String(),
		"inserts_window": sum,
		"owned_items":    in.OwnedItems,
		"cpu_pct":        res.CPUPct,
		"cpu_rise":       cpuRise,
		"mem_pct":        res.MemPct,
		"mem_rise":       memRise,
		"mem_projected":  projected,
		"disk_free_pct":  res.DiskFreePct,
		"heap_inuse":     res.HeapInuse,
		"rss":            res.RSS,
	}
	a.mu.Unlock()

	if a.cfg.ItemsLimit > 0 && in.OwnedItems >= a.cfg.ItemsLimit {
		a.mu.Lock()
		a.lastPressure["hot_signal"] = "items"
		a.mu.Unlock()
		return true, "items"
	}
	// Emergency: at/above write-refuse threshold — spawn immediately (no hold).
	if a.cfg.MemRefusePct > 0 && res.MemPct >= a.cfg.MemRefusePct {
		return true, "mem"
	}
	if a.cfg.DiskMinFreePct > 0 && res.DiskOK && res.DiskFreePct < a.cfg.DiskMinFreePct {
		return true, "disk"
	}
	if sustained == "" || held < holdNeed {
		return false, ""
	}
	return true, sustained
}

func (a *AutoscaleController) ClientWritesBlocked() bool {
	if a == nil || !a.cfg.Enabled {
		return false
	}
	if a.upInFlight.Load() {
		return true
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.clientWritesBlockedLocked()
}

func (a *AutoscaleController) clientWritesBlockedLocked() bool {
	if a.risingFast {
		return true
	}
	if a.upInFlight.Load() {
		return true
	}

	if a.reliefArrived {
		return false
	}
	if !a.lastUpAt.IsZero() && time.Since(a.lastUpAt) < a.cfg.Cooldown {
		return true
	}
	return false
}

func (a *AutoscaleController) MarkReliefArrived() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.reliefArrived = true
	a.mu.Unlock()
}

func (a *AutoscaleController) memRiseRateLocked(pct float64, now time.Time) float64 {
	previous, at := a.lastMem, a.lastMemAt
	if at.IsZero() || pct <= 0 {
		return 0
	}
	elapsed := now.Sub(at).Seconds()
	if elapsed <= 0 {
		return 0
	}
	rate := (pct - previous) / elapsed
	if rate < 0 {
		return 0
	}
	return rate
}

func (a *AutoscaleController) cpuRiseRateLocked(pct float64, ok bool, now time.Time) float64 {
	if !ok {
		a.lastCPU, a.lastCPUAt = 0, time.Time{}
		return 0
	}
	previous, at := a.lastCPU, a.lastCPUAt
	a.lastCPU, a.lastCPUAt = pct, now
	if at.IsZero() {
		return 0
	}
	elapsed := now.Sub(at).Seconds()
	if elapsed <= 0 {
		return 0
	}
	rate := (pct - previous) / elapsed
	if rate < 0 {
		return 0
	}
	return rate
}

func (a *AutoscaleController) projectMemoryLocked(pct float64, now time.Time) float64 {
	if pct <= 0 {
		return 0
	}
	previous, at := a.lastMem, a.lastMemAt
	a.lastMem, a.lastMemAt = pct, now
	if at.IsZero() {
		return pct
	}
	elapsed := now.Sub(at).Seconds()
	if elapsed <= 0 {
		return pct
	}
	slope := (pct - previous) / elapsed
	if slope <= 0 {
		return pct
	}
	return pct + slope*a.cfg.MemLead.Seconds()
}

func (a *AutoscaleController) Tick(in PressureInput, scaleUp func(ScaleUpRequest) error, scaleDown func() error) {
	if a == nil || !a.cfg.Enabled {
		return
	}
	sum := a.window.Sum()
	now := time.Now()

	fire, reason := a.evaluatePressure(in)

	if !fire {
		a.mu.Lock()
		a.lastReason = ""
		a.mu.Unlock()
	}

	a.mu.Lock()
	inCooldown := !a.lastUpAt.IsZero() && now.Sub(a.lastUpAt) < a.cfg.Cooldown
	a.mu.Unlock()

	if fire && !inCooldown && !a.upInFlight.Load() && !a.downInFlight.Load() {
		const spawnN = 1
		// Dichotomy over exclusive owned zones (~½ items), avoiding known peers.
		nears := a.buildPreferNears(spawnN, in.SelfName)
		if len(nears) == 0 {
			slog.Info("scale-up skipped: no stable PreferNear (no exclusive load among known peers)",
				"reason", reason, "self", in.SelfName)
			return
		}
		preferNear := nears[0]
		req := ScaleUpRequest{PreferNear: preferNear, PreferNears: nears, Reason: reason, SpawnCount: spawnN}
		a.mu.Lock()
		a.lastReason = reason
		a.lastPreferNear = preferNear
		a.mu.Unlock()
		a.upInFlight.Store(true)
		go func() {
			defer a.upInFlight.Store(false)
			if scaleUp != nil {
				if err := scaleUp(req); err != nil {
					slog.Warn("scale-up failed", "reason", req.Reason, "spawn_count", req.SpawnCount, "err", err)
					return
				}
			}
			a.mu.Lock()
			a.lastUpAt = time.Now()
			a.scaleUpsDone++
			a.reliefArrived = false

			a.queueHotSince = time.Time{}
			a.riseHotSince = time.Time{}
			a.mu.Unlock()
		}()
	}

	if a.cfg.Role != "spawned" {
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	inCooldown = !a.lastUpAt.IsZero() && now.Sub(a.lastUpAt) < a.cfg.Cooldown
	if inCooldown || a.downInFlight.Load() || a.upInFlight.Load() {
		a.downSince = time.Time{}
		return
	}
	if !a.downBackoffUntil.IsZero() && now.Before(a.downBackoffUntil) {
		a.downSince = time.Time{}
		return
	}
	if sum >= a.cfg.DownThreshold {
		a.wasHot = true
	}

	res := in.Resources
	if a.cfg.MemLimitPct > 0 && res.MemPct >= a.cfg.MemLimitPct {
		a.downSince = time.Time{}
		return
	}
	if a.cfg.CPULimitPct > 0 && res.CPUOK && res.CPUPct >= a.cfg.CPULimitPct {
		a.downSince = time.Time{}
		return
	}

	if a.risingFast {
		a.downSince = time.Time{}
		return
	}
	if a.cfg.DiskMinFreePct > 0 && res.DiskOK && res.DiskFreePct < a.cfg.DiskMinFreePct {
		a.downSince = time.Time{}
		return
	}
	if in.Queue >= a.cfg.QueueAbsThreshold {
		a.downSince = time.Time{}
		return
	}

	spawnGrace := 15 * time.Minute
	if raw := os.Getenv("INDEXUS_SPAWN_GRACE"); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
			spawnGrace = parsed
		}
	}
	if !a.wasHot && now.Sub(a.joinedAt) < spawnGrace {
		a.downSince = time.Time{}
		return
	}
	quiet := sum < a.cfg.DownThreshold
	if !quiet {
		a.downSince = time.Time{}
		return
	}
	if a.downSince.IsZero() {
		a.downSince = now
		return
	}
	hold := a.effectiveDownHold
	if hold <= 0 {
		hold = a.cfg.DownHold
	}
	if now.Sub(a.downSince) < hold {
		return
	}
	a.downInFlight.Store(true)
	a.downSince = time.Time{}
	go func() {
		defer a.downInFlight.Store(false)
		if scaleDown != nil {
			if err := scaleDown(); err != nil {
				slog.Warn("scale-down failed", "err", err)
				a.mu.Lock()
				a.downBackoffUntil = time.Now().Add(hold)
				a.mu.Unlock()
				return
			}
		}
		a.mu.Lock()
		a.lastDownAt = time.Now()
		a.downBackoffUntil = time.Time{}
		a.mu.Unlock()
	}()
}

func (a *AutoscaleController) PostScale(issuerURL, requesterID string, inserts int64, owned int, preferNear, reason string, spawnCount int, preferNears []string) error {
	if preferNear == "" {
		preferNear = requesterID
	}
	if reason == "" {
		reason = "resource_pressure"
	}
	if spawnCount < 1 {
		spawnCount = 1
	}
	if spawnCount > 3 {
		spawnCount = 3
	}
	if len(preferNears) == 0 {
		preferNears, _ = encoding.BASE64.PreferNearTargets(preferNear, spawnCount)
	}
	body, _ := json.Marshal(map[string]any{
		"requester_id":  requesterID,
		"local_inserts": inserts,
		"owned_zones":   owned,
		"reason":        reason,
		"role":          a.cfg.Role,
		"prefer_near":   preferNear,
		"prefer_nears":  preferNears,
		"spawn_count":   spawnCount,
	})
	return postJSON(issuerURL+"/v1/scale", body)
}

func (a *AutoscaleController) PostDownscale(issuerURL, instanceID, requesterID string) error {
	body, _ := json.Marshal(map[string]any{
		"requester_id": requesterID,
		"instance_id":  instanceID,
		"reason":       "idle_hysteresis",
	})
	return postJSON(issuerURL+"/v1/downscale", body)
}

func (a *AutoscaleController) PostDrainLock(issuerURL, instanceID, requesterID string) error {
	body, _ := json.Marshal(map[string]any{
		"requester_id": requesterID,
		"instance_id":  instanceID,
	})
	return postJSON(issuerURL+"/v1/drain-lock", body)
}

func (a *AutoscaleController) PostDrainUnlock(issuerURL, instanceID, requesterID string) error {
	body, _ := json.Marshal(map[string]any{
		"requester_id": requesterID,
		"instance_id":  instanceID,
	})
	return postJSON(issuerURL+"/v1/drain-unlock", body)
}

func postJSON(url string, body []byte) error {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if tok := os.Getenv("INDEXUS_BEARER"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("status %d: %s", resp.StatusCode, string(answer))
	}
	slog.Debug("issuer answered", "url", url, "answer", string(answer))
	return nil
}

func IMDSInstanceID() (string, error) {
	if id := os.Getenv("INDEXUS_INSTANCE_ID"); id != "" {
		return id, nil
	}
	client := &http.Client{Timeout: 2 * time.Second}
	tokReq, _ := http.NewRequest(http.MethodPut, "http://169.254.169.254/latest/api/token", nil)
	tokReq.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "60")
	tokResp, err := client.Do(tokReq)
	if err != nil {
		return "", err
	}
	defer tokResp.Body.Close()
	tok, _ := io.ReadAll(tokResp.Body)

	req, _ := http.NewRequest(http.MethodGet, "http://169.254.169.254/latest/meta-data/instance-id", nil)
	req.Header.Set("X-aws-ec2-metadata-token", string(tok))
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	id := strings.TrimSpace(string(body))
	if id == "" {
		return "", errors.New("instance metadata returned no instance id")
	}
	return id, nil
}
