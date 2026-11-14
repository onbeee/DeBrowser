package store

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/onbeee/DeBrowser/internal/database"
	"github.com/onbeee/DeBrowser/internal/model"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := database.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return New(db)
}

func intPtr(v int) *int { return &v }

// seedHistoricalProxy inserts a proxies row directly with SQL. The
// store deliberately exposes no proxy write API any more (PR #9
// scope cleanup), so tests seed historical rows exactly the way a
// migrated user database contains them.
func seedHistoricalProxy(t *testing.T, s *Store, id, name, protocol, secretRef string) {
	t.Helper()
	if _, err := s.q.ExecContext(context.Background(),
		`INSERT INTO proxies (id, name, protocol, secret_ref) VALUES (?, ?, ?, ?)`,
		id, name, protocol, secretRef); err != nil {
		t.Fatalf("seed historical proxy: %v", err)
	}
}

func TestProxyProtocolCheckConstraint(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.q.ExecContext(ctx,
		`INSERT INTO proxies (id, name, protocol) VALUES ('bad', 'bad', 'carrier-pigeon')`); err == nil {
		t.Fatal("invalid protocol accepted, want CHECK constraint error")
	}
	if _, err := s.q.ExecContext(ctx,
		`INSERT INTO proxies (id, name, protocol, port) VALUES ('badport', 'badport', 'http', 70000)`); err == nil {
		t.Fatal("out-of-range port accepted, want CHECK constraint error")
	}
}

func TestInstanceCRUDAndConstraints(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	seedHistoricalProxy(t, s, "hist-proxy-1", "p1", "http", "")
	proxyID := "hist-proxy-1"
	in, err := s.CreateInstance(ctx, model.Instance{
		Name: "工作", UserDataDir: "profiles/work", ProxyID: &proxyID,
		KernelName: "Chromium", KernelVersion: "130",
	})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	if in.Status != model.StatusStopped {
		t.Errorf("default status = %q, want stopped", in.Status)
	}
	if in.ProxyID == nil || *in.ProxyID != proxyID {
		t.Errorf("proxy link not stored: %+v", in)
	}

	// Unique name and unique user_data_dir are enforced.
	if _, err := s.CreateInstance(ctx, model.Instance{Name: "工作", UserDataDir: "profiles/other"}); err == nil {
		t.Error("duplicate instance name accepted, want UNIQUE error")
	}
	if _, err := s.CreateInstance(ctx, model.Instance{Name: "other", UserDataDir: "profiles/work"}); err == nil {
		t.Error("duplicate user_data_dir accepted, want UNIQUE error")
	}
	// Invalid status rejected by CHECK.
	bad := in
	bad.ID = ""
	bad.Name = "bad"
	bad.UserDataDir = "profiles/bad"
	bad.Status = "flying"
	if _, err := s.CreateInstance(ctx, bad); err == nil {
		t.Error("invalid status accepted, want CHECK error")
	}
	// FK: instance pointing at a missing proxy is rejected.
	ghost := "no-such-proxy"
	bad2 := in
	bad2.ID, bad2.Name, bad2.UserDataDir, bad2.Status = "", "ghost", "profiles/ghost", model.StatusStopped
	bad2.ProxyID = &ghost
	if _, err := s.CreateInstance(ctx, bad2); err == nil {
		t.Error("instance with nonexistent proxy accepted, want FK error")
	}

	if err := s.SetInstanceStatus(ctx, in.ID, model.StatusRunning); err != nil {
		t.Fatalf("SetInstanceStatus: %v", err)
	}
	got, err := s.GetInstance(ctx, in.ID)
	if err != nil || got.Status != model.StatusRunning {
		t.Errorf("status after set = %q (err %v), want running", got.Status, err)
	}

	// Deleting the proxy row (schema-level; no store API does this)
	// detaches instances instead of failing.
	if _, err := s.q.ExecContext(ctx, `DELETE FROM proxies WHERE id = ?`, proxyID); err != nil {
		t.Fatalf("delete proxy row: %v", err)
	}
	got, err = s.GetInstance(ctx, in.ID)
	if err != nil {
		t.Fatalf("GetInstance after proxy delete: %v", err)
	}
	if got.ProxyID != nil {
		t.Errorf("proxy_id = %v after proxy delete, want NULL (SET NULL)", *got.ProxyID)
	}
}

