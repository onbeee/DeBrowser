package store

import (
	"context"
	"errors"
	"testing"

	"github.com/onbeee/DeBrowser/internal/model"
)

// Manual browser registrations are pure path records: any name
// and any path can be stored — the store performs no identity or
// signature checks (there are no such fields anymore).
func TestExternalBrowserCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	b, err := s.CreateExternalBrowser(ctx, model.ExternalBrowser{
		Name:           "totally-not-a-browser",
		ExecutablePath: `C:\Users\one\Programs\totally-not-a-browser.exe`,
	})
	if err != nil {
		t.Fatalf("CreateExternalBrowser: %v", err)
	}
	if b.ID == "" || b.Name != "totally-not-a-browser" || b.CreatedAt == "" {
		t.Fatalf("browser = %+v", b)
	}
	// Duplicate path must be rejected by the unique index.
	if _, err := s.CreateExternalBrowser(ctx, model.ExternalBrowser{
		Name: "other", ExecutablePath: b.ExecutablePath,
	}); err == nil {
		t.Fatal("duplicate executable_path must fail")
	}
	byPath, err := s.GetExternalBrowserByPath(ctx, b.ExecutablePath)
	if err != nil || byPath.ID != b.ID {
		t.Fatalf("GetExternalBrowserByPath = %+v, %v", byPath, err)
	}
	// Re-selecting a path updates name + path in place.
	if err := s.UpdateExternalBrowserPath(ctx, b.ID, "renamed", `D:\elsewhere\renamed.exe`); err != nil {
		t.Fatalf("UpdateExternalBrowserPath: %v", err)
	}
	got, err := s.GetExternalBrowser(ctx, b.ID)
	if err != nil || got.Name != "renamed" || got.ExecutablePath != `D:\elsewhere\renamed.exe` {
		t.Fatalf("after update: %+v, %v", got, err)
	}
	if err := s.UpdateExternalBrowserPath(ctx, "missing-id", "x", `D:\x.exe`); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing err = %v, want ErrNotFound", err)
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
