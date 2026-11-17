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
	"fmt"
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

// Parse decodes and structurally validates a bookmarks document:
// a JSON object whose "roots" value is an object of folder nodes.
// Anything else is rejected so a foreign or corrupt file is never
// treated as editable Chromium data.
func Parse(data []byte) (*File, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("chromebookmarks: parse: %w", err)
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
		if t, _ := node["type"].(string); t != "folder" {
			return nil, fmt.Errorf("chromebookmarks: root %q is not a folder", key)
		}
	}
	return &File{doc: doc, roots: roots}, nil
}

// NewFile returns a minimal valid empty bookmarks document with the
// three standard roots, used when a stopped Chromium profile has no
// Bookmarks file yet.
func NewFile() *File {
	doc := map[string]any{
		"checksum": "",
		"version":  json.Number("1"),
		"roots": map[string]any{
			"bookmark_bar": newFolderNode("Bookmarks bar", "1"),
			"other":        newFolderNode("Other bookmarks", "2"),
			"synced":       newFolderNode("Mobile bookmarks", "3"),
		},
	}
	f, _ := Parse(mustJSON(doc))
	return f
}

func mustJSON(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
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

// LoadResult describes a loaded profile bookmarks state.
type LoadResult struct {
	File    *File
	Existed bool // false when the profile had no Bookmarks file
}

// Load reads and parses the profile's Bookmarks file. A missing
// file yields a fresh empty document (Existed=false), never an
// error: a Chromium profile that has never saved a bookmark simply
// has none yet. A present but unparsable file is an error and must
// never be overwritten blindly.
func Load(profileDir string) (*LoadResult, error) {
	path := filepath.Join(profileDir, FileName)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &LoadResult{File: NewFile(), Existed: false}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("chromebookmarks: read %s: %w", FileName, err)
	}
	parsed, err := Parse(data)
	if err != nil {
		return nil, err
	}
	return &LoadResult{File: parsed, Existed: true}, nil
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

// Save atomically replaces the profile's Bookmarks file with the
// document: the new content is written to a temporary file in the
// same directory, re-parsed for validation, then renamed over the
// original, and finally re-read and re-parsed to prove the write
// landed. Before replacement, the previous file (when one existed)
// is copied to the single rolling recovery copy Bookmarks.debwbak.
// Any failure leaves the original file in place.
func Save(profileDir string, f *File, hadOriginal bool) error {
	data, err := f.Serialize()
	if err != nil {
		return err
	}
	if _, err := Parse(data); err != nil {
		return fmt.Errorf("chromebookmarks: refusing to write invalid document: %w", err)
	}
	target := filepath.Join(profileDir, FileName)

	tmp, err := os.CreateTemp(profileDir, "Bookmarks.tmp-*")
	if err != nil {
		return fmt.Errorf("chromebookmarks: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chromebookmarks: write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chromebookmarks: sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("chromebookmarks: close temp file: %w", err)
	}
	// Validate the exact bytes on disk before they become the file.
	onDisk, err := os.ReadFile(tmpName)
	if err != nil {
		return fmt.Errorf("chromebookmarks: re-read temp file: %w", err)
	}
	if _, err := Parse(onDisk); err != nil {
		return fmt.Errorf("chromebookmarks: temp file failed validation: %w", err)
	}

	if hadOriginal {
		original, err := os.ReadFile(target)
		if err != nil {
			return fmt.Errorf("chromebookmarks: read original for backup: %w", err)
		}
		if err := os.WriteFile(filepath.Join(profileDir, backupName), original, 0o600); err != nil {
			return fmt.Errorf("chromebookmarks: write recovery copy: %w", err)
		}
	}
	if err := os.Rename(tmpName, target); err != nil {
		return fmt.Errorf("chromebookmarks: replace bookmarks file: %w", err)
	}
	// Prove the final file parses.
	finalData, err := os.ReadFile(target)
	if err != nil {
		return fmt.Errorf("chromebookmarks: verify final file: %w", err)
	}
	if _, err := Parse(finalData); err != nil {
		return fmt.Errorf("chromebookmarks: final file failed validation: %w", err)
	}
	return nil
}