func TestBookmarksScopesAndCascade(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	in, err := s.CreateInstance(ctx, model.Instance{Name: "inst", UserDataDir: "profiles/i1"})
	if err != nil {
		t.Fatalf("CreateInstance: %v", err)
	}
	global, err := s.CreateBookmark(ctx, model.Bookmark{Name: "全局", URL: "https://example.com"})
	if err != nil {
		t.Fatalf("CreateBookmark global: %v", err)
	}
	if global.InstanceID != nil {
		t.Errorf("global bookmark instance = %v, want nil", *global.InstanceID)
	}
	// Same URL is allowed in the instance scope...
	if _, err := s.CreateBookmark(ctx, model.Bookmark{
		Name: "实例内", URL: "https://example.com", InstanceID: &in.ID,
	}); err != nil {
		t.Fatalf("CreateBookmark in instance scope: %v", err)
	}
	// ...but not twice in the same scope (global or instance).
	if _, err := s.CreateBookmark(ctx, model.Bookmark{Name: "dup", URL: "https://example.com"}); err == nil {
		t.Error("duplicate global URL accepted, want scoped UNIQUE error")
	}
	if _, err := s.CreateBookmark(ctx, model.Bookmark{
		Name: "dup", URL: "https://example.com", InstanceID: &in.ID,
	}); err == nil {
		t.Error("duplicate instance URL accepted, want scoped UNIQUE error")
	}
	// FK: bookmark for a missing instance is rejected.
	ghost := "no-such-instance"
	if _, err := s.CreateBookmark(ctx, model.Bookmark{
		Name: "ghost", URL: "https://ghost.example", InstanceID: &ghost,
	}); err == nil {
		t.Error("bookmark with nonexistent instance accepted, want FK error")
	}

	globals, err := s.ListBookmarks(ctx, nil)
	if err != nil || len(globals) != 1 {
		t.Fatalf("global list = %d (err %v), want 1", len(globals), err)
	}
	locals, err := s.ListBookmarks(ctx, &in.ID)
	if err != nil || len(locals) != 1 {
		t.Fatalf("instance list = %d (err %v), want 1", len(locals), err)
	}

	// Deleting the instance cascades to its bookmarks only.
	if err := s.DeleteInstance(ctx, in.ID); err != nil {
		t.Fatalf("DeleteInstance: %v", err)
	}
	locals, err = s.ListBookmarks(ctx, &in.ID)
	if err != nil || len(locals) != 0 {
		t.Errorf("instance bookmarks after cascade = %d, want 0", len(locals))
	}
	globals, err = s.ListBookmarks(ctx, nil)
	if err != nil || len(globals) != 1 {
		t.Errorf("global bookmarks after cascade = %d, want 1", len(globals))
	}
}

func TestStoreTxAtomicAcrossRepositories(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Commit: instance + its bookmark appear together.
	err := s.WithTx(ctx, func(tx *Store) error {
		in, err := tx.CreateInstance(ctx, model.Instance{Name: "tx-inst", UserDataDir: "profiles/tx"})
		if err != nil {
			return err
		}
		_, err = tx.CreateBookmark(ctx, model.Bookmark{
			Name: "tx-bm", URL: "https://tx.example", InstanceID: &in.ID,
		})
		return err
	})
	if err != nil {
		t.Fatalf("WithTx commit: %v", err)
	}
	instances, _ := s.ListInstances(ctx)
	if len(instances) != 1 {
		t.Fatalf("instances after commit = %d, want 1", len(instances))
	}
	bms, _ := s.ListBookmarks(ctx, &instances[0].ID)
	if len(bms) != 1 {
		t.Fatalf("bookmarks after commit = %d, want 1", len(bms))
	}

	// Rollback: an error after partial writes leaves nothing behind.
	err = s.WithTx(ctx, func(tx *Store) error {
		if _, err := tx.CreateInstance(ctx, model.Instance{Name: "tx-rollback", UserDataDir: "profiles/txr"}); err != nil {
			return err
		}
		return fmt.Errorf("simulated failure")
	})
	if err == nil {
		t.Fatal("WithTx with failing fn succeeded, want error")
	}
	instances, _ = s.ListInstances(ctx)
	if len(instances) != 1 {
		t.Errorf("instances after rollback = %d, want still 1", len(instances))
	}

	// Nested transactions are rejected, not silently flattened.
	err = s.WithTx(ctx, func(tx *Store) error {
		return tx.WithTx(ctx, func(inner *Store) error { return nil })
	})
	if err == nil {
		t.Error("nested WithTx accepted, want error")
	}
}

func TestConcurrentAccess(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const workers = 8
	const perWorker = 10
	var wg sync.WaitGroup
	errs := make(chan error, workers*perWorker)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				name := fmt.Sprintf("inst-%d-%d", w, i)
				if _, err := s.CreateInstance(ctx, model.Instance{
					Name: name, UserDataDir: "profiles/" + name,
				}); err != nil {
					errs <- err
					return
				}
				if _, err := s.ListInstances(ctx); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent access error: %v", err)
	}
	instances, err := s.ListInstances(ctx)
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(instances) != workers*perWorker {
		t.Errorf("instances = %d, want %d", len(instances), workers*perWorker)
	}
}
