package e2e

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"droidfs/internal/mount"
)

// fakeADB imitates the adb server well enough for the daemon: it announces
// one device, and "runs" the agent by starting the locally built agent with
// the connection as its stdin/stdout. Unplugging the device kills the agent
// and withdraws the announcement, which is what a pulled cable looks like.
type fakeADB struct {
	ln    net.Listener
	agent string

	mu       sync.Mutex
	present  bool
	trackers []net.Conn
	agents   []*exec.Cmd
}

const fakeSerial = "FAKE123"

func (f *fakeADB) list() string {
	if !f.present {
		return ""
	}
	return fakeSerial + "                device product:fake model:Pixel_Test device:fake transport_id:1\n"
}

func block(s string) string { return fmt.Sprintf("%04x%s", len(s), s) }

func (f *fakeADB) setPresent(p bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.present = p
	if !p {
		for _, c := range f.agents {
			c.Process.Kill()
			go c.Wait()
		}
		f.agents = nil
	}
	for _, c := range f.trackers {
		io.WriteString(c, block(f.list()))
	}
}

func readReq(c net.Conn) (string, error) {
	var h [4]byte
	if _, err := io.ReadFull(c, h[:]); err != nil {
		return "", err
	}
	n, err := strconv.ParseUint(string(h[:]), 16, 32)
	if err != nil {
		return "", err
	}
	b := make([]byte, n)
	_, err = io.ReadFull(c, b)
	return string(b), err
}

func (f *fakeADB) serve() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.handle(c)
	}
}

func (f *fakeADB) handle(c net.Conn) {
	req, err := readReq(c)
	if err != nil {
		c.Close()
		return
	}
	switch {
	case req == "host:track-devices-l":
		f.mu.Lock()
		io.WriteString(c, "OKAY"+block(f.list()))
		f.trackers = append(f.trackers, c)
		f.mu.Unlock()
		return
	case req == "host:devices-l":
		f.mu.Lock()
		io.WriteString(c, "OKAY"+block(f.list()))
		f.mu.Unlock()
	case strings.HasPrefix(req, "host:transport:"):
		f.mu.Lock()
		ok := f.present
		f.mu.Unlock()
		if !ok {
			io.WriteString(c, "FAIL"+block("device '"+fakeSerial+"' not found"))
			break
		}
		io.WriteString(c, "OKAY")
		svc, err := readReq(c)
		if err != nil {
			break
		}
		switch {
		case svc == "exec:getprop ro.product.cpu.abi":
			io.WriteString(c, "OKAYarm64-v8a\n")
		case strings.HasPrefix(svc, "exec:/data/local/tmp/droidfs-agent-"):
			io.WriteString(c, "OKAY")
			// Hand the socket itself to the agent, so that no goroutine
			// of this (file-system-using) process sits in the data path.
			file, _ := c.(*net.TCPConn).File()
			cmd := exec.Command(f.agent)
			cmd.Stdin, cmd.Stdout = file, file
			if err := cmd.Start(); err == nil {
				f.mu.Lock()
				f.agents = append(f.agents, cmd)
				f.mu.Unlock()
			}
			file.Close()
		default:
			io.WriteString(c, "OKAY")
		}
	}
	c.Close()
}

