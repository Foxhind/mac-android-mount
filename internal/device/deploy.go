package device

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/Foxhind/macdroidfs/internal/adb"
	"github.com/Foxhind/macdroidfs/internal/agentbin"
	"github.com/Foxhind/macdroidfs/internal/proto"
)

const remoteDir = "/data/local/tmp"

// ADBDialer starts the agent on a device through adb, installing it first
// when it is missing or outdated.
type ADBDialer struct {
	ADB    *adb.Client
	Serial string

	mu     sync.Mutex
	remote string
	bin    []byte
}

func abiToArch(abi string) string {
	switch {
	case strings.HasPrefix(abi, "arm64"):
		return "arm64"
	case strings.HasPrefix(abi, "armeabi"):
		return "arm"
	case abi == "x86_64":
		return "amd64"
	case abi == "x86":
		return "386"
	}
	return ""
}

func (d *ADBDialer) resolve(ctx context.Context) (string, []byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.remote != "" {
		return d.remote, d.bin, nil
	}
	out, err := d.ADB.Exec(ctx, d.Serial, "getprop ro.product.cpu.abi", 10*time.Second)
	if err != nil {
		return "", nil, fmt.Errorf("query device ABI: %w", err)
	}
	abi := strings.TrimSpace(out)
	arch := abiToArch(abi)
	if arch == "" {
		return "", nil, fmt.Errorf("unsupported device ABI %q", abi)
	}
	bin, err := agentbin.Get(arch)
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(bin)
	d.remote = remoteDir + "/macdroidfs-agent-" + hex.EncodeToString(sum[:6])
	d.bin = bin
	return d.remote, d.bin, nil
}

// launch starts the agent and verifies its banner.
func (d *ADBDialer) launch(ctx context.Context, remote string) (net.Conn, error) {
	conn, err := d.ADB.Open(ctx, d.Serial, "exec:"+remote)
	if err != nil {
		return nil, err
	}
	conn.SetReadDeadline(time.Now().Add(8 * time.Second))
	buf := make([]byte, len(proto.Banner))
	n, err := io.ReadFull(conn, buf)
	if err != nil || string(buf) != proto.Banner {
		// Usually a shell error such as "not found"; grab the rest of it.
		rest := make([]byte, 256)
		conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		m, _ := conn.Read(rest)
		conn.Close()
		msg := strings.TrimSpace(string(buf[:n]) + string(rest[:m]))
		if msg == "" && err != nil {
			msg = err.Error()
		}
		return nil, fmt.Errorf("agent did not start: %s", msg)
	}
	conn.SetReadDeadline(time.Time{})
	return conn, nil
}

// Dial starts the agent on the device, reinstalling it if it is missing or
// stale, and returns its stream with the banner already consumed.
func (d *ADBDialer) Dial(ctx context.Context) (io.ReadWriteCloser, error) {
	remote, bin, err := d.resolve(ctx)
	if err != nil {
		return nil, err
	}
	conn, err := d.launch(ctx, remote)
	if err == nil {
		return conn, nil
	}
	if ctx.Err() != nil {
		return nil, err
	}
	// Missing or stale: (re)install and try once more.
	d.ADB.Exec(ctx, d.Serial, "rm -f "+remoteDir+"/macdroidfs-agent-*", 10*time.Second)
	if perr := d.ADB.Push(ctx, d.Serial, remote, 0o100755, bin); perr != nil {
		return nil, fmt.Errorf("install agent: %w (first attempt: %w)", perr, err)
	}
	return d.launch(ctx, remote)
}
