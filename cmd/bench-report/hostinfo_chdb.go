//go:build chdb

package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// hostInfo is the physical machine the benchmark numbers were measured on,
// captured at bench time from Linux /proc and /sys. Every field is
// best-effort: anything that can't be read (file missing, parse failure,
// non-Linux host) is left empty and the renderer omits the row, so the
// generator never crashes on a thinner /proc than this one.
type hostInfo struct {
	cpuModel  string // e.g. "Intel(R) Core(TM) i7-10510U CPU @ 1.80GHz"
	cpuVendor string // e.g. "GenuineIntel"
	cpuFamily string // e.g. "family 6, model 142, stepping 12"
	cores     int    // physical cores (distinct socket×core)
	threads   int    // logical CPUs (SMT siblings included)
	sockets   int    // physical packages
	maxMHz    string // turbo ceiling, e.g. "4900 MHz"
	caches    []cacheLevel
	memTotal  string // total RAM rendered as GiB, e.g. "31.2 GiB"
	osPretty  string // e.g. "Ubuntu 24.04.4 LTS"
	kernel    string // e.g. "6.8.0-110-generic"
}

// cacheLevel is one entry of the CPU cache hierarchy, sized per instance
// with the instance count (mirrors how lscpu reports L1/L2 as per-core,
// L3 as per-socket).
type cacheLevel struct {
	label     string // "L1d", "L1i", "L2", "L3"
	size      string // per-instance size, e.g. "32 KiB"
	instances int
}

// captureHost reads the host hardware profile. It is robust by
// construction: each sub-reader swallows its own errors and leaves the
// corresponding field zero, so a partial /proc never aborts the report.
func captureHost() hostInfo {
	var h hostInfo
	h.readCPUInfo() // /proc/cpuinfo: model, vendor, family, core/thread/socket counts
	h.readMaxMHz()  // /sys cpufreq: turbo ceiling
	h.readCaches()  // /sys cache hierarchy
	h.memTotal = readMemTotal()
	h.osPretty = readOSPretty()
	h.kernel = readKernel()
	return h
}

// readCPUInfo parses /proc/cpuinfo for the model/vendor/family identity and
// derives physical-core and socket counts from the (physical id, core id)
// pairs. Logical-thread count is the number of "processor" records.
func (h *hostInfo) readCPUInfo() {
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return
	}
	defer f.Close()

	type coreKey struct{ phys, core string }
	physical := map[string]struct{}{}
	cores := map[coreKey]struct{}{}
	var curPhys, curCore string
	var family, model, stepping string

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" { // record separator: flush the (phys,core) pair
			if curPhys != "" || curCore != "" {
				physical[curPhys] = struct{}{}
				cores[coreKey{curPhys, curCore}] = struct{}{}
			}
			curPhys, curCore = "", ""
			continue
		}
		key, val, ok := splitColon(line)
		if !ok {
			continue
		}
		switch key {
		case "processor":
			h.threads++
		case "model name":
			if h.cpuModel == "" {
				h.cpuModel = val
			}
		case "vendor_id":
			if h.cpuVendor == "" {
				h.cpuVendor = val
			}
		case "cpu family":
			family = val
		case "model":
			model = val
		case "stepping":
			stepping = val
		case "physical id":
			curPhys = val
		case "core id":
			curCore = val
		}
	}
	// flush a trailing record with no terminating blank line.
	if curPhys != "" || curCore != "" {
		physical[curPhys] = struct{}{}
		cores[coreKey{curPhys, curCore}] = struct{}{}
	}

	h.sockets = len(physical)
	h.cores = len(cores)
	// If the kernel didn't expose physical/core ids, fall back so we never
	// report a nonsensical zero.
	if h.sockets == 0 && h.threads > 0 {
		h.sockets = 1
	}
	if h.cores == 0 {
		h.cores = h.threads
	}
	if family != "" && model != "" {
		fam := fmt.Sprintf("family %s, model %s", family, model)
		if stepping != "" {
			fam += ", stepping " + stepping
		}
		h.cpuFamily = fam
	}
}

// readMaxMHz reads the turbo ceiling from cpufreq sysfs (kHz), preferring
// cpuinfo_max_freq (hardware ceiling) over scaling_max_freq (governor cap).
func (h *hostInfo) readMaxMHz() {
	for _, p := range []string{
		"/sys/devices/system/cpu/cpu0/cpufreq/cpuinfo_max_freq",
		"/sys/devices/system/cpu/cpu0/cpufreq/scaling_max_freq",
	} {
		if kHz, ok := readUint(p); ok && kHz > 0 {
			h.maxMHz = fmt.Sprintf("%d MHz", kHz/1000)
			return
		}
	}
}

