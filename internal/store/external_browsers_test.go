package store

import (
	"context"
	"errors"
	"testing"

	"github.com/onbeee/DeBrowser/internal/model"
)

func TestExternalBrowserCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	b, err := s.CreateExternalBrowser(ctx, model.ExternalBrowser{
		Name: "Google Chrome", BrowserType: "chrome", Version: "142.0.7444.162",
		ExecutablePath:     `C:\Program Files\Google\Chrome\Application\chrome.exe`,
		SignatureStatus:    "verified",
		SignaturePublisher: "Google LLC",
		LastVerifiedAt:     "2026-11-15T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("CreateExternalBrowser: %v", err)
	}
	if b.ID == "" || b.Status != "verified" {
		t.Fatalf("browser = %+v", b)
	}
	// Duplicate path must be rejected by the unique index.
	if _, err := s.CreateExternalBrowser(ctx, model.ExternalBrowser{
		Name: "Google Chrome", BrowserType: "chrome", ExecutablePath: b.ExecutablePath,
	}); err == nil {
		t.Fatal("duplicate executable_path must fail")
	}
	byPath, err := s.GetExternalBrowserByPath(ctx, b.ExecutablePath)
	if err != nil || byPath.ID != b.ID {
		t.Fatalf("GetExternalBrowserByPath = %+v, %v", byPath, err)
	}
	// Re-verification outcome updates identity + status.
	b.Status = "invalid"
	b.SignatureStatus = "invalid"
	b.LastVerifiedAt = "2026-11-16T00:00:00Z"
	if err := s.UpdateExternalBrowserVerification(ctx, b); err != nil {
		t.Fatalf("UpdateExternalBrowserVerification: %v", err)
	}
	got, err := s.GetExternalBrowser(ctx, b.ID)
	if err != nil || got.Status != "invalid" || got.LastVerifiedAt != "2026-11-16T00:00:00Z" {
		t.Fatalf("after update: %+v, %v", got, err)
	}
	list, err := s.ListExternalBrowsers(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListExternalBrowsers = %d, %v", len(list), err)
	}
	if err := s.DeleteExternalBrowser(ctx, b.ID); err != nil {
		t.Fatalf("DeleteExternalBrowser: %v", err)
	}
	if _, err := s.GetExternalBrowser(ctx, b.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete err = %v, want ErrNotFound", err)
	}
}
