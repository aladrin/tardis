//go:build openbsd || linux

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	DefaultConfigDir  = "/etc/tardis.d"
	DefaultConfigFile = "/etc/tardis.d/config.json"
	DefaultDBDir      = "/var/db/tardis"
	DefaultLockFile   = "/var/run/tardis.lock"
	DefaultLogFile    = "/var/log/tardis"

	ArchiveTimeLayout = "2006-01-02_15:04:05"
)

// Config holds persistent S3, SSL/cryptographic, and local OS path settings.
type Config struct {
	Bucket          string `json:"bucket"`
	Region          string `json:"region"`
	Endpoint        string `json:"endpoint"`
	ForcePathStyle  bool   `json:"force_path_style"`
	AWSProfile      string `json:"aws_profile"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	Client          string `json:"client"`
	KeyPath         string `json:"key_path"`
	DefaultTier     string `json:"default_tier"`
	OpenSSLBin      string `json:"openssl_bin,omitempty"`
	LDLibraryPath   string `json:"ld_library_path,omitempty"`
	DumpDatesFile   string `json:"dumpdates_file,omitempty"`
	ConfigDir       string `json:"config_dir"`
	DBDir           string `json:"db_dir"`
	LockFile        string `json:"lock_file"`
	LogFile         string `json:"log_file"`
}

// CLIOptions represents command-line flag overrides and execution modes.
type CLIOptions struct {
	ConfigPath string
	Bucket     string
	Client     string
	KeyPath    string
	OpenSSLBin string
	Tier       string
	Mount      string
	DumpLevel  int    // -1 when unset
	RestoreObj string // -r archive name
	RestoreDst string // -o optional alternate destination path
	DeleteObj  string // -D object or snapshot base name
	QueryMode  string // -q ALL|STANDARD|DEEP_ARCHIVE
	RunJobs    bool   // -J poll and execute pending restore jobs
	ForceTUI   bool   // -u force interactive TUI
}

// DefaultHostnameShort returns the short local hostname without domain suffix.
func DefaultHostnameShort() string {
	h, err := os.Hostname()
	if err != nil || strings.TrimSpace(h) == "" {
		return strings.ToLower(PlatformName)
	}
	h = strings.TrimSpace(h)
	if idx := strings.IndexByte(h, '.'); idx > 0 {
		h = h[:idx]
	}
	return h
}

// NormalizeTier normalizes Glacier retrieval tier names to "Standard" or "Bulk".
func NormalizeTier(tier string) string {
	if strings.EqualFold(strings.TrimSpace(tier), "standard") {
		return "Standard"
	}
	return "Bulk"
}

// DefaultConfig returns a baseline configuration populated with OS-appropriate defaults.
func DefaultConfig() *Config {
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = os.Getenv("AWS_DEFAULT_REGION")
	}
	if region == "" {
		region = "ap-southeast-2"
	}
	return &Config{
		Region:        region,
		AWSProfile:    os.Getenv("AWS_PROFILE"),
		Client:        DefaultHostnameShort(),
		DefaultTier:   "Bulk",
		OpenSSLBin:    DefaultOpenSSLBin,
		DumpDatesFile: DefaultDumpDatesPath,
		ConfigDir:     DefaultConfigDir,
		DBDir:         DefaultDBDir,
		LockFile:      DefaultLockFile,
		LogFile:       DefaultLogFile,
	}
}

// Clone returns a copy of Config for safe editing inside the TUI.
func (c *Config) Clone() *Config {
	if c == nil {
		return DefaultConfig()
	}
	cp := *c
	return &cp
}

// Normalize fills missing fields with system defaults without clobbering explicit settings.
func (c *Config) Normalize() {
	if strings.TrimSpace(c.Region) == "" {
		if env := os.Getenv("AWS_REGION"); env != "" {
			c.Region = env
		} else if env := os.Getenv("AWS_DEFAULT_REGION"); env != "" {
			c.Region = env
		} else {
			c.Region = "ap-southeast-2"
		}
	}
	if strings.TrimSpace(c.Client) == "" {
		c.Client = DefaultHostnameShort()
	}
	c.DefaultTier = NormalizeTier(c.DefaultTier)
	if strings.TrimSpace(c.ConfigDir) == "" {
		c.ConfigDir = DefaultConfigDir
	}
	if strings.TrimSpace(c.DBDir) == "" {
		c.DBDir = DefaultDBDir
	}
	if strings.TrimSpace(c.LockFile) == "" {
		c.LockFile = DefaultLockFile
	}
	if strings.TrimSpace(c.LogFile) == "" {
		c.LogFile = DefaultLogFile
	}
	if strings.TrimSpace(c.DumpDatesFile) == "" {
		c.DumpDatesFile = DefaultDumpDatesPath
	}
}

func (c *Config) EffectiveClient() string {
	if c == nil || strings.TrimSpace(c.Client) == "" {
		return DefaultHostnameShort()
	}
	return strings.TrimSpace(c.Client)
}

func (c *Config) EffectiveConfigDir() string {
	if c == nil || strings.TrimSpace(c.ConfigDir) == "" {
		return DefaultConfigDir
	}
	return c.ConfigDir
}

func (c *Config) EffectiveDBDir() string {
	if c == nil || strings.TrimSpace(c.DBDir) == "" {
		return DefaultDBDir
	}
	return c.DBDir
}

func (c *Config) EffectiveKeyPath() string {
	if c != nil && strings.TrimSpace(c.KeyPath) != "" {
		return strings.TrimSpace(c.KeyPath)
	}
	return filepath.Join(c.EffectiveConfigDir(), c.EffectiveClient()+".key")
}

func (c *Config) EffectiveLockFile() string {
	if c == nil || strings.TrimSpace(c.LockFile) == "" {
		return DefaultLockFile
	}
	return c.LockFile
}

func (c *Config) EffectiveLogFile() string {
	if c == nil || strings.TrimSpace(c.LogFile) == "" {
		return DefaultLogFile
	}
	return c.LogFile
}

func (c *Config) EffectiveDumpDatesPath() string {
	if c != nil && strings.TrimSpace(c.DumpDatesFile) != "" {
		return strings.TrimSpace(c.DumpDatesFile)
	}
	return DefaultDumpDatesPath
}

func (c *Config) EffectiveTier(override string) string {
	if strings.TrimSpace(override) != "" {
		return NormalizeTier(override)
	}
	if c != nil && strings.TrimSpace(c.DefaultTier) != "" {
		return NormalizeTier(c.DefaultTier)
	}
	return "Bulk"
}

func (c *Config) JobsDir() string {
	return filepath.Join(c.EffectiveDBDir(), "jobs")
}

func (c *Config) CatalogCacheDir(client string) string {
	if strings.TrimSpace(client) == "" {
		client = c.EffectiveClient()
	}
	return filepath.Join(c.EffectiveDBDir(), client)
}

// ApplyCLI overlays non-empty CLI flags onto the in-memory configuration.
func (c *Config) ApplyCLI(opts CLIOptions) {
	if strings.TrimSpace(opts.Bucket) != "" {
		c.Bucket = strings.TrimSpace(opts.Bucket)
	}
	if strings.TrimSpace(opts.Client) != "" {
		c.Client = strings.TrimSpace(opts.Client)
	}
	if strings.TrimSpace(opts.KeyPath) != "" {
		c.KeyPath = strings.TrimSpace(opts.KeyPath)
	}
	if strings.TrimSpace(opts.OpenSSLBin) != "" {
		c.OpenSSLBin = strings.TrimSpace(opts.OpenSSLBin)
	}
	if strings.TrimSpace(opts.Tier) != "" {
		c.DefaultTier = NormalizeTier(opts.Tier)
	}
	c.Normalize()
}

// ValidateS3 checks required S3 configuration parameters before remote calls.
func (c *Config) ValidateS3() error {
	if c == nil {
		return errors.New("configuration is nil")
	}
	if strings.TrimSpace(c.Bucket) == "" {
		return errors.New("S3 bucket (-b or config bucket) is not configured")
	}
	if strings.TrimSpace(c.EffectiveClient()) == "" {
		return errors.New("client identifier (-c or config client) is empty")
	}
	return nil
}

// applyLDLibraryPath prepends ldPath onto LD_LIBRARY_PATH in a child process environment slice.
func applyLDLibraryPath(baseEnv []string, ldPath string) []string {
	ldPath = strings.TrimSpace(ldPath)
	if ldPath == "" {
		return baseEnv
	}
	out := make([]string, 0, len(baseEnv)+1)
	var existing string
	for _, kv := range baseEnv {
		if strings.HasPrefix(kv, "LD_LIBRARY_PATH=") {
			existing = strings.TrimPrefix(kv, "LD_LIBRARY_PATH=")
			continue
		}
		out = append(out, kv)
	}
	if existing != "" {
		out = append(out, "LD_LIBRARY_PATH="+ldPath+":"+existing)
	} else {
		out = append(out, "LD_LIBRARY_PATH="+ldPath)
	}
	return out
}

// EnsureDirs creates required configuration, cache, and job directories with 0700 permissions.
func EnsureDirs(cfg *Config) error {
	dirs := []string{
		cfg.EffectiveConfigDir(),
		cfg.EffectiveDBDir(),
		cfg.JobsDir(),
		cfg.CatalogCacheDir(cfg.EffectiveClient()),
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0700); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}
	return nil
}

// LoadConfig loads /etc/tardis.d/config.json if present, returning OS defaults if absent.
func LoadConfig(path string) (*Config, error) {
	if strings.TrimSpace(path) == "" {
		path = DefaultConfigFile
	}
	cfg := DefaultConfig()
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			cfg.Normalize()
			return cfg, nil
		}
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	if err := json.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	cfg.Normalize()
	return cfg, nil
}

// SaveConfig atomically writes Config to disk with 0600 permissions.
func SaveConfig(path string, cfg *Config) error {
	if strings.TrimSpace(path) == "" {
		path = DefaultConfigFile
	}
	cfg.Normalize()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("mkdir config dir %s: %w", dir, err)
	}
	payload, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	payload = append(payload, '\n')

	tmp, err := os.CreateTemp(dir, ".config.json.tmp.*")
	if err != nil {
		return fmt.Errorf("create temp config: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp config: %w", err)
	}
	if _, err := tmp.Write(payload); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp config: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp config to %s: %w", path, err)
	}
	cleanup = false
	return nil
}

// Logf appends a timestamped line to /var/log/tardis on a best-effort basis.
func Logf(cfg *Config, format string, args ...any) {
	logPath := DefaultLogFile
	if cfg != nil {
		logPath = cfg.EffectiveLogFile()
	}
	_ = os.MkdirAll(filepath.Dir(logPath), 0755)
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return
	}
	defer f.Close()
	msg := fmt.Sprintf(format, args...)
	_, _ = fmt.Fprintf(f, "%s tardis[%d]: %s\n", time.Now().Format(time.RFC3339), os.Getpid(), msg)
}

// FileLock wraps an exclusive non-blocking flock(2) handle.
type FileLock struct {
	file *os.File
	path string
}

// AcquireLock acquires a non-blocking exclusive kernel flock(2) on lockPath.
func AcquireLock(lockPath string) (*FileLock, error) {
	if strings.TrimSpace(lockPath) == "" {
		lockPath = DefaultLockFile
	}
	if err := os.MkdirAll(filepath.Dir(lockPath), 0755); err != nil {
		return nil, fmt.Errorf("mkdir lock dir: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open lockfile %s: %w", lockPath, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		buf := make([]byte, 64)
		n, _ := f.Read(buf)
		_ = f.Close()
		holder := strings.TrimSpace(string(buf[:n]))
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			if holder != "" {
				return nil, fmt.Errorf("tardis: already running (pid %s)", holder)
			}
			return nil, errors.New("tardis: already running")
		}
		return nil, fmt.Errorf("flock %s: %w", lockPath, err)
	}
	_ = f.Truncate(0)
	_, _ = f.Seek(0, 0)
	_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
	_ = f.Sync()
	return &FileLock{file: f, path: lockPath}, nil
}

// Release unlocks and closes the underlying lockfile descriptor.
func (l *FileLock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	err := l.file.Close()
	l.file = nil
	return err
}

// MountInfo represents a mounted filesystem on OpenBSD or Linux.
type MountInfo struct {
	MountPoint      string
	Device          string // e.g. /dev/sd0a or /dev/mapper/vg0-root
	CanonicalDevice string // symlink-resolved block device (e.g. /dev/dm-0)
	RawDevice       string // OpenBSD raw character device (e.g. /dev/rsd0a)
	DUID            string // OpenBSD disklabel DUID or Linux filesystem UUID
	FSType          string // ffs, ext2, ext3, ext4, xfs, etc.
	Options         string
	DevID           uint64
}

func deriveRawDevice(dev string) string {
	if strings.HasPrefix(dev, "/dev/") && !strings.HasPrefix(dev, "/dev/r") {
		return "/dev/r" + strings.TrimPrefix(dev, "/dev/")
	}
	return dev
}

// MatchesDumpIdent reports whether a dumpdates device/DUID/UUID/LVM identifier matches this mount.
func (m MountInfo) MatchesDumpIdent(ident string) bool {
	ident = strings.TrimSpace(ident)
	if ident == "" {
		return false
	}
	if ident == m.Device || ident == m.CanonicalDevice || ident == m.RawDevice || ident == m.MountPoint {
		return true
	}
	if m.DUID != "" && (ident == m.DUID || strings.TrimPrefix(ident, "UUID=") == m.DUID) {
		return true
	}
	if deriveRawDevice(ident) == m.RawDevice && m.RawDevice != "" {
		return true
	}
	if strings.HasPrefix(ident, "/dev/") && m.CanonicalDevice != "" {
		if resolved, err := filepath.EvalSymlinks(ident); err == nil && resolved == m.CanonicalDevice {
			return true
		}
	}
	return false
}

// ResolveMount locates an active mountpoint by path. If multiple entries share the same mountpoint
// (such as Linux rootfs + real block mount), the last (topmost) mount table entry wins.
func ResolveMount(mountPoint string) (*MountInfo, error) {
	if strings.TrimSpace(mountPoint) == "" {
		return nil, errors.New("mountpoint (-m) is empty")
	}
	clean := filepath.Clean(mountPoint)
	mounts, err := ListMounts()
	if err != nil {
		return nil, err
	}
	var matched *MountInfo
	for i := range mounts {
		if mounts[i].MountPoint == clean {
			matched = &mounts[i]
		}
	}
	if matched != nil {
		return matched, nil
	}
	return nil, fmt.Errorf("%s: not mounted", clean)
}

// DumpDateEntry represents a single record in /etc/dumpdates or /var/lib/dumpdates.
type DumpDateEntry struct {
	Ident string
	Level int
	Time  time.Time
}

func parseDumpDateTimestamp(fields []string) (time.Time, bool) {
	// 5 tokens: "Mon Jan 2 15:04:05 2006"
	if len(fields) >= 5 {
		s5 := strings.Join(fields[:5], " ")
		if ts, err := time.ParseInLocation("Mon Jan 2 15:04:05 2006", s5, time.Local); err == nil {
			// Check if 6th token is a numeric timezone offset like "+0800" or "+1000" (Linux e2fsprogs dump)
			if len(fields) >= 6 {
				s6 := strings.Join(fields[:6], " ")
				if tsTZ, errTZ := time.Parse("Mon Jan 2 15:04:05 2006 -0700", s6); errTZ == nil {
					return tsTZ.Local(), true
				}
				if tsMST, errMST := time.ParseInLocation("Mon Jan 2 15:04:05 2006 MST", s6, time.Local); errMST == nil {
					return tsMST, true
				}
			}
			return ts, true
		}
	}
	// 6 tokens with TZ before year: "Mon Jan 2 15:04:05 MST 2006"
	if len(fields) >= 6 {
		s6 := strings.Join(fields[:6], " ")
		if ts, err := time.ParseInLocation("Mon Jan 2 15:04:05 MST 2006", s6, time.Local); err == nil {
			return ts, true
		}
	}
	return time.Time{}, false
}

// ReadDumpDates parses BSD and Linux dumpdates entries.
func ReadDumpDates(path string) ([]DumpDateEntry, error) {
	if strings.TrimSpace(path) == "" {
		path = DefaultDumpDatesPath
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var entries []DumpDateEntry
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 7 {
			continue
		}
		lvl, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		if ts, ok := parseDumpDateTimestamp(fields[2:]); ok {
			entries = append(entries, DumpDateEntry{
				Ident: fields[0],
				Level: lvl,
				Time:  ts,
			})
		}
	}
	return entries, scanner.Err()
}

// UpdateDumpDate records a completed dump timestamp for drivers (such as Linux xfsdump)
// that maintain their own binary inventory rather than writing dumpdates directly.
func UpdateDumpDate(path string, mnt *MountInfo, level int, ts time.Time) error {
	if mnt == nil {
		return nil
	}
	if strings.TrimSpace(path) == "" {
		path = DefaultDumpDatesPath
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	ident := mnt.CanonicalDevice
	if ident == "" {
		ident = mnt.Device
	}

	var keptLines []string
	if raw, err := os.ReadFile(path); err == nil {
		sc := bufio.NewScanner(strings.NewReader(string(raw)))
		for sc.Scan() {
			line := sc.Text()
			fields := strings.Fields(line)
			if len(fields) >= 7 {
				if lvl, lErr := strconv.Atoi(fields[1]); lErr == nil && lvl == level && mnt.MatchesDumpIdent(fields[0]) {
					continue
				}
			}
			if strings.TrimSpace(line) != "" {
				keptLines = append(keptLines, line)
			}
		}
	}

	newLine := fmt.Sprintf("%-16s %d %s", ident, level, ts.Local().Format("Mon Jan _2 15:04:05 2006 -0700"))
	keptLines = append(keptLines, newLine)
	content := strings.Join(keptLines, "\n") + "\n"
	return os.WriteFile(path, []byte(content), 0664)
}

// ReferenceDumpTime implements dump(8)'s incremental rule: for level N > 0,
// find the most recent dump timestamp for this filesystem whose level < N.
func ReferenceDumpTime(dumpDatesPath string, mnt *MountInfo, dumpLevel int) (time.Time, bool, error) {
	if dumpLevel <= 0 {
		return time.Time{}, false, nil
	}
	entries, err := ReadDumpDates(dumpDatesPath)
	if err != nil {
		return time.Time{}, false, err
	}
	var latest time.Time
	found := false
	for _, e := range entries {
		if mnt.MatchesDumpIdent(e.Ident) && e.Level < dumpLevel {
			if !found || e.Time.After(latest) {
				latest = e.Time
				found = true
			}
		}
	}
	return latest, found, nil
}

// ScanMountChanges walks mnt.MountPoint without crossing filesystem boundaries (-xdev),
// checking both mtime and ctime against since to match dump(8)/xfsdump(8) incremental semantics.
func ScanMountChanges(mnt *MountInfo, since time.Time, fullDump bool) (bool, int64, error) {
	var rootStat unix.Stat_t
	if err := unix.Lstat(mnt.MountPoint, &rootStat); err != nil {
		return false, 0, fmt.Errorf("lstat %s: %w", mnt.MountPoint, err)
	}
	devID := uint64(rootStat.Dev)
	changed := fullDump
	var totalBytes int64

	var walkDir func(dir string) error
	walkDir = func(dir string) error {
		f, err := os.Open(dir)
		if err != nil {
			if os.IsPermission(err) || os.IsNotExist(err) {
				return nil
			}
			return err
		}
		names, readErr := f.Readdirnames(-1)
		_ = f.Close()
		if readErr != nil {
			return nil
		}

		for _, name := range names {
			if strings.HasPrefix(name, ".tardis_stage_") || strings.HasPrefix(name, "xfsrestorehousekeeping") {
				continue
			}
			fullPath := filepath.Join(dir, name)
			var st unix.Stat_t
			if err := unix.Lstat(fullPath, &st); err != nil {
				continue
			}
			if uint64(st.Dev) != devID {
				continue
			}

			mtime := time.Unix(int64(st.Mtim.Sec), int64(st.Mtim.Nsec))
			ctime := time.Unix(int64(st.Ctim.Sec), int64(st.Ctim.Nsec))
			entryChanged := fullDump || since.IsZero() || mtime.After(since) || ctime.After(since)
			if entryChanged {
				changed = true
				if (uint32(st.Mode) & unix.S_IFMT) == unix.S_IFREG {
					totalBytes += int64(st.Size)
				}
			}
			if (uint32(st.Mode) & unix.S_IFMT) == unix.S_IFDIR {
				if err := walkDir(fullPath); err != nil {
					return err
				}
			}
		}
		return nil
	}

	if err := walkDir(mnt.MountPoint); err != nil {
		return false, 0, err
	}
	return changed, totalBytes, nil
}

// ArchiveMeta describes naming and S3 object keys for a backup snapshot.
type ArchiveMeta struct {
	BaseName   string    `json:"base_name"`
	DataFile   string    `json:"data_file"`
	DataKey    string    `json:"data_key"`
	DataHMAC   string    `json:"data_hmac"`
	ListFile   string    `json:"list_file"`
	ListKey    string    `json:"list_key"`
	ListHMAC   string    `json:"list_hmac"`
	Client     string    `json:"client"`
	MountSlug  string    `json:"mount_slug"`
	MountGuess string    `json:"mount_guess"`
	Level      int       `json:"level"`
	Timestamp  time.Time `json:"timestamp"`
}

// FormatArchiveBase constructs all object filenames/keys for a new snapshot.
func FormatArchiveBase(ts time.Time, client, mountPoint string, level int) ArchiveMeta {
	cleanMount := filepath.Clean(mountPoint)
	mountSlug := strings.ReplaceAll(cleanMount, "/", "_")
	base := fmt.Sprintf("%s_%s%s_%d", ts.Format(ArchiveTimeLayout), client, mountSlug, level)
	return buildArchiveMeta(base, client, mountSlug, cleanMount, level, ts)
}

func buildArchiveMeta(base, client, mountSlug, mountGuess string, level int, ts time.Time) ArchiveMeta {
	dataFile := base + ".zst.dat"
	listFile := base + "_list.zst.dat"
	return ArchiveMeta{
		BaseName:   base,
		DataFile:   dataFile,
		DataKey:    client + "/" + dataFile,
		DataHMAC:   client + "/" + base + ".hmac",
		ListFile:   listFile,
		ListKey:    client + "/" + listFile,
		ListHMAC:   client + "/" + base + "_list.hmac",
		Client:     client,
		MountSlug:  mountSlug,
		MountGuess: mountGuess,
		Level:      level,
		Timestamp:  ts,
	}
}

// ParseArchiveBase extracts snapshot metadata from an S3 key or archive filename.
func ParseArchiveBase(expectedClient, keyOrName string, knownMounts []MountInfo) (ArchiveMeta, bool) {
	s := strings.TrimSpace(keyOrName)
	var prefixClient string
	if idx := strings.LastIndexByte(s, '/'); idx != -1 {
		prefixClient = s[:idx]
		s = s[idx+1:]
	}

	for _, suffix := range []string{"_list.zst.dat", "_list.hmac", "_list.zst", ".zst.dat", ".hmac"} {
		if strings.HasSuffix(s, suffix) {
			s = strings.TrimSuffix(s, suffix)
			break
		}
	}

	if len(s) < 23 || s[19] != '_' {
		return ArchiveMeta{}, false
	}
	ts, err := time.ParseInLocation(ArchiveTimeLayout, s[:19], time.Local)
	if err != nil {
		return ArchiveMeta{}, false
	}

	rem := s[20:]
	lastUS := strings.LastIndexByte(rem, '_')
	if lastUS <= 0 || lastUS == len(rem)-1 {
		return ArchiveMeta{}, false
	}
	lvl, err := strconv.Atoi(rem[lastUS+1:])
	if err != nil || lvl < 0 || lvl > 9 {
		return ArchiveMeta{}, false
	}

	mid := rem[:lastUS]
	client := expectedClient
	if prefixClient != "" && !strings.Contains(prefixClient, "/") {
		client = prefixClient
	}

	var mountSlug string
	if client != "" && strings.HasPrefix(mid, client+"_") {
		mountSlug = mid[len(client):]
	} else {
		idx := strings.IndexByte(mid, '_')
		if idx <= 0 {
			return ArchiveMeta{}, false
		}
		client = mid[:idx]
		mountSlug = mid[idx:]
	}

	mountGuess := "/"
	if mountSlug != "_" {
		mountGuess = strings.ReplaceAll(mountSlug, "_", "/")
	}
	for _, m := range knownMounts {
		if strings.ReplaceAll(m.MountPoint, "/", "_") == mountSlug {
			mountGuess = m.MountPoint
			break
		}
	}

	return buildArchiveMeta(s, client, mountSlug, mountGuess, lvl, ts), true
}
