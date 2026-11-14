//go:build openbsd || linux

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"
	"golang.org/x/sys/unix"
)

const (
	RestoreModeOverwrite = "OVERWRITE"
	RestoreModeNewPath   = "NEW_PATH"

	JobQueued      = "QUEUED"
	JobThawing     = "THAWING"
	JobDownloading = "DOWNLOADING"
	JobVerifying   = "VERIFYING"
	JobCommitting  = "COMMITTING"
	JobCompleted   = "COMPLETED"
	JobFailed      = "FAILED"

	MaxCatalogBytes = 256 * 1024 * 1024
)

// DumpPipeline encapsulates OS- and filesystem-specific dump and TOC subprocesses.
type DumpPipeline struct {
	DriverName      string
	DumpCmd         *exec.Cmd
	TocCmd          *exec.Cmd
	ParseTOC        func(r io.Reader, mountPoint string, countAtomic *atomic.Int64) ([]CatalogEntry, error)
	RecordDumpDates bool
}

// FormatBytes formats a byte count into human-readable IEC units.
func FormatBytes(b int64) string {
	if b < 0 {
		b = 0
	}
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit && exp < 4; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

// CatalogEntry represents a single filesystem entry recorded inside a snapshot catalog.
type CatalogEntry struct {
	Inode      uint64 `json:"ino"`
	Type       string `json:"type"` // "d", "-", "l", "c", "b", "p", "s"
	Mode       uint32 `json:"mode"`
	UID        uint32 `json:"uid"`
	GID        uint32 `json:"gid"`
	Size       int64  `json:"size"`
	Mtime      int64  `json:"mtime"`
	LinkTarget string `json:"link,omitempty"`
	Path       string `json:"path"` // Relative to mount root without leading "./" or "/"
}

// ModeString renders a 10-character Unix permission string (e.g. drwxr-xr-x).
func (e CatalogEntry) ModeString() string {
	var buf [10]byte
	switch e.Type {
	case "d":
		buf[0] = 'd'
	case "l":
		buf[0] = 'l'
	case "c":
		buf[0] = 'c'
	case "b":
		buf[0] = 'b'
	case "p":
		buf[0] = 'p'
	case "s":
		buf[0] = 's'
	default:
		buf[0] = '-'
	}
	const rwx = "rwxrwxrwx"
	for i := 0; i < 9; i++ {
		if e.Mode&(1<<uint(8-i)) != 0 {
			buf[i+1] = rwx[i]
		} else {
			buf[i+1] = '-'
		}
	}
	if e.Mode&unix.S_ISUID != 0 {
		if buf[3] == 'x' {
			buf[3] = 's'
		} else {
			buf[3] = 'S'
		}
	}
	if e.Mode&unix.S_ISGID != 0 {
		if buf[6] == 'x' {
			buf[6] = 's'
		} else {
			buf[6] = 'S'
		}
	}
	if e.Mode&unix.S_ISVTX != 0 {
		if buf[9] == 'x' {
			buf[9] = 't'
		} else {
			buf[9] = 'T'
		}
	}
	return string(buf[:])
}

// CatalogNode is an expandable directory or file node inside a snapshot's interactive browse tree.
type CatalogNode struct {
	Name         string
	RelPath      string
	Entry        CatalogEntry
	IsDir        bool
	Expanded     bool
	Marked       bool
	PartialMark  bool
	Level        int
	Parent       *CatalogNode
	Children     []*CatalogNode
	childIndex   map[string]*CatalogNode
	SubtreeSize  int64
	SubtreeCount int
}

// CatalogTree indexes a snapshot's catalog entries into an interactive hierarchy.
type CatalogTree struct {
	MountPoint string
	Root       *CatalogNode
	ByPath     map[string]*CatalogNode
	TotalFiles int
	TotalBytes int64
}

// BuildCatalogTree constructs a sorted directory tree from flat CatalogEntry records.
func BuildCatalogTree(mountPoint string, entries []CatalogEntry) *CatalogTree {
	cleanMount := filepath.Clean(mountPoint)
	if cleanMount == "" || cleanMount == "." {
		cleanMount = "/"
	}
	root := &CatalogNode{
		Name:       cleanMount,
		RelPath:    "",
		IsDir:      true,
		Expanded:   true,
		Level:      0,
		Entry:      CatalogEntry{Type: "d", Mode: 0755},
		childIndex: make(map[string]*CatalogNode),
	}
	tree := &CatalogTree{
		MountPoint: cleanMount,
		Root:       root,
		ByPath:     map[string]*CatalogNode{"": root},
	}

	for _, ent := range entries {
		rel := strings.Trim(strings.TrimPrefix(ent.Path, "./"), "/")
		if rel == "" || rel == "." {
			continue
		}
		parts := strings.Split(rel, "/")
		cur := root
		var built string
		for i, part := range parts {
			if part == "" {
				continue
			}
			if built == "" {
				built = part
			} else {
				built = built + "/" + part
			}
			isLast := (i == len(parts)-1)
			child, exists := cur.childIndex[part]
			if !exists {
				isDir := !isLast || ent.Type == "d"
				child = &CatalogNode{
					Name:       part,
					RelPath:    built,
					IsDir:      isDir,
					Expanded:   cur.Level < 1,
					Level:      cur.Level + 1,
					Parent:     cur,
					childIndex: make(map[string]*CatalogNode),
					Entry: CatalogEntry{
						Type: "d",
						Mode: 0755,
						Path: built,
					},
				}
				cur.childIndex[part] = child
				cur.Children = append(cur.Children, child)
				tree.ByPath[built] = child
			}
			if isLast {
				child.Entry = ent
				if ent.Type == "d" {
					child.IsDir = true
				}
			} else {
				child.IsDir = true
			}
			cur = child
		}
	}

	var finalize func(n *CatalogNode) (int64, int)
	finalize = func(n *CatalogNode) (int64, int) {
		n.childIndex = nil
		if len(n.Children) > 0 {
			n.IsDir = true
			sort.Slice(n.Children, func(i, j int) bool {
				if n.Children[i].IsDir != n.Children[j].IsDir {
					return n.Children[i].IsDir
				}
				return strings.ToLower(n.Children[i].Name) < strings.ToLower(n.Children[j].Name)
			})
		}
		var sumBytes int64
		var count int
		if !n.IsDir {
			sumBytes = n.Entry.Size
			count = 1
		}
		for _, ch := range n.Children {
			cb, cc := finalize(ch)
			sumBytes += cb
			count += cc
		}
		n.SubtreeSize = sumBytes
		n.SubtreeCount = count
		return sumBytes, count
	}

	totalBytes, totalFiles := finalize(root)
	tree.TotalBytes = totalBytes
	tree.TotalFiles = totalFiles
	return tree
}

// ToggleMark flips the mark state of node (and its subtree if a directory).
func (t *CatalogTree) ToggleMark(node *CatalogNode) {
	if node == nil {
		return
	}
	t.SetMark(node, !node.Marked)
}

// SetMark marks or unmarks node (recursively across descendants) and updates ancestor partial indicators.
func (t *CatalogTree) SetMark(node *CatalogNode, mark bool) {
	if node == nil {
		return
	}
	var applyDown func(n *CatalogNode)
	applyDown = func(n *CatalogNode) {
		n.Marked = mark
		n.PartialMark = false
		for _, ch := range n.Children {
			applyDown(ch)
		}
	}
	applyDown(node)

	for p := node.Parent; p != nil; p = p.Parent {
		allMarked := len(p.Children) > 0
		anyMarked := false
		for _, ch := range p.Children {
			if ch.Marked {
				anyMarked = true
			} else {
				allMarked = false
				if ch.PartialMark {
					anyMarked = true
				}
			}
		}
		p.Marked = allMarked
		p.PartialMark = !allMarked && anyMarked
	}
}

// ClearMarks clears every mark in the catalog tree.
func (t *CatalogTree) ClearMarks() {
	if t == nil || t.Root == nil {
		return
	}
	t.SetMark(t.Root, false)
}

// MarkedSummary returns the count and aggregate byte size of marked leaf items (or empty directories).
func (t *CatalogTree) MarkedSummary() (int, int64) {
	if t == nil || t.Root == nil {
		return 0, 0
	}
	var count int
	var bytesSum int64
	var walk func(n *CatalogNode)
	walk = func(n *CatalogNode) {
		if n != t.Root && n.Marked && len(n.Children) == 0 {
			count++
			if !n.IsDir {
				bytesSum += n.Entry.Size
			}
			return
		}
		for _, ch := range n.Children {
			walk(ch)
		}
	}
	walk(t.Root)
	return count, bytesSum
}

// EffectiveRestorePaths collapses fully-marked subtrees into minimal relative paths for selective extraction.
// Returns an empty slice paired with fullRestore=true if the root node itself is fully marked.
func (t *CatalogTree) EffectiveRestorePaths() (paths []string, fullRestore bool) {
	if t == nil || t.Root == nil {
		return nil, false
	}
	if t.Root.Marked {
		return nil, true
	}
	var collect func(n *CatalogNode)
	collect = func(n *CatalogNode) {
		if n != t.Root && n.Marked {
			paths = append(paths, n.RelPath)
			return
		}
		for _, ch := range n.Children {
			if ch.Marked || ch.PartialMark {
				collect(ch)
			}
		}
	}
	collect(t.Root)
	return paths, false
}

// ExpandAncestors expands every parent directory above node so node is visible in the flattened view.
func (t *CatalogTree) ExpandAncestors(node *CatalogNode) {
	if node == nil {
		return
	}
	for p := node.Parent; p != nil; p = p.Parent {
		p.Expanded = true
	}
}

func matchCatalogFilter(relPath, name, filter string) bool {
	if filter == "" {
		return true
	}
	if rx, err := regexp.Compile("(?i)" + filter); err == nil {
		return rx.MatchString(relPath) || rx.MatchString(name)
	}
	up := strings.ToUpper(filter)
	return strings.Contains(strings.ToUpper(relPath), up) || strings.Contains(strings.ToUpper(name), up)
}

// Flatten produces the visible linear slice of CatalogNodes honoring directory expansion or active filter.
func (t *CatalogTree) Flatten(filter string) []*CatalogNode {
	if t == nil || t.Root == nil {
		return nil
	}
	var out []*CatalogNode
	if strings.TrimSpace(filter) == "" {
		var walk func(n *CatalogNode)
		walk = func(n *CatalogNode) {
			out = append(out, n)
			if n.Expanded {
				for _, ch := range n.Children {
					walk(ch)
				}
			}
		}
		walk(t.Root)
		return out
	}

	var walkFiltered func(n *CatalogNode)
	walkFiltered = func(n *CatalogNode) {
		if n != t.Root && matchCatalogFilter(n.RelPath, n.Name, filter) {
			out = append(out, n)
		}
		for _, ch := range n.Children {
			walkFiltered(ch)
		}
	}
	walkFiltered(t.Root)
	return out
}

// SnapshotChainForTarget computes the chronological dump level chain (L0 -> ... -> target) for a snapshot.
func SnapshotChainForTarget(allMountSnapshots []SnapshotRecord, target SnapshotRecord) []SnapshotRecord {
	var candidates []SnapshotRecord
	for _, s := range allMountSnapshots {
		if s.Meta.MountSlug == target.Meta.MountSlug && !s.Meta.Timestamp.After(target.Meta.Timestamp) {
			candidates = append(candidates, s)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Meta.Timestamp.Before(candidates[j].Meta.Timestamp)
	})

	var revChain []SnapshotRecord
	needMaxLevel := target.Meta.Level + 1
	for i := len(candidates) - 1; i >= 0; i-- {
		c := candidates[i]
		if c.Meta.BaseName == target.Meta.BaseName {
			revChain = append(revChain, c)
			needMaxLevel = c.Meta.Level
			if needMaxLevel == 0 {
				break
			}
			continue
		}
		if c.Meta.Level < needMaxLevel {
			revChain = append(revChain, c)
			needMaxLevel = c.Meta.Level
			if needMaxLevel == 0 {
				break
			}
		}
	}

	for l, r := 0, len(revChain)-1; l < r; l, r = l+1, r-1 {
		revChain[l], revChain[r] = revChain[r], revChain[l]
	}
	return revChain
}

// FileVersionInfo summarizes whether a specific relative path appears in a snapshot's catalog.
type FileVersionInfo struct {
	Snapshot      SnapshotRecord
	Entry         CatalogEntry
	Present       bool
	CatalogLoaded bool
}

// DumpProgress reports live throughput and byte counts during a filesystem dump.
type DumpProgress struct {
	Phase        string
	RawBytes     int64
	EstRawBytes  int64
	CipherBytes  int64
	SpeedBPS     float64
	CatalogCount int
}

// RestoreJob tracks persistent and live progress for S3 / Glacier Deep Archive restore operations.
type RestoreJob struct {
	ID            string      `json:"id"`
	CreatedAt     time.Time   `json:"created_at"`
	UpdatedAt     time.Time   `json:"updated_at"`
	Client        string      `json:"client"`
	Archive       ArchiveMeta `json:"archive"`
	MountPoint    string      `json:"mount_point"`
	Tier          string      `json:"tier"`
	Mode          string      `json:"mode"`        // OVERWRITE | NEW_PATH
	TargetDest    string      `json:"target_dest"` // Destination file/dir/suffix when Mode == NEW_PATH
	MarkedPaths   []string    `json:"marked_paths"`
	FullSnapshot  bool        `json:"full_snapshot"`
	Status        string      `json:"status"`
	TotalBytes    int64       `json:"total_bytes"`
	DoneBytes     int64       `json:"done_bytes"`
	SpeedBPS      float64     `json:"speed_bps"`
	Message       string      `json:"message"`
	Error         string      `json:"error,omitempty"`
	RestoredCount int         `json:"restored_count"`
}

func (j *RestoreJob) Clone() RestoreJob {
	cp := *j
	if len(j.MarkedPaths) > 0 {
		cp.MarkedPaths = append([]string(nil), j.MarkedPaths...)
	}
	return cp
}

func (j RestoreJob) IsTerminal() bool {
	return j.Status == JobCompleted || j.Status == JobFailed
}

// Engine coordinates configuration, SSL cryptography, S3 catalogs, dumps, and staged restores.
type Engine struct {
	cfgMu      sync.RWMutex
	cfg        *Config
	configPath string

	catMu        sync.RWMutex
	catalogCache map[string]map[string]CatalogEntry // baseName -> relPath -> entry
	catalogOrder map[string][]CatalogEntry          // baseName -> ordered slice

	jobMu      sync.Mutex
	jobs       map[string]*RestoreJob
	runningIDs map[string]bool
	execMu     sync.Mutex // Serializes local staging/extraction executions
}

// NewEngine initializes the core Tardis engine.
func NewEngine(cfg *Config, configPath string) *Engine {
	if cfg == nil {
		cfg = DefaultConfig()
	}
	cfg.Normalize()
	if strings.TrimSpace(configPath) == "" {
		configPath = DefaultConfigFile
	}
	return &Engine{
		cfg:          cfg.Clone(),
		configPath:   configPath,
		catalogCache: make(map[string]map[string]CatalogEntry),
		catalogOrder: make(map[string][]CatalogEntry),
		jobs:         make(map[string]*RestoreJob),
		runningIDs:   make(map[string]bool),
	}
}

// ConfigSnapshot returns a safe copy of the active Config.
func (e *Engine) ConfigSnapshot() *Config {
	e.cfgMu.RLock()
	defer e.cfgMu.RUnlock()
	return e.cfg.Clone()
}

// ConfigPath returns the active configuration file path.
func (e *Engine) ConfigPath() string {
	e.cfgMu.RLock()
	defer e.cfgMu.RUnlock()
	return e.configPath
}

// ApplyRuntimeConfig updates the in-memory configuration without writing to disk.
func (e *Engine) ApplyRuntimeConfig(newCfg *Config) {
	if newCfg == nil {
		return
	}
	cp := newCfg.Clone()
	cp.Normalize()
	e.cfgMu.Lock()
	e.cfg = cp
	e.cfgMu.Unlock()
}

// UpdateAndSaveConfig validates, saves to disk (0600), and activates newCfg.
func (e *Engine) UpdateAndSaveConfig(newCfg *Config) error {
	if newCfg == nil {
		return errors.New("nil config")
	}
	cp := newCfg.Clone()
	cp.Normalize()
	path := e.ConfigPath()
	if err := SaveConfig(path, cp); err != nil {
		return err
	}
	if err := EnsureDirs(cp); err != nil {
		return err
	}
	e.cfgMu.Lock()
	e.cfg = cp
	e.cfgMu.Unlock()
	return nil
}

// NewStore builds a fresh S3Store from the current engine configuration.
func (e *Engine) NewStore(ctx context.Context) (*S3Store, error) {
	cfg := e.ConfigSnapshot()
	return NewS3Store(ctx, cfg)
}

// ResolveSSL resolves and validates the active OS SSL runtime (LibreSSL on OpenBSD; LibreSSL or OpenSSL on Linux).
func (e *Engine) ResolveSSL() (*SSLRuntime, error) {
	return ResolveSSLRuntime(e.ConfigSnapshot())
}

// EnsureKeyForClient loads or atomically creates the 0400 master keyfile for client.
func (e *Engine) EnsureKeyForClient(client string, createIfMissing bool) (*KeyMaterial, error) {
	cfg := e.ConfigSnapshot()
	keyPath := cfg.EffectiveKeyPath()
	if strings.TrimSpace(client) != "" && client != cfg.EffectiveClient() && strings.TrimSpace(cfg.KeyPath) == "" {
		keyPath = filepath.Join(cfg.EffectiveConfigDir(), strings.TrimSpace(client)+".key")
	}
	if createIfMissing {
		return LoadOrCreateKey(keyPath)
	}
	return LoadKey(keyPath)
}

type byteCounterWriter struct {
	w io.Writer
	n atomic.Int64
}

func (b *byteCounterWriter) Write(p []byte) (int, error) {
	n, err := b.w.Write(p)
	if n > 0 {
		b.n.Add(int64(n))
	}
	return n, err
}

func parseStatToCatalogEntry(mntPath, relPath, fallbackType string, fallbackIno uint64) CatalogEntry {
	fullPath := filepath.Join(mntPath, relPath)
	var st unix.Stat_t
	if err := unix.Lstat(fullPath, &st); err != nil {
		t := "-"
		mode := uint32(0600)
		if fallbackType == "dir" {
			t = "d"
			mode = 0700
		}
		return CatalogEntry{
			Inode: fallbackIno,
			Type:  t,
			Mode:  mode,
			Path:  relPath,
		}
	}

	rawMode := uint32(st.Mode)
	var fType string
	switch rawMode & unix.S_IFMT {
	case unix.S_IFDIR:
		fType = "d"
	case unix.S_IFLNK:
		fType = "l"
	case unix.S_IFCHR:
		fType = "c"
	case unix.S_IFBLK:
		fType = "b"
	case unix.S_IFIFO:
		fType = "p"
	case unix.S_IFSOCK:
		fType = "s"
	default:
		fType = "-"
	}

	var linkTarget string
	if fType == "l" {
		if target, err := os.Readlink(fullPath); err == nil {
			linkTarget = target
		}
	}

	return CatalogEntry{
		Inode:      uint64(st.Ino),
		Type:       fType,
		Mode:       rawMode & 07777,
		UID:        uint32(st.Uid),
		GID:        uint32(st.Gid),
		Size:       int64(st.Size),
		Mtime:      int64(st.Mtim.Sec),
		LinkTarget: linkTarget,
		Path:       relPath,
	}
}

func parseRestoreTVOutput(r io.Reader, mountPoint string, countAtomic *atomic.Int64) ([]CatalogEntry, error) {
	var entries []CatalogEntry
	seen := make(map[string]struct{})
	br := bufio.NewReaderSize(r, 128*1024)

	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			clean := strings.TrimRight(line, "\r\n")
			var recType string
			var ino uint64
			var rawPath string

			parts := strings.SplitN(clean, "\t", 3)
			if len(parts) == 3 {
				kind := strings.TrimSpace(parts[0])
				if kind == "dir" || kind == "leaf" {
					recType = kind
					ino, _ = strconv.ParseUint(strings.TrimSpace(parts[1]), 10, 64)
					rawPath = parts[2]
				}
			} else if strings.HasPrefix(clean, "./") {
				recType = "leaf"
				rawPath = clean
			}

			if rawPath != "" && rawPath != "." {
				rel := strings.TrimPrefix(rawPath, "./")
				rel = strings.TrimLeft(rel, "/")
				if rel != "" && !strings.HasPrefix(rel, ".tardis_stage_") {
					if _, dup := seen[rel]; !dup {
						seen[rel] = struct{}{}
						ent := parseStatToCatalogEntry(mountPoint, rel, recType, ino)
						entries = append(entries, ent)
						if countAtomic != nil {
							countAtomic.Add(1)
						}
					}
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return entries, err
		}
	}
	return entries, nil
}

func encodeCatalogZstd(entries []CatalogEntry) ([]byte, error) {
	var rawJSONL bytes.Buffer
	enc := json.NewEncoder(&rawJSONL)
	enc.SetEscapeHTML(false)
	for _, ent := range entries {
		if err := enc.Encode(ent); err != nil {
			return nil, fmt.Errorf("encode catalog entry %q: %w", ent.Path, err)
		}
	}

	var compressed bytes.Buffer
	zw, err := zstd.NewWriter(&compressed, zstd.WithEncoderLevel(zstd.SpeedBestCompression))
	if err != nil {
		return nil, fmt.Errorf("create zstd catalog encoder: %w", err)
	}
	if _, err := zw.Write(rawJSONL.Bytes()); err != nil {
		_ = zw.Close()
		return nil, fmt.Errorf("compress catalog: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("finalize compressed catalog: %w", err)
	}
	return compressed.Bytes(), nil
}

func decodeCatalogZstd(compressed []byte) ([]CatalogEntry, error) {
	zr, err := zstd.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("open zstd catalog reader: %w", err)
	}
	defer zr.Close()

	var entries []CatalogEntry
	br := bufio.NewReaderSize(zr, 128*1024)
	for {
		line, readErr := br.ReadBytes('\n')
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 {
			var ent CatalogEntry
			if err := json.Unmarshal(trimmed, &ent); err != nil {
				return nil, fmt.Errorf("parse catalog JSONL record: %w", err)
			}
			entries = append(entries, ent)
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("read decompressed catalog: %w", readErr)
		}
	}
	return entries, nil
}

func writeAtomic0600(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp.tardis.*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// RunDump executes a full or incremental filesystem dump (OpenBSD FFS dump, Linux ext2/3/4 dump, or Linux XFS xfsdump),
// builds an exact S3 Standard catalog, encrypts via ChaCha20+PBKDF2, signs with domain-bound HMAC-SHA256,
// and streams to S3 DEEP_ARCHIVE. Returns (meta, changed=false, nil) if incremental scan found zero modified inodes.
func (e *Engine) RunDump(ctx context.Context, mountPoint string, level int, onProgress func(DumpProgress)) (*ArchiveMeta, bool, error) {
	if level < 0 || level > 9 {
		return nil, false, fmt.Errorf("invalid dump level %d (must be 0..9)", level)
	}
	cfg := e.ConfigSnapshot()
	if err := cfg.ValidateS3(); err != nil {
		return nil, false, err
	}
	if err := EnsureDirs(cfg); err != nil {
		return nil, false, err
	}
	sslRT, err := ResolveSSLRuntime(cfg)
	if err != nil {
		return nil, false, err
	}

	lock, err := AcquireLock(cfg.EffectiveLockFile())
	if err != nil {
		return nil, false, err
	}
	defer lock.Release()

	mnt, err := ResolveMount(mountPoint)
	if err != nil {
		return nil, false, err
	}
	if err := ValidateDumpMount(mnt); err != nil {
		return nil, false, err
	}

	km, err := e.EnsureKeyForClient(cfg.EffectiveClient(), true)
	if err != nil {
		return nil, false, err
	}
	defer km.Wipe()

	store, err := NewS3Store(ctx, cfg)
	if err != nil {
		return nil, false, err
	}

	dumpDatesPath := cfg.EffectiveDumpDatesPath()
	refTime, hasRef, err := ReferenceDumpTime(dumpDatesPath, mnt, level)
	if err != nil {
		return nil, false, err
	}

	if onProgress != nil {
		onProgress(DumpProgress{Phase: "Scanning inodes"})
	}
	fullDump := (level == 0 || !hasRef)
	changed, estBytes, err := ScanMountChanges(mnt, refTime, fullDump)
	if err != nil {
		return nil, false, err
	}
	if !fullDump && !changed {
		Logf(cfg, "dump skipped (no changes): mount=%s fs=%s level=%d since=%s",
			mnt.MountPoint, mnt.FSType, level, refTime.Format(time.RFC3339))
		return nil, false, nil
	}

	dumpStartTime := time.Now()
	client := cfg.EffectiveClient()
	meta := FormatArchiveBase(dumpStartTime, client, mnt.MountPoint, level)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	pipe, err := BuildDumpPipeline(runCtx, cfg, mnt, meta, level)
	if err != nil {
		return nil, true, err
	}

	Logf(cfg, "starting dump (%s): archive=%s mount=%s level=%d ssl=%q est_bytes=%d",
		pipe.DriverName, meta.BaseName, mnt.MountPoint, level, sslRT.Summary(), estBytes)

	var dumpStderr bytes.Buffer
	pipe.DumpCmd.Stderr = &dumpStderr
	dumpStdout, err := pipe.DumpCmd.StdoutPipe()
	if err != nil {
		return nil, true, fmt.Errorf("attach dump stdout: %w", err)
	}

	catPipeR, catPipeW := io.Pipe()
	pipe.TocCmd.Stdin = catPipeR
	var tocStderr bytes.Buffer
	pipe.TocCmd.Stderr = &tocStderr
	tocStdout, err := pipe.TocCmd.StdoutPipe()
	if err != nil {
		_ = catPipeR.Close()
		_ = catPipeW.Close()
		return nil, true, fmt.Errorf("attach TOC stdout: %w", err)
	}

	encPlainR, encPlainW := io.Pipe()
	encStream, err := NewEncryptStream(runCtx, sslRT, km, meta.DataKey, encPlainR)
	if err != nil {
		_ = catPipeR.Close()
		_ = catPipeW.Close()
		_ = encPlainR.Close()
		_ = encPlainW.Close()
		return nil, true, err
	}
	defer encStream.Close()

	if err := pipe.DumpCmd.Start(); err != nil {
		_ = catPipeR.Close()
		_ = catPipeW.Close()
		_ = encPlainW.Close()
		return nil, true, fmt.Errorf("start %s (%s): %w", pipe.DriverName, pipe.DumpCmd.Path, err)
	}
	if err := pipe.TocCmd.Start(); err != nil {
		_ = catPipeR.Close()
		_ = catPipeW.Close()
		_ = encPlainW.Close()
		if pipe.DumpCmd.Process != nil {
			_ = pipe.DumpCmd.Process.Kill()
		}
		_ = pipe.DumpCmd.Wait()
		return nil, true, fmt.Errorf("start TOC reader (%s): %w", pipe.TocCmd.Path, err)
	}

	var catalogCount atomic.Int64
	var rawDumpBytes atomic.Int64

	type catResult struct {
		entries []CatalogEntry
		err     error
	}
	catCh := make(chan catResult, 1)
	go func() {
		entries, parseErr := pipe.ParseTOC(tocStdout, mnt.MountPoint, &catalogCount)
		// Close catPipeR once TOC reader completes (early on BSD/ext dump Phase III, or at EOF on xfsdump).
		_ = catPipeR.Close()
		waitErr := pipe.TocCmd.Wait()
		if parseErr == nil && waitErr != nil {
			parseErr = fmt.Errorf("TOC reader (%s) failed: %w (%s)",
				pipe.DriverName, waitErr, strings.TrimSpace(tocStderr.String()))
		}
		catCh <- catResult{entries: entries, err: parseErr}
	}()

	pumpErrCh := make(chan error, 1)
	go func() {
		zw, zErr := zstd.NewWriter(encPlainW, zstd.WithWindowSize(8<<20), zstd.WithEncoderLevel(zstd.SpeedDefault))
		if zErr != nil {
			_ = catPipeW.Close()
			_ = encPlainW.CloseWithError(zErr)
			pumpErrCh <- zErr
			return
		}
		countedZstd := &byteCounterWriter{w: zw}
		buf := make([]byte, 64*1024)
		catOpen := true
		var pumpErr error

		for {
			n, rErr := dumpStdout.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				rawDumpBytes.Add(int64(n))
				if catOpen {
					if _, cwErr := catPipeW.Write(chunk); cwErr != nil {
						catOpen = false
						_ = catPipeW.Close()
					}
				}
				if _, zwErr := countedZstd.Write(chunk); zwErr != nil {
					pumpErr = fmt.Errorf("compress dump stream: %w", zwErr)
					break
				}
			}
			if rErr == io.EOF {
				break
			}
			if rErr != nil {
				pumpErr = fmt.Errorf("read dump stream: %w", rErr)
				break
			}
		}

		if catOpen {
			_ = catPipeW.Close()
		}
		if pumpErr != nil {
			_ = zw.Close()
			_ = encPlainW.CloseWithError(pumpErr)
			if pipe.DumpCmd.Process != nil {
				_ = pipe.DumpCmd.Process.Kill()
			}
			_ = pipe.DumpCmd.Wait()
			pumpErrCh <- pumpErr
			return
		}

		if closeErr := zw.Close(); closeErr != nil {
			_ = encPlainW.CloseWithError(closeErr)
			_ = pipe.DumpCmd.Wait()
			pumpErrCh <- fmt.Errorf("finalize zstd stream: %w", closeErr)
			return
		}
		if dumpWaitErr := pipe.DumpCmd.Wait(); dumpWaitErr != nil {
			wErr := fmt.Errorf("%s failed: %w (%s)", pipe.DriverName, dumpWaitErr, strings.TrimSpace(dumpStderr.String()))
			_ = encPlainW.CloseWithError(wErr)
			pumpErrCh <- wErr
			return
		}
		_ = encPlainW.Close()
		pumpErrCh <- nil
	}()

	stopProg := make(chan struct{})
	var progWG sync.WaitGroup
	if onProgress != nil {
		progWG.Add(1)
		go func() {
			defer progWG.Done()
			start := time.Now()
			tk := time.NewTicker(250 * time.Millisecond)
			defer tk.Stop()
			for {
				select {
				case <-stopProg:
					return
				case <-tk.C:
					raw := rawDumpBytes.Load()
					elapsed := time.Since(start).Seconds()
					var bps float64
					if elapsed > 0 {
						bps = float64(raw) / elapsed
					}
					onProgress(DumpProgress{
						Phase:        fmt.Sprintf("Streaming %s -> zstd -> %s -> S3 DEEP_ARCHIVE", pipe.DriverName, sslRT.Provider),
						RawBytes:     raw,
						EstRawBytes:  estBytes,
						CipherBytes:  encStream.CiphertextBytes(),
						SpeedBPS:     bps,
						CatalogCount: int(catalogCount.Load()),
					})
				}
			}
		}()
	}

	uploadErr := store.UploadDeepArchiveStream(runCtx, meta.DataKey, encStream)
	close(stopProg)
	progWG.Wait()

	if uploadErr != nil {
		cancel()
		_ = encStream.Close()
		<-pumpErrCh
		<-catCh
		return nil, true, uploadErr
	}

	if pumpErr := <-pumpErrCh; pumpErr != nil {
		cancel()
		<-catCh
		return nil, true, pumpErr
	}

	catRes := <-catCh
	if catRes.err != nil {
		return nil, true, catRes.err
	}

	// Upload domain-bound HMAC-SHA256 for the DEEP_ARCHIVE dump object
	_, dataHMACRec, err := encStream.SumHMAC()
	if err != nil {
		return nil, true, err
	}
	if err := store.PutStandardBytes(ctx, meta.DataHMAC, dataHMACRec); err != nil {
		return nil, true, err
	}

	// Compress, cache locally (0600), encrypt, sign, and upload catalog to S3 STANDARD
	if onProgress != nil {
		onProgress(DumpProgress{
			Phase:        "Uploading authenticated catalog to S3 STANDARD",
			RawBytes:     rawDumpBytes.Load(),
			EstRawBytes:  estBytes,
			CipherBytes:  encStream.CiphertextBytes(),
			CatalogCount: len(catRes.entries),
		})
	}

	compressedCat, err := encodeCatalogZstd(catRes.entries)
	if err != nil {
		return nil, true, err
	}
	localCachePath := filepath.Join(cfg.CatalogCacheDir(client), meta.BaseName+"_list.zst")
	if err := writeAtomic0600(localCachePath, compressedCat); err != nil {
		Logf(cfg, "warning: local catalog cache write failed (%s): %v", localCachePath, err)
	}

	cipherCat, listHMACRec, err := EncryptAndSignBytes(ctx, sslRT, km, meta.ListKey, compressedCat)
	if err != nil {
		return nil, true, fmt.Errorf("encrypt catalog: %w", err)
	}
	if err := store.PutStandardBytes(ctx, meta.ListKey, cipherCat); err != nil {
		return nil, true, err
	}
	if err := store.PutStandardBytes(ctx, meta.ListHMAC, listHMACRec); err != nil {
		return nil, true, err
	}

	if pipe.RecordDumpDates {
		if uErr := UpdateDumpDate(dumpDatesPath, mnt, level, dumpStartTime); uErr != nil {
			Logf(cfg, "warning: dumpdates update (%s) failed: %v", dumpDatesPath, uErr)
		}
	}

	e.cacheCatalogInMemory(meta.BaseName, catRes.entries)
	Logf(cfg, "dump complete (%s): archive=%s raw_bytes=%d cipher_bytes=%d entries=%d",
		pipe.DriverName, meta.BaseName, rawDumpBytes.Load(), encStream.CiphertextBytes(), len(catRes.entries))
	return &meta, true, nil
}

func (e *Engine) cacheCatalogInMemory(baseName string, entries []CatalogEntry) {
	byPath := make(map[string]CatalogEntry, len(entries))
	for _, ent := range entries {
		byPath[ent.Path] = ent
	}
	e.catMu.Lock()
	e.catalogCache[baseName] = byPath
	e.catalogOrder[baseName] = entries
	e.catMu.Unlock()
}

// CachedCatalogEntries returns an already-loaded snapshot catalog from memory without network or disk I/O.
func (e *Engine) CachedCatalogEntries(baseName string) ([]CatalogEntry, bool) {
	e.catMu.RLock()
	defer e.catMu.RUnlock()
	entries, ok := e.catalogOrder[baseName]
	return entries, ok
}

// LoadCatalog loads a snapshot's file catalog from memory, local 0600 cache, or S3 STANDARD (verifying HMAC-SHA256).
func (e *Engine) LoadCatalog(ctx context.Context, meta ArchiveMeta) ([]CatalogEntry, error) {
	if entries, ok := e.CachedCatalogEntries(meta.BaseName); ok {
		return entries, nil
	}

	cfg := e.ConfigSnapshot()
	localCachePath := filepath.Join(cfg.CatalogCacheDir(meta.Client), meta.BaseName+"_list.zst")
	if rawLocal, err := os.ReadFile(localCachePath); err == nil && len(rawLocal) > 0 {
		if entries, decErr := decodeCatalogZstd(rawLocal); decErr == nil {
			e.cacheCatalogInMemory(meta.BaseName, entries)
			return entries, nil
		}
	}

	sslRT, err := ResolveSSLRuntime(cfg)
	if err != nil {
		return nil, err
	}
	km, err := e.EnsureKeyForClient(meta.Client, false)
	if err != nil {
		return nil, fmt.Errorf("load key for client %s: %w", meta.Client, err)
	}
	defer km.Wipe()

	store, err := NewS3Store(ctx, cfg)
	if err != nil {
		return nil, err
	}

	cipherCat, err := store.GetBytes(ctx, meta.ListKey, MaxCatalogBytes)
	if err != nil {
		return nil, err
	}
	hmacRec, err := store.GetBytes(ctx, meta.ListHMAC, 4096)
	if err != nil {
		return nil, fmt.Errorf("fetch catalog HMAC (%s): %w", meta.ListHMAC, err)
	}

	compressedCat, err := VerifyAndDecryptBytes(ctx, sslRT, km, meta.ListKey, cipherCat, hmacRec)
	if err != nil {
		return nil, err
	}

	entries, err := decodeCatalogZstd(compressedCat)
	if err != nil {
		return nil, err
	}

	_ = writeAtomic0600(localCachePath, compressedCat)
	e.cacheCatalogInMemory(meta.BaseName, entries)
	return entries, nil
}

// FileHistory inspects cached catalogs across snapshots of the same mountpoint to show version history for relPath.
func (e *Engine) FileHistory(mountSnapshots []SnapshotRecord, relPath string) []FileVersionInfo {
	relPath = strings.Trim(strings.TrimPrefix(relPath, "./"), "/")
	e.catMu.RLock()
	defer e.catMu.RUnlock()

	out := make([]FileVersionInfo, 0, len(mountSnapshots))
	for _, snap := range mountSnapshots {
		byPath, loaded := e.catalogCache[snap.Meta.BaseName]
		if !loaded {
			out = append(out, FileVersionInfo{Snapshot: snap, CatalogLoaded: false})
			continue
		}
		if relPath == "" {
			out = append(out, FileVersionInfo{
				Snapshot:      snap,
				Present:       true,
				CatalogLoaded: true,
			})
			continue
		}
		ent, ok := byPath[relPath]
		out = append(out, FileVersionInfo{
			Snapshot:      snap,
			Entry:         ent,
			Present:       ok,
			CatalogLoaded: true,
		})
	}
	return out
}

func newJobID() string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("job_%s_%s", time.Now().Format("20060102_150405"), hex.EncodeToString(b[:]))
}

