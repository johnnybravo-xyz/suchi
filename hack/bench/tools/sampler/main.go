// sampler: poll /proc/<pid>/status and /proc/<pid>/stat at a fixed interval,
// emit one JSONL row per tick. Exits cleanly on SIGINT/SIGTERM or when the
// target process disappears.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type sample struct {
	utime, stime uint64
	threads      int
	rssKB        uint64
	vmDataKB     uint64
	wall         time.Time
}

func main() {
	pid := flag.Int("pid", 0, "target pid")
	interval := flag.Duration("interval", 100*time.Millisecond, "sample interval")
	out := flag.String("out", "", "output jsonl file")
	flag.Parse()

	if *pid <= 0 || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: sampler -pid <int> -interval <dur> -out <file>")
		os.Exit(2)
	}

	clkTck := int64(100)
	if v := os.Getenv("CLK_TCK"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			clkTck = n
		}
	}
	numCPU := runtime.NumCPU()

	f, err := os.Create(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open out:", err)
		os.Exit(1)
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	defer w.Flush()

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)

	tick := time.NewTicker(*interval)
	defer tick.Stop()

	var prev *sample
	for {
		select {
		case <-sigc:
			w.Flush()
			return
		case now := <-tick.C:
			s, err := readSample(*pid, now)
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					fmt.Fprintln(os.Stderr, "target exited")
					w.Flush()
					return
				}
				fmt.Fprintln(os.Stderr, "read sample:", err)
				continue
			}
			cpuPct := 0.0
			if prev != nil {
				dTicks := float64(s.utime+s.stime) - float64(prev.utime+prev.stime)
				dWall := s.wall.Sub(prev.wall).Seconds() * float64(clkTck)
				if dWall > 0 {
					cpuPct = (dTicks / dWall) * 100.0 * float64(numCPU)
				}
			}
			fmt.Fprintf(w, `{"t":%q,"rss_kb":%d,"vmdata_kb":%d,"cpu_pct":%.2f,"threads":%d}`+"\n",
				s.wall.Format("2006-01-02T15:04:05.000Z07:00"), s.rssKB, s.vmDataKB, cpuPct, s.threads)
			w.Flush()
			prev = s
		}
	}
}

func readSample(pid int, now time.Time) (*sample, error) {
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return nil, err
	}
	s := &sample{wall: now}
	for _, line := range strings.Split(string(status), "\n") {
		switch {
		case strings.HasPrefix(line, "VmRSS:"):
			s.rssKB = parseKB(line)
		case strings.HasPrefix(line, "VmData:"):
			s.vmDataKB = parseKB(line)
		}
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return nil, err
	}
	line := string(stat)
	rp := strings.LastIndex(line, ")")
	if rp < 0 || rp+2 >= len(line) {
		return nil, fmt.Errorf("bad stat")
	}
	rest := strings.Fields(line[rp+2:])
	// After ')' the next field is (3) state. Field 14 utime → rest[11],
	// field 15 stime → rest[12], field 20 num_threads → rest[17].
	if len(rest) < 18 {
		return nil, fmt.Errorf("stat fields=%d", len(rest))
	}
	s.utime, _ = strconv.ParseUint(rest[11], 10, 64)
	s.stime, _ = strconv.ParseUint(rest[12], 10, 64)
	th, _ := strconv.ParseInt(rest[17], 10, 64)
	s.threads = int(th)
	return s, nil
}

func parseKB(line string) uint64 {
	f := strings.Fields(line)
	if len(f) < 2 {
		return 0
	}
	n, _ := strconv.ParseUint(f[1], 10, 64)
	return n
}
