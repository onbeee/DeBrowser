//go:build !windows

package browserprobe

import (
	"fmt"
	"os"
)

// File verification is a Windows feature (DeBrowser's only target
// platform). On other platforms — used only for development and
// unit tests — the inspector can still stat files, and version and
// signature reads report themselves unavailable so the
// platform-neutral verification logic behaves predictably. There
// is deliberately no browser discovery on any platform.

// NewInspector returns the non-Windows inspector.
func NewInspector() Inspector { return otherInspector{} }

type otherInspector struct{}

func (otherInspector) Exists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}

func (otherInspector) VersionInfo(string) (VersionInfo, error) {
	return VersionInfo{}, fmt.Errorf("version information is only available on Windows")
}

func (otherInspector) Signature(string) (Signature, error) {
	return Signature{Status: "unavailable"}, nil
}
