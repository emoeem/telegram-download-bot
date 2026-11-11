package cmd

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/iyear/tdl/license"
	"github.com/iyear/tdl/pkg/consts"
)

// buildLicenceGate assembles the activation gate for `archive serve`.
//
// It returns (nil, nil) when this build carries no verifier material, which is
// the normal state of a source build: a developer running the tree should not
// need the vendor's master key, and refusing to start would make the whole
// repository unusable. A release build compiles the verifier in, and from then
// on the gate is mandatory.
func buildLicenceGate(dir, deviceMaterial string, disabled bool) (*license.Manager, error) {
	if disabled {
		slog.Warn("licence gate disabled by --no-license; every feature is available without a key")
		return nil, nil
	}
	if !license.Injected() {
		slog.Warn("licence gate not compiled into this build; the archive is available without a key",
			slog.String("how_to_enable", "build with -ldflags from `tdl license verifier --key-file <master>`"))
		return nil, nil
	}
	ver, err := license.BuildVerifier()
	if err != nil {
		return nil, fmt.Errorf("授权校验材料无效: %w", err)
	}

	var device uint64
	switch {
	case strings.TrimSpace(deviceMaterial) != "":
		// Android supplies ANDROID_ID + build fingerprint; hashing happens here
		// so the platform-specific composition lives in one place.
		device = license.DeviceCode(deviceMaterial)
		slog.Info("licence device derived from supplied material",
			slog.String("code", license.FormatDeviceCode(device)))
	default:
		code, parts, derr := license.LocalDeviceCode()
		if derr != nil {
			return nil, fmt.Errorf("读取本机标识失败: %w", derr)
		}
		device = code
		slog.Info("licence device code",
			slog.String("code", license.FormatDeviceCode(code)),
			slog.String("from", strings.Join(parts, " + ")))
	}

	if strings.TrimSpace(dir) == "" {
		dir = filepath.Join(consts.DataDir, "license")
	}
	mgr := license.NewManager(dir, ver, device)
	st := mgr.Status(time.Now())
	slog.Info("licence gate enabled",
		slog.String("device", st.ThisDevice),
		slog.Bool("activated", st.Activated),
		slog.Bool("valid", st.Valid),
		slog.String("reason", st.Reason),
		slog.String("state_file", dir))
	return mgr, nil
}