func (e *Engine) saveJobLocked(job *RestoreJob) error {
	cfg := e.ConfigSnapshot()
	path := filepath.Join(cfg.JobsDir(), job.ID+".json")
	payload, err := json.MarshalIndent(job, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	return writeAtomic0600(path, payload)
}

// LoadJobs loads persisted RestoreJob records from `/var/db/tardis/jobs/*.json`.
func (e *Engine) LoadJobs() error {
	cfg := e.ConfigSnapshot()
	if err := EnsureDirs(cfg); err != nil {
		return err
	}
	entries, err := os.ReadDir(cfg.JobsDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}

	e.jobMu.Lock()
	defer e.jobMu.Unlock()

	for _, de := range entries {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".json") || strings.HasPrefix(de.Name(), ".") {
			continue
		}
		full := filepath.Join(cfg.JobsDir(), de.Name())
		raw, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		var job RestoreJob
		if err := json.Unmarshal(raw, &job); err != nil || job.ID == "" {
			continue
		}
		if !e.runningIDs[job.ID] && (job.Status == JobDownloading || job.Status == JobVerifying || job.Status == JobCommitting) {
			job.Status = JobQueued
			job.SpeedBPS = 0
			job.Message = "Interrupted previously; queued to retry"
			_ = e.saveJobLocked(&job)
		}
		e.jobs[job.ID] = &job
	}
	return nil
}

