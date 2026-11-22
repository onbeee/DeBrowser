package mihomo

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// fakeMMDB builds content that passes validation: above the
// size floor and carrying the MaxMind metadata marker in its
// tail.
func fakeMMDB(size int) []byte {
	data := bytes.Repeat([]byte{0x42}, size)
	copy(data[size-64:], mmdbMetadataMarker)
	return data
}

func serveBytes(t *testing.T, status int, body []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDownloadGeoIPSuccess(t *testing.T) {
	want := fakeMMDB(600 << 10)
	srv := serveBytes(t, http.StatusOK, want)
	dir := t.TempDir()
	if GeoIPPresent(dir) {
		t.Fatal("fresh dir must not report a GeoIP database")
	}
	if err := DownloadGeoIP(context.Background(), dir, srv.URL); err != nil {
		t.Fatal(err)
	}
	if !GeoIPPresent(dir) {
		t.Fatal("database must be present after a successful download")
	}
	got, err := os.ReadFile(GeoIPPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("landed file differs: %d bytes, want %d", len(got), len(want))
	}
	// No temporary file is left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}

func TestDownloadGeoIPFailuresLeaveNothing(t *testing.T) {
	cases := map[string]struct {
		status int
		body   []byte
	}{
		"http error":         {http.StatusInternalServerError, fakeMMDB(600 << 10)},
		"not a database":     {http.StatusOK, bytes.Repeat([]byte{0x42}, 600<<10)},
		"too small":          {http.StatusOK, fakeMMDB(1024)},
		"empty body":         {http.StatusOK, nil},
		"marker not in tail": {http.StatusOK, append(append([]byte{}, mmdbMetadataMarker...), bytes.Repeat([]byte{0x42}, 600<<10)...)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := serveBytes(t, tc.status, tc.body)
			dir := t.TempDir()
			if err := DownloadGeoIP(context.Background(), dir, srv.URL); err == nil {
				t.Fatal("download must fail")
			}
			if GeoIPPresent(dir) {
				t.Fatal("a failed download must not leave a database behind")
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("failed download left files behind: %v", entries)
			}
		})
	}
}

func TestGeoIPPresentEmptyFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(GeoIPPath(dir), nil, 0o640); err != nil {
		t.Fatal(err)
	}
	if GeoIPPresent(dir) {
		t.Fatal("an empty file is not a usable database")
	}
}
