// Package probe checks the agent's container from the inside: what the agent could do if it
// tried. Most checks are an attempt at something the sandbox must not allow; two must work:
// the tools in the agent's shell and the way to the model.
package probe

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Check is one attempt and its verdict.
type Check struct {
	Name string
	OK   bool
	Note string
}

// Run makes the attempts; egress is the proxy's host:port.
func Run(egress string) []Check {
	var cs []Check
	add := func(name string, ok bool, note string, args ...any) {
		cs = append(cs, Check{Name: name, OK: ok, Note: fmt.Sprintf(note, args...)})
	}

	add("not root", os.Getuid() != 0, "uid %d", os.Getuid())

	status, _ := os.ReadFile("/proc/self/status")
	// The bounding set: what even a setuid binary could gain. A non-root user has no effective
	// capabilities anyway; only --cap-drop ALL empties this one.
	capBnd, nnp := field(string(status), "CapBnd"), field(string(status), "NoNewPrivs")
	add("no capabilities", strings.Trim(capBnd, "0") == "", "CapBnd %s", capBnd)
	add("no new privileges", nnp == "1", "NoNewPrivs %s", nnp)

	// Read-only, not just not ours: /tmp-like holes aside, nothing of the image can change.
	err := os.WriteFile("/home/agent/probe-write", []byte("x"), 0o644)
	add("root filesystem read-only", err != nil && strings.Contains(err.Error(), "read-only file system"), "%v", err)

	var devs []string
	entries, _ := os.ReadDir("/dev")
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.Mode()&os.ModeDevice != 0 && info.Mode()&os.ModeCharDevice == 0 {
			devs = append(devs, e.Name())
		}
	}
	add("no block devices (disks, partitions)", len(devs) == 0, "%v", devs)

	var seen []string
	for _, p := range []string{"/mnt/c", "/mnt/f", "/mnt/host", "/host_mnt", "/run/desktop", "/var/run/docker.sock", "/run/docker.sock"} {
		if _, err := os.Stat(p); err == nil {
			seen = append(seen, p)
		}
	}
	add("no host drives, no Docker socket", len(seen) == 0, "%v", seen)

	// The agent's commands run in a login shell, which rebuilds PATH: the tools must survive it.
	out, err := exec.Command("bash", "-lc", "command -v go && command -v gofmt && command -v git").CombinedOutput()
	add("go, gofmt and git in a login shell", err == nil, "%s", strings.Join(strings.Fields(string(out)), " "))

	c, err := net.DialTimeout("tcp", "1.1.1.1:443", 3*time.Second)
	if c != nil {
		c.Close()
	}
	add("no direct way out", err != nil, "dial 1.1.1.1:443: %v", err)

	code, err := connect(egress, "example.com:443")
	add("proxy refuses other hosts", code == "403", "example.com: %s %v", code, err)
	code, err = connect(egress, "chatgpt.com:443")
	add("proxy lets the model through", code == "200", "chatgpt.com: %s %v", code, err)
	return cs
}

// Passed: every check holds.
func Passed(cs []Check) bool {
	for _, c := range cs {
		if !c.OK {
			return false
		}
	}
	return true
}

func field(status, name string) string {
	for _, l := range strings.Split(status, "\n") {
		if v, ok := strings.CutPrefix(l, name+":"); ok {
			return strings.TrimSpace(v)
		}
	}
	return "?"
}

// connect asks the proxy for a tunnel and returns the status code.
func connect(proxy, target string) (string, error) {
	c, err := net.DialTimeout("tcp", proxy, 5*time.Second)
	if err != nil {
		return "", err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(15 * time.Second))
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	line, err := bufio.NewReader(c).ReadString('\n')
	if f := strings.Fields(line); len(f) >= 2 {
		return f[1], nil
	}
	return "", err
}