// JobsSnapshot returns all RestoreJobs sorted newest-first.
func (e *Engine) JobsSnapshot() []RestoreJob {
	e.jobMu.Lock()
	defer e.jobMu.Unlock()

	list := make([]RestoreJob, 0, len(e.jobs))
	for _, j := range e.jobs {
		list = append(list, j.Clone())
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].CreatedAt.After(list[j].CreatedAt)
	})
	return list
}

// EnqueueRestoreJob creates, persists, and returns a new RestoreJob ready for execution or Glacier thaw.
func (e *Engine) EnqueueRestoreJob(
	client string,
	archive ArchiveMeta,
	mountPoint string,
	tier string,
	mode string,
	targetDest string,
	markedPaths []string,
	fullSnapshot bool,
) (*RestoreJob, error) {
	cfg := e.ConfigSnapshot()
	if err := EnsureDirs(cfg); err != nil {
		return nil, err
	}
	cleanMount := filepath.Clean(mountPoint)
	if cleanMount == "" || cleanMount == "." {
		cleanMount = archive.MountGuess
	}
	if mode != RestoreModeNewPath {
		mode = RestoreModeOverwrite
	}
	if mode == RestoreModeNewPath && strings.TrimSpace(targetDest) == "" {
		return nil, errors.New("NEW_PATH restore mode requires a non-empty target filename/path/suffix")
	}

	now := time.Now()
	job := &RestoreJob{
		ID:           newJobID(),
		CreatedAt:    now,
		UpdatedAt:    now,
		Client:       client,
		Archive:      archive,
		MountPoint:   cleanMount,
		Tier:         cfg.EffectiveTier(tier),
		Mode:         mode,
		TargetDest:   strings.TrimSpace(targetDest),
		MarkedPaths:  append([]string(nil), markedPaths...),
		FullSnapshot: fullSnapshot || len(markedPaths) == 0,
		Status:       JobQueued,
		Message:      "Queued — checking S3 / Glacier Deep Archive status",
	}

	e.jobMu.Lock()
	e.jobs[job.ID] = job
	err := e.saveJobLocked(job)
	cp := job.Clone()
	e.jobMu.Unlock()
	if err != nil {
		return nil, err
	}
	return &cp, nil
}

