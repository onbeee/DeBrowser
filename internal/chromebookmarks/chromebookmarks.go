// Package chromebookmarks reads and writes the Chromium native
// bookmarks file (the "Bookmarks" JSON document at a profile root).
//
// The document is handled as decoded JSON maps end to end, never as
// typed structs: every field Chromium wrote — node ids, GUIDs,
// timestamps, root nodes this package does not know, and unknown
// fields at any level — survives a load/modify/save round trip
// untouched. Numbers decode as json.Number so 64-bit Chrome epoch
// timestamps keep their exact digits.
//
// This package knows nothing about DeBrowser instances or running
// processes. Callers must only invoke it for a profile whose
// browser is confirmed stopped: a running Chromium browser owns
// this file in memory and overwrites external edits.
package chromebookmarks

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// FileName is the bookmarks document name inside a profile root.
const FileName = "Bookmarks"

// backupName is the single rolling recovery copy kept next to the
// bookmarks file. It is replaced on every write, never accumulated.
const backupName = "Bookmarks.debwbak"

// File is one parsed bookmarks document.
type File struct {
	doc   map[string]any
	roots map[string]any
}

// Parse decodes and fully validates a bookmarks document: the
// input must be exactly one JSON value (no trailing content), an
// object whose "roots" value is an object of folder nodes, and
// every node in every tree must be a recognizable bookmark node —
// a folder whose children form an array of valid nodes, or a url
// node carrying a string url. Anything else is rejected so a
// foreign or corrupt file is never treated as editable Chromium
// data, and no node is ever silently dropped. Unknown fields at
// any level are preserved untouched.
func Parse(data []byte) (*File, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("chromebookmarks: parse: %w", err)
	}
	// The document must be the only JSON value in the input.
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("chromebookmarks: trailing content after the JSON document")
		}
		return nil, fmt.Errorf("chromebookmarks: parse trailing content: %w", err)
	}
	if doc == nil {
		return nil, fmt.Errorf("chromebookmarks: document is not a JSON object")
	}
	rootsRaw, ok := doc["roots"]
	if !ok {
		return nil, fmt.Errorf("chromebookmarks: document has no roots")
	}
	roots, ok := rootsRaw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("chromebookmarks: roots is not an object")
	}
	for key, raw := range roots {
		node, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("chromebookmarks: root %q is not an object", key)
		}
		if err := validateNode(node); err != nil {
			return nil, fmt.Errorf("chromebookmarks: root %q: %w", key, err)
		}
		if nodeType(node) != "folder" {
			return nil, fmt.Errorf("chromebookmarks: root %q is not a folder", key)
		}
	}
	return &File{doc: doc, roots: roots}, nil
}

// validateNode recursively checks one bookmark node's structure.
// It never modifies the node; unknown fields are ignored here and
// preserved by the document model.
func validateNode(n map[string]any) error {
	switch nodeType(n) {
	case "folder":
		raw, ok := n["children"]
		if !ok {
			return fmt.Errorf("folder node has no children")
		}
		children, ok := raw.([]any)
		if !ok {
			return fmt.Errorf("folder node children is not an array")
		}
		for i, item := range children {
			child, ok := item.(map[string]any)
			if !ok {
				return fmt.Errorf("child %d is not an object", i)
			}
			if err := validateNode(child); err != nil {
				return err
			}
		}
		return nil
	case "url":
		if _, ok := n["url"].(string); !ok {
			return fmt.Errorf("url node has no string url")
		}
		return nil
	default:
		return fmt.Errorf("node has unrecognized type %q", nodeType(n))
	}
}

// Serialize renders the document back to JSON.
func (f *File) Serialize() ([]byte, error) {
	data, err := json.MarshalIndent(f.doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("chromebookmarks: serialize: %w", err)
	}
	return append(data, '\n'), nil
}

// Root returns the named root folder node (e.g. "bookmark_bar"),
// or nil when the document has no such root.
func (f *File) Root(key string) map[string]any {
	node, _ := f.roots[key].(map[string]any)
	return node
}

// EnsureBookmarkBar returns the bookmark_bar root, creating a
// minimal one when the document lacks it (other roots untouched).
func (f *File) EnsureBookmarkBar() map[string]any {
	if bar := f.Root("bookmark_bar"); bar != nil {
		return bar
	}
	bar := newFolderNode("Bookmarks bar", f.nextID())
	f.roots["bookmark_bar"] = bar
	return bar
}

// ---- node helpers -------------------------------------------------

