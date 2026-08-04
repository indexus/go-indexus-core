package core_test

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/indexus/go-indexus-core/core"
)

func TestInsertWindowSum(t *testing.T) {
	w := core.NewInsertWindow(3 * time.Minute)
	w.Record(10)
	w.Record(5)
	if got := w.Sum(); got != 15 {
		t.Fatalf("sum=%d want 15", got)
	}
}

func TestInsertWindowShortExpires(t *testing.T) {
	w := core.NewInsertWindow(2 * time.Second)
	w.Record(7)
	if got := w.Sum(); got != 7 {
		t.Fatalf("sum=%d want 7", got)
	}
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if w.Sum() == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("short window did not expire, sum=%d", w.Sum())
}

func pressure(queue int) core.PressureInput {
	return core.PressureInput{Queue: queue, SelfName: "selfNode000000000000000000000000"}
}

func memHot(pct float64) core.PressureInput {
	return core.PressureInput{
		SelfName:  "selfNode000000000000000000000000",
		Resources: core.ResourceSample{MemPct: pct, DiskFreePct: 80, DiskOK: true},
	}
}

func underPressure() core.AutoscaleConfig {
	return core.AutoscaleConfig{
		Enabled:        true,
		Role:           "bootstrap",
		Window:         time.Minute,
		Cooldown:       time.Hour,
		PressureHold:   time.Millisecond,
		MemLimitPct:    65,
		MemFloorPct:    20,
		DiskMinFreePct: 1,
		CPULimitPct:    99,
		MemRisePct:     1000,
		CPURisePct:     1000,
		RiseHold:       time.Hour,
	}
}

func TestAutoscaleUpTriggersOnce(t *testing.T) {
	var ups atomic.Int64
	a := core.NewAutoscaleController(underPressure())

	up := func(req core.ScaleUpRequest) error { ups.Add(1); return nil }
	a.Tick(memHot(80), up, nil)
	time.Sleep(50 * time.Millisecond)
	a.Tick(memHot(80), up, nil)
	time.Sleep(50 * time.Millisecond)

	if got := ups.Load(); got != 1 {
		t.Fatalf("ups=%d want 1", got)
	}
}

func TestAutoscaleIgnoresWriteVolumeWithoutBacklog(t *testing.T) {
	var ups atomic.Int64
	a := core.NewAutoscaleController(underPressure())

	for i := 0; i < 500_000; i++ {
		a.RecordInsert()
	}
	for i := 0; i < 5; i++ {
		a.Tick(pressure(0), func(core.ScaleUpRequest) error { ups.Add(1); return nil }, nil)
		time.Sleep(5 * time.Millisecond)
	}

	if got := ups.Load(); got != 0 {
		t.Fatalf("a node keeping up with its writes asked for %d nodes", got)
	}
}

func TestAutoscaleIgnoresQueueBacklog(t *testing.T) {
	var ups atomic.Int64
	cfg := underPressure()
	cfg.QueueAbsThreshold = 10
	cfg.QueueRiseThreshold = 5
	a := core.NewAutoscaleController(cfg)

	up := func(core.ScaleUpRequest) error { ups.Add(1); return nil }
	a.Tick(pressure(0), up, nil)
	a.Tick(pressure(5000), up, nil)
	time.Sleep(20 * time.Millisecond)
	a.Tick(pressure(5000), up, nil)
	time.Sleep(50 * time.Millisecond)

	if got := ups.Load(); got != 0 {
		t.Fatalf("scaled on a queue alone: ups=%d", got)
	}
}

func TestAutoscaleWaitsForTheSignalToLast(t *testing.T) {
	var ups atomic.Int64
	cfg := underPressure()
	cfg.PressureHold = time.Hour
	cfg.MemLimitPct = 90
	a := core.NewAutoscaleController(cfg)

	up := func(core.ScaleUpRequest) error { ups.Add(1); return nil }

	a.Tick(memHot(50), up, nil)
	time.Sleep(20 * time.Millisecond)
	a.Tick(memHot(55), up, nil)
	time.Sleep(50 * time.Millisecond)

	if got := ups.Load(); got != 0 {
		t.Fatalf("scaled on a signal younger than the hold: ups=%d", got)
	}
}