// DeleteJob removes a non-running job record from memory and `/var/db/tardis/jobs/`.
func (e *Engine) DeleteJob(id string) error {
	cfg := e.ConfigSnapshot()
	e.jobMu.Lock()
	defer e.jobMu.Unlock()

	if e.runningIDs[id] {
		return errors.New("cannot delete an actively running job")
	}
	delete(e.jobs, id)
	path := filepath.Join(cfg.JobsDir(), id+".json")
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// RetryJob resets a FAILED job back to QUEUED and triggers background processing.
func (e *Engine) RetryJob(ctx context.Context, id string) error {
	e.jobMu.Lock()
	job, ok := e.jobs[id]
	if !ok {
		e.jobMu.Unlock()
		return errors.New("job not found")
	}
	if e.runningIDs[id] {
		e.jobMu.Unlock()
		return errors.New("job is already running")
	}
	job.Status = JobQueued
	job.Error = ""
	job.DoneBytes = 0
	job.SpeedBPS = 0
	job.UpdatedAt = time.Now()
	job.Message = "Queued for retry"
	_ = e.saveJobLocked(job)
	e.jobMu.Unlock()

	e.StartJobAsync(ctx, id)
	return nil
}

// StartJobAsync launches background processing for a specific RestoreJob if not already active.
func (e *Engine) StartJobAsync(ctx context.Context, id string) {
	e.jobMu.Lock()
	if e.runningIDs[id] {
		e.jobMu.Unlock()
		return
	}
	if _, ok := e.jobs[id]; !ok {
		e.jobMu.Unlock()
		return
	}
	e.runningIDs[id] = true
	e.jobMu.Unlock()

	go func() {
		defer func() {
			e.jobMu.Lock()
			delete(e.runningIDs, id)
			e.jobMu.Unlock()
		}()
		_ = e.stepJob(ctx, id, nil)
	}()
}

// PollAndRunPendingJobs checks all QUEUED and THAWING jobs synchronously (used by `-J` cron mode and TUI poll).
func (e *Engine) PollAndRunPendingJobs(ctx context.Context) error {
	if err := e.LoadJobs(); err != nil {
		return err
	}
	jobs := e.JobsSnapshot()
	var firstErr error
	for i := len(jobs) - 1; i >= 0; i-- {
		j := jobs[i]
		if j.Status != JobQueued && j.Status != JobThawing {
			continue
		}
		e.jobMu.Lock()
		if e.runningIDs[j.ID] {
			e.jobMu.Unlock()
			continue
		}
		e.runningIDs[j.ID] = true
		e.jobMu.Unlock()

		err := e.stepJob(ctx, j.ID, nil)

		e.jobMu.Lock()
		delete(e.runningIDs, j.ID)
		e.jobMu.Unlock()

		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (e *Engine) updateJob(id string, persist bool, fn func(j *RestoreJob)) RestoreJob {
	e.jobMu.Lock()
	defer e.jobMu.Unlock()
	j, ok := e.jobs[id]
	if !ok {
		return RestoreJob{}
	}
	fn(j)
	j.UpdatedAt = time.Now()
	if persist {
		_ = e.saveJobLocked(j)
	}
	return j.Clone()
}

func escapeRestoreGlob(relPath string) string {
	var sb strings.Builder
	sb.Grow(len(relPath) + 4)
	sb.WriteString("./")
	for i := 0; i < len(relPath); i++ {
		ch := relPath[i]
		switch ch {
		case '\\', '*', '?', '[':
			sb.WriteByte('\\')
		}
		sb.WriteByte(ch)
	}
	return sb.String()
}

func applyStatMetadata(dst string, st *unix.Stat_t) {
	if st == nil {
		return
	}
	rawMode := uint32(st.Mode)
	_ = os.Lchown(dst, int(st.Uid), int(st.Gid))
	if (rawMode & unix.S_IFMT) != unix.S_IFLNK {
		_ = os.Chmod(dst, os.FileMode(rawMode&07777))
	}
	ts := []unix.Timespec{st.Atim, st.Mtim}
	_ = unix.UtimesNanoAt(unix.AT_FDCWD, dst, ts, unix.AT_SYMLINK_NOFOLLOW)
}

func copySingleFileCrossFS(src, dst string, st *unix.Stat_t) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	rawMode := uint32(st.Mode)
	switch rawMode & unix.S_IFMT {
	case unix.S_IFLNK:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		_ = os.Remove(dst)
		if err := os.Symlink(target, dst); err != nil {
			return err
		}
		applyStatMetadata(dst, st)
		return nil

	case unix.S_IFIFO:
		_ = os.Remove(dst)
		if err := unix.Mkfifo(dst, rawMode&07777); err != nil {
			return err
		}
		applyStatMetadata(dst, st)
		return nil

	case unix.S_IFCHR, unix.S_IFBLK:
		_ = os.Remove(dst)
		if err := unix.Mknod(dst, rawMode, int(st.Rdev)); err != nil {
			return err
		}
		applyStatMetadata(dst, st)
		return nil

	case unix.S_IFREG:
		in, err := os.Open(src)
		if err != nil {
			return err
		}
		defer in.Close()

		tmpDst := fmt.Sprintf("%s.tardis_tmp_%d", dst, os.Getpid())
		out, err := os.OpenFile(tmpDst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(rawMode&0777))
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			_ = out.Close()
			_ = os.Remove(tmpDst)
			return err
		}
		if err := out.Sync(); err != nil {
			_ = out.Close()
			_ = os.Remove(tmpDst)
			return err
		}
		if err := out.Close(); err != nil {
			_ = os.Remove(tmpDst)
			return err
		}
		applyStatMetadata(tmpDst, st)
		if err := os.Rename(tmpDst, dst); err != nil {
			_ = os.Remove(tmpDst)
			return err
		}
		return nil

	default:
		return nil
	}
}

// commitNode merges/moves a verified staged entry (file, symlink, or directory tree) onto dst,
// preserving untouched sibling files inside existing target directories during incremental restores.
func commitNode(src, dst string, count *int) error {
	var st unix.Stat_t
	if err := unix.Lstat(src, &st); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return fmt.Errorf("lstat staged path %s: %w", src, err)
	}

	rawMode := uint32(st.Mode)
	if (rawMode & unix.S_IFMT) != unix.S_IFDIR {
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return err
		}
		if dstInfo, err := os.Lstat(dst); err == nil && dstInfo.IsDir() {
			if rmErr := os.Remove(dst); rmErr != nil {
				return fmt.Errorf("cannot overwrite non-empty directory %s with non-directory: %w", dst, rmErr)
			}
		}
		if err := os.Rename(src, dst); err != nil {
			if errors.Is(err, unix.EXDEV) {
				if cErr := copySingleFileCrossFS(src, dst, &st); cErr != nil {
					return cErr
				}
				_ = os.Remove(src)
			} else {
				return fmt.Errorf("commit %s -> %s: %w", src, dst, err)
			}
		}
		if count != nil {
			*count++
		}
		return nil
	}

	if dstInfo, err := os.Lstat(dst); err == nil && !dstInfo.IsDir() {
		if rmErr := os.Remove(dst); rmErr != nil {
			return fmt.Errorf("replace file %s with directory: %w", dst, rmErr)
		}
	}
	if err := os.MkdirAll(dst, os.FileMode(rawMode&0777)); err != nil {
		return fmt.Errorf("mkdir %s: %w", dst, err)
	}

	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("readdir staged %s: %w", src, err)
	}
	for _, de := range entries {
		name := de.Name()
		if name == "restoresymtable" || strings.HasPrefix(name, "xfsrestorehousekeeping") {
			continue
		}
		if err := commitNode(filepath.Join(src, name), filepath.Join(dst, name), count); err != nil {
			return err
		}
	}
	applyStatMetadata(dst, &st)
	return nil
}

