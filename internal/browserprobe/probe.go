// Package browserprobe verifies the identity of ONE browser
// executable that the user has explicitly chosen (Stage 9A,
// revised product decision).
//
// DeBrowser does NOT detect browsers automatically: there is no
// registry scanning, no disk scanning, no list of well-known
// install directories anywhere in this package or its callers.
// The only entry point is Verify, applied to a file the user
// picked by hand (or to a previously registered file being
// re-verified, or to a managed kernel's chrome.exe).
//
// Verification is conservative:
//   - The file must exist.
//   - Its version resource must identify the product (Google
//     Chrome, Brave or Chromium). A file merely NAMED chrome.exe
//     proves nothing.
//   - Its Authenticode signature must be valid AND issued to the
//     expected publisher (Google LLC / Brave Software, Inc.).
//   - The file is never executed.
package browserprobe

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// Kind is the verified browser family.
type Kind string

const (
	KindChrome   Kind = "chrome"
	KindBrave    Kind = "brave"
	KindChromium Kind = "chromium"
)

// Signature statuses reported for a verified file.
const (
	SignatureVerified          = "verified"           // valid signature, expected publisher
	SignaturePublisherMismatch = "publisher_mismatch" // valid signature, wrong publisher
	SignatureInvalid           = "invalid"            // signature present but invalid
	SignatureUnsigned          = "unsigned"           // no signature
	SignatureUnavailable       = "unavailable"        // platform cannot verify
)

// Detection statuses for a verification result.
const (
	DetectionVerified           = "verified"            // identity and signature confirmed
	DetectionVerificationFailed = "verification_failed" // identity or signature rejected
	DetectionUnverifiable       = "unverifiable"        // platform cannot perform verification
)

// VersionInfo is the identity data read from a file's version
// resource.
type VersionInfo struct {
	ProductName string
	FileVersion string
}

// Signature is the Authenticode verdict for one file.
type Signature struct {
	// Status is one of: "valid", "invalid", "unsigned",
	// "unavailable" (raw platform verdicts; Verify maps them onto
	// the Signature* constants using the expected publisher).
	Status    string
	Publisher string
}

// Inspector reads facts about one executable file. It never
// executes the file.
type Inspector interface {
	// Exists reports whether path is an existing regular file.
	Exists(path string) bool
	// VersionInfo reads the product name and file version.
	VersionInfo(path string) (VersionInfo, error)
	// Signature verifies the Authenticode signature and returns
	// the signing publisher.
	Signature(path string) (Signature, error)
}

// Info is the verification record for one executable.
type Info struct {
	Name            string `json:"name"`
	Kind            Kind   `json:"kind"`
	Version         string `json:"version"`
	ExecutablePath  string `json:"executablePath"`
	SignatureStatus string `json:"signatureStatus"`
	Publisher       string `json:"publisher"`
	DetectionStatus string `json:"detectionStatus"`
	StatusReason    string `json:"statusReason"`
	VerifiedAt      string `json:"verifiedAt"`
}

// ExpectedPublisher returns the Authenticode publisher a browser
// kind must be signed by to be trusted.
func ExpectedPublisher(kind Kind) string {
	switch kind {
	case KindChrome, KindChromium:
		return "Google LLC"
	case KindBrave:
		return "Brave Software, Inc."
	}
	return ""
}

// DisplayName returns the product display name for a kind.
func DisplayName(kind Kind) string {
	switch kind {
	case KindChrome:
		return "Google Chrome"
	case KindBrave:
		return "Brave"
	case KindChromium:
		return "Chromium"
	}
	return string(kind)
}

// kindFromProductName decides the browser family from the file's
// own product name — never from the file name.
func kindFromProductName(product string) (Kind, bool) {
	p := strings.ToLower(product)
	switch {
	case strings.Contains(p, "brave"):
		return KindBrave, true
	case strings.Contains(p, "google chrome"):
		return KindChrome, true
	case strings.Contains(p, "chromium"):
		return KindChromium, true
	}
	return "", false
}

// Verify verifies one user-chosen executable and builds its
// record. Manual selection never bypasses verification: the same
// checks apply to a hand-picked file as to any other candidate.
func Verify(path string, insp Inspector) Info {
	clean := filepath.Clean(path)
	if abs, err := filepath.Abs(clean); err == nil {
		clean = abs
	}
	info := Info{
		ExecutablePath: clean,
		VerifiedAt:     time.Now().UTC().Format(time.RFC3339),
	}
	vi, err := insp.VersionInfo(clean)
	if err != nil || vi.ProductName == "" {
		info.SignatureStatus = SignatureUnavailable
		info.DetectionStatus = DetectionVerificationFailed
		info.StatusReason = "无法读取版本信息，不能确认浏览器身份"
		return info
	}
	kind, ok := kindFromProductName(vi.ProductName)
	if !ok {
		info.Name = vi.ProductName
		info.Version = vi.FileVersion
		info.SignatureStatus = SignatureUnavailable
		info.DetectionStatus = DetectionVerificationFailed
		info.StatusReason = fmt.Sprintf("产品名称 %q 不是受支持的浏览器（仅支持 Google Chrome、Brave 和 Chromium）", vi.ProductName)
		return info
	}
	info.Kind = kind
	info.Name = DisplayName(kind)
	info.Version = vi.FileVersion

	sig, err := insp.Signature(clean)
	if err != nil {
		info.SignatureStatus = SignatureUnavailable
		info.DetectionStatus = DetectionUnverifiable
		info.StatusReason = "当前平台无法执行签名验证"
		return info
	}
	info.Publisher = sig.Publisher
	expected := ExpectedPublisher(kind)
	switch sig.Status {
	case "valid":
		if strings.EqualFold(strings.TrimSpace(sig.Publisher), expected) {
			info.SignatureStatus = SignatureVerified
			info.DetectionStatus = DetectionVerified
		} else {
			info.SignatureStatus = SignaturePublisherMismatch
			info.DetectionStatus = DetectionVerificationFailed
			info.StatusReason = fmt.Sprintf("签名发布者 %q 与预期 %q 不符", sig.Publisher, expected)
		}
	case "unsigned":
		info.SignatureStatus = SignatureUnsigned
		info.DetectionStatus = DetectionVerificationFailed
		info.StatusReason = "文件没有数字签名"
	case "invalid":
		info.SignatureStatus = SignatureInvalid
		info.DetectionStatus = DetectionVerificationFailed
		info.StatusReason = "数字签名无效"
	default: // "unavailable" or anything unexpected
		info.SignatureStatus = SignatureUnavailable
		info.DetectionStatus = DetectionUnverifiable
		info.StatusReason = "当前平台无法执行签名验证"
	}
	return info
}
