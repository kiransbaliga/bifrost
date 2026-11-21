package sysguard

import (
	"bufio"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/valkyrie-stack/bifrost/internal/domain"
)

var (
	startTime      = time.Now()
	cpuUsageMu     sync.Mutex
	lastCPUTotal   uint64
	lastCPUIdle    uint64
	lastCalculated float64
	lastCheckTime  time.Time
)

// ApplyMemoryHacks tunes the Go runtime for optimal memory efficiency across any host.
func ApplyMemoryHacks(softLimitMB int64) {
	if softLimitMB <= 0 {
		softLimitMB = 32
	}
	debug.SetMemoryLimit(softLimitMB * 1024 * 1024)
	debug.SetGCPercent(50)
}

// GetSystemMetrics collects real-time host telemetry and Bifrost process resource consumption.
func GetSystemMetrics(activeContainers, activeRoutes int) domain.SystemMetrics {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	hostname, _ := os.Hostname()

	metrics := domain.SystemMetrics{
		OS:               runtime.GOOS,
		Arch:             runtime.GOARCH,
		Hostname:         hostname,
		CPUCores:         runtime.NumCPU(),
		NumGoroutines:    runtime.NumGoroutine(),
		GoAllocMB:        m.Alloc / 1024 / 1024,
		GoSysMB:          m.Sys / 1024 / 1024,
		UptimeSeconds:    int64(time.Since(startTime).Seconds()),
		ActiveContainers: activeContainers,
		ActiveRoutes:     activeRoutes,
	}

	// Capture Bifrost Process RSS
	metrics.ProcessRSSMB = getProcessRSSMB()

	// Capture Disk Stats for root filesystem
	getDiskStats("/", &metrics)

	// Capture Host Memory & CPU
	if runtime.GOOS == "linux" {
		parseLinuxMemInfo(&metrics)
		metrics.CPUUsagePercent = calculateLinuxCPU()
	} else if runtime.GOOS == "darwin" {
		parseDarwinMemInfo(&metrics)
	}

	// Calculate percentages
	if metrics.TotalRAMMB > 0 {
		metrics.UsedRAMMB = metrics.TotalRAMMB - metrics.AvailableRAMMB
		metrics.RAMUsagePercent = (float64(metrics.UsedRAMMB) / float64(metrics.TotalRAMMB)) * 100.0
	}
	if metrics.TotalSwapMB > 0 {
		metrics.UsedSwapMB = metrics.TotalSwapMB - metrics.FreeSwapMB
		metrics.SwapUsagePercent = (float64(metrics.UsedSwapMB) / float64(metrics.TotalSwapMB)) * 100.0
	}

	return metrics
}

func getProcessRSSMB() uint64 {
	var ru unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &ru); err == nil {
		if runtime.GOOS == "darwin" {
			// On Darwin, ru.Maxrss is in bytes
			return uint64(ru.Maxrss) / 1024 / 1024
		}
		// On Linux, ru.Maxrss is in KiB
		return uint64(ru.Maxrss) / 1024
	}
	return 0
}

func getDiskStats(path string, metrics *domain.SystemMetrics) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err == nil {
		bsize := uint64(stat.Bsize)
		totalBytes := stat.Blocks * bsize
		freeBytes := stat.Bavail * bsize
		usedBytes := totalBytes - freeBytes

		metrics.DiskTotalGB = round2(float64(totalBytes) / (1024 * 1024 * 1024))
		metrics.DiskFreeGB = round2(float64(freeBytes) / (1024 * 1024 * 1024))
		metrics.DiskUsedGB = round2(float64(usedBytes) / (1024 * 1024 * 1024))
		if metrics.DiskTotalGB > 0 {
			metrics.DiskUsagePercent = round2((metrics.DiskUsedGB / metrics.DiskTotalGB) * 100.0)
		}
	}
}

func parseLinuxMemInfo(metrics *domain.SystemMetrics) {
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		key := strings.TrimSuffix(parts[0], ":")
		valKb, err := strconv.ParseUint(parts[1], 10, 64)
		if err != nil {
			continue
		}
		valMb := valKb / 1024

		switch key {
		case "MemTotal":
			metrics.TotalRAMMB = valMb
		case "MemFree":
			metrics.FreeRAMMB = valMb
		case "MemAvailable":
			metrics.AvailableRAMMB = valMb
		case "SwapTotal":
			metrics.TotalSwapMB = valMb
		case "SwapFree":
			metrics.FreeSwapMB = valMb
		}
	}
	if metrics.AvailableRAMMB == 0 && metrics.FreeRAMMB > 0 {
		metrics.AvailableRAMMB = metrics.FreeRAMMB
	}
}

func parseDarwinMemInfo(metrics *domain.SystemMetrics) {
	// Total RAM via sysctl hw.memsize
	if memBytes, err := unix.SysctlUint64("hw.memsize"); err == nil {
		metrics.TotalRAMMB = memBytes / 1024 / 1024
		// Estimate available on Darwin based on resident set
		metrics.AvailableRAMMB = metrics.TotalRAMMB * 60 / 100
		metrics.FreeRAMMB = metrics.TotalRAMMB * 40 / 100
	}
	// Total Swap on Darwin
	if swapBytes, err := unix.SysctlUint64("vm.swapusage"); err == nil {
		metrics.TotalSwapMB = swapBytes / 1024 / 1024
		metrics.FreeSwapMB = metrics.TotalSwapMB
	} else {
		metrics.TotalSwapMB = 2048
		metrics.FreeSwapMB = 2048
	}
}

func calculateLinuxCPU() float64 {
	cpuUsageMu.Lock()
	defer cpuUsageMu.Unlock()

	now := time.Now()
	if now.Sub(lastCheckTime) < 1*time.Second && lastCalculated > 0 {
		return lastCalculated
	}

	file, err := os.Open("/proc/stat")
	if err != nil {
		return 0
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	if !scanner.Scan() {
		return 0
	}
	fields := strings.Fields(scanner.Text())
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0
	}

	var total, idle uint64
	for i := 1; i < len(fields); i++ {
		val, _ := strconv.ParseUint(fields[i], 10, 64)
		total += val
		if i == 4 { // idle is the 4th field
			idle = val
		}
	}

	if lastCPUTotal > 0 && total > lastCPUTotal {
		diffTotal := float64(total - lastCPUTotal)
		diffIdle := float64(idle - lastCPUIdle)
		if diffTotal > 0 {
			lastCalculated = round2(((diffTotal - diffIdle) / diffTotal) * 100.0)
		}
	}

	lastCPUTotal = total
	lastCPUIdle = idle
	lastCheckTime = now

	return lastCalculated
}

func round2(val float64) float64 {
	return float64(int(val*100+0.5)) / 100
}