func resolveNewPathTarget(mountPoint, relPath, rawDest string, multiItem bool) string {
	dest := strings.TrimSpace(rawDest)
	origFull := filepath.Join(mountPoint, relPath)

	if !strings.Contains(dest, "/") && (strings.HasPrefix(dest, ".") || strings.HasPrefix(dest, "~")) {
		return origFull + dest
	}

	if multiItem {
		if !filepath.IsAbs(dest) {
			dest = filepath.Join(mountPoint, dest)
		}
		return filepath.Join(dest, relPath)
	}

	if !strings.Contains(dest, "/") {
		return filepath.Join(filepath.Dir(origFull), dest)
	}
	if !filepath.IsAbs(dest) {
		dest = filepath.Join(mountPoint, dest)
	}
	if strings.HasSuffix(rawDest, "/") {
		return filepath.Join(dest, filepath.Base(relPath))
	}
	if st, err := os.Stat(dest); err == nil && st.IsDir() {
		return filepath.Join(dest, filepath.Base(relPath))
	}
	return dest
}

func (e *Engine) stepJob(ctx context.Context, id string, onProgress func(RestoreJob)) error {
	e.jobMu.Lock()
	jobPtr, exists := e.jobs[id]
	if !exists {
		e.jobMu.Unlock()
		return errors.New("job not found")
	}
	jobSnap := jobPtr.Clone()
	e.jobMu.Unlock()

	notify := func(j RestoreJob) {
		if onProgress != nil {
			onProgress(j)
		}
	}

	cfg := e.ConfigSnapshot()
	store, err := NewS3Store(ctx, cfg)
	if err != nil {
		snap := e.updateJob(id, true, func(j *RestoreJob) {
			j.Status = JobFailed
			j.Error = err.Error()
			j.Message = "S3 initialization failed"
		})
		notify(snap)
		return err
	}

	// 1. Check S3 HeadObject & Glacier Deep Archive thaw state before acquiring local execution lock
	headSt, err := store.HeadStatus(ctx, jobSnap.Archive.DataKey)
	if err != nil {
		snap := e.updateJob(id, true, func(j *RestoreJob) {
			j.Status = JobFailed
			j.Error = err.Error()
			j.Message = "Archive not found in S3"
		})
		notify(snap)
		return err
	}

	if headSt.GlacierState != GlacierReady {
		thawState, thawErr := store.RequestThaw(ctx, jobSnap.Archive.DataKey, jobSnap.Tier, DefaultRestoreDays)
		if thawErr != nil {
			snap := e.updateJob(id, true, func(j *RestoreJob) {
				j.Status = JobFailed
				j.Error = thawErr.Error()
				j.Message = "Glacier RestoreObject request failed"
			})
			notify(snap)
			return thawErr
		}
		snap := e.updateJob(id, true, func(j *RestoreJob) {
			j.Status = JobThawing
			j.TotalBytes = headSt.Size
			j.Message = fmt.Sprintf("Glacier %s thaw in progress (%s tier)", headSt.StorageClass, j.Tier)
		})
		notify(snap)
		if thawState != GlacierReady {
			return nil
		}
	}

	// 2. Archive is READY: serialize local mount staging & verification
	e.execMu.Lock()
	defer e.execMu.Unlock()

	lock, err := AcquireLock(cfg.EffectiveLockFile())
	if err != nil {
		snap := e.updateJob(id, true, func(j *RestoreJob) {
			j.Message = "Waiting for active local tardis lock (" + err.Error() + ")"
		})
		notify(snap)
		return err
	}
	defer lock.Release()

	sslRT, err := ResolveSSLRuntime(cfg)
	if err != nil {
		snap := e.updateJob(id, true, func(j *RestoreJob) {
			j.Status = JobFailed
			j.Error = err.Error()
			j.Message = "SSL runtime resolution failed"
		})
		notify(snap)
		return err
	}

	km, err := e.EnsureKeyForClient(jobSnap.Client, false)
	if err != nil {
		snap := e.updateJob(id, true, func(j *RestoreJob) {
			j.Status = JobFailed
			j.Error = err.Error()
			j.Message = "Keyfile load failed"
		})
		notify(snap)
		return err
	}
	defer km.Wipe()

	hmacRaw, err := store.GetBytes(ctx, jobSnap.Archive.DataHMAC, 4096)
	if err != nil {
		snap := e.updateJob(id, true, func(j *RestoreJob) {
			j.Status = JobFailed
			j.Error = err.Error()
			j.Message = "Missing archive .hmac record in S3"
		})
		notify(snap)
		return err
	}
	expectedMAC, err := ParseHMACRecord(hmacRaw, jobSnap.Archive.DataKey)
	if err != nil {
		snap := e.updateJob(id, true, func(j *RestoreJob) {
			j.Status = JobFailed
			j.Error = err.Error()
			j.Message = "Invalid archive .hmac record"
		})
		notify(snap)
		return err
	}

	if st, statErr := os.Stat(jobSnap.MountPoint); statErr != nil || !st.IsDir() {
		mErr := fmt.Errorf("target mountpoint %s does not exist or is not a directory", jobSnap.MountPoint)
		snap := e.updateJob(id, true, func(j *RestoreJob) {
			j.Status = JobFailed
			j.Error = mErr.Error()
			j.Message = mErr.Error()
		})
		notify(snap)
		return mErr
	}

	stageDir := filepath.Join(jobSnap.MountPoint, ".tardis_stage_"+jobSnap.ID)
	_ = os.RemoveAll(stageDir)
	if err := os.MkdirAll(stageDir, 0700); err != nil {
		snap := e.updateJob(id, true, func(j *RestoreJob) {
			j.Status = JobFailed
			j.Error = err.Error()
			j.Message = "Failed to create 0700 staging directory"
		})
		notify(snap)
		return err
	}
	defer func() {
		_ = os.RemoveAll(stageDir)
	}()

	s3Body, contentLen, err := store.OpenStream(ctx, jobSnap.Archive.DataKey)
	if err != nil {
		snap := e.updateJob(id, true, func(j *RestoreJob) {
			j.Status = JobFailed
			j.Error = err.Error()
			j.Message = "Failed to open S3 archive stream"
		})
		notify(snap)
		return err
	}
	defer s3Body.Close()

	snap := e.updateJob(id, true, func(j *RestoreJob) {
		j.Status = JobDownloading
		j.TotalBytes = contentLen
		j.DoneBytes = 0
		j.Message = fmt.Sprintf("Streaming S3 -> HMAC-SHA256 -> %s -> zstd -> staging", sslRT.Provider)
	})
	notify(snap)

	decStream, err := NewDecryptStream(ctx, sslRT, km, jobSnap.Archive.DataKey, expectedMAC, s3Body)
	if err != nil {
		snap = e.updateJob(id, true, func(j *RestoreJob) {
			j.Status = JobFailed
			j.Error = err.Error()
			j.Message = "Failed to start decryption stream"
		})
		notify(snap)
		return err
	}
	defer decStream.Close()

	zstdDec, err := zstd.NewReader(decStream)
	if err != nil {
		snap = e.updateJob(id, true, func(j *RestoreJob) {
			j.Status = JobFailed
			j.Error = err.Error()
			j.Message = "Failed to initialize zstd decoder"
		})
		notify(snap)
		return err
	}
	defer zstdDec.Close()

	// Peek decompressed magic header (without consuming bytes) so Linux auto-selects restore vs xfsrestore
	peekReader := bufio.NewReaderSize(zstdDec, 64*1024)
	magicHeader, peekErr := peekReader.Peek(16)
	if len(magicHeader) == 0 && peekErr != nil && peekErr != io.EOF {
		snap = e.updateJob(id, true, func(j *RestoreJob) {
			j.Status = JobFailed
			j.Error = peekErr.Error()
			j.Message = "Stream decryption/decompression failed on archive header"
		})
		notify(snap)
		return peekErr
	}

	restoreCmd, err := BuildRestoreCmd(ctx, magicHeader, stageDir, jobSnap.MountPoint, jobSnap.MarkedPaths, jobSnap.FullSnapshot)
	if err != nil {
		snap = e.updateJob(id, true, func(j *RestoreJob) {
			j.Status = JobFailed
			j.Error = err.Error()
			j.Message = err.Error()
		})
		notify(snap)
		return err
	}

	resStdinR, resStdinW := io.Pipe()
	restoreCmd.Stdin = resStdinR
	var restoreStderr bytes.Buffer
	restoreCmd.Stderr = &restoreStderr

	if err := restoreCmd.Start(); err != nil {
		_ = resStdinR.Close()
		_ = resStdinW.Close()
		snap = e.updateJob(id, true, func(j *RestoreJob) {
			j.Status = JobFailed
			j.Error = err.Error()
			j.Message = "Failed to start restore tool"
		})
		notify(snap)
		return err
	}

	copyErrCh := make(chan error, 1)
	go func() {
		_, cErr := io.Copy(resStdinW, peekReader)
		_ = resStdinW.Close()
		copyErrCh <- cErr
	}()

	stopProg := make(chan struct{})
	var progWG sync.WaitGroup
	progWG.Add(1)
	go func() {
		defer progWG.Done()
		start := time.Now()
		tk := time.NewTicker(250 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-stopProg:
				return
			case <-tk.C:
				done := decStream.CiphertextBytesRead()
				elapsed := time.Since(start).Seconds()
				var bps float64
				if elapsed > 0 {
					bps = float64(done) / elapsed
				}
				cur := e.updateJob(id, false, func(j *RestoreJob) {
					j.DoneBytes = done
					j.SpeedBPS = bps
				})
				notify(cur)
			}
		}
	}()

	restoreWaitErr := restoreCmd.Wait()
	_ = resStdinR.Close()
	copyErr := <-copyErrCh

	if restoreWaitErr != nil {
		close(stopProg)
		progWG.Wait()
		errMsg := fmt.Errorf("%s failed: %w (%s)", filepath.Base(restoreCmd.Path), restoreWaitErr, strings.TrimSpace(restoreStderr.String()))
		if copyErr != nil && !errors.Is(copyErr, io.ErrClosedPipe) && !errors.Is(copyErr, unix.EPIPE) {
			errMsg = fmt.Errorf("%v (stream error: %v)", errMsg, copyErr)
		}
		snap = e.updateJob(id, true, func(j *RestoreJob) {
			j.Status = JobFailed
			j.Error = errMsg.Error()
			j.Message = "Staging extraction failed; live filesystem untouched"
		})
		notify(snap)
		return errMsg
	}

	if copyErr != nil && !errors.Is(copyErr, io.ErrClosedPipe) && !errors.Is(copyErr, unix.EPIPE) {
		close(stopProg)
		progWG.Wait()
		snap = e.updateJob(id, true, func(j *RestoreJob) {
			j.Status = JobFailed
			j.Error = copyErr.Error()
			j.Message = "Stream decompression error; staging discarded"
		})
		notify(snap)
		return copyErr
	}

	// 3. Enforce full-object HMAC-SHA256 authenticity BEFORE moving any staged file into place
	snap = e.updateJob(id, false, func(j *RestoreJob) {
		j.Status = JobVerifying
		j.Message = "Verifying full-archive HMAC-SHA256 before committing files"
	})
	notify(snap)

	verifyErr := decStream.VerifyComplete()
	close(stopProg)
	progWG.Wait()

	if verifyErr != nil {
		snap = e.updateJob(id, true, func(j *RestoreJob) {
			j.Status = JobFailed
			j.Error = verifyErr.Error()
			j.Message = "CRITICAL: HMAC-SHA256 verification failed! Staging wiped, zero files touched"
		})
		notify(snap)
		Logf(cfg, "SECURITY ALERT: restore HMAC verification failed for %s: %v", jobSnap.Archive.DataKey, verifyErr)
		return verifyErr
	}

	// 4. Authenticated commit from staging directory to target paths
	snap = e.updateJob(id, false, func(j *RestoreJob) {
		j.Status = JobCommitting
		j.DoneBytes = j.TotalBytes
		j.Message = "HMAC verified — committing staged files to target paths"
	})
	notify(snap)

	restoredCount := 0
	if jobSnap.FullSnapshot || len(jobSnap.MarkedPaths) == 0 {
		dstRoot := jobSnap.MountPoint
		if jobSnap.Mode == RestoreModeNewPath {
			dstRoot = jobSnap.TargetDest
			if !filepath.IsAbs(dstRoot) {
				dstRoot = filepath.Join(jobSnap.MountPoint, dstRoot)
			}
		}
		if err := commitNode(stageDir, dstRoot, &restoredCount); err != nil {
			snap = e.updateJob(id, true, func(j *RestoreJob) {
				j.Status = JobFailed
				j.Error = err.Error()
				j.Message = "Commit from staging failed"
			})
			notify(snap)
			return err
		}
	} else {
		multiItem := len(jobSnap.MarkedPaths) > 1
		for _, rel := range jobSnap.MarkedPaths {
			cleanRel := strings.Trim(strings.TrimPrefix(rel, "./"), "/")
			if cleanRel == "" {
				continue
			}
			src := filepath.Join(stageDir, cleanRel)
			var dst string
			if jobSnap.Mode == RestoreModeNewPath {
				dst = resolveNewPathTarget(jobSnap.MountPoint, cleanRel, jobSnap.TargetDest, multiItem)
			} else {
				dst = filepath.Join(jobSnap.MountPoint, cleanRel)
			}
			if err := commitNode(src, dst, &restoredCount); err != nil {
				snap = e.updateJob(id, true, func(j *RestoreJob) {
					j.Status = JobFailed
					j.Error = err.Error()
					j.Message = fmt.Sprintf("Commit failed on %s", cleanRel)
				})
				notify(snap)
				return err
			}
		}
	}

	snap = e.updateJob(id, true, func(j *RestoreJob) {
		j.Status = JobCompleted
		j.DoneBytes = j.TotalBytes
		j.SpeedBPS = 0
		j.RestoredCount = restoredCount
		j.Message = fmt.Sprintf("Verified & restored (%s mode, %d file items)", j.Mode, restoredCount)
	})
	notify(snap)
	Logf(cfg, "restore complete: job=%s archive=%s mode=%s items=%d",
		id, jobSnap.Archive.BaseName, jobSnap.Mode, restoredCount)
	return nil
}