func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	start := time.Now()
	for time.Since(start) < d {
		if cond() {
			t.Logf("%s: after %s", what, time.Since(start).Round(10*time.Millisecond))
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s: not within %s", what, d)
}

// TestDaemon drives the real droidfs binary through a device's whole life:
// plug, use, unplug, quick return, long absence, return, daemon crash.
func TestDaemon(t *testing.T) {
	tmp, err := os.MkdirTemp("", "droidfs-daemon-")
	if err != nil {
		t.Fatal(err)
	}
	tmp, _ = filepath.EvalSymlinks(tmp)
	bin, agent := filepath.Join(tmp, "droidfs"), filepath.Join(tmp, "agent")
	dev, dir := filepath.Join(tmp, "device"), filepath.Join(tmp, "mounts")
	os.Mkdir(dev, 0o755)
	for out, pkg := range map[string]string{bin: "droidfs/cmd/droidfs", agent: "droidfs/cmd/droidfs-agent"} {
		if b, err := exec.Command("go", "build", "-o", out, pkg).CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", pkg, err, b)
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeADB{ln: ln, agent: agent, present: true}
	go fake.serve()
	defer ln.Close()

	point := filepath.Join(dir, "Pixel Test")
	var logs bytes.Buffer
	var logMu sync.Mutex
	start := func() *exec.Cmd {
		cmd := exec.Command(bin, "-dir", dir, "-root", dev, "-grace", "4s")
		cmd.Env = append(os.Environ(),
			"ANDROID_ADB_SERVER_PORT="+strconv.Itoa(ln.Addr().(*net.TCPAddr).Port),
			"HOME="+tmp) // private lock file: do not collide with a real daemon
		r, w, _ := os.Pipe()
		cmd.Stderr = w
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		w.Close()
		go func() {
			buf := make([]byte, 4096)
			for {
				n, err := r.Read(buf)
				logMu.Lock()
				logs.Write(buf[:n])
				logMu.Unlock()
				if err != nil {
					return
				}
			}
		}()
		return cmd
	}
	daemon := start()
	defer func() {
		daemon.Process.Kill()
		daemon.Wait()
		mount.Unmount(point, true)
		logMu.Lock()
		t.Logf("daemon log:\n%s", logs.String())
		logMu.Unlock()
		if !mount.IsMounted(point) {
			os.RemoveAll(tmp)
		}
	}()
	mounted := func() bool { return mount.IsMounted(point) }

	waitFor(t, 15*time.Second, "mounted after plug-in", mounted)
	if err := os.WriteFile(filepath.Join(point, "note.txt"), []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dev, "note.txt")); string(b) != "kept" {
		t.Fatalf("device has %q", b)
	}

	// --- cable pulled, plugged back within the grace period
	os.WriteFile(filepath.Join(dev, "uncached-1"), []byte("1"), 0o644)
	time.Sleep(2 * time.Second)
	fake.setPresent(false)
	within(t, 4*time.Second, "stat right after unplug", func() {
		if _, err := os.Stat(filepath.Join(point, "uncached-1")); err == nil {
			t.Error("stat succeeded on an unplugged device")
		} else {
			t.Logf("stat: %v", err)
		}
	})
	if !mounted() {
		t.Fatal("mount dropped before the grace period ended")
	}
	fake.setPresent(true)
	waitFor(t, 6*time.Second, "usable again after quick re-plug", func() bool {
		b, err := os.ReadFile(filepath.Join(point, "uncached-1"))
		return err == nil && string(b) == "1"
	})
	if !mounted() {
		t.Fatal("mount did not survive a quick re-plug")
	}

	// --- cable pulled for good: the volume must go away by itself
	fake.setPresent(false)
	waitFor(t, 20*time.Second, "unmounted after grace period", func() bool { return !mounted() })
	if _, err := os.Stat(point); err == nil {
		t.Errorf("mount point directory left behind")
	}

	// --- and come back when the device does
	fake.setPresent(true)
	waitFor(t, 15*time.Second, "remounted after return", mounted)
	if b, err := os.ReadFile(filepath.Join(point, "note.txt")); err != nil || string(b) != "kept" {
		t.Fatalf("after remount: %q, %v", b, err)
	}

	// --- daemon crashes; its successor cleans up and remounts
	daemon.Process.Kill()
	daemon.Wait()
	if !mounted() {
		t.Fatal("expected an orphaned mount after the crash")
	}
	daemon = start()
	// A write that reaches the device proves a live server is behind the
	// mount again (reads could be answered from the kernel's cache).
	waitFor(t, 60*time.Second, "successor serving the volume", func() bool {
		if os.WriteFile(filepath.Join(point, "after-crash"), []byte("alive"), 0o644) != nil {
			return false
		}
		b, _ := os.ReadFile(filepath.Join(dev, "after-crash"))
		return string(b) == "alive"
	})

	// --- clean shutdown
	daemon.Process.Signal(syscall.SIGINT)
	done := make(chan struct{})
	go func() {
		daemon.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("daemon did not exit within 30s of SIGINT")
	}
	if mounted() {
		t.Fatal("volume still mounted after clean shutdown")
	}
}
