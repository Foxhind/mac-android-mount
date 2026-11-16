// droidfs mounts the storage of Android devices as ordinary folders on
// macOS, over adb, and keeps the Mac responsive when a device goes away.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"droidfs/internal/manager"
)

func usage() {
	fmt.Fprintf(os.Stderr, `droidfs - mount Android devices as folders (over adb)

Usage:
  droidfs [flags]      mount every connected device and keep watching
  droidfs devices      list the devices adb can see
  droidfs unmount      force-unmount all droidfs volumes (cleanup)

Flags:
`)
	flag.PrintDefaults()
}

func main() {
	home, _ := os.UserHomeDir()
	var cfg manager.Config
	flag.StringVar(&cfg.Mode, "mode", "auto", "transport: adb, mtp, or auto (adb when the device offers it, else mtp)")
	flag.StringVar(&cfg.Serial, "s", "", "only mount the device with this serial")
	flag.StringVar(&cfg.Root, "root", "/sdcard", "directory on the device to mount (adb only)")
	flag.StringVar(&cfg.MountDir, "dir", filepath.Join(home, "DroidFS"), "where mount points are created")
	flag.DurationVar(&cfg.Grace, "grace", 10*time.Second, "keep a mount this long after its device disconnects")
	flag.BoolVar(&cfg.ReadOnly, "ro", false, "mount read-only")
	flag.BoolVar(&cfg.KeepMacMetadata, "keep-mac-metadata", false, "store ._* and .DS_Store files on the device instead of in memory")
	flag.BoolVar(&cfg.KeepPTPCamerad, "keep-ptpcamerad", false, "MTP: do not stop macOS's camera helper when it holds the device")
	flag.BoolVar(&cfg.Open, "open", false, "open each volume in Finder when it is mounted")
	flag.BoolVar(&cfg.Trace, "trace", false, "log every NFS request")
	flag.Usage = usage
	flag.Parse()
	cfg.Log = log.New(os.Stderr, "", log.Ltime)
	if cfg.Mode != "auto" && cfg.Mode != "adb" && cfg.Mode != "mtp" {
		cfg.Log.Fatalf("unknown -mode %q (want auto, adb or mtp)", cfg.Mode)
	}

	switch flag.Arg(0) {
	case "":
	case "devices":
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		devs, err := manager.Devices(ctx)
		if err != nil {
			cfg.Log.Printf("adb: %v", err)
		}
		for _, d := range devs {
			fmt.Printf("adb  %-24s %-14s %s\n", d.Serial, d.State, d.Model)
		}
		mtps := manager.MTPDevices()
		for _, d := range mtps {
			fmt.Printf("mtp  %-24s %-14s %s\n", d.Serial, "file transfer", d.Product)
		}
		if len(devs)+len(mtps) == 0 {
			fmt.Println("no devices")
		}
		return
	case "unmount":
		if err := manager.UnmountAll(cfg.Log); err != nil {
			cfg.Log.Fatal(err)
		}
		return
	default:
		usage()
		os.Exit(2)
	}

	// One daemon at a time: a second one would treat the first one's
	// mounts as stale and tear them down.
	unlock, err := singleInstance()
	if err != nil {
		cfg.Log.Fatal(err)
	}
	defer unlock()

	ctx, cancel := context.WithCancel(context.Background())
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-sig
		cfg.Log.Printf("shutting down (signal again to exit immediately)")
		cancel()
		<-sig
		os.Exit(1)
	}()

	if err := manager.New(cfg).Run(ctx); err != nil {
		cfg.Log.Fatal(err)
	}
}

func singleInstance() (func(), error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		dir = os.TempDir()
	}
	os.MkdirAll(dir, 0o700)
	path := filepath.Join(dir, "droidfs.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("droidfs is already running (lock %s)", path)
	}
	return func() { f.Close() }, nil
}