// RunCLIRestore executes a CLI restore (`tardis -r <archive> -m <mount> [-t Standard|Bulk] [-o <dest>]`),
// polling Glacier Deep Archive thaw progress automatically if frozen.
func (e *Engine) RunCLIRestore(
	ctx context.Context,
	archiveArg string,
	mountPoint string,
	tier string,
	targetDest string,
	onProgress func(RestoreJob),
) error {
	cfg := e.ConfigSnapshot()
	if err := cfg.ValidateS3(); err != nil {
		return err
	}
	if strings.TrimSpace(mountPoint) == "" {
		return errors.New("mountpoint (-m) is required for restore")
	}

	mounts, _ := ListMounts()
	meta, ok := ParseArchiveBase(cfg.EffectiveClient(), archiveArg, mounts)
	if !ok {
		return fmt.Errorf("cannot parse archive name %q", archiveArg)
	}

	mode := RestoreModeOverwrite
	if strings.TrimSpace(targetDest) != "" {
		mode = RestoreModeNewPath
	}

	job, err := e.EnqueueRestoreJob(meta.Client, meta, mountPoint, tier, mode, targetDest, nil, true)
	if err != nil {
		return err
	}

	for {
		if err := e.stepJob(ctx, job.ID, onProgress); err != nil {
			return err
		}
		var latest RestoreJob
		for _, j := range e.JobsSnapshot() {
			if j.ID == job.ID {
				latest = j
				break
			}
		}
		if latest.Status == JobCompleted {
			return nil
		}
		if latest.Status == JobFailed {
			return errors.New(latest.Error)
		}
		if onProgress != nil {
			onProgress(latest)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(15 * time.Minute):
		}
	}
}
