//go:build linux

package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

const (
	PlatformName         = "Linux"
	DefaultDumpDatesPath = "/var/lib/dumpdates"
	DefaultOpenSSLBin    = ""
	DefaultLDLibraryPath = "/opt/tardis/lib"

	xfsDumpMagic = "xFSdUmP\x00"
)

func getTermios(fd int) (*unix.Termios, error) {
	return unix.IoctlGetTermios(fd, unix.TCGETS)
}

func setTermios(fd int, term *unix.Termios) error {
	return unix.IoctlSetTermios(fd, unix.TCSETS, term)
}

func isSymlinkOpenErr(err error) bool {
	return errors.Is(err, unix.ELOOP)
}

func resolveSysBin(name string, extraDirs ...string) (string, error) {
	searchDirs := append([]string{}, extraDirs...)
	searchDirs = append(searchDirs, "/opt/tardis/bin", "/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/sbin", "/usr/bin", "/bin")
	for _, dir := range searchDirs {
		if dir == "" {
			continue
		}
		cand := filepath.Join(dir, name)
		if st, err := os.Stat(cand); err == nil && !st.IsDir() && (st.Mode()&0111) != 0 {
			return cand, nil
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("%s binary not found in system paths", name)
}

// ResolveSSLRuntime locates and validates either LibreSSL or OpenSSL on Linux,
// selecting the matching ChaCha20 CLI cipher flag (-chacha for LibreSSL, -chacha20 for OpenSSL).
func ResolveSSLRuntime(cfg *Config) (*SSLRuntime, error) {
	ldPath := ""
	if cfg != nil && strings.TrimSpace(cfg.LDLibraryPath) != "" {
		ldPath = strings.TrimSpace(cfg.LDLibraryPath)
	} else if st, err := os.Stat(DefaultLDLibraryPath); err == nil && st.IsDir() {
		ldPath = DefaultLDLibraryPath
	}

	var candidates []string
	if cfg != nil && strings.TrimSpace(cfg.OpenSSLBin) != "" {
		candidates = append(candidates, strings.TrimSpace(cfg.OpenSSLBin))
	} else {
		for _, c := range []string{"/opt/tardis/bin/openssl", "/usr/local/bin/openssl"} {
			if st, err := os.Stat(c); err == nil && !st.IsDir() && (st.Mode()&0111) != 0 {
				candidates = append(candidates, c)
			}
		}
		if p, err := exec.LookPath("openssl"); err == nil {
			candidates = append(candidates, p)
		}
		candidates = append(candidates, "/usr/bin/openssl", "/bin/openssl")
	}

	var lastErr error
	seen := make(map[string]bool)
	for _, bin := range candidates {
		if bin == "" || seen[bin] {
			continue
		}
		seen[bin] = true

		cmd := exec.Command(bin, "version")
		if ldPath != "" {
			cmd.Env = applyLDLibraryPath(os.Environ(), ldPath)
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			lastErr = fmt.Errorf("execute %s version: %w", bin, err)
			continue
		}

		ver := strings.TrimSpace(string(out))
		fields := strings.Fields(ver)
		if len(fields) == 0 {
			lastErr = fmt.Errorf("empty version output from %s", bin)
			continue
		}

		var provider string
		var cipherChoices []string
		switch {
		case fields[0] == "LibreSSL":
			provider = "LibreSSL"
			cipherChoices = []string{"-chacha", "-chacha20"}
		case strings.HasPrefix(fields[0], "OpenSSL"):
			provider = "OpenSSL"
			cipherChoices = []string{"-chacha20", "-chacha"}
		default:
			lastErr = fmt.Errorf("unsupported SSL implementation %q at %s", ver, bin)
			continue
		}

		selectedCipher := ""
		for _, flagName := range cipherChoices {
			if probeCipherSupport(bin, ldPath, flagName) {
				selectedCipher = flagName
				break
			}
		}
		if selectedCipher == "" {
			lastErr = fmt.Errorf("%s (%s) does not support ChaCha20 stream encryption with PBKDF2", bin, ver)
			continue
		}

		return &SSLRuntime{
			Binary:        bin,
			Version:       ver,
			Provider:      provider,
			CipherFlag:    selectedCipher,
			LDLibraryPath: ldPath,
		}, nil
	}

	if lastErr != nil {
		return nil, lastErr
	}
	return nil, errors.New("neither OpenSSL nor LibreSSL binary found")
}

func probeCipherSupport(bin, ldPath, cipherFlag string) bool {
	cmd := exec.Command(bin, "enc", cipherFlag, "-pbkdf2", "-iter", "1000", "-salt", "-e", "-pass", "pass:tardisprobe")
	if ldPath != "" {
		cmd.Env = applyLDLibraryPath(os.Environ(), ldPath)
	}
	cmd.Stdin = bytes.NewReader([]byte("probe"))
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run() == nil
}

func unescapeMountField(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var out strings.Builder
	out.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				out.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		out.WriteByte(s[i])
	}
	return out.String()
}

func loadLinuxUUIDMap() map[string]string {
	uuidByCanon := make(map[string]string)
	entries, err := os.ReadDir("/dev/disk/by-uuid")
	if err != nil {
		return uuidByCanon
	}
	for _, de := range entries {
		full := filepath.Join("/dev/disk/by-uuid", de.Name())
		if canon, err := filepath.EvalSymlinks(full); err == nil && canon != "" {
			uuidByCanon[canon] = de.Name()
		}
	}
	return uuidByCanon
}

// ListMounts parses Linux `/proc/self/mountinfo` and resolves LVM/mapper/UUID symlinks.
func ListMounts() ([]MountInfo, error) {
	raw, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return nil, fmt.Errorf("read /proc/self/mountinfo: %w", err)
	}
	uuidMap := loadLinuxUUIDMap()
	var mounts []MountInfo

	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		sepIdx := strings.Index(line, " - ")
		if sepIdx <= 0 {
			continue
		}
		leftFields := strings.Fields(line[:sepIdx])
		rightFields := strings.Fields(line[sepIdx+3:])
		if len(leftFields) < 6 || len(rightFields) < 2 {
			continue
		}

		mntPoint := filepath.Clean(unescapeMountField(leftFields[4]))
		mntOpts := leftFields[5]
		fsType := unescapeMountField(rightFields[0])
		dev := unescapeMountField(rightFields[1])

		canonDev := dev
		if strings.HasPrefix(dev, "/dev/") {
			if resolved, rErr := filepath.EvalSymlinks(dev); rErr == nil && resolved != "" {
				canonDev = resolved
			}
		}

		info := MountInfo{
			MountPoint:      mntPoint,
			Device:          dev,
			CanonicalDevice: canonDev,
			RawDevice:       deriveRawDevice(canonDev),
			DUID:            uuidMap[canonDev],
			FSType:          fsType,
			Options:         mntOpts,
		}
		var st unix.Stat_t
		if err := unix.Stat(mntPoint, &st); err == nil {
			info.DevID = uint64(st.Dev)
		}
		mounts = append(mounts, info)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return mounts, nil
}

