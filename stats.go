package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// cpuSample reads the aggregate "cpu" line from /proc/stat.
type cpuSample struct {
	idle, total uint64
}

func readCPUSample() (cpuSample, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return cpuSample{}, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 8 || fields[0] != "cpu" {
			continue
		}
		var sum, idle uint64
		for i, v := range fields[1:] {
			n, _ := strconv.ParseUint(v, 10, 64)
			sum += n
			if i == 3 { // idle field
				idle = n
			}
		}
		return cpuSample{idle: idle, total: sum}, nil
	}
	return cpuSample{}, fmt.Errorf("no cpu line in /proc/stat")
}

// cpuUsagePercent samples /proc/stat twice over a short window.
func cpuUsagePercent() (float64, error) {
	a, err := readCPUSample()
	if err != nil {
		return 0, err
	}
	time.Sleep(200 * time.Millisecond)
	b, err := readCPUSample()
	if err != nil {
		return 0, err
	}
	totalDelta := float64(b.total - a.total)
	idleDelta := float64(b.idle - a.idle)
	if totalDelta <= 0 {
		return 0, nil
	}
	return (1 - idleDelta/totalDelta) * 100, nil
}

type memInfo struct {
	totalKB, availKB uint64
}

func readMemInfo() (memInfo, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return memInfo{}, err
	}
	defer f.Close()

	var mi memInfo
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		v, _ := strconv.ParseUint(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			mi.totalKB = v
		case "MemAvailable:":
			mi.availKB = v
		}
	}
	return mi, nil
}

func diskUsage(path string) (totalBytes, freeBytes uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	total := uint64(st.Blocks) * uint64(st.Bsize)
	free := uint64(st.Bavail) * uint64(st.Bsize)
	return total, free, nil
}

func bytesReadable(b uint64) string {
	return readableSize(int64(b))
}

func (a *App) statsText() string {
	var b strings.Builder
	b.WriteString("📊 Server stats\n\n")

	if pct, err := cpuUsagePercent(); err == nil {
		fmt.Fprintf(&b, "🧠 CPU: %.1f%%\n", pct)
	} else {
		b.WriteString("🧠 CPU: unavailable\n")
	}

	if mi, err := readMemInfo(); err == nil && mi.totalKB > 0 {
		usedKB := mi.totalKB - mi.availKB
		pct := float64(usedKB) / float64(mi.totalKB) * 100
		fmt.Fprintf(&b, "💾 RAM: %s / %s (%.1f%%)\n",
			bytesReadable(usedKB*1024), bytesReadable(mi.totalKB*1024), pct)
	} else {
		b.WriteString("💾 RAM: unavailable\n")
	}

	if total, free, err := diskUsage("/"); err == nil && total > 0 {
		used := total - free
		pct := float64(used) / float64(total) * 100
		fmt.Fprintf(&b, "🗄️ Storage: %s / %s (%.1f%%)\n",
			bytesReadable(used), bytesReadable(total), pct)
	} else {
		b.WriteString("🗄️ Storage: unavailable\n")
	}

	fmt.Fprintf(&b, "📁 Cached file entries: %d\n", a.cache.count())
	fmt.Fprintf(&b, "⏱️ Uptime: %s\n", readableTime(int64(time.Since(a.startedAt).Seconds())))

	return b.String()
}
