// Package mtp implements the host side of the Media Transfer Protocol
// (PTP/MTP over USB bulk pipes), including the Android extensions that
// allow reading and writing at arbitrary offsets inside an object.
package mtp

import (
	"encoding/binary"
	"errors"
	"fmt"
	"time"
	"unicode/utf16"
)

// Operation codes.
const (
	OpGetDeviceInfo      = 0x1001
	OpOpenSession        = 0x1002
	OpCloseSession       = 0x1003
	OpGetStorageIDs      = 0x1004
	OpGetStorageInfo     = 0x1005
	OpGetObjectHandles   = 0x1007
	OpGetObjectInfo      = 0x1008
	OpDeleteObject       = 0x100B
	OpSendObjectInfo     = 0x100C
	OpSendObject         = 0x100D
	OpMoveObject         = 0x1019
	OpGetPartialObject   = 0x101B
	OpGetObjectPropValue = 0x9803
	OpSetObjectPropValue = 0x9804

	// Android extensions ("android.com: 1.0").
	OpGetPartialObject64 = 0x95C1
	OpSendPartialObject  = 0x95C2
	OpTruncateObject     = 0x95C3
	OpBeginEditObject    = 0x95C4
	OpEndEditObject      = 0x95C5
)

// Response codes.
const (
	RespOK                   = 0x2001
	RespGeneralError         = 0x2002
	RespSessionNotOpen       = 0x2003
	RespOperationNotSupp     = 0x2005
	RespParameterNotSupp     = 0x2006
	RespInvalidStorageID     = 0x2008
	RespInvalidObjectHandle  = 0x2009
	RespStoreFull            = 0x200C
	RespObjectWriteProtected = 0x200D
	RespStoreReadOnly        = 0x200E
	RespAccessDenied         = 0x200F
	RespPartialDeletion      = 0x2012
	RespStoreNotAvailable    = 0x2013
	RespSpecByFormatUnsupp   = 0x2014
	RespDeviceBusy           = 0x2019
	RespInvalidParentObject  = 0x201A
	RespInvalidParameter     = 0x201D
	RespSessionAlreadyOpen   = 0x201E
	RespInvalidObjectPropVal = 0xA803
	RespObjectTooLarge       = 0xA809
)

// Event codes (interrupt endpoint).
const (
	EvObjectAdded       = 0x4002
	EvObjectRemoved     = 0x4003
	EvStoreAdded        = 0x4004
	EvStoreRemoved      = 0x4005
	EvObjectInfoChanged = 0x4007
	EvStorageInfoChange = 0x400C
)

const (
	FormatUndefined   = 0x3000
	FormatAssociation = 0x3001 // a folder

	PropObjectSize     = 0xDC04
	PropObjectFileName = 0xDC07

	// RootParent addresses the top level of a storage.
	RootParent = 0xFFFFFFFF

	containerCommand  = 1
	containerData     = 2
	containerResponse = 3
	containerEvent    = 4
	headerLen         = 12
)

// RespError is a failure reported by the device.
type RespError uint16

// Error implements error.
func (e RespError) Error() string { return fmt.Sprintf("mtp: device answered 0x%04X", uint16(e)) }

var errMalformed = errors.New("mtp: malformed data from device")

// ----------------------------------------------------------------- codec

type enc struct{ b []byte }

func (e *enc) u8(v uint8)   { e.b = append(e.b, v) }
func (e *enc) u16(v uint16) { e.b = binary.LittleEndian.AppendUint16(e.b, v) }
func (e *enc) u32(v uint32) { e.b = binary.LittleEndian.AppendUint32(e.b, v) }
func (e *enc) u64(v uint64) { e.b = binary.LittleEndian.AppendUint64(e.b, v) }