func isExtDumpFS(fsType string) bool {
	switch fsType {
	case "ext2", "ext3", "ext4":
		return true
	default:
		return false
	}
}

// ValidateDumpMount verifies the Linux mountpoint uses ext2/ext3/ext4 or xfs and required tools are installed.
func ValidateDumpMount(mnt *MountInfo) error {
	if mnt == nil {
		return errors.New("nil mount info")
	}
	switch {
	case isExtDumpFS(mnt.FSType):
		if _, err := resolveSysBin("dump"); err != nil {
			return fmt.Errorf("ext filesystem %s requires dump(8): %w", mnt.MountPoint, err)
		}
		if _, err := resolveSysBin("restore"); err != nil {
			return fmt.Errorf("ext filesystem %s requires restore(8): %w", mnt.MountPoint, err)
		}
		return nil

	case mnt.FSType == "xfs":
		if _, err := resolveSysBin("xfsdump"); err != nil {
			return fmt.Errorf("xfs filesystem %s requires xfsdump(8): %w", mnt.MountPoint, err)
		}
		if _, err := resolveSysBin("xfsrestore"); err != nil {
			return fmt.Errorf("xfs filesystem %s requires xfsrestore(8): %w", mnt.MountPoint, err)
		}
		return nil

	default:
		return fmt.Errorf("%s is %s (Linux tardis supports ext2/ext3/ext4 via dump and xfs via xfsdump)", mnt.MountPoint, mnt.FSType)
	}
}

