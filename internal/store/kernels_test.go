package store

import (
	"context"
	"errors"
	"testing"

	"github.com/onbeee/DeBrowser/internal/model"
)

func TestKernelCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	k, err := s.CreateKernel(ctx, model.Kernel{
		Version: "142.0.7444.162", InstallPath: "kernels/142.0.7444.162",
		ExecutablePath: "kernels/142.0.7444.162/chrome.exe",
	})
	if err != nil {
		t.Fatalf("CreateKernel: %v", err)
	}
	if k.ID == "" || k.Channel != "stable" || k.Platform != "win64" || k.Status != "ready" {
		t.Fatalf("kernel defaults wrong: %+v", k)
	}
	// Duplicate (version, platform) must be rejected.
	if _, err := s.CreateKernel(ctx, model.Kernel{
		Version: "142.0.7444.162", InstallPath: "kernels/142.0.7444.162",
		ExecutablePath: "kernels/142.0.7444.162/chrome.exe",
	}); err == nil {
		t.Fatal("duplicate kernel version must fail")
	}
	byVersion, err := s.GetKernelByVersion(ctx, "142.0.7444.162", "win64")
	if err != nil || byVersion.ID != k.ID {
		t.Fatalf("GetKernelByVersion = %+v, %v", byVersion, err)
	}
	if err := s.UpdateKernelStatus(ctx, k.ID, "verification_failed"); err != nil {
		t.Fatalf("UpdateKernelStatus: %v", err)
	}
	got, err := s.GetKernel(ctx, k.ID)
	if err != nil || got.Status != "verification_failed" {
		t.Fatalf("after status update: %+v, %v", got, err)
	}
	list, err := s.ListKernels(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListKernels = %d, %v", len(list), err)
	}
	if err := s.DeleteKernel(ctx, k.ID); err != nil {
		t.Fatalf("DeleteKernel: %v", err)
	}
	if _, err := s.GetKernel(ctx, k.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete, GetKernel err = %v, want ErrNotFound", err)
	}
	if err := s.DeleteKernel(ctx, k.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete err = %v, want ErrNotFound", err)
	}
}
