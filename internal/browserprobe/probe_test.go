package browserprobe

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

type fakeSource struct {
	cands []Candidate
	err   error
}

func (f fakeSource) Candidates(context.Context) ([]Candidate, error) { return f.cands, f.err }

type fakeInspector struct {
	existing map[string]bool
	versions map[string]VersionInfo
	verErr   map[string]error
	sigs     map[string]Signature
}

func (f *fakeInspector) Exists(p string) bool { return f.existing[filepath.Clean(p)] }
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

func TestDetectChromeUserAndSystem(t *testing.T) {
	user := abs(t, "user/Google/Chrome/Application/chrome.exe")
	sys := abs(t, "sys/Google/Chrome/Application/chrome.exe")
	insp := &fakeInspector{
		existing: map[string]bool{user: true, sys: true},
		versions: map[string]VersionInfo{
			user: {ProductName: "Google Chrome", FileVersion: "142.0.7444.162"},
			sys:  {ProductName: "Google Chrome", FileVersion: "141.0.7390.65"},
		},
		sigs: map[string]Signature{
			user: {Status: "valid", Publisher: "Google LLC"},
			sys:  {Status: "valid", Publisher: "Google LLC"},
		},
	}
	src := fakeSource{cands: []Candidate{
		{Path: user, Scope: ScopeUser, KindHint: KindChrome},
		{Path: sys, Scope: ScopeSystem, KindHint: KindChrome},
	}}
	got := Detect(context.Background(), []CandidateSource{src}, insp)
	if len(got) != 2 {
		t.Fatalf("records = %d, want 2", len(got))
	}
	byScope := map[Scope]Info{}
	for _, r := range got {
		byScope[r.Scope] = r
		if r.DetectionStatus != DetectionVerified || r.SignatureStatus != SignatureVerified {
			t.Fatalf("scope %s: status %s/%s, want verified", r.Scope, r.DetectionStatus, r.SignatureStatus)
		}
		if r.Kind != KindChrome || r.Source != "system" || r.ID == "" {
			t.Fatalf("scope %s: bad record %+v", r.Scope, r)
		}
	}
	if byScope[ScopeUser].Version != "142.0.7444.162" || byScope[ScopeSystem].Version != "141.0.7390.65" {
		t.Fatalf("versions wrong: %+v", byScope)
	}
}

func TestDetectBraveBothScopes(t *testing.T) {
	user := abs(t, "u/BraveSoftware/Brave-Browser/Application/brave.exe")
	sys := abs(t, "s/BraveSoftware/Brave-Browser/Application/brave.exe")
	insp := &fakeInspector{
		existing: map[string]bool{user: true, sys: true},
		versions: map[string]VersionInfo{
			user: {ProductName: "Brave Browser", FileVersion: "1.73.91"},
			sys:  {ProductName: "Brave Browser", FileVersion: "1.73.91"},
		},
		sigs: map[string]Signature{
			user: {Status: "valid", Publisher: "Brave Software, Inc."},
			sys:  {Status: "valid", Publisher: "Brave Software, Inc."},
		},
	}
	got := Detect(context.Background(), []CandidateSource{fakeSource{cands: []Candidate{
		{Path: user, Scope: ScopeUser}, {Path: sys, Scope: ScopeSystem},
	}}}, insp)
	if len(got) != 2 {
		t.Fatalf("records = %d, want 2", len(got))
	}
	for _, r := range got {
		if r.Kind != KindBrave || r.DetectionStatus != DetectionVerified {
			t.Fatalf("bad brave record %+v", r)
		}
	}
}

func TestDetectDedupesAndDropsMissing(t *testing.T) {
	p := abs(t, "c/chrome.exe")
	missing := abs(t, "gone/chrome.exe")
	insp := &fakeInspector{
		existing: map[string]bool{p: true},
		versions: map[string]VersionInfo{p: {ProductName: "Google Chrome", FileVersion: "1.0"}},
		sigs:     map[string]Signature{p: {Status: "valid", Publisher: "Google LLC"}},
	}
	srcs := []CandidateSource{
		fakeSource{cands: []Candidate{{Path: p, Scope: ScopeSystem}, {Path: missing, Scope: ScopeSystem}}},
		fakeSource{cands: []Candidate{{Path: p, Scope: ScopeUser}}}, // duplicate from a second source
		fakeSource{err: fmt.Errorf("registry unavailable")},         // broken source tolerated
	}
	got := Detect(context.Background(), srcs, insp)
	if len(got) != 1 {
		t.Fatalf("records = %d, want 1 (dedup + missing dropped)", len(got))
	}
	if got[0].Scope != ScopeSystem {
		t.Fatalf("first-seen scope should win, got %s", got[0].Scope)
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
				existing: map[string]bool{p: true},
				versions: map[string]VersionInfo{p: tc.version},
				verErr:   map[string]error{},
				sigs:     map[string]Signature{p: tc.sig},
			}
			if tc.verErr != nil {
				insp.verErr[p] = tc.verErr
			}
			got := Verify(p, ScopeUser, insp)
			if got.DetectionStatus != tc.wantDetect || got.SignatureStatus != tc.wantSig {
				t.Fatalf("got %s/%s, want %s/%s", got.DetectionStatus, got.SignatureStatus, tc.wantDetect, tc.wantSig)
			}
			if got.DetectionStatus != DetectionVerified && got.StatusReason == "" {
				t.Fatal("failed verification must carry a reason")
			}
		})
	}
}

func TestVerifyIgnoresFilenameAndHint(t *testing.T) {
	// A file NAMED chrome.exe whose product is Brave must be
	// classified as Brave: identity comes from the file itself.
	p := abs(t, "weird/chrome.exe")
	insp := &fakeInspector{
		existing: map[string]bool{p: true},
		versions: map[string]VersionInfo{p: {ProductName: "Brave Browser", FileVersion: "1.0"}},
		sigs:     map[string]Signature{p: {Status: "valid", Publisher: "Brave Software, Inc."}},
	}
	got := Verify(p, ScopeSystem, insp)
	if got.Kind != KindBrave || got.DetectionStatus != DetectionVerified {
		t.Fatalf("got %+v, want verified Brave", got)
	}
}
