// Command monitor logs the running host's CPU and memory once per interval,
// with the processes it started (agents, terminals, servers) totalled apart.
//
//	go run ./bench/monitor                 # finds the host listening on :51999
//	go run ./bench/monitor -pid 1234 -every 500ms -csv /tmp/host.csv
//
// "go heap" is the Go heap in use, read from the host's REPOGO_PPROF address
// when it has one (scripts/restart.sh sets 127.0.0.1:6061); "-" otherwise.
//
// Ctrl-C prints the average and peak of each column.
package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type proc struct {
	pid     int
	ppid    int
	cpuTime time.Duration
	rssKB   int64
}

type sample struct {
	hostCPU    float64
	hostRSSMB  float64
	childCPU   float64
	childRSSMB float64
	childCount int
	goHeapMB   float64 // -1 when the host serves no profiles
}

func main() {
	pid := flag.Int("pid", 0, "host pid (default: the process listening on -port)")
	port := flag.Int("port", 51999, "host port, used to find the pid")
	every := flag.Duration("every", time.Second, "sample interval")
	csvPath := flag.String("csv", "", "also write samples to this CSV file")
	pprofAddr := flag.String("pprof", "127.0.0.1:6061", "the host's REPOGO_PPROF address")
	flag.Parse()

	if *pid == 0 {
		found, err := listenerPID(*port)
		if err != nil {
			fmt.Fprintf(os.Stderr, "no host on :%d: %v\n", *port, err)
			os.Exit(1)
		}
		*pid = found
	}

	var csv *os.File
	if *csvPath != "" {
		f, err := os.Create(*csvPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		defer f.Close()
		fmt.Fprintln(f, "time,host_cpu_pct,host_rss_mb,child_cpu_pct,child_rss_mb,children,go_heap_mb")
		csv = f
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	fmt.Printf("monitoring host pid %d every %s\n\n", *pid, *every)
	fmt.Printf("%-8s  %8s  %9s  %9s  %10s  %8s  %9s\n", "time", "host cpu", "host rss", "child cpu", "child rss", "children", "go heap")

	prev, err := snapshot()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	prevAt := time.Now()

	var samples []sample
	ticker := time.NewTicker(*every)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			summarize(samples)
			return
		case now := <-ticker.C:
			cur, err := snapshot()
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
				continue
			}
			if _, ok := cur[*pid]; !ok {
				fmt.Printf("\nhost pid %d exited\n", *pid)
				summarize(samples)
				return
			}
			s := measure(*pid, prev, cur, now.Sub(prevAt))
			s.goHeapMB = goHeapMB(*pprofAddr)
			samples = append(samples, s)
			prev, prevAt = cur, now

			stamp := now.Format("15:04:05")
			fmt.Printf("%-8s  %7.1f%%  %6.1f MB  %8.1f%%  %7.1f MB  %8d  %9s\n",
				stamp, s.hostCPU, s.hostRSSMB, s.childCPU, s.childRSSMB, s.childCount, megabytes(s.goHeapMB))
			if csv != nil {
				fmt.Fprintf(csv, "%s,%.2f,%.2f,%.2f,%.2f,%d,%.2f\n",
					now.Format(time.RFC3339), s.hostCPU, s.hostRSSMB, s.childCPU, s.childRSSMB, s.childCount, s.goHeapMB)
			}
		}
	}
}

// measure turns two snapshots into CPU percentages, where 100% is one core.
// A process that started between them counts its whole CPU time.
func measure(hostPID int, prev, cur map[int]proc, elapsed time.Duration) sample {
	cpuPct := func(pid int) float64 {
		used := cur[pid].cpuTime - prev[pid].cpuTime
		return 100 * used.Seconds() / elapsed.Seconds()
	}

	host := cur[hostPID]
	s := sample{
		hostCPU:   cpuPct(hostPID),
		hostRSSMB: float64(host.rssKB) / 1024,
	}
	for _, child := range descendants(hostPID, cur) {
		s.childCPU += cpuPct(child)
		s.childRSSMB += float64(cur[child].rssKB) / 1024
		s.childCount++
	}
	return s
}

