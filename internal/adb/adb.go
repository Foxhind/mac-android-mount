// Package adb is a minimal client for the local adb server (the "smart
// socket" protocol on 127.0.0.1:5037). Every blocking step has a deadline:
// nothing in here may wait on a device forever.
package adb

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const stepTimeout = 10 * time.Second

type Client struct {
	Addr string

	startMu   sync.Mutex
	lastStart time.Time
}

func New() *Client {
	addr := "127.0.0.1:5037"
	if p := os.Getenv("ANDROID_ADB_SERVER_PORT"); p != "" {
		addr = "127.0.0.1:" + p
	}
	return &Client{Addr: addr}
}

// Device is one line of `adb devices -l`.
type Device struct {
	Serial string
	State  string // "device", "offline", "unauthorized", ...
	Model  string
	Props  map[string]string
}

func (d Device) Ready() bool { return d.State == "device" }

// FindADB locates the adb binary.
func FindADB() (string, error) {
	if p := os.Getenv("ADB"); p != "" {
		return p, nil
	}
	if p, err := exec.LookPath("adb"); err == nil {
		return p, nil
	}
	home, _ := os.UserHomeDir()
	for _, p := range []string{
		"/opt/homebrew/bin/adb",
		"/usr/local/bin/adb",
		filepath.Join(home, "Library/Android/sdk/platform-tools/adb"),
	} {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", errors.New("adb not found (install android-platform-tools or set $ADB)")
}

// startServer runs `adb start-server`, at most once every few seconds.
func (c *Client) startServer(ctx context.Context) error {
	c.startMu.Lock()
	defer c.startMu.Unlock()
	if time.Since(c.lastStart) < 5*time.Second {
		return errors.New("adb server was just (re)started")
	}
	c.lastStart = time.Now()
	bin, err := FindADB()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	// No pipes: the daemon would inherit them and Wait would never return.
	cmd := exec.CommandContext(ctx, bin, "start-server")
	return cmd.Run()
}

func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	d := net.Dialer{Timeout: 3 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", c.Addr)
	if err == nil {
		return conn, nil
	}
	if serr := c.startServer(ctx); serr != nil {
		return nil, fmt.Errorf("adb server unreachable: %v (%v)", err, serr)
	}
	return d.DialContext(ctx, "tcp", c.Addr)
}

// request sends one smart-socket request and consumes the OKAY/FAIL status.
func request(conn net.Conn, service string) error {
	conn.SetDeadline(time.Now().Add(stepTimeout))
	defer conn.SetDeadline(time.Time{})
	if _, err := fmt.Fprintf(conn, "%04x%s", len(service), service); err != nil {
		return err
	}
	var st [4]byte
	if _, err := io.ReadFull(conn, st[:]); err != nil {
		return err
	}
	switch string(st[:]) {
	case "OKAY":
		return nil
	case "FAIL":
		msg, err := readHexBlock(conn)
		if err != nil {
			return fmt.Errorf("adb: request %q failed", service)
		}
		return fmt.Errorf("adb: %s", msg)
	}
	return fmt.Errorf("adb: unexpected reply %q", st[:])
}

func readHexBlock(r io.Reader) (string, error) {
	var h [4]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return "", err
	}
	n, err := strconv.ParseUint(string(h[:]), 16, 32)
	if err != nil {
		return "", fmt.Errorf("adb: bad length %q", h[:])
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}

func parseDevices(s string) []Device {
	var out []Device
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		d := Device{Serial: f[0], State: f[1], Props: map[string]string{}}
		for _, kv := range f[2:] {
			if k, v, ok := strings.Cut(kv, ":"); ok {
				d.Props[k] = v
			}
		}
		d.Model = d.Props["model"]
		out = append(out, d)
	}
	return out
}

// Devices returns the current device list.
func (c *Client) Devices(ctx context.Context) ([]Device, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := request(conn, "host:devices-l"); err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Now().Add(stepTimeout))
	s, err := readHexBlock(conn)
	if err != nil {
		return nil, err
	}
	return parseDevices(s), nil
}

// Track calls fn with the full device list every time it changes. It returns
// when the connection to the adb server breaks or ctx is cancelled; callers
// are expected to call it again.
func (c *Client) Track(ctx context.Context, fn func([]Device)) error {
	conn, err := c.dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if err := request(conn, "host:track-devices-l"); err != nil {
		return err
	}
	for {
		// Legitimately blocks for as long as nothing changes.
		s, err := readHexBlock(conn)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		fn(parseDevices(s))
	}
}

// Open connects to a service on the device (e.g. "exec:cmd", "sync:"). The
// returned connection has no deadline set.
func (c *Client) Open(ctx context.Context, serial, service string) (net.Conn, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	if err := request(conn, "host:transport:"+serial); err != nil {
		conn.Close()
		return nil, err
	}
	if err := request(conn, service); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// Exec runs a shell command on the device and returns its raw output. The
// whole call is bounded by timeout.
func (c *Client) Exec(ctx context.Context, serial, cmd string, timeout time.Duration) (string, error) {
	conn, err := c.Open(ctx, serial, "exec:"+cmd)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))
	out, err := io.ReadAll(io.LimitReader(conn, 1<<20))
	return string(out), err
}

// Push writes data to a file on the device through the sync service.
func (c *Client) Push(ctx context.Context, serial, remote string, mode uint32, data []byte) error {
	conn, err := c.Open(ctx, serial, "sync:")
	if err != nil {
		return err
	}
	defer conn.Close()

	var buf []byte
	packet := func(id string, body []byte) {
		buf = append(buf, id...)
		buf = binary.LittleEndian.AppendUint32(buf, uint32(len(body)))
		buf = append(buf, body...)
	}
	flush := func() error {
		conn.SetDeadline(time.Now().Add(stepTimeout))
		_, err := conn.Write(buf)
		buf = buf[:0]
		return err
	}
	packet("SEND", []byte(fmt.Sprintf("%s,%d", remote, mode)))
	for len(data) > 0 {
		n := min(len(data), 64<<10)
		packet("DATA", data[:n])
		data = data[n:]
		if err := flush(); err != nil {
			return err
		}
	}
	buf = append(buf, "DONE"...)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(time.Now().Unix()))
	if err := flush(); err != nil {
		return err
	}
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	var h [8]byte
	if _, err := io.ReadFull(conn, h[:]); err != nil {
		return err
	}
	n := binary.LittleEndian.Uint32(h[4:])
	switch string(h[:4]) {
	case "OKAY":
		return nil
	case "FAIL":
		msg := make([]byte, min(n, 4096))
		io.ReadFull(conn, msg)
		return fmt.Errorf("adb push: %s", msg)
	}
	return fmt.Errorf("adb push: unexpected reply %q", h[:4])
}
