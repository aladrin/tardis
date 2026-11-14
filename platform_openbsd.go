//go:build openbsd

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	PlatformName         = "OpenBSD"
	DefaultDumpDatesPath = "/etc/dumpdates"
	DefaultOpenSSLBin    = "/usr/bin/openssl"
	DefaultLDLibraryPath = ""

	openbsdDumpBin    = "/sbin/dump"
	openbsdRestoreBin = "/sbin/restore"
	openbsdMountBin   = "/sbin/mount"
)

func getTermios(fd int) (*unix.Termios, error) {
	return unix.IoctlGetTermios(fd, unix.TIOCGETA)
}

func setTermios(fd int, term *unix.Termios) error {
	return unix.IoctlSetTermios(fd, unix.TIOCSETA, term)
}

func isSymlinkOpenErr(err error) bool {
	return errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EFTYPE)
}

// ResolveSSLRuntime strictly enforces OpenBSD LibreSSL with the -chacha cipher flag.
func ResolveSSLRuntime(cfg *Config) (*SSLRuntime, error) {
	bin := DefaultOpenSSLBin
	if cfg != nil && strings.TrimSpace(cfg.OpenSSLBin) != "" {
		bin = strings.TrimSpace(cfg.OpenSSLBin)
	}
	out, err := exec.Command(bin, "version").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("execute %s version: %w", bin, err)
	}
	ver := strings.TrimSpace(string(out))
	fields := strings.Fields(ver)
	if len(fields) == 0 || fields[0] != "LibreSSL" {
		return nil, fmt.Errorf("tardis: OpenBSD requires LibreSSL, got %q", ver)
	}
	return &SSLRuntime{
		Binary:        bin,
		Version:       ver,
		Provider:      "LibreSSL",
		CipherFlag:    "-chacha",
		LDLibraryPath: "",
	}, nil
}

// ListMounts parses OpenBSD `/sbin/mount -v` output including disklabel DUIDs and st_dev IDs.
func ListMounts() ([]MountInfo, error) {
	out, err := exec.Command(openbsdMountBin, "-v").Output()
	if err != nil {
		return nil, fmt.Errorf("execute %s -v: %w", openbsdMountBin, err)
	}
	var mounts []MountInfo
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		idxOn := strings.Index(line, " on ")
		if idxOn == -1 {
			continue
		}
		left := strings.TrimSpace(line[:idxOn])
		right := line[idxOn+4:]
		idxType := strings.LastIndex(right, " type ")
		if idxType == -1 {
			continue
		}
		mntPoint := filepath.Clean(strings.TrimSpace(right[:idxType]))
		rest := strings.TrimSpace(right[idxType+6:])

		fsType := rest
		opts := ""
		if idxParen := strings.Index(rest, " ("); idxParen != -1 && strings.HasSuffix(rest, ")") {
			fsType = strings.TrimSpace(rest[:idxParen])
			opts = rest[idxParen+2 : len(rest)-1]
		}

		dev := left
		duid := ""
		if idxParen := strings.Index(left, " ("); idxParen != -1 && strings.HasSuffix(left, ")") {
			dev = strings.TrimSpace(left[:idxParen])
			duid = strings.TrimSpace(left[idxParen+2 : len(left)-1])
		}

		info := MountInfo{
			MountPoint:      mntPoint,
			Device:          dev,
			CanonicalDevice: dev,
			RawDevice:       deriveRawDevice(dev),
			DUID:            duid,
			FSType:          fsType,
			Options:         opts,
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

// ValidateDumpMount confirms the target mountpoint is an OpenBSD FFS filesystem and base tools exist.
func ValidateDumpMount(mnt *MountInfo) error {
	if mnt == nil {
		return errors.New("nil mount info")
	}
	if mnt.FSType != "ffs" {
		return fmt.Errorf("%s is %s (OpenBSD /sbin/dump requires ffs)", mnt.MountPoint, mnt.FSType)
	}
	for _, bin := range []string{openbsdDumpBin, openbsdRestoreBin} {
		if _, err := os.Stat(bin); err != nil {
			return fmt.Errorf("required base binary %s missing: %w", bin, err)
		}
	}
	return nil
}

// BuildDumpPipeline constructs the OpenBSD `/sbin/dump` and `/sbin/restore -t -v` commands.
func BuildDumpPipeline(ctx context.Context, _ *Config, mnt *MountInfo, _ ArchiveMeta, level int) (*DumpPipeline, error) {
	if err := ValidateDumpMount(mnt); err != nil {
		return nil, err
	}
	dumpCmd := exec.CommandContext(ctx, openbsdDumpBin, fmt.Sprintf("-%da", level), "-u", "-f", "-", mnt.MountPoint)
	dumpCmd.SysProcAttr = &unix.SysProcAttr{Setsid: true}

	tocCmd := exec.CommandContext(ctx, openbsdRestoreBin, "-t", "-v", "-f", "-")
	tocCmd.SysProcAttr = &unix.SysProcAttr{Setsid: true}

	return &DumpPipeline{
		DriverName:      "openbsd-ffs-dump",
		DumpCmd:         dumpCmd,
		TocCmd:          tocCmd,
		ParseTOC:        parseRestoreTVOutput,
		RecordDumpDates: false,
	}, nil
}

// BuildRestoreCmd constructs the OpenBSD `/sbin/restore` command inside stageDir.
func BuildRestoreCmd(ctx context.Context, magicHeader []byte, stageDir string, _ string, markedPaths []string, fullSnapshot bool) (*exec.Cmd, error) {
	if len(magicHeader) >= 8 && string(magicHeader[:8]) == "xFSdUmP\x00" {
		return nil, errors.New("archive is a Linux xfsdump stream; restore on a Linux host with xfsrestore")
	}
	if _, err := os.Stat(openbsdRestoreBin); err != nil {
		return nil, fmt.Errorf("missing %s: %w", openbsdRestoreBin, err)
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

	cmd := exec.CommandContext(ctx, openbsdRestoreBin, args...)
	cmd.Dir = stageDir
	cmd.SysProcAttr = &unix.SysProcAttr{Setsid: true}
	return cmd, nil
}

// ProbeHostTools reports OpenBSD backup binaries for `tardis -S`.
func ProbeHostTools() []string {
	var lines []string
	for _, item := range [][2]string{
		{"ffs dump", openbsdDumpBin},
		{"ffs restore", openbsdRestoreBin},
	} {
		if _, err := os.Stat(item[1]); err == nil {
			lines = append(lines, fmt.Sprintf("%-11s: READY — %s", item[0], item[1]))
		} else {
			lines = append(lines, fmt.Sprintf("%-11s: MISSING (%s)", item[0], item[1]))
		}
	}
	return lines
}
