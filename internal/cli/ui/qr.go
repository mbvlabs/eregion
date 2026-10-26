package ui

import (
	"strings"

	"rsc.io/qr"
)

// qrQuiet is the quiet zone (in modules) required around a QR code.
const qrQuiet = 2

// QR renders text as a scannable QR code using half-block characters, so one
// terminal row carries two modules.
func QR(text string) (string, error) {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	lo, hi := -qrQuiet, code.Size+qrQuiet
	for y := lo; y < hi; y += 2 {
		b.WriteString("\x1b[30;107m")
		for x := lo; x < hi; x++ {
			top, bottom := code.Black(x, y), y+1 < hi && code.Black(x, y+1)
			switch {
			case top && bottom:
				b.WriteString("█")
			case top:
				b.WriteString("▀")
			case bottom:
				b.WriteString("▄")
			default:
				b.WriteString(" ")
			}
		}
		b.WriteString("\x1b[0m\n")
	}
	return b.String(), nil
}
