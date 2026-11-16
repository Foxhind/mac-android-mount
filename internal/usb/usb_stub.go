//go:build !(darwin && cgo && !nomtp)

// Package usb: stub used when the program is built without libusb (no cgo,
// or the nomtp build tag). MTP mode is then unavailable.
package usb

import (
	"errors"
	"time"
)

const Available = false

var errUnavailable = errors.New("usb: built without libusb support")

type Error int

const (
	ErrIO       Error = -1
	ErrAccess   Error = -3
	ErrNoDevice Error = -4
	ErrNotFound Error = -5
	ErrBusy     Error = -6
	ErrTimeout  Error = -7
	ErrOverflow Error = -8
	ErrPipe     Error = -9
)

func (e Error) Error() string { return "usb error" }

type Interface struct {
	Num, Class, SubClass, Protocol uint8
	NameIdx                        uint8
	In, Out, Intr                  uint8
	InMax, OutMax                  int
	Endpoints                      int
}

type Info struct {
	Bus, Addr uint8
	VID, PID  uint16
	Ifaces    []Interface
}

type Device struct{ Info }

func List() ([]Info, error)                 { return nil, nil }
func Open(bus, addr uint8) (*Device, error) { return nil, errUnavailable }

func (d *Device) String(uint8) string  { return "" }
func (d *Device) Manufacturer() string { return "" }
func (d *Device) Product() string      { return "" }
func (d *Device) Serial() string       { return "" }
func (d *Device) Claim(uint8) error    { return errUnavailable }
func (d *Device) Bulk(uint8, []byte, time.Duration) (int, error) {
	return 0, errUnavailable
}
func (d *Device) Interrupt(uint8, []byte, time.Duration) (int, error) {
	return 0, errUnavailable
}
func (d *Device) ClearHalt(uint8) error { return errUnavailable }
func (d *Device) Reset() error          { return errUnavailable }
func (d *Device) Close()                {}
