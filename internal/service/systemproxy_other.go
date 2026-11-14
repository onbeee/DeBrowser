//go:build !windows

package service

// detectSystemProxy: the Fake-IP fallback targets the Windows
// desktop proxy environment; on other platforms no system proxy
// is reported (direct fetch only). DeBrowser ships for Windows;
// this stub keeps development and CI builds on other platforms
// compiling.
func detectSystemProxy() (string, bool) {
	return "", false
}
