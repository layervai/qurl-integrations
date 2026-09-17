package main

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	usageCommandTimeout = 30 * time.Second
	goosLinux           = "linux"
)

// usageSample is the resident daemon's footprint plus the machine's TCP
// picture at one instant. Remote peers are aggregated by port, never listed.
type usageSample struct {
	At                    time.Time      `json:"at"`
	Label                 string         `json:"label"`
	PID                   int            `json:"pid"`
	RSSKB                 int            `json:"rss_kb"`
	Threads               int            `json:"threads"`
	FDs                   int            `json:"fds"`
	TCPEstablished        int            `json:"tcp_established"`
	TCPByRemotePort       map[string]int `json:"tcp_established_by_remote_port,omitempty"`
	UDPSockets            int            `json:"udp_sockets"`
	MachineTCPEstablished int            `json:"machine_tcp_established"`
	TunnelPort            string         `json:"tunnel_port,omitempty"`
	MachineTCPToTunnel    int            `json:"machine_tcp_established_to_tunnel_port"`
	Errors                []string       `json:"errors,omitempty"`
}

var (
	launchctlPID = regexp.MustCompile(`(?m)^\s*pid = (\d+)`)
	lsofRemote   = regexp.MustCompile(`->\S+:(\d+) \(ESTABLISHED\)`)
)

func sampleUsage(ctx context.Context, env *environment, label string, originPort int) usageSample {
	sample := usageSample{At: time.Now(), Label: label}
	pid, err := daemonPID(ctx, env)
	if err != nil {
		sample.Errors = append(sample.Errors, "pid: "+err.Error())
	}
	sample.PID = pid
	if pid > 0 {
		sample.RSSKB = intOutput(ctx, &sample, "rss", "ps", "-o", "rss=", "-p", itoa(pid))
		sample.Threads = threadCount(ctx, &sample, pid)
		sample.FDs = fdCount(ctx, &sample, pid)
		sample.tcpForPID(ctx, pid, itoa(originPort))
	}
	sample.MachineTCPEstablished, sample.MachineTCPToTunnel = machineEstablished(ctx, &sample)
	return sample
}

func daemonPID(ctx context.Context, env *environment) (int, error) {
	if runtime.GOOS == "darwin" {
		out, err := commandOutput(ctx, "launchctl", "print", "gui/"+itoa(os.Getuid())+"/"+launchAgentLabel)
		if err == nil {
			if m := launchctlPID.FindStringSubmatch(out); m != nil {
				return strconv.Atoi(m[1])
			}
		}
	}
	out, err := commandOutput(ctx, "pgrep", "-f", "daemon run --state-dir "+env.StateDir)
	if err != nil {
		return 0, err
	}
	first, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	return strconv.Atoi(strings.TrimSpace(first))
}

func commandOutput(ctx context.Context, name string, args ...string) (string, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, usageCommandTimeout)
	defer cancel()
	out, err := exec.CommandContext(cmdCtx, name, args...).Output() //nolint:gosec // G204: fixed diagnostic tools (ps, lsof, netstat, launchctl, pgrep) with harness-built arguments.
	return string(out), err
}

func intOutput(ctx context.Context, sample *usageSample, what, name string, args ...string) int {
	out, err := commandOutput(ctx, name, args...)
	if err != nil {
		sample.Errors = append(sample.Errors, what+": "+err.Error())
		return 0
	}
	value, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		sample.Errors = append(sample.Errors, what+": "+err.Error())
		return 0
	}
	return value
}

func lineCount(out string) int {
	return len(strings.Split(strings.TrimSpace(out), "\n"))
}

func threadCount(ctx context.Context, sample *usageSample, pid int) int {
	if runtime.GOOS == goosLinux {
		entries, err := os.ReadDir("/proc/" + itoa(pid) + "/task")
		if err != nil {
			sample.Errors = append(sample.Errors, "threads: "+err.Error())
			return 0
		}
		return len(entries)
	}
	out, err := commandOutput(ctx, "ps", "-M", "-p", itoa(pid))
	if err != nil {
		sample.Errors = append(sample.Errors, "threads: "+err.Error())
		return 0
	}
	return lineCount(out) - 1
}

func fdCount(ctx context.Context, sample *usageSample, pid int) int {
	if runtime.GOOS == goosLinux {
		entries, err := os.ReadDir("/proc/" + itoa(pid) + "/fd")
		if err != nil {
			sample.Errors = append(sample.Errors, "fds: "+err.Error())
			return 0
		}
		return len(entries)
	}
	out, err := commandOutput(ctx, "lsof", "-a", "-p", itoa(pid), "-n", "-P")
	if err != nil {
		sample.Errors = append(sample.Errors, "fds: "+err.Error())
		return 0
	}
	return lineCount(out) - 1
}

func (s *usageSample) tcpForPID(ctx context.Context, pid int, originPort string) {
	// lsof exits 1 with no output when the process has no sockets at all;
	// that is a legitimate zero, not a sampling error.
	out, _ := commandOutput(ctx, "lsof", "-a", "-p", itoa(pid), "-i", "-n", "-P")
	s.TCPByRemotePort = map[string]int{}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, " UDP ") {
			s.UDPSockets++
		}
		if m := lsofRemote.FindStringSubmatch(line); m != nil {
			s.TCPEstablished++
			s.TCPByRemotePort[m[1]]++
		}
	}
	s.TunnelPort = tunnelPort(s.TCPByRemotePort, originPort)
}

// tunnelPort is the remote port the daemon holds the most sessions to,
// excluding the loopback origin: with N shares that is the tunnel server.
func tunnelPort(byPort map[string]int, exclude string) string {
	best, bestCount := "", 0
	for port, count := range byPort {
		if port != exclude && (count > bestCount || (count == bestCount && port < best)) {
			best, bestCount = port, count
		}
	}
	return best
}

func machineEstablished(ctx context.Context, sample *usageSample) (total, toTunnel int) {
	var out string
	var err error
	if runtime.GOOS == goosLinux {
		out, err = commandOutput(ctx, "ss", "-Htan", "state", "established")
	} else {
		out, err = commandOutput(ctx, "netstat", "-an", "-p", "tcp")
	}
	if err != nil {
		sample.Errors = append(sample.Errors, "machine tcp: "+err.Error())
		return 0, 0
	}
	for _, line := range strings.Split(out, "\n") {
		established := strings.Contains(line, "ESTABLISHED") || (runtime.GOOS == goosLinux && strings.TrimSpace(line) != "")
		if !established {
			continue
		}
		total++
		if sample.TunnelPort != "" && remotePortIs(line, sample.TunnelPort) {
			toTunnel++
		}
	}
	return total, toTunnel
}

// remotePortIs matches only the foreign-address column against one port:
// netstat prints "proto recv send local foreign state" (ip.port), ss with
// -H prints "recv send local peer" (ip:port). The local column is never
// consulted, so a listener on the tunnel port cannot inflate the count.
func remotePortIs(line, port string) bool {
	fields := strings.Fields(line)
	var foreign string
	switch {
	case len(fields) >= 6 && strings.HasPrefix(fields[0], "tcp"):
		foreign = fields[4]
	case len(fields) >= 4 && !strings.HasPrefix(fields[0], "tcp"):
		foreign = fields[3]
	default:
		return false
	}
	return strings.HasSuffix(foreign, "."+port) || strings.HasSuffix(foreign, ":"+port)
}
