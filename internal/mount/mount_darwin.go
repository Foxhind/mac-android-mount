// Package mount attaches the loopback NFS server to the macOS file tree
// with the system's own NFS client (no kernel extension needed), and takes
// it away again. Every external command runs under a deadline, and the
// mount table is read without touching the mounts themselves, so a dead
// mount can never block us.
package mount

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// SourcePrefix marks our mounts in the mount table.
const SourcePrefix = "127.0.0.1:/droidfs/"

// Info is one entry of the system mount table.
type Info struct {
	Point, From, Type string
}

func cstr(b []int8) string {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if c == 0 {
			break
		}
		out = append(out, byte(c))
	}
	return string(out)
}

// List returns the mount table. MNT_NOWAIT makes the kernel answer from its
// cache instead of asking each file system, which would hang on a dead one.
func List() ([]Info, error) {
	const mntNoWait = 2
	n, err := syscall.Getfsstat(nil, mntNoWait)
	if err != nil {
		return nil, err
	}
	buf := make([]syscall.Statfs_t, n+8)
	n, err = syscall.Getfsstat(buf, mntNoWait)
	if err != nil {
		return nil, err
	}
	out := make([]Info, 0, n)
	for i := range buf[:n] {
		out = append(out, Info{
			Point: cstr(buf[i].Mntonname[:]),
			From:  cstr(buf[i].Mntfromname[:]),
			Type:  cstr(buf[i].Fstypename[:]),
		})
	}
	return out, nil
}

// Ours returns the mounts created by this program (by any instance).
func Ours() []Info {
	all, _ := List()
	var out []Info
	for _, m := range all {
		if m.Type == "nfs" && strings.HasPrefix(m.From, SourcePrefix) {
			out = append(out, m)
		}
	}
	return out
}

// IsMounted reports whether something is mounted exactly at point.
func IsMounted(point string) bool {
	all, _ := List()
	for _, m := range all {
		if m.Point == point {
			return true
		}
	}
	return false
}

// Options describe how Mount attaches a droidfs NFS server.
type Options struct {
	Port     int
	Name     string // appears in the mount source
	ReadOnly bool
}

// Mount mounts the server at point, creating the directory if needed.
//
// The options are what keeps a broken setup from hanging the Mac:
//
//	soft,intr    calls fail with an error instead of retrying forever
//	timeo/retrans bound how long an unanswered request is retried
//	deadtimeout  an unresponsive mount is forcibly unmounted by the kernel
//
// In normal operation none of them fire, because the daemon answers every
// request within its own (shorter) time bounds, device or no device.
func Mount(ctx context.Context, point string, o Options) error {
	// Check the mount table before touching the path: if a dead mount is
	// sitting there, even a stat of it would stall.
	if IsMounted(point) {
		return fmt.Errorf("%s is already a mount point", point)
	}
	if err := os.MkdirAll(point, 0o755); err != nil {
		return err
	}
	opts := []string{
		"vers=3", "tcp",
		fmt.Sprintf("port=%d", o.Port),
		fmt.Sprintf("mountport=%d", o.Port),
		"locallocks", // file locks are local; the device knows nothing of them
		"soft", "intr",
		"timeo=300", "retrans=2", "deadtimeout=45",
		// "Try again later" is our normal answer while a huge folder is
		// still being listed; it must not count as the server being dead.
		"mutejukebox",
		// Moderate I/O size: with ~16 requests in flight this keeps the
		// queue small enough that even slow storage answers each one well
		// inside the timeouts, at no measurable cost on fast devices.
		"rsize=262144", "wsize=262144", "dsize=65536",
		"rdirplus",
		"nfc", // Android stores names precomposed
		"acregmin=1", "acregmax=3", "acdirmin=1", "acdirmax=3",
		"nosuid", "nodev", "noatime",
	}
	if o.ReadOnly {
		opts = append(opts, "ro")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/sbin/mount_nfs", "-o", strings.Join(opts, ","), SourcePrefix+o.Name, point)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mount_nfs: %w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

func run(timeout time.Duration, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, bytes.TrimSpace(out))
	}
	return nil
}

// Unmount detaches point. With force false it first tries a clean unmount,
// which lets the kernel flush pending writes; it then escalates until the
// mount is gone. It never blocks for more than about half a minute.
func Unmount(point string, force bool) error {
	if !IsMounted(point) {
		return nil
	}
	var err error
	if !force {
		if err = run(10*time.Second, "/sbin/umount", point); err == nil || !IsMounted(point) {
			return nil
		}
	}
	if err = run(10*time.Second, "/sbin/umount", "-f", point); err == nil || !IsMounted(point) {
		return nil
	}
	if err = run(15*time.Second, "/usr/sbin/diskutil", "unmount", "force", point); err == nil || !IsMounted(point) {
		return nil
	}
	return err
}
