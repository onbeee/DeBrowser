package service

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/onbeee/DeBrowser/internal/logging"
	"github.com/onbeee/DeBrowser/internal/version"
)

// Log record exports and capacity management (unified recording
// system). The data-management page offers four exports — all
// records, operation records, error records, runtime records —
// each with a date range read live from the records actually
// retained, plus the single total log capacity the user can
// configure. Everything here reuses the existing logging system;
// no second store is created.

// LogExportCategories are the export selectors the UI may use.
var logExportCategories = map[string][]string{
	"all":       {logging.EventCategoryOperation, logging.EventCategoryError, logging.EventCategoryRuntime},
	"operation": {logging.EventCategoryOperation},
	"error":     {logging.EventCategoryError},
	"runtime":   {logging.EventCategoryRuntime},
}

// LogExportInfo is the dynamic date range of one export category,
// computed from the records currently on disk.
type LogExportInfo struct {
	Category     string `json:"category"`
	HasRecords   bool   `json:"hasRecords"`
	EarliestDate string `json:"earliestDate"` // YYYY-MM-DD, local
	LatestDate   string `json:"latestDate"`   // YYYY-MM-DD, local
	RecordCount  int    `json:"recordCount"`
}

// GetLogExportInfo returns the live date range for one export
// category ("all" | "operation" | "error" | "runtime"). Because
// old records are cleaned up automatically, the range is read
// from the retained records on every call — never fixed.
func (s *DiagnosticsService) GetLogExportInfo(category string) (LogExportInfo, error) {
	cats, ok := logExportCategories[category]
	if !ok {
		return LogExportInfo{}, fmt.Errorf("未知的记录类别：%s", category)
	}
	info := LogExportInfo{Category: category}
	var earliest, latest time.Time
	for _, cat := range cats {
		e, l, n, _, has := s.logs.DateRange(cat)
		if !has {
			continue
		}
		info.HasRecords = true
		info.RecordCount += n
		if earliest.IsZero() || e.Before(earliest) {
			earliest = e
		}
		if latest.IsZero() || l.After(latest) {
			latest = l
		}
	}
	if info.HasRecords {
		info.EarliestDate = earliest.Format("2006-01-02")
		info.LatestDate = latest.Format("2006-01-02")
	}
	return info, nil
}

// LogCapacityView is the capacity state shown by the UI.
type LogCapacityView struct {
	LimitBytes     int64 `json:"limitBytes"`
	UsedBytes      int64 `json:"usedBytes"`
	RemainingBytes int64 `json:"remainingBytes"`
}

func (s *DiagnosticsService) capacityView() LogCapacityView {
	return LogCapacityView{
		LimitBytes:     s.logs.CapacityLimit(),
		UsedBytes:      s.logs.TotalBytes(),
		RemainingBytes: s.logs.RemainingBytes(),
	}
}

// SetLogCapacity persists a new total log capacity and applies it
// immediately: the setting is saved first (so it survives
// restarts), then the logging system enforces the new limit right
// away, cleaning up the oldest records when current usage exceeds
// it. A cleanup failure is returned as an explicit error — the
// operation never pretends to have succeeded.
func (s *DiagnosticsService) SetLogCapacity(operationID string, bytes int64) (LogCapacityView, error) {
	op := beginOp(s.logs, operationID, "数据管理", "logs.set_capacity", "settings", "")
	if bytes < logging.MinCapacityBytes || bytes > logging.MaxCapacityBytes {
		err := fmt.Errorf("日志容量必须在 %d MB 到 %d GB 之间", logging.MinCapacityBytes>>20, logging.MaxCapacityBytes>>30)
		finishOp(op, err)
		return LogCapacityView{}, err
	}
	if err := s.app.SaveLogCapacity(bytes); err != nil {
		finishOp(op, err)
		return LogCapacityView{}, err
	}
	op.Step("保存容量设置", logging.StatusSuccess, nil)
	if _, err := s.logs.SetCapacityLimit(bytes); err != nil {
		op.Step("执行容量清理", logging.StatusFailed, nil)
		finishOp(op, err)
		return LogCapacityView{}, err
	}
	op.Step("执行容量清理", logging.StatusSuccess, nil)
	finishOp(op, nil)
	return s.capacityView(), nil
}