func descendants(root int, procs map[int]proc) []int {
	children := map[int][]int{}
	for _, p := range procs {
		children[p.ppid] = append(children[p.ppid], p.pid)
	}
	var out []int
	queue := children[root]
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		out = append(out, pid)
		queue = append(queue, children[pid]...)
	}
	return out
}

// snapshot reads every process's cumulative CPU time and resident memory.
func snapshot() (map[int]proc, error) {
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,time=,rss=").Output()
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	procs := map[int]proc{}
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 4 {
			continue
		}
		pid, _ := strconv.Atoi(fields[0])
		ppid, _ := strconv.Atoi(fields[1])
		rss, _ := strconv.ParseInt(fields[3], 10, 64)
		procs[pid] = proc{pid: pid, ppid: ppid, cpuTime: parseCPUTime(fields[2]), rssKB: rss}
	}
	return procs, scanner.Err()
}

// parseCPUTime reads ps's "[[dd-]hh:]mm:ss.ss" format.
func parseCPUTime(value string) time.Duration {
	var days float64
	if d, rest, ok := strings.Cut(value, "-"); ok {
		days, _ = strconv.ParseFloat(d, 64)
		value = rest
	}
	var seconds float64
	for _, part := range strings.Split(value, ":") {
		n, _ := strconv.ParseFloat(part, 64)
		seconds = seconds*60 + n
	}
	seconds += days * 86400
	return time.Duration(seconds * float64(time.Second))
}

func listenerPID(port int) (int, error) {
	out, err := exec.Command("lsof", "-t", "-sTCP:LISTEN", fmt.Sprintf("-iTCP:%d", port)).Output()
	if err != nil {
		return 0, fmt.Errorf("lsof: %w", err)
	}
	first, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	return strconv.Atoi(first)
}

func summarize(samples []sample) {
	if len(samples) == 0 {
		return
	}
	var sum, peak sample
	for _, s := range samples {
		sum.hostCPU += s.hostCPU
		sum.hostRSSMB += s.hostRSSMB
		sum.childCPU += s.childCPU
		sum.childRSSMB += s.childRSSMB
		peak.hostCPU = max(peak.hostCPU, s.hostCPU)
		peak.hostRSSMB = max(peak.hostRSSMB, s.hostRSSMB)
		peak.childCPU = max(peak.childCPU, s.childCPU)
		peak.childRSSMB = max(peak.childRSSMB, s.childRSSMB)
		peak.childCount = max(peak.childCount, s.childCount)
	}
	n := float64(len(samples))
	fmt.Printf("\n%d samples\n", len(samples))
	fmt.Printf("%-8s  %7.1f%%  %6.1f MB  %8.1f%%  %7.1f MB\n", "avg",
		sum.hostCPU/n, sum.hostRSSMB/n, sum.childCPU/n, sum.childRSSMB/n)
	fmt.Printf("%-8s  %7.1f%%  %6.1f MB  %8.1f%%  %7.1f MB  %8d\n", "peak",
		peak.hostCPU, peak.hostRSSMB, peak.childCPU, peak.childRSSMB, peak.childCount)
}

// goHeapMB reads HeapInuse from the runtime stats that close the text heap profile.
func goHeapMB(addr string) float64 {
	resp, err := http.Get("http://" + addr + "/debug/pprof/heap?debug=1")
	if err != nil {
		return -1
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return -1
	}
	for _, line := range strings.Split(string(body), "\n") {
		if value, ok := strings.CutPrefix(line, "# HeapInuse = "); ok {
			bytes, _ := strconv.ParseFloat(value, 64)
			return bytes / (1 << 20)
		}
	}
	return -1
}

func megabytes(mb float64) string {
	if mb < 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f MB", mb)
}
