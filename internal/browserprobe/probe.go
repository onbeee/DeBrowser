// Package browserprobe detects browsers that are already installed
// on the machine (Stage 9A) and verifies their identity.
//
// Detection is deliberately conservative:
//   - Candidates come only from regular system information — the
//     Windows registry (App Paths and the uninstall keys) and the
//     well-known per-browser install directories. The disk is never
//     scanned and no detected executable is ever launched.
//   - A candidate becomes a browser record only after verification:
//     the file must exist, its version resource must identify the
//     product, and its Authenticode signature must be valid AND
//     issued to the expected publisher. A file merely NAMED
//     chrome.exe proves nothing.
//   - Detection results are runtime data. They are never written
//     to the database and DeBrowser never modifies, copies or
//     deletes the detected installations or their profiles.
//
// The detection pipeline is platform-neutral: candidate sources and
// the file inspector are interfaces, so the full decision logic is
// unit-tested with fakes on any OS. The Windows implementations
// live in probe_windows.go; other platforms report no candidates.
package browserprobe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sort"
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

// Scope is where a browser installation lives.
type Scope string

const (
	ScopeUser    Scope = "user"    // current-user installation
	ScopeSystem  Scope = "system"  // machine-wide installation
	ScopeManaged Scope = "managed" // installed by DeBrowser itself
)

// Signature statuses reported for a candidate file.
const (
	SignatureVerified          = "verified"           // valid signature, expected publisher
	SignaturePublisherMismatch = "publisher_mismatch" // valid signature, wrong publisher
	SignatureInvalid           = "invalid"            // signature present but invalid
	SignatureUnsigned          = "unsigned"           // no signature
	SignatureUnavailable       = "unavailable"        // platform cannot verify
)

// Detection statuses for a record.
const (
	DetectionVerified           = "verified"            // identity and signature confirmed
	DetectionVerificationFailed = "verification_failed" // identity or signature rejected
	DetectionUnverifiable       = "unverifiable"        // platform cannot perform verification
)

// Candidate is one possible executable location from a source. The
// KindHint records which browser the SOURCE expected; the verified
// kind is always decided from the file itself, never from the hint.
type Candidate struct {
	Path     string
	Scope    Scope
	KindHint Kind
}

// CandidateSource yields candidate executable paths from one
// information source (registry App Paths, uninstall records,
// well-known directories).
type CandidateSource interface {
	Candidates(ctx context.Context) ([]Candidate, error)
}

// VersionInfo is the identity data read from a file's version
// resource.
type VersionInfo struct {
	ProductName string
	FileVersion string
}

// Signature is the Authenticode verdict for one file.
type Signature struct {
	// Status is one of: "valid", "invalid", "unsigned",
	// "unavailable" (raw platform verdicts; Detect maps them onto
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

// Info is one detection record.
type Info struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Kind            Kind   `json:"kind"`
	Version         string `json:"version"`
	ExecutablePath  string `json:"executablePath"`
	Source          string `json:"source"` // "system" | "managed"
	Scope           Scope  `json:"scope"`
	SignatureStatus string `json:"signatureStatus"`
	Publisher       string `json:"publisher"`
	DetectionStatus string `json:"detectionStatus"`
	StatusReason    string `json:"statusReason"`
	DetectedAt      string `json:"detectedAt"`
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

// canonical normalizes a candidate path for deduplication: cleaned,
// absolute, and compared case-insensitively (Windows paths).
func canonical(path string) (clean, key string) {
	clean = filepath.Clean(path)
	if abs, err := filepath.Abs(clean); err == nil {
		clean = abs
	}
	return clean, strings.ToLower(clean)
}

// SystemID derives the stable runtime ID of a system browser from
// its canonical path. System browsers have no database row; this
// ID exists only so the UI can address one detection record.
func SystemID(canonicalPath string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(canonicalPath)))
	return "sys-" + hex.EncodeToString(sum[:])[:16]
}

// Detect runs every source, deduplicates candidates and verifies
// each surviving executable. Candidates whose file does not exist
// are dropped silently (stale registry entries are normal). Source
// errors are tolerated: one broken source must not hide the
// browsers another source found.
func Detect(ctx context.Context, sources []CandidateSource, insp Inspector) []Info {
	seen := map[string]bool{}
	var cands []Candidate
	for _, src := range sources {
		list, err := src.Candidates(ctx)
		if err != nil {
			continue
		}
		for _, c := range list {
			clean, key := canonical(c.Path)
			if seen[key] {
				continue
			}
			seen[key] = true
			c.Path = clean
			cands = append(cands, c)
		}
	}
	out := []Info{}
	for _, c := range cands {
		if !insp.Exists(c.Path) {
			continue
		}
		out = append(out, Verify(c.Path, c.Scope, insp))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].ExecutablePath < out[j].ExecutablePath
	})
	return out
}

// Verify verifies one executable and builds its record. It is used
// for automatic candidates and for a manually chosen file alike:
// manual selection never bypasses verification.
func Verify(path string, scope Scope, insp Inspector) Info {
	clean, _ := canonical(path)
	info := Info{
		ID:             SystemID(clean),
		ExecutablePath: clean,
		Source:         "system",
		Scope:          scope,
		DetectedAt:     time.Now().UTC().Format(time.RFC3339),
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