// BuildDumpPipeline constructs either an ext2/3/4 `dump`/`restore` pipeline or an XFS `xfsdump`/`xfsrestore` pipeline.
func BuildDumpPipeline(ctx context.Context, cfg *Config, mnt *MountInfo, meta ArchiveMeta, level int) (*DumpPipeline, error) {
	if err := ValidateDumpMount(mnt); err != nil {
		return nil, err
	}
	dumpDatesFile := DefaultDumpDatesPath
	if cfg != nil {
		dumpDatesFile = cfg.EffectiveDumpDatesPath()
	}
	if err := os.MkdirAll(filepath.Dir(dumpDatesFile), 0755); err == nil {
		if f, oErr := os.OpenFile(dumpDatesFile, os.O_CREATE|os.O_RDWR, 0664); oErr == nil {
			_ = f.Close()
		}
	}

	if isExtDumpFS(mnt.FSType) {
		dumpBin, err := resolveSysBin("dump")
		if err != nil {
			return nil, err
		}
		restoreBin, err := resolveSysBin("restore")
		if err != nil {
			return nil, err
		}

		dumpCmd := exec.CommandContext(
			ctx,
			dumpBin,
			fmt.Sprintf("-%da", level),
			"-u",
			"-D", dumpDatesFile,
			"-f", "-",
			mnt.MountPoint,
		)
		dumpCmd.SysProcAttr = &unix.SysProcAttr{Setsid: true}

		tocCmd := exec.CommandContext(ctx, restoreBin, "-t", "-v", "-f", "-")
		tocCmd.SysProcAttr = &unix.SysProcAttr{Setsid: true}

		return &DumpPipeline{
			DriverName:      "linux-ext-dump",
			DumpCmd:         dumpCmd,
			TocCmd:          tocCmd,
			ParseTOC:        parseRestoreTVOutput,
			RecordDumpDates: false,
		}, nil
	}

	// XFS pipeline via xfsdump / xfsrestore
	xfsDumpBin, err := resolveSysBin("xfsdump")
	if err != nil {
		return nil, err
	}
	xfsRestoreBin, err := resolveSysBin("xfsrestore")
	if err != nil {
		return nil, err
	}
	_ = os.MkdirAll("/var/lib/xfsdump/inventory", 0755)

	sessionLabel := meta.BaseName
	if len(sessionLabel) > 250 {
		sessionLabel = sessionLabel[:250]
	}
	mediaLabel := fmt.Sprintf("tardis_L%d", level)

	// xfsdump(8): lone "-" must follow all options and immediately precede the mountpoint (no -f flag).
	dumpCmd := exec.CommandContext(
		ctx,
		xfsDumpBin,
		"-F",
		"-v", "silent",
		"-l", strconv.Itoa(level),
		"-L", sessionLabel,
		"-M", mediaLabel,
		"-",
		mnt.MountPoint,
	)
	dumpCmd.SysProcAttr = &unix.SysProcAttr{Setsid: true}

	// xfsrestore(8): -F inhibits prompts, -J prevents TOC reader from mutating inventory, -t lists paths from stdin "-".
	tocCmd := exec.CommandContext(
		ctx,
		xfsRestoreBin,
		"-F",
		"-J",
		"-v", "silent",
		"-t",
		"-",
	)
	tocCmd.SysProcAttr = &unix.SysProcAttr{Setsid: true}

	return &DumpPipeline{
		DriverName:      "linux-xfsdump",
		DumpCmd:         dumpCmd,
		TocCmd:          tocCmd,
		ParseTOC:        parseXfsRestoreTOCOutput,
		RecordDumpDates: true,
	}, nil
}