func nodeType(n map[string]any) string {
	t, _ := n["type"].(string)
	return t
}

func nodeName(n map[string]any) string {
	s, _ := n["name"].(string)
	return s
}

func nodeURL(n map[string]any) string {
	s, _ := n["url"].(string)
	return s
}

func nodeID(n map[string]any) string {
	s, _ := n["id"].(string)
	return s
}

// Children returns the folder's child nodes (nil for url nodes).
func Children(n map[string]any) []map[string]any {
	raw, ok := n["children"].([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if child, ok := item.(map[string]any); ok {
			out = append(out, child)
		}
	}
	return out
}

func setChildren(n map[string]any, children []map[string]any) {
	raw := make([]any, len(children))
	for i, c := range children {
		raw[i] = c
	}
	n["children"] = raw
}

// AppendChild appends a node to a folder, preserving existing order.
func AppendChild(folder, child map[string]any) {
	setChildren(folder, append(Children(folder), child))
}

// FindChildFolder returns the direct child folder with the given
// name, or nil.
func FindChildFolder(folder map[string]any, name string) map[string]any {
	for _, c := range Children(folder) {
		if nodeType(c) == "folder" && nodeName(c) == name {
			return c
		}
	}
	return nil
}

// ChildHasURL reports whether a direct child url node uses url.
func ChildHasURL(folder map[string]any, url string) bool {
	for _, c := range Children(folder) {
		if nodeType(c) == "url" && nodeURL(c) == url {
			return true
		}
	}
	return false
}

// chromeTime renders a Chrome epoch timestamp (microseconds since
// 1601-01-01) the way Chromium stores it: as a decimal string.
func chromeTime(t time.Time) string {
	const epochOffsetMicros = 11644473600000000
	return fmt.Sprintf("%d", t.UnixMicro()+epochOffsetMicros)
}

func newGUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure leaves uniqueness to the node id; use
		// a time-derived placeholder rather than failing the write.
		return fmt.Sprintf("debrowser-%d", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func newFolderNode(name, id string) map[string]any {
	now := chromeTime(time.Now())
	return map[string]any{
		"children":       []any{},
		"date_added":     now,
		"date_last_used": "0",
		"date_modified":  now,
		"guid":           newGUID(),
		"id":             id,
		"name":           name,
		"type":           "folder",
	}
}

func newURLNode(name, url, id string) map[string]any {
	return map[string]any{
		"date_added":     chromeTime(time.Now()),
		"date_last_used": "0",
		"guid":           newGUID(),
		"id":             id,
		"name":           name,
		"type":           "url",
		"url":            url,
	}
}

// nextID returns the next free numeric node id in the document.
func (f *File) nextID() string {
	max := 0
	var walk func(n map[string]any)
	walk = func(n map[string]any) {
		if id := nodeID(n); id != "" {
			var v int
			if _, err := fmt.Sscanf(id, "%d", &v); err == nil && v > max {
				max = v
			}
		}
		for _, c := range Children(n) {
			walk(c)
		}
	}
	for _, raw := range f.roots {
		if node, ok := raw.(map[string]any); ok {
			walk(node)
		}
	}
	return fmt.Sprintf("%d", max+1)
}

// NewFolder builds a fresh folder node with a document-unique id.
func (f *File) NewFolder(name string) map[string]any {
	return newFolderNode(name, f.nextID())
}

// NewBookmark builds a fresh url node with a document-unique id.
func (f *File) NewBookmark(name, url string) map[string]any {
	return newURLNode(name, url, f.nextID())
}

// ---- walking & lookup ----------------------------------------------

// FlatBookmark is one url node located inside the document.
type FlatBookmark struct {
	RootKey  string
	NodeID   string
	ParentID string
	Name     string
	URL      string
	// Path holds the folder names from the root down to the
	// bookmark's parent folder (empty at root level).
	Path []string
}

// FlatFolder is one folder node located inside the document.
type FlatFolder struct {
	RootKey  string
	NodeID   string
	ParentID string
	Name     string
	Path     []string
}

// WalkBookmarks visits every url node in every root.
func (f *File) WalkBookmarks(fn func(FlatBookmark)) {
	for key, raw := range f.roots {
		root, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		f.walkBookmarks(key, root, nil, fn)
	}
}

func (f *File) walkBookmarks(rootKey string, folder map[string]any, path []string, fn func(FlatBookmark)) {
	for _, c := range Children(folder) {
		switch nodeType(c) {
		case "url":
			fn(FlatBookmark{
				RootKey:  rootKey,
				NodeID:   nodeID(c),
				ParentID: nodeID(folder),
				Name:     nodeName(c),
				URL:      nodeURL(c),
				Path:     append([]string{}, path...),
			})
		case "folder":
			f.walkBookmarks(rootKey, c, append(append([]string{}, path...), nodeName(c)), fn)
		}
	}
}

// WalkFolders visits every folder node below the roots (the roots
// themselves are not folders a user manages).
func (f *File) WalkFolders(fn func(FlatFolder)) {
	for key, raw := range f.roots {
		root, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		f.walkFolders(key, root, nil, fn)
	}
}

func (f *File) walkFolders(rootKey string, folder map[string]any, path []string, fn func(FlatFolder)) {
	for _, c := range Children(folder) {
		if nodeType(c) != "folder" {
			continue
		}
		fn(FlatFolder{
			RootKey:  rootKey,
			NodeID:   nodeID(c),
			ParentID: nodeID(folder),
			Name:     nodeName(c),
			Path:     append([]string{}, path...),
		})
		f.walkFolders(rootKey, c, append(append([]string{}, path...), nodeName(c)), fn)
	}
}

// CountBookmarks returns the number of url nodes in the document.
func (f *File) CountBookmarks() int {
	count := 0
	f.WalkBookmarks(func(FlatBookmark) { count++ })
	return count
}

// FindByID locates a node by its Chromium node id, returning the
// node, its parent folder (nil for a root) and the root key.
func (f *File) FindByID(id string) (node, parent map[string]any, rootKey string, ok bool) {
	if id == "" {
		return nil, nil, "", false
	}
	for key, raw := range f.roots {
		root, isMap := raw.(map[string]any)
		if !isMap {
			continue
		}
		if nodeID(root) == id {
			return root, nil, key, true
		}
		if n, p := findInFolder(root, id); n != nil {
			return n, p, key, true
		}
	}
	return nil, nil, "", false
}

func findInFolder(folder map[string]any, id string) (node, parent map[string]any) {
	for _, c := range Children(folder) {
		if nodeID(c) == id {
			return c, folder
		}
		if nodeType(c) == "folder" {
			if n, p := findInFolder(c, id); n != nil {
				return n, p
			}
		}
	}
	return nil, nil
}

// Locate finds a node by id and also reports the folder-name path
// from its root down to the node's parent, so callers can verify a
// previously captured target is still exactly where it was found.
func (f *File) Locate(id string) (node, parent map[string]any, rootKey string, path []string, ok bool) {
	if id == "" {
		return nil, nil, "", nil, false
	}
	for key, raw := range f.roots {
		root, isMap := raw.(map[string]any)
		if !isMap {
			continue
		}
		if nodeID(root) == id {
			return root, nil, key, nil, true
		}
		if n, p, foundPath := locateInFolder(root, id, nil); n != nil {
			return n, p, key, foundPath, true
		}
	}
	return nil, nil, "", nil, false
}

func locateInFolder(folder map[string]any, id string, path []string) (node, parent map[string]any, foundPath []string) {
	for _, c := range Children(folder) {
		if nodeID(c) == id {
			return c, folder, path
		}
		if nodeType(c) == "folder" {
			if n, p, fp := locateInFolder(c, id, append(append([]string{}, path...), nodeName(c))); n != nil {
				return n, p, fp
			}
		}
	}
	return nil, nil, nil
}

// SetName renames a node in place.
func SetName(n map[string]any, name string) { n["name"] = name }

// SetURL rewrites a url node's target in place.
func SetURL(n map[string]any, url string) { n["url"] = url }

// NameOf / URLOf / IDOf expose node fields to callers verifying a
// located node before mutating it.
func NameOf(n map[string]any) string { return nodeName(n) }
func URLOf(n map[string]any) string  { return nodeURL(n) }
func IDOf(n map[string]any) string   { return nodeID(n) }

// TypeOf exposes the node type ("url" or "folder").
func TypeOf(n map[string]any) string { return nodeType(n) }

// RemoveFromParent detaches node from its parent folder. Nodes are
// matched by their Chromium id; a node without an id falls back to
// an exact type/name/url match.
func RemoveFromParent(parent, node map[string]any) bool {
	children := Children(parent)
	for i, c := range children {
		same := false
		if id := nodeID(node); id != "" {
			same = nodeID(c) == id
		} else {
			same = nodeType(c) == nodeType(node) && nodeName(c) == nodeName(node) && nodeURL(c) == nodeURL(node)
		}
		if same {
			setChildren(parent, append(children[:i:i], children[i+1:]...))
			return true
		}
	}
	return false
}

// ---- file IO --------------------------------------------------------

// ErrNotChromium marks a bookmarks file that cannot be confirmed
// to use the Chromium native format — for the strict Load, most
// importantly a file that does not exist at all: Load never
// invents a document. Callers working with a profile directory
// that ResolveProfileDir has already established as the instance's
// Chromium profile use LoadOrEmpty instead, where a missing file
// is the legitimate "no bookmarks yet" state.
var ErrNotChromium = errors.New("chromebookmarks: cannot confirm Chromium bookmarks format")

// Load reads and fully validates the profile's Bookmarks file.
// A missing file is an error (ErrNotChromium), and a present but
// invalid file is an error too; neither is ever overwritten
// blindly.
func Load(profileDir string) (*File, error) {
	path := filepath.Join(profileDir, FileName)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("%w: %s does not exist", ErrNotChromium, FileName)
	}
	if err != nil {
		return nil, fmt.Errorf("chromebookmarks: read %s: %w", FileName, err)
	}
	return Parse(data)
}

// EmptyFile returns a fresh, empty bookmarks document with the
// standard Chromium structure — the three root folders
// (bookmark_bar, other, synced) and version 1 — built with the
// same node constructors as every other node this package creates,
// so a document started from it serializes exactly like one a
// browser would have written.
func EmptyFile() *File {
	roots := map[string]any{
		"bookmark_bar": newFolderNode("Bookmarks bar", "1"),
		"other":        newFolderNode("Other bookmarks", "2"),
		"synced":       newFolderNode("Mobile bookmarks", "3"),
	}
	doc := map[string]any{
		"roots":   roots,
		"version": 1,
	}
	return &File{doc: doc, roots: roots}
}

// LoadOrEmpty loads the profile's Bookmarks file for a profile
// directory already established as an instance's Chromium profile
// (see ResolveProfileDir). A file that does not exist yet means
// the browser has never created bookmarks in this profile — a
// legitimate empty state, answered with EmptyFile. A file that
// exists but cannot be read or parsed is still an error:
// corruption is never disguised as emptiness.
func LoadOrEmpty(profileDir string) (*File, error) {
	data, err := os.ReadFile(filepath.Join(profileDir, FileName))
	if os.IsNotExist(err) {
		return EmptyFile(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("chromebookmarks: read %s: %w", FileName, err)
	}
	return Parse(data)
}

var profileLocks sync.Map // abs profile dir -> *sync.Mutex

// LockProfile serializes bookmark file operations for one profile
// directory and returns the unlock function.
func LockProfile(profileDir string) func() {
	abs, err := filepath.Abs(profileDir)
	if err != nil {
		abs = profileDir
	}
	key := strings.ToLower(abs)
	mu, _ := profileLocks.LoadOrStore(key, &sync.Mutex{})
	m := mu.(*sync.Mutex)
	m.Lock()
	return m.Unlock
}

// postReplaceHook, when non-nil, runs immediately after the
// atomic replacement inside Save, before final verification. It
// exists so tests can simulate a write that lands corrupted.
var postReplaceHook func(profileDir string)

// writeStagingFile writes data to a fresh temporary file in dir,
// fsyncs and closes it, and returns its path. The caller removes
// the file when it is not renamed into place.
func writeStagingFile(dir, pattern string, data []byte) (string, error) {
	tmp, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", fmt.Errorf("chromebookmarks: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("chromebookmarks: write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("chromebookmarks: sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", fmt.Errorf("chromebookmarks: close temp file: %w", err)
	}
	return tmpName, nil
}

// replaceFile atomically replaces target with data via a staging
// file in the same directory, so a failure never leaves a partial
// file behind at target.
func replaceFile(dir, target string, data []byte) error {
	staged, err := writeStagingFile(dir, "Bookmarks.stage-*", data)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(staged) }()
	if err := os.Rename(staged, target); err != nil {
		return fmt.Errorf("chromebookmarks: replace %s: %w", filepath.Base(target), err)
	}
	return nil
}

// Save replaces the profile's Bookmarks file with the document:
//
//  1. The new content is staged in a same-directory temp file and
//     its exact bytes are validated.
//  2. The current file, when one exists, is copied to the single
//     rolling recovery copy Bookmarks.debwbak, itself written via
//     staging+rename so it can never be left half-written. When
//     no file exists yet there is nothing to preserve: the write
//     creates the file, and no recovery copy is made.
//  3. The staged content is atomically renamed over the original.
//  4. The final file is re-read and fully validated. If that
//     fails, Save restores the original from the recovery copy
//     (again via staging+rename) — or, when there was no original,
//     removes the unverified new file — and reports the outcome:
//     a failed write is never reported as success, and never
//     leaves an unverified document behind.
func Save(profileDir string, f *File) error {
	data, err := f.Serialize()
	if err != nil {
		return err
	}
	if _, err := Parse(data); err != nil {
		return fmt.Errorf("chromebookmarks: refusing to write invalid document: %w", err)
	}
	target := filepath.Join(profileDir, FileName)
	backupPath := filepath.Join(profileDir, backupName)

	staged, err := writeStagingFile(profileDir, "Bookmarks.tmp-*", data)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(staged) }()
	onDisk, err := os.ReadFile(staged)
	if err != nil {
		return fmt.Errorf("chromebookmarks: re-read temp file: %w", err)
	}
	if _, err := Parse(onDisk); err != nil {
		return fmt.Errorf("chromebookmarks: temp file failed validation: %w", err)
	}

	original, err := os.ReadFile(target)
	hadOriginal := true
	if os.IsNotExist(err) {
		// No bookmarks file yet: nothing to preserve, the write
		// creates it.
		hadOriginal = false
	} else if err != nil {
		return fmt.Errorf("chromebookmarks: read original for recovery copy: %w", err)
	}
	if hadOriginal {
		if err := replaceFile(profileDir, backupPath, original); err != nil {
			return fmt.Errorf("chromebookmarks: write recovery copy: %w", err)
		}
	}
	// failVerify reports a write whose replacement landed but did
	// not verify, restoring the pre-write state: the original file
	// from its recovery copy, or — when there was no original —
	// no file at all.
	failVerify := func(verifyErr error) error {
		if hadOriginal {
			return restoreAfterFailedVerify(profileDir, target, verifyErr)
		}
		if rerr := os.Remove(target); rerr != nil {
			return fmt.Errorf("chromebookmarks: write failed (%v) and removing the unverified new file failed: %w", verifyErr, rerr)
		}
		return fmt.Errorf("chromebookmarks: write failed (%v); the unverified new file was removed", verifyErr)
	}

	if err := os.Rename(staged, target); err != nil {
		return fmt.Errorf("chromebookmarks: replace bookmarks file: %w", err)
	}
	if postReplaceHook != nil {
		postReplaceHook(profileDir)
	}
	// Prove the final file reads back as a valid document.
	finalData, rerr := os.ReadFile(target)
	if rerr != nil {
		return failVerify(fmt.Errorf("re-read final file: %w", rerr))
	}
	if _, perr := Parse(finalData); perr != nil {
		return failVerify(fmt.Errorf("final file failed validation: %w", perr))
	}
	return nil
}

