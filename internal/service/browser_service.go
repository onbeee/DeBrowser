package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/onbeee/DeBrowser/internal/database"
	"github.com/onbeee/DeBrowser/internal/logging"
	"github.com/onbeee/DeBrowser/internal/model"
	"github.com/onbeee/DeBrowser/internal/paths"
	"github.com/onbeee/DeBrowser/internal/store"
)

// BrowserService is the browser registration service exposed to
// the frontend over Wails bindings.
//
// Product decision: DeBrowser does NOT detect browsers
// automatically — no registry scanning, no disk scanning, no
// auto-discovery of any kind. A browser becomes available only
// when the user explicitly adds it by picking a program
// executable in the file picker; DeBrowser registers ONLY its
// path (migration 0011, reshaped by 0012). There is NO identity,
// version, signature or publisher check on the chosen file — the
// user chose it, and judging or launching it is not DeBrowser's
// business. DeBrowser never copies, modifies, uninstalls or
// deletes the registered program or reads its profile; removing
// a registration deletes only the database row.
//
// The former DeBrowser-managed downloadable Chromium kernel
// feature was removed by product decision (2026-11-15): the
// download/install/verify code is gone. The kernels table and
// migration 0010 remain untouched as historical data structure
// so existing databases keep upgrading compatibly; no interface
// exposes them any more.
//
// This service never launches a browser process.
type BrowserService struct {
	store  *store.Store
	layout paths.Layout
	log    *slog.Logger

	// Auditor, when set, receives structured operation records
	// for user-initiated mutations.
	Auditor *logging.System

	// ExecutablePicker opens the native file picker for manual
	// browser registration. It is injected by the app package (the
	// service layer stays GUI-free, like the diagnostics save
	// picker). Nil means manual adding is unavailable.
	ExecutablePicker func() (string, error)
}

// BrowserView is the presentation record for one manually
// registered browser: a pure path record.
type BrowserView struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	ExecutablePath string `json:"executablePath"`
	Source         string `json:"source"` // always "manual"
}

// NewBrowserService builds the service.
func NewBrowserService(db *database.DB, layout paths.Layout, log *slog.Logger) *BrowserService {
	return &BrowserService{
		store:  store.New(db),
		layout: layout,
		log:    log,
	}
}

// manualView renders one persisted manual registration. It is a
// pure path record: no file access, no checks of any kind.
func (s *BrowserService) manualView(b model.ExternalBrowser) BrowserView {
	return BrowserView{
		ID:             b.ID,
		Name:           b.Name,
		ExecutablePath: b.ExecutablePath,
		Source:         "manual",
	}
}

// displayNameForPath derives a registration's display name from
// the chosen file's name (extension stripped). It is pure string
// handling of the path the user picked — the file itself is
// never opened, read or executed.
func displayNameForPath(path string) string {
	base := filepath.Base(path)
	if ext := filepath.Ext(base); ext != "" {
		if stem := strings.TrimSuffix(base, ext); stem != "" {
			return stem
		}
	}
	return base
}

