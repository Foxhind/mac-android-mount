# droidfs

Mounts Android device storage as ordinary macOS folders, read-write, with no
kext or macFUSE. When the connection drops, operations fail with an error
instead of hanging; when the device comes back, the volume recovers on its own.

Two transports:

- **ADB** (USB or Wi-Fi) is the primary one: faster and more reliable, but the
  device must have "USB debugging" enabled.
- **MTP** is the fallback: selecting "File transfer" on the device is enough.

In `auto` mode (the default) a device is mounted over ADB if ADB can see it,
otherwise over MTP.

## Running

MTP needs libusb (`brew install libusb pkg-config`); without it you can build
an ADB-only variant with `make build-nomtp`.

```bash
make build
```

```bash
./bin/droidfs
```

The daemon waits for devices and mounts each one at `~/DroidFS/<model>`.
`Ctrl-C` unmounts cleanly and exits.

| Flag | Default | Meaning |
|---|---|---|
| `-mode auto\|adb\|mtp` | `auto` | which transport to use |
| `-s <serial>` | all devices | mount only one device |
| `-root <path>` | `/sdcard` | which device directory to expose (ADB only) |
| `-dir <path>` | `~/DroidFS` | where to create mount points |
| `-grace <duration>` | `10s` | how long to keep the volume after the connection is lost |
| `-ro` | off | read-only |
| `-keep-mac-metadata` | off | write `._*` and `.DS_Store` to the device |
| `-keep-ptpcamerad` | off | MTP: do not kill the system `ptpcamerad`, even if it holds the device |
| `-open` | off | open the volume in Finder after mounting |
| `-trace` | off | log every NFS request |
| `-control <socket>` | none | serve status and commands on a Unix socket (see below) |
| `-stdin-lifeline` | off | shut down cleanly when stdin closes |

`droidfs devices` lists the devices seen over adb and MTP. `droidfs unmount`
force-unmounts all droidfs volumes (cleanup after a crash).

## The DroidFS app

`make app` (needs Xcode) builds `bin/DroidFS.app`, a SwiftUI app around the
same daemon:

- a window with the devices, their state and volume actions (open in Finder,
  eject, mount again), and the daemon's log;
- a menu bar icon with the same devices and actions;
- settings: launch at login, show the menu bar icon, show the Dock icon. The
  Dock icon can be hidden only while the menu bar icon is shown; with it
  hidden, the app starts in the menu bar without opening its window.

The app runs the daemon from its bundle (as `droidfsd`) with `-control` and
`-stdin-lifeline`, so the daemon exits with the app, even if the app crashes.
Quitting the app unmounts every volume. Only one daemon runs at a time: stop a
`droidfs` started from the terminal before opening the app.

The bundle is signed ad hoc, for the Mac it was built on.

### Control API

With `-control <socket>` the daemon answers HTTP/JSON on that Unix socket
(mode 0600, so only its user can reach it):

| Request | Answer |
|---|---|
| `GET /v1/devices` | every device seen since start: `id`, `transport`, `model`, `serial`, `point`, `state` (`mounted`, `offline`, `connecting`, `ejected`, `disconnected`), `error` |
| `POST /v1/devices/{id}/eject` | 204; unmounts the volume and keeps it unmounted until `mount` or a re-plug |
| `POST /v1/devices/{id}/mount` | 204; mounts an ejected volume again |

Both commands return before the work is done; poll `GET /v1/devices` for the
outcome. An unknown `id` gives 404.

## How it works

```
Finder / any application
        │  ordinary file system calls
macOS kernel: built-in NFS client (mount_nfs, soft)
        │  NFSv3 over TCP, 127.0.0.1
droidfs: NFS server → VFS (handles, caches) → sessions with watchdog
        │  adb exec: (binary multiplexed protocol)
droidfs-agent on the device: pread/pwrite/stat/rename/…
```

The diagram above is for ADB. In MTP mode the bottom two layers are replaced by
`MTP backend → libusb → Android MTP server`; everything else is shared.