// ClientEventInput is one client-side record sent by the frontend:
// page openings/closings and the exact feedback message a user
// action produced in the UI. The backend validates the category
// and sanitizes every field before recording.
type ClientEventInput struct {
	Category    string            `json:"category"`
	EventType   string            `json:"eventType"`
	Module      string            `json:"module"`
	Action      string            `json:"action"`
	TargetType  string            `json:"targetType"`
	TargetID    string            `json:"targetId"`
	OperationID string            `json:"operationId"`
	Status      string            `json:"status"`
	UserMessage string            `json:"userMessage"`
	Metadata    map[string]string `json:"metadata"`
}

// RecordClientEvent records one frontend-originated event. It is
// fire-and-forget for the UI: recording problems never surface as
// business failures (an invalid category is the only error).
func (s *DiagnosticsService) RecordClientEvent(input ClientEventInput) error {
	switch input.Category {
	case logging.EventCategoryOperation, logging.EventCategoryError, logging.EventCategoryRuntime:
	default:
		return fmt.Errorf("未知的记录类别：%s", input.Category)
	}
	eventType := input.EventType
	if eventType == "" {
		eventType = logging.EventTypeClient
	}
	s.logs.RecordEvent(logging.Event{
		EventType:   eventType,
		Category:    input.Category,
		Module:      input.Module,
		Action:      input.Action,
		TargetType:  input.TargetType,
		TargetID:    input.TargetID,
		OperationID: input.OperationID,
		Trigger:     "user",
		Status:      input.Status,
		UserMessage: input.UserMessage,
		Metadata:    input.Metadata,
	})
	return nil
}

// ExportRecords exports one record category (or all) as a ZIP:
// the selected records inside the chosen date range, a manifest
// with counts, the actual earliest/latest event times and SHA-256
// of every file, and a README. When the primary selection is
// errors (or operations), the linked records of the counterpart
// stream for the same operation IDs are attached as clearly
// separated context files — they are never counted or presented
// as primary records of the chosen category.
func (s *DiagnosticsService) ExportRecords(operationID string, category string, startDate string, endDate string) (ExportResult, error) {
	op := beginOp(s.logs, operationID, "数据管理", "logs.export", "logs", category)
	cats, ok := logExportCategories[category]
	if !ok {
		err := fmt.Errorf("未知的记录类别：%s", category)
		finishOp(op, err)
		return ExportResult{}, err
	}
	from, to, err := parseExportRange(startDate, endDate)
	if err != nil {
		finishOp(op, err)
		return ExportResult{}, err
	}
	if s.SavePathPicker == nil {
		err := fmt.Errorf("保存对话框不可用")
		finishOp(op, err)
		return ExportResult{}, err
	}
	defaultName := "debrowser-logs-" + category + "-" + time.Now().Format("20060102-150405") + ".zip"
	dest, err := s.SavePathPicker(defaultName)
	if err != nil {
		finishOp(op, err)
		return ExportResult{}, fmt.Errorf("打开保存对话框失败：%w", err)
	}
	if dest == "" {
		op.Cancelled("用户取消保存对话框")
		return ExportResult{Cancelled: true}, nil
	}
	op.Step("读取记录并生成导出包", logging.StatusInfo, nil)
	summary, err := s.exportRecordsTo(dest, category, cats, from, to)
	finishOp(op, err)
	if err != nil {
		return ExportResult{}, err
	}
	s.mu.Lock()
	s.lastExport = &summary
	s.mu.Unlock()
	return ExportResult{Path: summary.Path, Files: summary.Files, Bytes: summary.Bytes}, nil
}

// parseExportRange converts the UI's date strings (YYYY-MM-DD,
// local time) into an inclusive timestamp range. Empty means open.
func parseExportRange(startDate, endDate string) (time.Time, time.Time, error) {
	var from, to time.Time
	if startDate != "" {
		d, err := time.ParseInLocation("2006-01-02", startDate, time.Local)
		if err != nil {
			return from, to, fmt.Errorf("开始日期无效：%s", startDate)
		}
		from = d
	}
	if endDate != "" {
		d, err := time.ParseInLocation("2006-01-02", endDate, time.Local)
		if err != nil {
			return from, to, fmt.Errorf("结束日期无效：%s", endDate)
		}
		to = d.AddDate(0, 0, 1).Add(-time.Nanosecond)
	}
	if !from.IsZero() && !to.IsZero() && from.After(to) {
		return from, to, fmt.Errorf("开始日期不能晚于结束日期")
	}
	return from, to, nil
}

