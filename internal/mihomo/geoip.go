package mihomo

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

// The GeoIP database (geoip.metadb) is a hard load-time
// dependency of any generated configuration that contains a
// GEOIP rule — which every rule-mode instance's sub-rules do.
// The core downloads the database itself when it is missing,
// and refuses to load the configuration at all when that
// download fails, taking every instance's proxy down with it.
// DeBrowser therefore prepares the database BEFORE the core
// ever sees such a configuration (ProxyService.ensureGeoIP),
// with the same discipline as the core binary itself: fetch to
// a temporary file, validate, then atomically rename into
// place. The GEOIP rule itself is unchanged.

// GeoIPFileName is the database file name inside the core
// working directory.
const GeoIPFileName = "geoip.metadb"

// GeoIPDownloadURL is the database's official release address —
// the same one the core would use.
const GeoIPDownloadURL = "https://github.com/MetaCubeX/meta-rules-dat/releases/download/latest/geoip.metadb"

// geoIPMinSize rejects error pages and truncated downloads: the
// real database is several megabytes.
const geoIPMinSize = 512 << 10

// geoIPMaxSize bounds a runaway download.
const geoIPMaxSize = 256 << 20

// mmdbMetadataMarker is the MaxMind DB metadata signature; a
// genuine database carries it near the end of the file.
var mmdbMetadataMarker = []byte("\xab\xcd\xefMaxMind.com")

// GeoIPPath is the database's place inside the core working
// directory.
func GeoIPPath(dir string) string { return filepath.Join(dir, GeoIPFileName) }

// GeoIPPresent reports whether a usable database file is
// already in place: it exists and is non-empty. A present file
// is reused as-is and never re-downloaded.
func GeoIPPresent(dir string) bool {
	info, err := os.Stat(GeoIPPath(dir))
	return err == nil && !info.IsDir() && info.Size() > 0
}

// DownloadGeoIP fetches the database from downloadURL into dir.
// The download lands in a temporary file in the same directory,
// is validated (size floor + MaxMind metadata marker), and only
// then atomically renamed to geoip.metadb — a failed or invalid
// download leaves no file behind and never disturbs an existing
// one.
func DownloadGeoIP(ctx context.Context, dir string, downloadURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return fmt.Errorf("mihomo: build GeoIP download request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("mihomo: download GeoIP database: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("mihomo: download GeoIP database: unexpected status %s", resp.Status)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("mihomo: create core workdir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "geoip-*.tmp")
	if err != nil {
		return fmt.Errorf("mihomo: create GeoIP temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op after the rename
	written, err := io.Copy(tmp, io.LimitReader(resp.Body, geoIPMaxSize+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("mihomo: download GeoIP database: %w", err)
	}
	if written > geoIPMaxSize {
		return fmt.Errorf("mihomo: GeoIP database exceeds %d bytes, refusing it", geoIPMaxSize)
	}
	if err := validateGeoIPFile(tmpName); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o640); err != nil {
		return fmt.Errorf("mihomo: prepare GeoIP database: %w", err)
	}
	if err := os.Rename(tmpName, GeoIPPath(dir)); err != nil {
		return fmt.Errorf("mihomo: install GeoIP database: %w", err)
	}
	return nil
}

// validateGeoIPFile checks a downloaded file really is a MaxMind
// database: a plausible size and the metadata marker in its
// tail.
func validateGeoIPFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("mihomo: stat downloaded GeoIP database: %w", err)
	}
	if info.Size() < geoIPMinSize {
		return fmt.Errorf("mihomo: downloaded GeoIP database is only %d bytes, not a real database", info.Size())
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("mihomo: open downloaded GeoIP database: %w", err)
	}
	defer f.Close()
	const tailSize = 256 << 10
	tail := info.Size()
	if tail > tailSize {
		tail = tailSize
	}
	buf := make([]byte, tail)
	if _, err := f.ReadAt(buf, info.Size()-tail); err != nil {
		return fmt.Errorf("mihomo: read downloaded GeoIP database: %w", err)
	}
	if !bytes.Contains(buf, mmdbMetadataMarker) {
		return fmt.Errorf("mihomo: downloaded file lacks the MaxMind database marker, not a real database")
	}
	return nil
}