func parseXfsRestoreTOCOutput(r io.Reader, mountPoint string, countAtomic *atomic.Int64) ([]CatalogEntry, error) {
	var entries []CatalogEntry
	seen := make(map[string]struct{})
	br := bufio.NewReaderSize(r, 128*1024)

	for {
		line, err := br.ReadString('\n')
		if len(line) > 0 {
			clean := strings.TrimRight(line, "\r\n")
			trimmed := strings.TrimSpace(clean)
			if trimmed != "" &&
				!strings.HasPrefix(trimmed, "xfsrestore:") &&
				!strings.HasPrefix(trimmed, "xfsdump:") {

				rawPath := clean
				var fallbackIno uint64

				// Handle optional leading inode number column if emitted by older/verbose xfsrestore builds.
				fields := strings.Fields(trimmed)
				if len(fields) >= 2 {
					if parsedIno, pErr := strconv.ParseUint(fields[0], 10, 64); pErr == nil {
						idx := strings.Index(clean, fields[0])
						afterIno := strings.TrimLeft(clean[idx+len(fields[0]):], " \t")
						if afterIno != "" {
							if _, fullErr := os.Lstat(filepath.Join(mountPoint, clean)); fullErr != nil {
								if _, strippedErr := os.Lstat(filepath.Join(mountPoint, afterIno)); strippedErr == nil {
									fallbackIno = parsedIno
									rawPath = afterIno
								}
							}
						}
					}
				}

				rel := strings.TrimPrefix(rawPath, "./")
				rel = strings.TrimLeft(rel, "/")
				rel = strings.TrimRight(rel, "/")
				if rel != "" && rel != "." &&
					!strings.HasPrefix(rel, ".tardis_stage_") &&
					!strings.HasPrefix(rel, "xfsrestorehousekeeping") {
					if _, dup := seen[rel]; !dup {
						seen[rel] = struct{}{}
						ent := parseStatToCatalogEntry(mountPoint, rel, "", fallbackIno)
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

// BuildRestoreCmd auto-detects XFS (`xFSdUmP\0`) vs BSD/ext2-4 `dump` streams from the decompressed header
// and constructs the non-interactive `xfsrestore` or `restore` extraction command targeting stageDir.
func BuildRestoreCmd(ctx context.Context, magicHeader []byte, stageDir string, mountPoint string, markedPaths []string, fullSnapshot bool) (*exec.Cmd, error) {
	isXFS := len(magicHeader) >= len(xfsDumpMagic) && string(magicHeader[:len(xfsDumpMagic)]) == xfsDumpMagic
	if !isXFS && len(magicHeader) == 0 {
		if mnt, err := ResolveMount(mountPoint); err == nil && mnt.FSType == "xfs" {
			isXFS = true
		}
	}

	if isXFS {
		xfsRestoreBin, err := resolveSysBin("xfsrestore")
		if err != nil {
			return nil, fmt.Errorf("archive is an xfsdump stream: %w", err)
		}
		args := []string{"-F", "-J", "-v", "silent"}
		if !fullSnapshot && len(markedPaths) > 0 {
			for _, p := range markedPaths {
				cleanRel := strings.Trim(strings.TrimPrefix(p, "./"), "/")
				if cleanRel != "" {
					args = append(args, "-s", cleanRel)
				}
			}
		}
		// xfsrestore(8): lone "-" reads standard input and must immediately precede destination directory.
		args = append(args, "-", stageDir)
		cmd := exec.CommandContext(ctx, xfsRestoreBin, args...)
		cmd.Dir = stageDir
		cmd.SysProcAttr = &unix.SysProcAttr{Setsid: true}
		return cmd, nil
	}

	restoreBin, err := resolveSysBin("restore")
	if err != nil {
		return nil, fmt.Errorf("archive is an ext/BSD dump stream: %w", err)
	}
	args := []string{"-x", "-y", "-f", "-"}
	if !fullSnapshot && len(markedPaths) > 0 {
		for _, p := range markedPaths {
			cleanRel := strings.Trim(strings.TrimPrefix(p, "./"), "/")
			if cleanRel != "" {
				args = append(args, escapeRestoreGlob(cleanRel))
			}
		}
	}
	cmd := exec.CommandContext(ctx, restoreBin, args...)
	cmd.Dir = stageDir
	cmd.SysProcAttr = &unix.SysProcAttr{Setsid: true}
	return cmd, nil
}

// ProbeHostTools reports availability of ext2/3/4 dump/restore and XFS xfsdump/xfsrestore for `tardis -S`.
func ProbeHostTools() []string {
	var lines []string
	for _, item := range [][2]string{
		{"ext dump", "dump"},
		{"ext restore", "restore"},
		{"xfsdump", "xfsdump"},
		{"xfsrestore", "xfsrestore"},
	} {
		if path, err := resolveSysBin(item[1]); err == nil {
			lines = append(lines, fmt.Sprintf("%-11s: READY — %s", item[0], path))
		} else {
			lines = append(lines, fmt.Sprintf("%-11s: NOT INSTALLED (%s)", item[0], item[1]))
		}
	}
	return lines
}