func TestAutoscaleReportsMemoryReason(t *testing.T) {
	reason := make(chan string, 4)
	a := core.NewAutoscaleController(underPressure())

	a.Tick(memHot(80), func(req core.ScaleUpRequest) error { reason <- req.Reason; return nil }, nil)
	time.Sleep(20 * time.Millisecond)

	select {
	case got := <-reason:
		if got != "mem" {
			t.Fatalf("reason=%q want mem", got)
		}
	case <-time.After(time.Second):
		t.Fatal("a node past its memory limit never asked for a node")
	}
}

func TestAutoscaleUpSkipsFailed(t *testing.T) {
	cfg := underPressure()
	cfg.Cooldown = time.Millisecond
	a := core.NewAutoscaleController(cfg)

	a.Tick(memHot(80), func(core.ScaleUpRequest) error {
		return fmt.Errorf("issuer refused")
	}, nil)
	time.Sleep(50 * time.Millisecond)
	if snap := a.Snapshot(); snap["scale_ups_done"].(int) != 0 {
		t.Fatalf("scale_ups_done=%v want 0 after failure", snap["scale_ups_done"])
	}

	a.Tick(memHot(80), func(core.ScaleUpRequest) error { return nil }, nil)
	time.Sleep(50 * time.Millisecond)
	if snap := a.Snapshot(); snap["scale_ups_done"].(int) != 1 {
		t.Fatalf("scale_ups_done=%v want 1 after success", snap["scale_ups_done"])
	}
}

func TestAutoscaleSpawnedCanScaleUp(t *testing.T) {
	var ups atomic.Int64
	cfg := underPressure()
	cfg.Role = "spawned"
	cfg.DownThreshold = 1
	cfg.DownHold = time.Hour
	a := core.NewAutoscaleController(cfg)

	up := func(core.ScaleUpRequest) error { ups.Add(1); return nil }
	down := func() error { t.Error("should not downscale while scaling up"); return nil }
	a.Tick(memHot(80), up, down)
	time.Sleep(50 * time.Millisecond)

	if ups.Load() != 1 {
		t.Fatalf("spawned ups=%d want 1", ups.Load())
	}
}

func TestAutoscaleCPUSaturationCounts(t *testing.T) {
	var ups atomic.Int64
	cfg := underPressure()
	cfg.CPULimitPct = 80
	a := core.NewAutoscaleController(cfg)

	hot := core.PressureInput{
		Queue:     0,
		SelfName:  "self",
		Resources: core.ResourceSample{CPUPct: 92, CPUOK: true, DiskFreePct: 80, DiskOK: true},
	}
	up := func(req core.ScaleUpRequest) error {
		if req.Reason != "cpu" {
			t.Errorf("reason=%q want cpu", req.Reason)
		}
		ups.Add(1)
		return nil
	}
	a.Tick(hot, up, nil)
	time.Sleep(20 * time.Millisecond)
	a.Tick(hot, up, nil)
	time.Sleep(50 * time.Millisecond)

	if ups.Load() != 1 {
		t.Fatalf("ups=%d want 1 on sustained cpu saturation", ups.Load())
	}
}

func TestAutoscaleIgnoresMissingCPUReading(t *testing.T) {
	var ups atomic.Int64
	a := core.NewAutoscaleController(underPressure())

	blind := core.PressureInput{
		Queue:     0,
		SelfName:  "self",
		Resources: core.ResourceSample{CPUPct: 0, CPUOK: false, DiskFreePct: 80, DiskOK: true},
	}
	for i := 0; i < 4; i++ {
		a.Tick(blind, func(core.ScaleUpRequest) error { ups.Add(1); return nil }, nil)
		time.Sleep(10 * time.Millisecond)
	}

	if ups.Load() != 0 {
		t.Fatalf("scaled with no cpu reading at all: ups=%d", ups.Load())
	}
}