// readCaches walks /sys/devices/system/cpu/cpu*/cache to build the cache
// hierarchy, labelling by (level, type) and counting distinct shared_cpu
// instances so an L3 shared across all cores reports a single instance.
func (h *hostInfo) readCaches() {
	cpus, err := filepath.Glob("/sys/devices/system/cpu/cpu[0-9]*/cache/index[0-9]*")
	if err != nil || len(cpus) == 0 {
		return
	}
	type agg struct {
		label string
		size  string
		seen  map[string]struct{} // distinct shared_cpu_list values
	}
	byLabel := map[string]*agg{}
	for _, idx := range cpus {
		level := readTrim(filepath.Join(idx, "level"))
		ctype := readTrim(filepath.Join(idx, "type"))
		size := readTrim(filepath.Join(idx, "size"))
		if level == "" || size == "" {
			continue
		}
		label := cacheLabel(level, ctype)
		a := byLabel[label]
		if a == nil {
			a = &agg{label: label, size: normalizeCacheSize(size), seen: map[string]struct{}{}}
			byLabel[label] = a
		}
		// Count one instance per distinct sharing domain; fall back to the
		// sysfs index path so we never undercount when the list is absent.
		shared := readTrim(filepath.Join(idx, "shared_cpu_list"))
		if shared == "" {
			shared = idx
		}
		a.seen[shared] = struct{}{}
	}
	out := make([]cacheLevel, 0, len(byLabel))
	for _, a := range byLabel {
		out = append(out, cacheLevel{label: a.label, size: a.size, instances: len(a.seen)})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return cacheOrder(out[i].label) < cacheOrder(out[j].label)
	})
	h.caches = out
}

// cacheLabel maps a (level, type) pair to a short label: L1 splits into
// L1d / L1i, higher levels are unified (just "L2" / "L3").
func cacheLabel(level, ctype string) string {
	switch {
	case level == "1" && strings.EqualFold(ctype, "Data"):
		return "L1d"
	case level == "1" && strings.EqualFold(ctype, "Instruction"):
		return "L1i"
	default:
		return "L" + level
	}
}

// cacheOrder gives a stable display order: L1d, L1i, L2, L3, …
func cacheOrder(label string) int {
	switch label {
	case "L1d":
		return 0
	case "L1i":
		return 1
	}
	// "L2" -> 2, "L3" -> 3, etc.; default large for anything unexpected.
	if n, err := strconv.Atoi(strings.TrimPrefix(label, "L")); err == nil {
		return n
	}
	return 99
}

// normalizeCacheSize turns the sysfs "32K"/"8192K" form into "32 KiB" /
// "8 MiB" — KiB-rounded, with MiB once it divides evenly.
func normalizeCacheSize(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	unit := s[len(s)-1]
	numPart := s
	var kib int64
	switch unit {
	case 'K', 'k':
		numPart = s[:len(s)-1]
		if n, err := strconv.ParseInt(strings.TrimSpace(numPart), 10, 64); err == nil {
			kib = n
		}
	case 'M', 'm':
		numPart = s[:len(s)-1]
		if n, err := strconv.ParseInt(strings.TrimSpace(numPart), 10, 64); err == nil {
			kib = n * 1024
		}
	default:
		return s // unknown form — pass through untouched
	}
	if kib == 0 {
		return s
	}
	if kib%1024 == 0 {
		return fmt.Sprintf("%d MiB", kib/1024)
	}
	return fmt.Sprintf("%d KiB", kib)
}

// readMemTotal reads MemTotal (kB) from /proc/meminfo and renders it as GiB.
func readMemTotal() string {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, val, ok := splitColon(sc.Text())
		if !ok || key != "MemTotal" {
			continue
		}
		// val is like "32698572 kB".
		fields := strings.Fields(val)
		if len(fields) == 0 {
			return ""
		}
		kb, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			return ""
		}
		const kbPerGiB = 1024 * 1024
		return fmt.Sprintf("%.1f GiB", kb/kbPerGiB)
	}
	return ""
}

// readOSPretty reads PRETTY_NAME from /etc/os-release (quotes stripped).
func readOSPretty() string {
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return ""
}

// readKernel reads the kernel release string from /proc/sys/kernel/osrelease
// (the file behind `uname -r`), preferring a file read over shelling out.
func readKernel() string {
	return readTrim("/proc/sys/kernel/osrelease")
}

// --- small parsing helpers ----------------------------------------------

// splitColon splits a "key : value" or "key=value"-style line on the first
// colon, trimming whitespace on both sides.
func splitColon(line string) (key, val string, ok bool) {
	i := strings.IndexByte(line, ':')
	if i < 0 {
		return "", "", false
	}
	return strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:]), true
}

// readTrim reads a whole small sysfs/proc file and trims trailing newline.
func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// readUint reads a small file expected to hold a single unsigned integer.
func readUint(path string) (uint64, bool) {
	s := readTrim(path)
	if s == "" {
		return 0, false
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}