- **Agent.** A static Go binary (arm, arm64, x86, x86_64) is embedded in
  `droidfs`; on first connection it is pushed to `/data/local/tmp` and started
  via `adb exec:`. It provides true random file access (writes at an offset,
  truncate, fsync) and exact error codes, with no parsing of shell output.
- **Stateless protocol.** Every operation is addressed by path. File handles
  live on the Mac side, so after a reconnect a new agent keeps serving the same
  volume.
- **macOS metadata** (`._*`, `.DS_Store`) is kept in the daemon's memory and
  never reaches the device.

## MTP mode

- Devices are found by polling USB once a second. Only devices whose interface
  is named "MTP" (as Android does) are used; cameras and iPhones are left
  alone.
- Writes at an offset go through Android's MTP extension (begin edit / send
  partial object / truncate / end edit). A device without it is mounted
  read-only.
- An edit is finished on `fsync`/file close or after 3 s of inactivity; at that
  point Android re-indexes the file and it shows up in the gallery.
- If the device has several storages (internal memory + SD card), they appear
  as separate folders at the root of the volume.
- A locked phone does not expose its storages: the daemon logs this and keeps
  retrying until the screen is unlocked.
- macOS itself claims PTP interfaces with the `ptpcamerad` process. If it holds
  the device, droidfs kills it (`killall ptpcamerad`; launchd restarts it on
  demand). Disable this with the `-keep-ptpcamerad` flag.

Where MTP is worse than ADB:

- one operation at a time: a large copy delays folder browsing;
- the first time a folder is opened costs one request per object, so thousands
  of photos take tens of seconds to list;
- replacing a file by rename is not atomic (delete + rename);
- file modification time cannot be set, and there are no symbolic links;
- moving between folders requires MoveObject (not available on every Android
  version); otherwise Finder copies and deletes.

## Disconnects and hangs

| What happened | What happens next |
|---|---|
| Cable unplugged | adb closes the stream and all operations get `EIO` immediately. The volume is kept for `-grace`, then force-unmounted. |
| Device came back within the grace period | It reconnects and the same volume keeps working. |
| Came back later | The volume is mounted again. |
| Device is "silent" (hung, link still up) | ADB: ping every ~1.5 s; not a single byte for 6 s means the link is considered dead. MTP: an operation gets `EIO` on its own timeout, and after 25 s of silence the session is considered dead. Then as for a disconnect. |
| One operation is stuck, link is alive | Operation timeout (12 s metadata, 20 s data, 25 s fsync) → `EIO` for that operation only. Four in a row and the agent is restarted. |
| A huge directory takes long to read | The client is told "try again later" while the listing finishes in the background. |
| The daemon itself crashed | The volume is mounted `soft` with `deadtimeout`: calls time out, and the next daemon start removes the orphaned volume. |
| The user ejected the volume in Finder | It is not remounted until the device is reconnected. |

An interrupted copy leaves a partially written file on the device, as with any
other copying method.

## Limitations

- In ADB mode on Android 10 and later, written files do not appear in
  MediaStore (the gallery) until a rescan.
- Permissions and owners are not carried over: everything is shown as owned by
  the current user, and the device performs the real access checks.
- The NFS server listens only on `127.0.0.1`; the root handle is given only to
  the user who started the daemon, but this is not full protection against
  other local users.
- The app is signed ad hoc and not notarized, so it runs on the Mac that built
  it. Its interface is in English only; the strings go through a String
  Catalog (`gui/Resources/Localizable.xcstrings`), so translations can be
  added.

## Tests

```bash
make test
```

End-to-end tests set up a real macOS kernel NFS mount and run the same set of
scenarios through both transports:

- ADB: the device is replaced by a locally running agent (same code, no adb);
  in `TestDaemon` the built `droidfs` binary works through a fake adb server.
- MTP: the device is replaced by an emulator (`internal/mtp/mtpsim`) that
  speaks the protocol at the USB packet level.

The emulator was written from the specification and Android sources, not
captured from a real phone: the tests do not prove it matches real devices.