// AddBrowserManually lets the user point at a program executable
// through the native file picker and registers ONLY the chosen
// path. There is deliberately NO check of any kind on the file:
// no identity, product, version, signature or publisher
// verification, and the file is not even required to be read —
// the picker already guarantees the user picked it deliberately.
// Registering the same path twice returns the existing
// registration instead of duplicating it.
func (s *BrowserService) AddBrowserManually(operationID string) (BrowserView, error) {
	var out BrowserView
	var err error
	op := beginOp(s.Auditor, operationID, "浏览器内核", "browser.add_manual", "browser", "")
	defer func() { finishOp(op, err) }()
	if s.ExecutablePicker == nil {
		err = fmt.Errorf("文件选择器不可用")
		return BrowserView{}, err
	}
	path, perr := s.ExecutablePicker()
	if perr != nil {
		err = perr
		return BrowserView{}, err
	}
	if path == "" {
		err = fmt.Errorf("未选择文件")
		return BrowserView{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	op.Step("用户选择文件", logging.StatusSuccess, nil)
	if existing, gerr := s.store.GetExternalBrowserByPath(ctx, path); gerr == nil {
		op.Step("路径已登记，返回现有记录", logging.StatusSuccess, nil)
		out = s.manualView(existing)
		return out, nil
	} else if !errors.Is(gerr, store.ErrNotFound) {
		err = gerr
		return BrowserView{}, err
	}
	op.Step("保存路径登记", logging.StatusInfo, nil)
	created, cerr := s.store.CreateExternalBrowser(ctx, model.ExternalBrowser{
		Name:           displayNameForPath(path),
		ExecutablePath: path,
	})
	if cerr != nil {
		err = cerr
		return BrowserView{}, err
	}
	out = s.manualView(created)
	return out, nil
}

// UpdateBrowserPath re-points one manual registration at a newly
// picked executable path (the "重新选择路径" action). Like
// AddBrowserManually it performs no check on the chosen file; it
// only refuses to steal a path another registration already
// holds.
func (s *BrowserService) UpdateBrowserPath(operationID string, id string) (BrowserView, error) {
	var out BrowserView
	var err error
	op := beginOp(s.Auditor, operationID, "浏览器内核", "browser.update_path", "browser", id)
	defer func() { finishOp(op, err) }()
	if s.ExecutablePicker == nil {
		err = fmt.Errorf("文件选择器不可用")
		return BrowserView{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	b, gerr := s.store.GetExternalBrowser(ctx, id)
	if gerr != nil {
		if errors.Is(gerr, store.ErrNotFound) {
			err = fmt.Errorf("未找到该浏览器登记")
		} else {
			err = gerr
		}
		return BrowserView{}, err
	}
	path, perr := s.ExecutablePicker()
	if perr != nil {
		err = perr
		return BrowserView{}, err
	}
	if path == "" {
		err = fmt.Errorf("未选择文件")
		return BrowserView{}, err
	}
	if other, oerr := s.store.GetExternalBrowserByPath(ctx, path); oerr == nil && other.ID != id {
		err = fmt.Errorf("该路径已登记为其他浏览器")
		return BrowserView{}, err
	} else if oerr != nil && !errors.Is(oerr, store.ErrNotFound) {
		err = oerr
		return BrowserView{}, err
	}
	op.Step("用户重新选择文件", logging.StatusSuccess, nil)
	name := displayNameForPath(path)
	if uerr := s.store.UpdateExternalBrowserPath(ctx, id, name, path); uerr != nil {
		err = uerr
		return BrowserView{}, err
	}
	b.Name = name
	b.ExecutablePath = path
	out = s.manualView(b)
	return out, nil
}

// RemoveBrowserRegistration removes one manual browser
// registration. It deletes only the database record — the browser
// file itself is never deleted, moved or modified.
func (s *BrowserService) RemoveBrowserRegistration(operationID string, id string) error {
	var err error
	op := beginOp(s.Auditor, operationID, "浏览器内核", "browser.remove_registration", "browser", id)
	defer func() { finishOp(op, err) }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if derr := s.store.DeleteExternalBrowser(ctx, id); derr != nil {
		if errors.Is(derr, store.ErrNotFound) {
			err = fmt.Errorf("未找到该浏览器登记")
		} else {
			err = derr
		}
		return err
	}
	return nil
}

// ListBrowsers returns the user's persisted manual registrations.
// Nothing is auto-detected; the list comes only from explicit
// user registrations.
func (s *BrowserService) ListBrowsers() ([]BrowserView, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out := []BrowserView{}
	bs, err := s.store.ListExternalBrowsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("读取浏览器登记失败：%w", err)
	}
	for _, b := range bs {
		out = append(out, s.manualView(b))
	}
	return out, nil
}

// GetBrowserDetails returns one manual registration exactly as
// stored (it is a pure path record — there is nothing to
// re-verify).
func (s *BrowserService) GetBrowserDetails(id string) (BrowserView, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	b, err := s.store.GetExternalBrowser(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return BrowserView{}, fmt.Errorf("未找到该浏览器记录")
		}
		return BrowserView{}, err
	}
	return s.manualView(b), nil
}
