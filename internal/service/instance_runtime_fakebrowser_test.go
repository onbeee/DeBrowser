package service

import (
	"os"
	"strings"
	"testing"
)

// TestMain doubles as the fake browser for the real-process
// runtime tests: when the test binary is launched BY the service
// as an instance's "browser" (DEBROWSER_FAKE_BROWSER=1), it
// records its arguments and then simply stays alive like a
// browser would — until it is closed gracefully (SIGTERM on Unix,
// WM_CLOSE to its window on Windows) or killed.
func TestMain(m *testing.M) {
	if os.Getenv("DEBROWSER_FAKE_BROWSER") == "1" {
		fakeBrowserMain()
		return
	}
	os.Exit(m.Run())
}

func fakeBrowserMain() {
	if f := os.Getenv("DEBROWSER_FAKE_ARGS_FILE"); f != "" {
		_ = os.WriteFile(f, []byte(strings.Join(os.Args, "\x00")), 0o644)
	}
	fakeBrowserBlock()
}
