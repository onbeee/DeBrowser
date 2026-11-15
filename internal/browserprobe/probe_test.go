package browserprobe

import (
	"fmt"
	"path/filepath"
	"testing"
)

type fakeInspector struct {
	versions map[string]VersionInfo
	verErr   map[string]error
	sigs     map[string]Signature
}

func (f *fakeInspector) Exists(p string) bool {
	_, ok := f.versions[filepath.Clean(p)]
	return ok
}
func (f *fakeInspector) VersionInfo(p string) (VersionInfo, error) {
	if err := f.verErr[filepath.Clean(p)]; err != nil {
		return VersionInfo{}, err
	}
	v, ok := f.versions[filepath.Clean(p)]
	if !ok {
		return VersionInfo{}, fmt.Errorf("no version resource")
	}
	return v, nil
}
func (f *fakeInspector) Signature(p string) (Signature, error) {
	s, ok := f.sigs[filepath.Clean(p)]
	if !ok {
		return Signature{Status: "unavailable"}, nil
	}
	return s, nil
}

func abs(t *testing.T, rel string) string {
	t.Helper()
	a, err := filepath.Abs(rel)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(a)
}

func TestVerifyChromeAndBrave(t *testing.T) {
	chrome := abs(t, "apps/chrome.exe")
	brave := abs(t, "apps/brave.exe")
	insp := &fakeInspector{
		versions: map[string]VersionInfo{
			chrome: {ProductName: "Google Chrome", FileVersion: "142.0.7444.162"},
			brave:  {ProductName: "Brave Browser", FileVersion: "1.73.91"},
		},
		sigs: map[string]Signature{
			chrome: {Status: "valid", Publisher: "Google LLC"},
			brave:  {Status: "valid", Publisher: "Brave Software, Inc."},
		},
	}
	got := Verify(chrome, insp)
	if got.Kind != KindChrome || got.DetectionStatus != DetectionVerified || got.Version != "142.0.7444.162" {
		t.Fatalf("chrome verify = %+v", got)
	}
	got = Verify(brave, insp)
	if got.Kind != KindBrave || got.DetectionStatus != DetectionVerified {
		t.Fatalf("brave verify = %+v", got)
	}
}

func TestVerifyRejectsBadExecutables(t *testing.T) {
	cases := []struct {
		name       string
		version    VersionInfo
		verErr     error
		sig        Signature
		wantDetect string
		wantSig    string
	}{
		{"version read fails", VersionInfo{}, fmt.Errorf("no resource"), Signature{Status: "valid", Publisher: "Google LLC"}, DetectionVerificationFailed, SignatureUnavailable},
		{"unknown product", VersionInfo{ProductName: "Totally Not A Browser", FileVersion: "9"}, nil, Signature{Status: "valid", Publisher: "Google LLC"}, DetectionVerificationFailed, SignatureUnavailable},
		{"unsigned", VersionInfo{ProductName: "Google Chrome", FileVersion: "1"}, nil, Signature{Status: "unsigned"}, DetectionVerificationFailed, SignatureUnsigned},
		{"invalid signature", VersionInfo{ProductName: "Google Chrome", FileVersion: "1"}, nil, Signature{Status: "invalid"}, DetectionVerificationFailed, SignatureInvalid},
		{"publisher mismatch", VersionInfo{ProductName: "Google Chrome", FileVersion: "1"}, nil, Signature{Status: "valid", Publisher: "Evil Corp"}, DetectionVerificationFailed, SignaturePublisherMismatch},
		{"brave signed by google is a mismatch", VersionInfo{ProductName: "Brave Browser", FileVersion: "1"}, nil, Signature{Status: "valid", Publisher: "Google LLC"}, DetectionVerificationFailed, SignaturePublisherMismatch},
		{"signature unavailable", VersionInfo{ProductName: "Google Chrome", FileVersion: "1"}, nil, Signature{Status: "unavailable"}, DetectionUnverifiable, SignatureUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := abs(t, "x/chrome.exe")
			insp := &fakeInspector{
				versions: map[string]VersionInfo{p: tc.version},
				verErr:   map[string]error{},
				sigs:     map[string]Signature{p: tc.sig},
			}
			if tc.verErr != nil {
				insp.verErr[p] = tc.verErr
			}
			got := Verify(p, insp)
			if got.DetectionStatus != tc.wantDetect || got.SignatureStatus != tc.wantSig {
				t.Fatalf("got %s/%s, want %s/%s", got.DetectionStatus, got.SignatureStatus, tc.wantDetect, tc.wantSig)
			}
			if got.DetectionStatus != DetectionVerified && got.StatusReason == "" {
				t.Fatal("failed verification must carry a reason")
			}
		})
	}
}

func TestVerifyIgnoresFilename(t *testing.T) {
	// A file NAMED chrome.exe whose product is Brave must be
	// classified as Brave: identity comes from the file itself,
	// never from its name.
	p := abs(t, "weird/chrome.exe")
	insp := &fakeInspector{
		versions: map[string]VersionInfo{p: {ProductName: "Brave Browser", FileVersion: "1.0"}},
		sigs:     map[string]Signature{p: {Status: "valid", Publisher: "Brave Software, Inc."}},
	}
	got := Verify(p, insp)
	if got.Kind != KindBrave || got.DetectionStatus != DetectionVerified {
		t.Fatalf("got %+v, want verified Brave", got)
	}
}