type exportFileData struct {
	arcName string
	data    []byte
	sha256  string
	records int
	kind    string // "primary" | "context" | "meta"
}

func joinLines(lines [][]byte) []byte {
	var buf bytes.Buffer
	for _, ln := range lines {
		buf.Write(ln)
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

// exportRecordsTo builds the records ZIP at dest (temp file +
// atomic replace, exactly like the diagnostics export).
func (s *DiagnosticsService) exportRecordsTo(dest string, category string, cats []string, from, to time.Time) (ExportSummary, error) {
	exportedAt := time.Now()
	var files []exportFileData
	primaryCounts := map[string]int{}
	var damagedTotal int
	var actualEarliest, actualLatest time.Time
	noteRange := func(refs []logging.RecordRef) {
		for _, r := range refs {
			if actualEarliest.IsZero() || r.Timestamp.Before(actualEarliest) {
				actualEarliest = r.Timestamp
			}
			if actualLatest.IsZero() || r.Timestamp.After(actualLatest) {
				actualLatest = r.Timestamp
			}
		}
	}
	// Primary records per category.
	primaryOpIDs := map[string]bool{}
	for _, cat := range cats {
		lines, refs, damaged := s.logs.CollectLines(cat, from, to)
		damagedTotal += damaged
		if len(lines) == 0 {
			continue
		}
		noteRange(refs)
		for _, r := range refs {
			if r.OperationID != "" {
				primaryOpIDs[r.OperationID] = true
			}
		}
		data := redactPaths(joinLines(lines))
		if err := validateJSONLines(data); err != nil {
			return ExportSummary{}, fmt.Errorf("记录格式验证未通过（%s）：%w", cat, err)
		}
		if err := ScanForSecrets(data); err != nil {
			return ExportSummary{}, fmt.Errorf("敏感信息检查未通过（%s）：%w", cat, err)
		}
		sum := sha256.Sum256(data)
		files = append(files, exportFileData{
			arcName: "records/" + cat + ".jsonl", data: data,
			sha256: hex.EncodeToString(sum[:]), records: len(lines), kind: "primary",
		})
		primaryCounts[cat] = len(lines)
	}
	// Linked context: for an error export, the operation chains
	// those errors belong to; for an operation export, the errors
	// those operations produced. "all" already contains every
	// stream, so it needs no separate context.
	contextCounts := map[string]int{}
	if category == "error" || category == "operation" {
		ctxCat := logging.EventCategoryOperation
		if category == "operation" {
			ctxCat = logging.EventCategoryError
		}
		allLines, allRefs, damaged := s.logs.CollectLines(ctxCat, time.Time{}, time.Time{})
		damagedTotal += damaged
		var ctxLines [][]byte
		var ctxRefs []logging.RecordRef
		for i, r := range allRefs {
			if r.OperationID != "" && primaryOpIDs[r.OperationID] {
				ctxLines = append(ctxLines, allLines[i])
				ctxRefs = append(ctxRefs, r)
			}
		}
		if len(ctxLines) > 0 {
			data := redactPaths(joinLines(ctxLines))
			if err := validateJSONLines(data); err != nil {
				return ExportSummary{}, fmt.Errorf("记录格式验证未通过（关联上下文）：%w", err)
			}
			if err := ScanForSecrets(data); err != nil {
				return ExportSummary{}, fmt.Errorf("敏感信息检查未通过（关联上下文）：%w", err)
			}
			sum := sha256.Sum256(data)
			files = append(files, exportFileData{
				arcName: "context/" + ctxCat + "-context.jsonl", data: data,
				sha256: hex.EncodeToString(sum[:]), records: len(ctxLines), kind: "context",
			})
			contextCounts[ctxCat] = len(ctxLines)
		}
	}

	manifestFiles := make([]map[string]any, 0, len(files))
	totalRecords := 0
	for _, f := range files {
		manifestFiles = append(manifestFiles, map[string]any{
			"path": f.arcName, "size": len(f.data), "sha256": f.sha256,
			"records": f.records, "kind": f.kind,
		})
		totalRecords += f.records
	}
	manifest := map[string]any{
		"formatVersion":  1,
		"exportKind":     "log-records",
		"appName":        version.Name,
		"appVersion":     version.Version,
		"exportedAt":     exportedAt.Format(time.RFC3339),
		"category":       category,
		"primaryCounts":  primaryCounts,
		"contextCounts":  contextCounts,
		"totalRecords":   totalRecords,
		"damagedSkipped": damagedTotal,
		"requestedRange": map[string]any{"start": dateOrEmpty(from), "end": dateOrEmpty(to)},
		"files":          manifestFiles,
	}
	if !actualEarliest.IsZero() {
		manifest["actualEarliest"] = actualEarliest.Format(time.RFC3339)
		manifest["actualLatest"] = actualLatest.Format(time.RFC3339)
	}
	manifestJSON, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return ExportSummary{}, err
	}
	manifestJSON = redactPaths(manifestJSON)
	if err := ScanForSecrets(manifestJSON); err != nil {
		return ExportSummary{}, fmt.Errorf("敏感信息检查未通过（manifest.json）：%w", err)
	}
	readme := buildRecordsReadme(category, exportedAt, primaryCounts, contextCounts)
	if err := ScanForSecrets(readme); err != nil {
		return ExportSummary{}, fmt.Errorf("敏感信息检查未通过（README.md）：%w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dest), ".logs-export-*.tmp")
	if err != nil {
		return ExportSummary{}, fmt.Errorf("创建导出文件失败：%w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	zw := zip.NewWriter(tmp)
	writeEntry := func(name string, data []byte) error {
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}
	entries := []exportFileData{
		{arcName: "manifest.json", data: manifestJSON},
		{arcName: "README.md", data: readme},
	}
	entries = append(entries, files...)
	for _, e := range entries {
		if err := writeEntry(e.arcName, e.data); err != nil {
			_ = tmp.Close()
			return ExportSummary{}, fmt.Errorf("写入导出文件失败：%w", err)
		}
	}
	if err := zw.Close(); err != nil {
		_ = tmp.Close()
		return ExportSummary{}, fmt.Errorf("完成导出文件失败：%w", err)
	}
	if err := tmp.Close(); err != nil {
		return ExportSummary{}, fmt.Errorf("完成导出文件失败：%w", err)
	}
	if err := s.replaceFile(tmpName, dest); err != nil {
		return ExportSummary{}, fmt.Errorf("保存导出文件失败：%w", err)
	}
	st, err := os.Stat(dest)
	if err != nil {
		return ExportSummary{}, err
	}
	return ExportSummary{
		At:    exportedAt.Format(time.RFC3339),
		Path:  dest,
		Files: len(files),
		Bytes: st.Size(),
	}, nil
}

func dateOrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02")
}

// buildRecordsReadme writes the human-readable export note that
// ships inside every records ZIP, in Simplified Chinese.
func buildRecordsReadme(category string, at time.Time, primary, context map[string]int) []byte {
	var b strings.Builder
	b.WriteString("# DeBrowser 记录导出\n\n")
	fmt.Fprintf(&b, "导出时间：%s\n\n", at.Format("2006-01-02 15:04:05"))
	fmt.Fprintf(&b, "导出类别：%s\n\n", category)
	b.WriteString("## 内容说明\n\n")
	b.WriteString("- `records/` 下是本次选择类别的主要记录（JSON Lines，每行一条记录）。\n")
	b.WriteString("- `context/` 下是为还原操作过程而附加的关联上下文记录，与主要记录分开存放，不计入主要记录数量。\n")
	b.WriteString("- `manifest.json` 记录导出范围、实际包含的最早/最新记录时间、各类别数量与每个文件的 SHA-256。\n\n")
	b.WriteString("## 记录类别\n\n")
	b.WriteString("- operation：操作记录（用户操作及其后台步骤与结果）。\n")
	b.WriteString("- error：错误记录（错误详情与用户实际看到的提示）。\n")
	b.WriteString("- runtime：运行记录（应用生命周期、页面与模块运行事实）。\n\n")
	b.WriteString("## 本次数量\n\n")
	keys := make([]string, 0, len(primary))
	for k := range primary {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "- 主要记录 %s：%d 条\n", k, primary[k])
	}
	keys = keys[:0]
	for k := range context {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "- 关联上下文 %s：%d 条\n", k, context[k])
	}
	b.WriteString("\n导出内容已经过路径脱敏与敏感信息扫描，不包含凭据、Token、Cookie 或浏览器 Profile 内容。\n")
	return []byte(b.String())
}
