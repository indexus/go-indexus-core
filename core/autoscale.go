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

	QueueAbsThreshold  int
	QueueRiseThreshold int
	PressureHold       time.Duration

	MemLimitPct    float64
	DiskMinFreePct float64
	CPULimitPct    float64
	MemLead        time.Duration
	MemFloorPct    float64
	CPUFloorPct    float64
	MemRisePct     float64
	CPURisePct     float64
	RiseHold       time.Duration
	MemRefusePct   float64
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

type hotTracker struct {
	mu     sync.Mutex
	counts map[string]int64
	hits   int64
}

func newHotTracker() *hotTracker {
	return &hotTracker{counts: make(map[string]int64)}
}

func (h *hotTracker) hit(collection, location string) {
	if h == nil {
		return
	}
	p := location
	if p == "" {
		p = encoding.BASE64.Root()
	}
	if len(p) > 4 {
		p = p[:4]
	}
	key := collection + "|" + p
	h.mu.Lock()
	h.counts[key]++
	h.hits++

	if h.hits%2048 == 0 {
		for k, v := range h.counts {
			v /= 2
			if v == 0 {
				delete(h.counts, k)
			} else {
				h.counts[k] = v
			}
		}
	}
	h.mu.Unlock()
}

func (h *hotTracker) hottest() (collection, prefix string, n int64) {
	if h == nil {
		return "", "", 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for k, v := range h.counts {
		if v > n {
			n = v
			parts := strings.SplitN(k, "|", 2)
			if len(parts) == 2 {
				collection, prefix = parts[0], parts[1]
			}
		}
	}
	return collection, prefix, n
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
	Resources  ResourceSample
	SelfName   string

	Rebalancing bool
}

type AutoscaleController struct {
	cfg AutoscaleConfig

	window *InsertWindow
	hot    *hotTracker

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
}

func NewAutoscaleController(cfg AutoscaleConfig) *AutoscaleController {
	if cfg.Window <= 0 {
		cfg.Window = 2 * time.Minute
	}
	if cfg.DownThreshold <= 0 {
		cfg.DownThreshold = 200
	}
	if cfg.DownHold <= 0 {

		cfg.DownHold = 8 * time.Minute
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = 3 * time.Minute
	}
	if cfg.Role == "" {
		cfg.Role = "bootstrap"
	}
	if cfg.QueueAbsThreshold <= 0 {
		cfg.QueueAbsThreshold = 500
	}
	if cfg.QueueRiseThreshold <= 0 {
		cfg.QueueRiseThreshold = 50
	}
	if cfg.PressureHold <= 0 {

		cfg.PressureHold = 15 * time.Second
	}

	if cfg.MemLimitPct <= 0 {
		cfg.MemLimitPct = 50
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
	if cfg.RiseHold <= 0 {
		cfg.RiseHold = 12 * time.Second
	}

	if v := envInt("INDEXUS_QUEUE_ABS", 0); v > 0 {
		cfg.QueueAbsThreshold = v
	}
	if v := envInt("INDEXUS_QUEUE_RISE", 0); v > 0 {
		cfg.QueueRiseThreshold = v
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
	if v := envFloat("INDEXUS_MEM_LIMIT_PCT", 0); v > 0 {
		cfg.MemLimitPct = v
	}
	if v := envFloat("INDEXUS_MEM_FLOOR_PCT", 0); v > 0 {
		cfg.MemFloorPct = v
	}
	if v := envFloat("INDEXUS_CPU_FLOOR_PCT", 0); v > 0 {
		cfg.CPUFloorPct = v
	}
	if v := envFloat("INDEXUS_DISK_MIN_FREE_PCT", 0); v > 0 {
		cfg.DiskMinFreePct = v
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
		cfg.MemRefusePct = 68
	}
	if v := envFloat("INDEXUS_MEM_REFUSE_PCT", 0); v > 0 {
		cfg.MemRefusePct = v
	}
	hold := cfg.DownHold
	if cfg.Role == "spawned" && hold > 0 {

		hold = hold + time.Duration(rand.Int63n(int64(hold)/2+1))
	}
	return &AutoscaleController{
		cfg:               cfg,
		window:            NewInsertWindow(cfg.Window),
		hot:               newHotTracker(),
		joinedAt:          time.Now(),
		effectiveDownHold: hold,
		lastPressure:      map[string]any{},
	}
}

func (a *AutoscaleController) RecordInsert() {
	a.RecordInsertAt("", "")
}

func (a *AutoscaleController) RecordInsertAt(collection, location string) {
	if a == nil || !a.cfg.Enabled {
		return
	}
	a.window.Record(1)
	if collection != "" || location != "" {
		a.hot.hit(collection, location)
	}
}

func (a *AutoscaleController) Snapshot() map[string]any {
	if a == nil {
		return map[string]any{"enabled": false}
	}
	sum := a.window.Sum()
	_, pref, hotN := a.hot.hottest()
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
		"queue_rise":       a.cfg.QueueRiseThreshold,
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
		"rising_fast":      a.risingFast,
		"admit_blocked":    a.clientWritesBlockedLocked(),
		"hot_prefix":       pref,
		"hot_prefix_hits":  hotN,
		"last_reason":      a.lastReason,
		"last_prefer_near": a.lastPreferNear,
		"pressure":         a.lastPressure,
	}
	return out
}

func (a *AutoscaleController) PreferNearKey(selfName string) string {
	if a == nil {
		return selfName
	}
	_, prefix, n := a.hot.hottest()
	if n > 0 && prefix != "" {
		if key := locationToNearKey(prefix); key != "" {
			return key
		}
	}
	return selfName
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

	if a.cfg.MemLimitPct > 0 && res.MemPct >= a.cfg.MemLimitPct {
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

func SpawnCountFor(reason string, memPct, memRefusePct float64) int {
	nearRefuse := memRefusePct > 0 && memPct >= memRefusePct*0.9
	n := 1
	switch reason {
	case "mem_rise", "cpu_rise":
		n = 2
		if nearRefuse {
			n = 3
		}
	case "mem":
		n = 2
		if nearRefuse {
			n = 3
		}
	case "mem_filling", "cpu", "disk":
		n = 1
	}
	if n < 1 {
		n = 1
	}
	if n > 3 {
		n = 3
	}
	return n
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
	preferNear := a.PreferNearKey(in.SelfName)

	if in.Rebalancing && reason != "" && reason != "mem" && reason != "disk" {
		fire = false
		reason = ""
	}

	a.mu.Lock()
	inCooldown := !a.lastUpAt.IsZero() && now.Sub(a.lastUpAt) < a.cfg.Cooldown
	a.mu.Unlock()

	if fire && !inCooldown && !a.upInFlight.Load() && !a.downInFlight.Load() {
		spawnN := SpawnCountFor(reason, in.Resources.MemPct, a.cfg.MemRefusePct)
		nears, _ := encoding.BASE64.PreferNearTargets(preferNear, spawnN)
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

func envInt(key string, def int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	var n int
	if _, err := fmt.Sscanf(raw, "%d", &n); err != nil {
		return def
	}
	return n
}

func envFloat(key string, def float64) float64 {
	raw := os.Getenv(key)
	if raw == "" {
		return def
	}
	var n float64
	if _, err := fmt.Sscanf(raw, "%f", &n); err != nil {
		return def
	}
	return n
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

func (n *Node) EnableAutoscale(cfg AutoscaleConfig) {
	n.autoscale = NewAutoscaleController(cfg)
}

func (n *Node) AutoscaleSnapshot() map[string]any {
	if n.autoscale == nil {
		return map[string]any{"enabled": false}
	}
	return n.autoscale.Snapshot()
}

func (n *Node) ownedZoneCount() int {
	return len(n.listOwnedKeys())
}

func (n *Node) AutoscaleTick() {
	if n.autoscale == nil {
		return
	}
	cfg := n.autoscale.cfg
	diskPath := os.Getenv("INDEXUS_STORAGE")
	if diskPath == "" {
		diskPath = "."
	}
	in := PressureInput{
		Queue:       n.Queue(),
		OwnedZones:  n.ownedZoneCount(),
		Resources:   SampleResources(diskPath),
		SelfName:    n.Name(),
		Rebalancing: n.Rebalancing(),
	}
	n.autoscale.Tick(in,
		func(req ScaleUpRequest) error {
			inserts := n.autoscale.window.Sum()
			spawnN := req.SpawnCount
			if spawnN < 1 {
				spawnN = 1
			}
			slog.Info("under pressure, asking for peers",
				"reason", req.Reason,
				"spawn_count", spawnN,
				"prefer_near", req.PreferNear,
				"prefer_nears", req.PreferNears,
				"queue", in.Queue,
				"inserts_window", inserts,
				"cpu_pct", in.Resources.CPUPct,
				"mem_pct", in.Resources.MemPct,
				"disk_free_pct", in.Resources.DiskFreePct,
			)
			return n.autoscale.PostScale(
				cfg.IssuerURL, n.Name(), inserts, n.ownedZoneCount(),
				req.PreferNear, req.Reason, spawnN, req.PreferNears,
			)
		},
		func() error {
			instance, err := IMDSInstanceID()
			if err != nil {
				return fmt.Errorf("no instance id for downscale: %w", err)
			}
			slog.Info("requesting scale-down", "instance", instance)

			if err := n.autoscale.PostDrainLock(cfg.IssuerURL, instance, n.Name()); err != nil {
				return fmt.Errorf("drain lock: %w", err)
			}

			defer func() { _ = n.autoscale.PostDrainUnlock(cfg.IssuerURL, instance, n.Name()) }()

			done := make(chan struct{})
			defer close(done)
			go func() {
				t := time.NewTicker(60 * time.Second)
				defer t.Stop()
				for {
					select {
					case <-done:
						return
					case <-t.C:
						if err := n.autoscale.PostDrainLock(cfg.IssuerURL, instance, n.Name()); err != nil {
							slog.Warn("drain lock renew failed", "instance", instance, "err", err)
						}
					}
				}
			}()

			result := n.SoftLeave(leaveTimeout())
			if !result.OK {
				return fmt.Errorf("soft-leave incomplete: owned=%d queue=%d: %s",
					result.RemainingOwned, result.RemainingQueue, result.Error)
			}
			slog.Info("drained, asking the issuer to terminate",
				"instance", instance,
				"transferred_items", result.TransferredItems,
				"elapsed_ms", result.ElapsedMS)

			if err := n.autoscale.PostDownscale(cfg.IssuerURL, instance, n.Name()); err != nil {

				n.leaving.Store(false)
				return err
			}
			return nil
		},
	)
}

func leaveTimeout() time.Duration {
	if raw := os.Getenv("INDEXUS_LEAVE_TIMEOUT"); raw != "" {
		if parsed, err := time.ParseDuration(raw); err == nil && parsed > 0 {
			return parsed
		}
		slog.Warn("ignoring invalid INDEXUS_LEAVE_TIMEOUT", "value", raw)
	}
	return 90 * time.Second
}