// str writes an MTP string: a character count (including the terminating
// NUL) followed by UTF-16LE code units. The empty string is a single zero.
func (e *enc) str(s string) {
	if s == "" {
		e.u8(0)
		return
	}
	u := utf16.Encode([]rune(s))
	if len(u) > 254 {
		u = u[:254]
	}
	e.u8(uint8(len(u) + 1))
	for _, c := range u {
		e.u16(c)
	}
	e.u16(0)
}

type dec struct {
	b   []byte
	err error
}

func (d *dec) take(n int) []byte {
	if d.err != nil || len(d.b) < n {
		d.err = errMalformed
		return nil
	}
	b := d.b[:n]
	d.b = d.b[n:]
	return b
}

func (d *dec) u8() uint8 {
	if b := d.take(1); b != nil {
		return b[0]
	}
	return 0
}
func (d *dec) u16() uint16 {
	if b := d.take(2); b != nil {
		return binary.LittleEndian.Uint16(b)
	}
	return 0
}
func (d *dec) u32() uint32 {
	if b := d.take(4); b != nil {
		return binary.LittleEndian.Uint32(b)
	}
	return 0
}
func (d *dec) u64() uint64 {
	if b := d.take(8); b != nil {
		return binary.LittleEndian.Uint64(b)
	}
	return 0
}