func TestAutoscaleAsksBeforeMemoryRunsOut(t *testing.T) {
	var reason atomic.Value
	cfg := underPressure()
	cfg.MemLimitPct = 85
	cfg.MemLead = 3 * time.Minute
	cfg.MemFloorPct = 50
	a := core.NewAutoscaleController(cfg)

	up := func(req core.ScaleUpRequest) error { reason.Store(req.Reason); return nil }

	for i, pct := range []float64{60, 63, 66} {
		a.Tick(core.PressureInput{
			Queue:     0,
			SelfName:  "self",
			Resources: core.ResourceSample{MemPct: pct, DiskFreePct: 80, DiskOK: true},
		}, up, nil)
		if i == 0 && reason.Load() != nil {
			t.Fatal("asked on the very first memory reading, with no trend to go on")
		}
		time.Sleep(30 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)

	if got := reason.Load(); got != "mem_filling" {
		t.Fatalf("reason=%v want mem_filling well before the limit", got)
	}
}

func TestAutoscaleIgnoresFlatMemory(t *testing.T) {
	var ups atomic.Int64
	cfg := underPressure()
	cfg.MemLimitPct = 85
	a := core.NewAutoscaleController(cfg)

	flat := core.PressureInput{
		Queue:     0,
		SelfName:  "self",
		Resources: core.ResourceSample{MemPct: 60, DiskFreePct: 80, DiskOK: true},
	}
	for i := 0; i < 5; i++ {
		a.Tick(flat, func(core.ScaleUpRequest) error { ups.Add(1); return nil }, nil)
		time.Sleep(10 * time.Millisecond)
	}

	if ups.Load() != 0 {
		t.Fatalf("scaled on memory that never moved: ups=%d", ups.Load())
	}
}

func TestAutoscaleIgnoresAColdStartRamp(t *testing.T) {
	var ups atomic.Int64
	cfg := underPressure()
	cfg.MemLimitPct = 85
	cfg.MemFloorPct = 50
	a := core.NewAutoscaleController(cfg)

	for _, pct := range []float64{2, 9, 17, 24} {
		a.Tick(core.PressureInput{
			Queue:     0,
			SelfName:  "self",
			Resources: core.ResourceSample{MemPct: pct, DiskFreePct: 80, DiskOK: true},
		}, func(core.ScaleUpRequest) error { ups.Add(1); return nil }, nil)
		time.Sleep(20 * time.Millisecond)
	}

	if ups.Load() != 0 {
		t.Fatalf("a node booting into half an empty host asked for help: ups=%d", ups.Load())
	}
}

func TestAutoscaleHardMemTriggers(t *testing.T) {
	var ups atomic.Int64
	cfg := underPressure()
	cfg.MemLimitPct = 50
	a := core.NewAutoscaleController(cfg)

	in := core.PressureInput{
		Queue:    0,
		SelfName: "self",
		Resources: core.ResourceSample{
			MemPct:      90,
			DiskFreePct: 80,
			DiskOK:      true,
		},
	}
	a.Tick(in, func(req core.ScaleUpRequest) error {
		if req.Reason != "mem" {
			t.Errorf("reason=%q want mem", req.Reason)
		}
		ups.Add(1)
		return nil
	}, nil)
	time.Sleep(50 * time.Millisecond)

	if ups.Load() != 1 {
		t.Fatalf("ups=%d want 1 on hard mem", ups.Load())
	}
}

func TestAutoscaleHotPreferNear(t *testing.T) {
	a := core.NewAutoscaleController(core.AutoscaleConfig{
		Enabled: true,
		Role:    "bootstrap",
	})
	for i := 0; i < 20; i++ {
		a.RecordInsertAt("World", "AbCdEfGh")
	}
	for i := 0; i < 3; i++ {
		a.RecordInsertAt("World", "zzzz")
	}
	key := a.PreferNearKey("fallbackName00000000000000000000")
	if key == "" || key == "fallbackName00000000000000000000" {
		t.Fatalf("prefer_near=%q want hot location key", key)
	}
	if key[:4] != "AbCd" {
		t.Fatalf("prefer_near=%q want AbCd prefix", key)
	}
}

func TestAutoscaleDownNeedsHold(t *testing.T) {
	var downs atomic.Int64
	a := core.NewAutoscaleController(core.AutoscaleConfig{
		Enabled:            true,
		Role:               "spawned",
		DownThreshold:      5,
		DownHold:           80 * time.Millisecond,
		Window:             time.Minute,
		Cooldown:           0,
		QueueAbsThreshold:  10000,
		QueueRiseThreshold: 10000,
		MemLimitPct:        99,
		DiskMinFreePct:     1,
	})
	for i := 0; i < 6; i++ {
		a.RecordInsert()
	}
	a.Tick(pressure(0), nil, func() error { downs.Add(1); return nil })
	if downs.Load() != 0 {
		t.Fatal("should not downscale while still hot")
	}
	a = core.NewAutoscaleController(core.AutoscaleConfig{
		Enabled:            true,
		Role:               "spawned",
		DownThreshold:      5,
		DownHold:           80 * time.Millisecond,
		Window:             2 * time.Second,
		Cooldown:           0,
		QueueAbsThreshold:  10000,
		QueueRiseThreshold: 10000,
		MemLimitPct:        99,
		DiskMinFreePct:     1,
	})
	for i := 0; i < 6; i++ {
		a.RecordInsert()
	}
	a.Tick(pressure(0), nil, nil)
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if a.Snapshot()["inserts_window"].(int64) == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	a.Tick(pressure(0), nil, func() error { downs.Add(1); return nil })
	if downs.Load() != 0 {
		t.Fatal("should not downscale immediately on first quiet tick")
	}
	time.Sleep(200 * time.Millisecond)
	a.Tick(pressure(0), nil, func() error { downs.Add(1); return nil })
	time.Sleep(50 * time.Millisecond)
	if got := downs.Load(); got != 1 {
		t.Fatalf("downs=%d want 1", got)
	}
}

func TestAutoscaleDownSkipsVirgin(t *testing.T) {
	var downs atomic.Int64
	a := core.NewAutoscaleController(core.AutoscaleConfig{
		Enabled:            true,
		Role:               "spawned",
		DownThreshold:      5,
		DownHold:           time.Millisecond,
		Window:             time.Minute,
		Cooldown:           0,
		QueueAbsThreshold:  10000,
		QueueRiseThreshold: 10000,
		MemLimitPct:        99,
		DiskMinFreePct:     1,
	})
	for i := 0; i < 20; i++ {
		a.Tick(pressure(0), nil, func() error { downs.Add(1); return nil })
		time.Sleep(5 * time.Millisecond)
	}
	if downs.Load() != 0 {
		t.Fatalf("virgin spawned SoftLeft: downs=%d", downs.Load())
	}
}

func TestAutoscaleDownWithPendingQueue(t *testing.T) {
	var downs atomic.Int64
	a := core.NewAutoscaleController(core.AutoscaleConfig{
		Enabled:            true,
		Role:               "spawned",
		DownThreshold:      5,
		DownHold:           50 * time.Millisecond,
		Window:             2 * time.Second,
		Cooldown:           0,
		QueueAbsThreshold:  100000,
		QueueRiseThreshold: 100000,
		MemLimitPct:        99,
		DiskMinFreePct:     1,
	})
	for i := 0; i < 6; i++ {
		a.RecordInsert()
	}
	a.Tick(pressure(0), nil, nil)
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if a.Snapshot()["inserts_window"].(int64) == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	a.Tick(pressure(5000), nil, func() error { downs.Add(1); return nil })
	time.Sleep(200 * time.Millisecond)
	a.Tick(pressure(5000), nil, func() error { downs.Add(1); return nil })
	time.Sleep(50 * time.Millisecond)
	if got := downs.Load(); got != 1 {
		t.Fatalf("downs=%d want 1 with non-empty queue", got)
	}
}

func TestAutoscaleRapidMemRiseAsksForNode(t *testing.T) {
	var reason atomic.Value
	cfg := underPressure()
	cfg.MemLimitPct = 90
	cfg.MemFloorPct = 80
	cfg.MemRisePct = 1
	cfg.RiseHold = time.Millisecond
	a := core.NewAutoscaleController(cfg)

	up := func(req core.ScaleUpRequest) error { reason.Store(req.Reason); return nil }

	for i, pct := range []float64{2, 7, 12} {
		a.Tick(core.PressureInput{
			SelfName:  "self",
			Resources: core.ResourceSample{MemPct: pct, DiskFreePct: 80, DiskOK: true},
		}, up, nil)
		if i == 0 && reason.Load() != nil {
			t.Fatal("asked on the first sample, with no rise rate yet")
		}
		time.Sleep(40 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)

	if got := reason.Load(); got != "mem_rise" {
		t.Fatalf("reason=%v want mem_rise", got)
	}
}

func TestAutoscaleFlatLowMemDoesNotRise(t *testing.T) {
	var ups atomic.Int64
	cfg := underPressure()
	cfg.MemLimitPct = 90
	cfg.MemRisePct = 1
	cfg.RiseHold = time.Millisecond
	a := core.NewAutoscaleController(cfg)

	flat := core.PressureInput{
		SelfName:  "self",
		Resources: core.ResourceSample{MemPct: 8, DiskFreePct: 80, DiskOK: true},
	}
	for i := 0; i < 6; i++ {
		a.Tick(flat, func(core.ScaleUpRequest) error { ups.Add(1); return nil }, nil)
		time.Sleep(20 * time.Millisecond)
	}

	if ups.Load() != 0 {
		t.Fatalf("scaled on flat low memory: ups=%d", ups.Load())
	}
}

func TestAutoscaleCPURiseBelowFloorIgnored(t *testing.T) {
	var ups atomic.Int64
	cfg := underPressure()
	cfg.CPULimitPct = 90
	cfg.CPUFloorPct = 20
	cfg.CPURisePct = 1
	cfg.RiseHold = time.Millisecond
	cfg.MemRisePct = 1000
	a := core.NewAutoscaleController(cfg)

	up := func(core.ScaleUpRequest) error { ups.Add(1); return nil }
	for i, pct := range []float64{2, 8, 14} {
		a.Tick(core.PressureInput{
			SelfName: "self",
			Resources: core.ResourceSample{
				CPUPct: pct, CPUOK: true, MemPct: 5, DiskFreePct: 80, DiskOK: true,
			},
		}, up, nil)
		if i == 0 && ups.Load() != 0 {
			t.Fatal("asked on first CPU sample")
		}
		time.Sleep(40 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if ups.Load() != 0 {
		t.Fatalf("cpu_rise below floor spawned: ups=%d", ups.Load())
	}
}

func TestSpawnCountFor(t *testing.T) {
	refuse := 68.0
	cases := []struct {
		reason string
		mem    float64
		want   int
	}{
		{"mem_filling", 40, 1},
		{"cpu", 40, 1},
		{"disk", 40, 1},
		{"mem_rise", 40, 2},
		{"cpu_rise", 40, 2},
		{"mem", 56, 2},
		{"mem", 62, 3},
		{"mem_rise", 62, 3},
		{"unknown", 90, 1},
		{"", 90, 1},
	}
	for _, tc := range cases {
		if got := core.SpawnCountFor(tc.reason, tc.mem, refuse); got != tc.want {
			t.Fatalf("SpawnCountFor(%q, %.0f)=%d want %d", tc.reason, tc.mem, got, tc.want)
		}
	}

	if got := core.SpawnCountFor("mem", 90, 0); got != 2 {
		t.Fatalf("SpawnCountFor mem with refuse=0: %d want 2", got)
	}
}

func TestAutoscaleRapidMemRiseAsksSpawnCountTwo(t *testing.T) {
	var gotCount atomic.Int64
	var reason atomic.Value
	cfg := underPressure()
	cfg.MemLimitPct = 90
	cfg.MemFloorPct = 80
	cfg.MemRisePct = 1
	cfg.RiseHold = time.Millisecond
	cfg.MemRefusePct = 68
	a := core.NewAutoscaleController(cfg)

	up := func(req core.ScaleUpRequest) error {
		reason.Store(req.Reason)
		gotCount.Store(int64(req.SpawnCount))
		return nil
	}
	for i, pct := range []float64{2, 7, 12} {
		a.Tick(core.PressureInput{
			SelfName:  "self",
			Resources: core.ResourceSample{MemPct: pct, DiskFreePct: 80, DiskOK: true},
		}, up, nil)
		if i == 0 && reason.Load() != nil {
			t.Fatal("asked on the first sample")
		}
		time.Sleep(40 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if reason.Load() != "mem_rise" {
		t.Fatalf("reason=%v want mem_rise", reason.Load())
	}
	if gotCount.Load() != 2 {
		t.Fatalf("spawn_count=%d want 2", gotCount.Load())
	}
}

func TestAutoscaleHardMemSpawnCount(t *testing.T) {
	cfg := underPressure()
	cfg.MemLimitPct = 55
	cfg.MemRefusePct = 68
	a := core.NewAutoscaleController(cfg)

	var count atomic.Int64
	a.Tick(memHot(56), func(req core.ScaleUpRequest) error {
		count.Store(int64(req.SpawnCount))
		return nil
	}, nil)
	time.Sleep(50 * time.Millisecond)
	if count.Load() != 2 {
		t.Fatalf("hard mem spawn_count=%d want 2", count.Load())
	}

	a = core.NewAutoscaleController(cfg)
	count.Store(0)
	a.Tick(memHot(65), func(req core.ScaleUpRequest) error {
		count.Store(int64(req.SpawnCount))
		return nil
	}, nil)
	time.Sleep(50 * time.Millisecond)
	if count.Load() != 3 {
		t.Fatalf("near-refuse spawn_count=%d want 3", count.Load())
	}
}

func TestAutoscaleMemFillingSpawnCountOne(t *testing.T) {
	cfg := underPressure()
	cfg.MemLimitPct = 55
	cfg.MemFloorPct = 20
	cfg.MemLead = time.Minute
	cfg.PressureHold = time.Millisecond
	cfg.MemRisePct = 1000
	cfg.MemRefusePct = 68
	a := core.NewAutoscaleController(cfg)

	var reason atomic.Value
	var count atomic.Int64
	up := func(req core.ScaleUpRequest) error {
		reason.Store(req.Reason)
		count.Store(int64(req.SpawnCount))
		return nil
	}

	for _, pct := range []float64{30, 31, 32} {
		a.Tick(core.PressureInput{
			SelfName:  "self",
			Resources: core.ResourceSample{MemPct: pct, DiskFreePct: 80, DiskOK: true},
		}, up, nil)
		time.Sleep(30 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if reason.Load() != "mem_filling" {
		t.Fatalf("reason=%v want mem_filling", reason.Load())
	}
	if count.Load() != 1 {
		t.Fatalf("spawn_count=%d want 1 for mem_filling", count.Load())
	}
}

func TestAutoscaleBlocksClientsWhileUpInFlight(t *testing.T) {
	cfg := underPressure()
	cfg.Cooldown = time.Hour
	a := core.NewAutoscaleController(cfg)

	release := make(chan struct{})
	started := make(chan struct{})
	a.Tick(memHot(80), func(core.ScaleUpRequest) error {
		close(started)
		<-release
		return nil
	}, nil)

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("scale-up never started")
	}
	if !a.ClientWritesBlocked() {
		t.Fatal("clients still admitted while up_in_flight")
	}
	close(release)
	time.Sleep(50 * time.Millisecond)

	if !a.ClientWritesBlocked() {
		t.Fatal("clients admitted during post-scale spawn grace")
	}
}

func TestAutoscaleAsksForOneNodePerDecision(t *testing.T) {
	var ups atomic.Int64
	a := core.NewAutoscaleController(core.AutoscaleConfig{
		Enabled:        true,
		Role:           "bootstrap",
		Window:         time.Minute,
		Cooldown:       time.Hour,
		PressureHold:   time.Millisecond,
		MemLimitPct:    65,
		DiskMinFreePct: 1,
	})

	up := func(core.ScaleUpRequest) error { ups.Add(1); return nil }
	hot := core.PressureInput{
		SelfName:  "self",
		Resources: core.ResourceSample{MemPct: 80, DiskFreePct: 80, DiskOK: true},
	}
	for i := 0; i < 5; i++ {
		a.Tick(hot, up, nil)
		time.Sleep(20 * time.Millisecond)
	}

	if n := ups.Load(); n != 1 {
		t.Fatalf("scale requests=%d want 1 while the cooldown holds", n)
	}
	if snap := a.Snapshot(); snap["scale_ups_done"].(int) != 1 {
		t.Fatalf("scale_ups_done=%v want 1", snap["scale_ups_done"])
	}
}
