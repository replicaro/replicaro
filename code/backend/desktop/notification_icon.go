package desktop

import (
	"bytes"
	_ "embed"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"sync"

	"github.com/local/replicaro/appdata"
)

var (
	notificationSuccessColor = color.NRGBA{R: 0x1a, G: 0x8a, B: 0x9e, A: 0xff}
	notificationWarningColor = color.NRGBA{R: 0xf2, G: 0xc0, B: 0x63, A: 0xff}
	notificationFailureColor = color.NRGBA{R: 0xf0, G: 0x77, B: 0x6f, A: 0xff}
)

func tintedNotificationPNG(source []byte, status string) ([]byte, error) {
	decoded, err := png.Decode(bytes.NewReader(source))
	if err != nil {
		return nil, err
	}
	tint := notificationFailureColor
	if status == "success" {
		tint = notificationSuccessColor
	}
	if status == "completed_with_issues" {
		tint = notificationWarningColor
	}
	bounds := decoded.Bounds()
	tinted := image.NewNRGBA(bounds)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			_, _, _, alpha := decoded.At(x, y).RGBA()
			tinted.SetNRGBA(x, y, color.NRGBA{R: tint.R, G: tint.G, B: tint.B, A: uint8(alpha >> 8)})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, tinted); err != nil {
		return nil, err
	}
	return encoded.Bytes(), nil
}

func notificationStatusFromSuccess(successful bool) string {
	if successful {
		return "success"
	}
	return "failed"
}

//go:embed assets/replicaro-favicon.png
var notificationSourceIcon []byte
var notificationIconMu sync.Mutex

func notificationIconPath(status string) (string, error) {
	// Concurrent desktop deliveries share these three immutable presentation
	// files. Serialize publication so no caller observes another partial write.
	notificationIconMu.Lock()
	defer notificationIconMu.Unlock()
	filename := "replicaro-notification-failure.png"
	if status == "success" {
		filename = "replicaro-notification-success.png"
	}
	if status == "completed_with_issues" {
		filename = "replicaro-notification-warning.png"
	}
	path, err := appdata.File(filename)
	if err != nil {
		return "", err
	}
	icon, err := tintedNotificationPNG(notificationSourceIcon, status)
	if err != nil {
		return "", fmt.Errorf("tint notification icon: %w", err)
	}

	current, err := os.ReadFile(path)
	if err == nil && bytes.Equal(current, icon) {
		return path, nil
	}
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("read notification icon: %w", err)
	}
	if err := os.WriteFile(path, icon, 0o600); err != nil {
		return "", fmt.Errorf("write notification icon: %w", err)
	}
	return path, nil
}
