package license

import (
	"crypto/sha256"
	"fmt"
	"os"
	"runtime"
	"strings"
)

// DeviceCodeLen is how many Crockford characters a device code renders as.
// Eight characters is exactly the 40 bits a payload carries, so a device code is
// copied into a key without lossy conversion in either direction.
const DeviceCodeLen = 8

// deviceDomain keeps device-code hashing from colliding with anything else that
// might hash the same inputs in a future revision.
const deviceDomain = "tdl-device-v1\x00"

// DeviceCode hashes arbitrary identification material into the 40-bit code that
// a licence binds to.
//
// Inputs are whatever the platform can supply stably: on Android the ANDROID_ID
// plus the build fingerprint, on a desktop the machine-id plus the hostname.
// Hashing rather than embedding them means the key never carries a value that
// identifies the machine elsewhere, while still binding to it.
func DeviceCode(parts ...string) uint64 {
	h := sha256.New()
	h.Write([]byte(deviceDomain))
	for _, p := range parts {
		// Length-prefix each field so ("ab","c") and ("a","bc") do not collide.
		fmt.Fprintf(h, "%d:", len(p))
		h.Write([]byte(p))
	}
	sum := h.Sum(nil)
	// Take the low 40 bits of the first five bytes.
	var v uint64
	for i := 0; i < 5; i++ {
		v = v<<8 | uint64(sum[i])
	}
	return v
}

// FormatDeviceCode renders a code in the same 8-character form the customer
// reads off the activation screen and sends to the issuer.
func FormatDeviceCode(code uint64) string {
	return encode36(code&maxDevice, DeviceCodeLen)
}

// ParseDeviceCode accepts what a human typed, in any case and with or without
// separators, and returns the code.
func ParseDeviceCode(s string) (uint64, error) {
	body := stripSeparators(s)
	if len(body) != DeviceCodeLen {
		return 0, fmt.Errorf("%w：设备码应为 %d 个字符", ErrMalformed, DeviceCodeLen)
	}
	v, ok := decode32(body, DeviceCodeLen, 40)
	if !ok {
		return 0, ErrMalformed
	}
	return v, nil
}

// LocalDeviceCode derives this machine's code from whatever the platform
// exposes.
//
// The Android app supplies its own value through the bridge (ANDROID_ID and the
// build fingerprint), because a Go process inside an app sandbox cannot read
// them. This function is the desktop path.
func LocalDeviceCode() (uint64, []string, error) {
	parts, err := desktopIdentity()
	if err != nil {
		return 0, nil, err
	}
	return DeviceCode(parts...), parts, nil
}

// desktopIdentity collects stable-ish machine identifiers.
func desktopIdentity() ([]string, error) {
	var parts []string

	// machine-id is the canonical Linux stable identifier: it survives reboots,
	// kernel updates and hostname changes, which is exactly the lifetime a
	// licence needs.
	for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if b, err := os.ReadFile(p); err == nil {
			if s := strings.TrimSpace(string(b)); s != "" {
				parts = append(parts, "machine-id:"+s)
				break
			}
		}
	}

	host, _ := os.Hostname()
	if host == "" {
		host = "unknown-host"
	}
	parts = append(parts, "host:"+host)
	parts = append(parts, "os:"+runtime.GOOS+"/"+runtime.GOARCH)

	if len(parts) == 2 {
		// Only the fallbacks are present. That is still a usable code, but it is
		// weaker: a hostname is not unique. Reporting the inputs lets the issuer
		// see what the code was built from.
		return parts, nil
	}
	return parts, nil
}