func (d *dec) str() string {
	n := int(d.u8())
	if n == 0 {
		return ""
	}
	b := d.take(n * 2)
	if b == nil {
		return ""
	}
	u := make([]uint16, 0, n)
	for i := 0; i < n; i++ {
		c := binary.LittleEndian.Uint16(b[i*2:])
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	return string(utf16.Decode(u))
}

func (d *dec) u16s() []uint16 {
	n := d.u32()
	if d.err != nil || int(n) > len(d.b)/2 {
		d.err = errMalformed
		return nil
	}
	out := make([]uint16, n)
	for i := range out {
		out[i] = d.u16()
	}
	return out
}

func (d *dec) u32s() []uint32 {
	n := d.u32()
	if d.err != nil || int(n) > len(d.b)/4 {
		d.err = errMalformed
		return nil
	}
	out := make([]uint32, n)
	for i := range out {
		out[i] = d.u32()
	}
	return out
}

// --------------------------------------------------------------- datasets

// DeviceInfo is the part of the MTP DeviceInfo dataset that macdroidfs uses.
type DeviceInfo struct {
	VendorExtension string
	Operations      []uint16
	Events          []uint16
	Manufacturer    string
	Model           string
	Version         string
	Serial          string
}

// Supports reports whether the device implements operation op.
func (di *DeviceInfo) Supports(op uint16) bool {
	for _, o := range di.Operations {
		if o == op {
			return true
		}
	}
	return false
}

func parseDeviceInfo(b []byte) (DeviceInfo, error) {
	d := dec{b: b}
	var di DeviceInfo
	d.u16() // standard version
	d.u32() // vendor extension id
	d.u16() // vendor extension version
	di.VendorExtension = d.str()
	d.u16() // functional mode
	di.Operations = d.u16s()
	di.Events = d.u16s()
	d.u16s() // device properties
	d.u16s() // capture formats
	d.u16s() // playback formats
	di.Manufacturer = d.str()
	di.Model = d.str()
	di.Version = d.str()
	di.Serial = d.str()
	return di, d.err
}

// StorageInfo is the part of the MTP StorageInfo dataset that macdroidfs uses.
type StorageInfo struct {
	ID          uint32
	Access      uint16 // 0 = read-write
	Capacity    uint64
	Free        uint64
	Description string
	Label       string
}

func parseStorageInfo(b []byte) (StorageInfo, error) {
	d := dec{b: b}
	var si StorageInfo
	d.u16() // storage type
	d.u16() // file system type
	si.Access = d.u16()
	si.Capacity = d.u64()
	si.Free = d.u64()
	d.u32() // free space in objects
	si.Description = d.str()
	si.Label = d.str()
	return si, d.err
}

// ObjectInfo is the part of the MTP ObjectInfo dataset that macdroidfs uses.
type ObjectInfo struct {
	Storage  uint32
	Format   uint16
	Size     uint32 // 0xFFFFFFFF means "4 GiB or more"
	Parent   uint32
	Name     string
	Modified time.Time
}

// IsDir reports whether the object is a folder.
func (oi *ObjectInfo) IsDir() bool { return oi.Format == FormatAssociation }

func parseObjectInfo(b []byte) (ObjectInfo, error) {
	d := dec{b: b}
	var oi ObjectInfo
	oi.Storage = d.u32()
	oi.Format = d.u16()
	d.u16() // protection status
	oi.Size = d.u32()
	d.u16() // thumb format
	d.u32() // thumb size
	d.u32() // thumb width
	d.u32() // thumb height
	d.u32() // image width
	d.u32() // image height
	d.u32() // image bit depth
	oi.Parent = d.u32()
	d.u16() // association type
	d.u32() // association description
	d.u32() // sequence number
	oi.Name = d.str()
	d.str() // capture date
	oi.Modified = parseDate(d.str())
	return oi, d.err
}

func (oi *ObjectInfo) encode() []byte {
	var e enc
	e.u32(oi.Storage)
	e.u16(oi.Format)
	e.u16(0)
	e.u32(oi.Size)
	e.u16(0)
	e.u32(0)
	e.u32(0)
	e.u32(0)
	e.u32(0)
	e.u32(0)
	e.u32(0)
	e.u32(oi.Parent)
	if oi.Format == FormatAssociation {
		e.u16(1) // generic folder
	} else {
		e.u16(0)
	}
	e.u32(0)
	e.u32(0)
	e.str(oi.Name)
	e.str("")
	e.str(formatDate(oi.Modified))
	e.str("")
	return e.b
}

// parseDate reads "YYYYMMDDThhmmss" with an optional fraction and an
// optional "Z" or "+hhmm" suffix. Android sends device-local time without a
// suffix; that is taken to be this machine's time zone.
func parseDate(s string) time.Time {
	if len(s) < 15 {
		return time.Time{}
	}
	base, rest := s[:15], s[15:]
	if len(rest) > 0 && rest[0] == '.' {
		i := 1
		for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
			i++
		}
		rest = rest[i:]
	}
	loc := time.Local
	if rest == "Z" {
		loc = time.UTC
	} else if len(rest) == 5 && (rest[0] == '+' || rest[0] == '-') {
		if t, err := time.Parse("20060102T150405-0700", base+rest); err == nil {
			return t
		}
	}
	t, err := time.ParseInLocation("20060102T150405", base, loc)
	if err != nil {
		return time.Time{}
	}
	return t
}

func formatDate(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("20060102T150405")
}

// FormatForName picks an object format from a file name. Old Android
// releases use it to decide how to index a new file.
func FormatForName(name string) uint16 {
	ext := ""
	for i := len(name) - 1; i >= 0 && name[i] != '/'; i-- {
		if name[i] == '.' {
			ext = name[i+1:]
			break
		}
	}
	b := []byte(ext)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	switch string(b) {
	case "jpg", "jpeg":
		return 0x3801
	case "png":
		return 0x380B
	case "gif":
		return 0x3807
	case "bmp":
		return 0x3804
	case "txt":
		return 0x3004
	case "html", "htm":
		return 0x3005
	case "mp3":
		return 0x3009
	case "wav":
		return 0x3008
	case "avi":
		return 0x300A
	case "mpg", "mpeg":
		return 0x300B
	case "mp4", "m4v":
		return 0xB982
	case "3gp":
		return 0xB984
	case "wma":
		return 0xB901
	case "ogg":
		return 0xB902
	case "aac":
		return 0xB903
	case "flac":
		return 0xB906
	case "wmv":
		return 0xB981
	}
	return FormatUndefined
}