// restoreAfterFailedVerify puts the original bookmarks file back
// from the recovery copy after a replacement landed but failed
// verification, and reports the outcome precisely: the returned
// error always states the write failed, and whether the original
// was restored or the restore itself failed.
func restoreAfterFailedVerify(profileDir, target string, verifyErr error) error {
	backup, err := os.ReadFile(filepath.Join(profileDir, backupName))
	if err != nil {
		return fmt.Errorf("chromebookmarks: write failed (%v) and restore failed: read recovery copy: %w", verifyErr, err)
	}
	if _, err := Parse(backup); err != nil {
		return fmt.Errorf("chromebookmarks: write failed (%v) and restore failed: recovery copy is invalid: %w", verifyErr, err)
	}
	if err := replaceFile(profileDir, target, backup); err != nil {
		return fmt.Errorf("chromebookmarks: write failed (%v) and restore failed: %w", verifyErr, err)
	}
	restored, err := os.ReadFile(target)
	if err != nil {
		return fmt.Errorf("chromebookmarks: write failed (%v) and restore failed: re-read restored file: %w", verifyErr, err)
	}
	if _, err := Parse(restored); err != nil {
		return fmt.Errorf("chromebookmarks: write failed (%v) and restore failed: restored file is invalid: %w", verifyErr, err)
	}
	return fmt.Errorf("chromebookmarks: write failed (%v); the original file was restored from the recovery copy", verifyErr)
}
