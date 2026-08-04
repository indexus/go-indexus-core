package core

import (
	"bufio"
	"log/slog"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type ResourceSample struct {
	HeapInuse   uint64  `json:"heap_inuse"`
	RSS         uint64  `json:"rss_bytes"`
	MemTotal    uint64  `json:"mem_total_bytes"`
	MemPct      float64 `json:"mem_pct"`
	DiskPath    string  `json:"disk_path,omitempty"`
	DiskFreePct float64 `json:"disk_free_pct"`
	DiskOK      bool    `json:"disk_ok"`

	CPUPct float64 `json:"cpu_pct"`
	CPUOK  bool    `json:"cpu_ok"`
}

func SampleResources(diskPath string) ResourceSample {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	s := ResourceSample{
		HeapInuse:   ms.HeapInuse,
		DiskPath:    diskPath,
		DiskFreePct: 100,
		DiskOK:      true,
	}
	s.RSS = processRSS()
	s.MemTotal = systemMemTotal()
	if s.MemTotal > 0 && s.RSS > 0 {
		s.MemPct = 100 * float64(s.RSS) / float64(s.MemTotal)
	}
	if diskPath != "" {
		if free, ok := diskFreePct(diskPath); ok {
			s.DiskFreePct = free
			s.DiskOK = true
		} else {
			s.DiskOK = false
		}
	}
	s.CPUPct, s.CPUOK = cpu.sample()
	return s
}

type cpuMeter struct {
	mu   sync.Mutex
	at   time.Time
	used time.Duration
}

var cpu = &cpuMeter{}

func (m *cpuMeter) sample() (float64, bool) {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0, false
	}
	used := time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
	now := time.Now()

	m.mu.Lock()
	defer m.mu.Unlock()

	previousAt, previousUsed := m.at, m.used
	m.at, m.used = now, used
	if previousAt.IsZero() {
		return 0, false
	}
	elapsed := now.Sub(previousAt)
	cores := float64(runtime.NumCPU())
	if elapsed <= 0 || cores <= 0 {
		return 0, false
	}
	return 100 * float64(used-previousUsed) / (float64(elapsed) * cores), true
}

func CapHeapToHost() {
	if os.Getenv("GOMEMLIMIT") != "" {
		return
	}
	total := systemMemTotal()
	if total == 0 {
		return
	}
	limit := int64(float64(total) * 0.75)
	debug.SetMemoryLimit(limit)
	slog.Info("heap capped to host memory", "limit_bytes", limit, "host_bytes", total)
}

func memoryUsedPct() float64 {
	rss := processRSS()
	total := systemMemTotal()
	if rss == 0 || total == 0 {
		return 0
	}
	return 100 * float64(rss) / float64(total)
}

func processRSS() uint64 {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return kb * 1024
	}
	return 0
}

func systemMemTotal() uint64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return kb * 1024
	}
	return 0
}

func diskFreePct(path string) (float64, bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, false
	}
	total := st.Blocks * uint64(st.Bsize)
	if total == 0 {
		return 0, false
	}
	free := st.Bavail * uint64(st.Bsize)
	return 100 * float64(free) / float64(total), true
}
