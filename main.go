//go:build openbsd

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TIOCGETA)
	return err == nil
}

func printUsage(w io.Writer, prog string) {
	fmt.Fprintf(w, "usage: %s [-u | -J | -S] [-f config] [-b bucket] [-c client] [-k key]\n", prog)
	fmt.Fprintf(w, "       %*s [-m mount [-d 0..9 | -r archive [-t Standard|Bulk] [-o dest]]]\n", len(prog), "")
	fmt.Fprintf(w, "       %*s [-q ALL|STANDARD|DEEP_ARCHIVE] [-D archive_or_object]\n\n", len(prog), "")
	fmt.Fprintf(w, "modes:\n")
	fmt.Fprintf(w, "  (no action)  launch interactive console UI when attached to a TTY\n")
	fmt.Fprintf(w, "  -u, --ui     launch interactive console UI\n")
	fmt.Fprintf(w, "  -d level     dump filesystem at -m (level 0..9) to S3 DEEP_ARCHIVE + STANDARD catalog\n")
	fmt.Fprintf(w, "  -r archive   restore archive into -m (auto-initiates/polls Glacier thaw if frozen)\n")
	fmt.Fprintf(w, "  -J           poll and execute pending/thawing restore jobs (cron-friendly)\n")
	fmt.Fprintf(w, "  -q class     query S3 objects for client (ALL | STANDARD | DEEP_ARCHIVE)\n")
	fmt.Fprintf(w, "  -D target    delete snapshot base name (all 4 objects) or single S3 object\n")
	fmt.Fprintf(w, "  -S           run LibreSSL ChaCha20+PBKDF2+HMAC-SHA256 & S3 self-test\n\n")
	fmt.Fprintf(w, "options:\n")
	fmt.Fprintf(w, "  -f path      configuration file (default %s)\n", DefaultConfigFile)
	fmt.Fprintf(w, "  -b bucket    S3 bucket name\n")
	fmt.Fprintf(w, "  -c client    client identifier (default short hostname)\n")
	fmt.Fprintf(w, "  -k path      LibreSSL/HMAC 0400 keyfile path\n")
	fmt.Fprintf(w, "  -m mount     OpenBSD FFS mountpoint (e.g. /, /var, /home)\n")
	fmt.Fprintf(w, "  -t tier      Glacier retrieval tier: Standard (3-5h) | Bulk (5-12h)\n")
	fmt.Fprintf(w, "  -o dest      restore to alternate filename/directory instead of overwriting\n")
}

func parseCLI(prog string, args []string) (CLIOptions, bool, bool, error) {
	opts := CLIOptions{
		ConfigPath: DefaultConfigFile,
		DumpLevel:  -1,
	}
	var showHelp bool
	var selfTest bool

	fs := flag.NewFlagSet(prog, flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	fs.BoolVar(&showHelp, "h", false, "help")
	fs.BoolVar(&showHelp, "help", false, "help")
	fs.BoolVar(&opts.ForceTUI, "u", false, "interactive UI")
	fs.BoolVar(&opts.ForceTUI, "ui", false, "interactive UI")
	fs.BoolVar(&opts.RunJobs, "J", false, "poll pending jobs")
	fs.BoolVar(&selfTest, "S", false, "self-test")

	fs.StringVar(&opts.ConfigPath, "f", DefaultConfigFile, "config path")
	fs.StringVar(&opts.Bucket, "b", "", "S3 bucket")
	fs.StringVar(&opts.Client, "c", "", "client name")
	fs.StringVar(&opts.KeyPath, "k", "", "keyfile path")
	fs.StringVar(&opts.Tier, "t", "", "Glacier tier")
	fs.StringVar(&opts.Mount, "m", "", "mountpoint")
	fs.IntVar(&opts.DumpLevel, "d", -1, "dump level")
	fs.StringVar(&opts.RestoreObj, "r", "", "restore archive")
	fs.StringVar(&opts.RestoreDst, "o", "", "alternate restore path")
	fs.StringVar(&opts.DeleteObj, "D", "", "delete object or snapshot")
	fs.StringVar(&opts.QueryMode, "q", "", "query storage class")

	if err := fs.Parse(args); err != nil {
		return opts, false, false, err
	}
	if showHelp {
		return opts, true, false, nil
	}
	if fs.NArg() > 0 {
		return opts, false, false, fmt.Errorf("unexpected positional argument %q", fs.Arg(0))
	}
	if opts.Tier != "" {
		up := strings.ToLower(strings.TrimSpace(opts.Tier))
		if up != "standard" && up != "bulk" {
			return opts, false, false, fmt.Errorf("invalid Glacier tier %q (expected Standard or Bulk)", opts.Tier)
		}
	}
	return opts, false, selfTest, nil
}

func runQuery(ctx context.Context, eng *Engine, mode string) error {
	q := strings.ToUpper(strings.TrimSpace(mode))
	switch q {
	case "ALL", "STANDARD", "DEEP_ARCHIVE":
	default:
		return fmt.Errorf("invalid -q mode %q (expected ALL, STANDARD, or DEEP_ARCHIVE)", mode)
	}

	cfg := eng.ConfigSnapshot()
	store, err := eng.NewStore(ctx)
	if err != nil {
		return err
	}
	client := cfg.EffectiveClient()
	objs, err := store.ListObjects(ctx, client, q)
	if err != nil {
		return err
	}

	// Probe HeadObject on archive-class dumps with bounded concurrency to report live thaw states.
	jobs := make(chan int, len(objs))
	for i := range objs {
		if (objs[i].StorageClass == "DEEP_ARCHIVE" || objs[i].StorageClass == "GLACIER") &&
			strings.HasSuffix(objs[i].Key, ".zst.dat") {
			jobs <- i
		}
	}
	close(jobs)

	var wg sync.WaitGroup
	for w := 0; w < 6; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				if st, hErr := store.HeadStatus(ctx, objs[idx].Key); hErr == nil {
					objs[idx].GlacierState = st.GlacierState
					objs[idx].RestoreExpiry = st.RestoreExpiry
				}
			}
		}()
	}
	wg.Wait()

	if len(objs) == 0 {
		return nil
	}

	fmt.Printf("%-13s  %-8s  %12s  %-19s  %s\n", "STORAGE_CLASS", "STATE", "SIZE", "LAST_MODIFIED", "KEY")
	for _, o := range objs {
		ts := "—"
		if !o.LastModified.IsZero() {
			ts = o.LastModified.Format("2006-01-02 15:04:05")
		}
		fmt.Printf("%-13s  %-8s  %12s  %-19s  %s\n",
			o.StorageClass,
			o.GlacierState,
			FormatBytes(o.Size),
			ts,
			o.Key,
		)
	}
	return nil
}

func runDelete(ctx context.Context, eng *Engine, target string) error {
	cfg := eng.ConfigSnapshot()
	store, err := eng.NewStore(ctx)
	if err != nil {
		return err
	}
	client := cfg.EffectiveClient()
	arg := strings.TrimSpace(target)

	hasExplicitExt := strings.HasSuffix(arg, ".zst.dat") ||
		strings.HasSuffix(arg, ".hmac") ||
		strings.HasSuffix(arg, ".zst")

	if !hasExplicitExt {
		mounts, _ := ListMounts()
		if meta, ok := ParseArchiveBase(client, arg, mounts); ok {
			if err := store.DeleteSnapshot(ctx, meta); err != nil {
				return err
			}
			fmt.Printf("deleted snapshot bundle: %s (%s, %s, %s, %s)\n",
				meta.BaseName, meta.DataKey, meta.DataHMAC, meta.ListKey, meta.ListHMAC)
			return nil
		}
	}

	key := ResolveClientKey(client, arg)
	if err := store.DeleteObject(ctx, key); err != nil {
		return err
	}
	fmt.Printf("deleted object: s3://%s/%s\n", cfg.Bucket, key)
	return nil
}

func runSelfTest(ctx context.Context, eng *Engine) error {
	cfg := eng.ConfigSnapshot()
	if err := EnsureDirs(cfg); err != nil {
		return err
	}
	client := cfg.EffectiveClient()
	km, err := eng.EnsureKeyForClient(client, true)
	if err != nil {
		return err
	}
	defer km.Wipe()

	ver, err := SelfTestLibreSSL(ctx, km)
	if err != nil {
		return err
	}
	keyAction := "verified"
	if km.Created {
		keyAction = "generated (0400)"
	}
	fmt.Printf("crypto : PASS — %s | ChaCha20-PBKDF2(100k) + HMAC-SHA256\n", ver)
	fmt.Printf("keyfile: %s [%s, id=%s]\n", km.Path, keyAction, km.Fingerprint())

	if strings.TrimSpace(cfg.Bucket) == "" {
		fmt.Printf("s3     : SKIPPED — bucket not configured yet (use -b or TUI Settings)\n")
		return nil
	}
	store, err := eng.NewStore(ctx)
	if err != nil {
		return err
	}
	if err := store.TestConnection(ctx, client); err != nil {
		return err
	}
	fmt.Printf("s3     : PASS — s3://%s/%s/ reachable (%s)\n", cfg.Bucket, client, cfg.Region)
	return nil
}

func main() {
	prog := filepath.Base(os.Args[0])
	opts, showHelp, selfTest, err := parseCLI(prog, os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n\n", prog, err)
		printUsage(os.Stderr, prog)
		os.Exit(1)
	}
	if showHelp {
		printUsage(os.Stdout, prog)
		return
	}

	actionCount := 0
	if opts.ForceTUI {
		actionCount++
	}
	if opts.DumpLevel != -1 {
		actionCount++
	}
	if opts.RestoreObj != "" {
		actionCount++
	}
	if opts.DeleteObj != "" {
		actionCount++
	}
	if opts.QueryMode != "" {
		actionCount++
	}
	if opts.RunJobs {
		actionCount++
	}
	if selfTest {
		actionCount++
	}
	if actionCount > 1 {
		fmt.Fprintf(os.Stderr, "%s: conflicting action flags specified\n\n", prog)
		printUsage(os.Stderr, prog)
		os.Exit(1)
	}

	cfg, err := LoadConfig(opts.ConfigPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", prog, err)
		os.Exit(1)
	}
	cfg.ApplyCLI(opts)

	eng := NewEngine(cfg, opts.ConfigPath)

	ctx, stopSignals := signal.NotifyContext(context.Background(), unix.SIGINT, unix.SIGTERM)
	defer stopSignals()

	ttyOut := isTerminal(os.Stderr)

	// Default to TUI when invoked interactively with no CLI action flags (or explicitly via -u/--ui).
	if opts.ForceTUI || actionCount == 0 {
		if !opts.ForceTUI && !isTerminal(os.Stdin) {
			printUsage(os.Stderr, prog)
			os.Exit(1)
		}
		if err := RunTUI(ctx, eng); err != nil {
			fmt.Fprintf(os.Stderr, "%s: UI error: %v\n", prog, err)
			os.Exit(1)
		}
		return
	}

	switch {
	case selfTest:
		if err := runSelfTest(ctx, eng); err != nil {
			fmt.Fprintf(os.Stderr, "%s: self-test failed: %v\n", prog, err)
			os.Exit(1)
		}

	case opts.DumpLevel != -1:
		if strings.TrimSpace(opts.Mount) == "" {
			fmt.Fprintf(os.Stderr, "%s: -m missing argument\n\n", prog)
			printUsage(os.Stderr, prog)
			os.Exit(1)
		}
		var onProg func(DumpProgress)
		if ttyOut {
			onProg = func(p DumpProgress) {
				fmt.Fprintf(os.Stderr, "\r\033[K[%s] raw=%s cipher=%s rate=%.2f MiB/s catalog=%d",
					p.Phase,
					FormatBytes(p.RawBytes),
					FormatBytes(p.CipherBytes),
					p.SpeedBPS/1048576.0,
					p.CatalogCount,
				)
			}
		}
		meta, changed, err := eng.RunDump(ctx, opts.Mount, opts.DumpLevel, onProg)
		if ttyOut {
			fmt.Fprint(os.Stderr, "\r\033[K")
		}
		if err != nil {
			if errors.Is(err, context.Canceled) {
				fmt.Fprintf(os.Stderr, "%s: dump interrupted\n", prog)
			} else {
				fmt.Fprintf(os.Stderr, "%s: %v\n", prog, err)
			}
			os.Exit(1)
		}
		if !changed {
			fmt.Printf("%s: no change for %s (dump level %d)\n", prog, opts.Mount, opts.DumpLevel)
			return
		}
		if ttyOut && meta != nil {
			fmt.Printf("%s: uploaded %s (+ HMAC & S3 STANDARD catalog)\n", prog, meta.DataKey)
		}

	case opts.RestoreObj != "":
		if strings.TrimSpace(opts.Mount) == "" {
			fmt.Fprintf(os.Stderr, "%s: -m missing argument\n\n", prog)
			printUsage(os.Stderr, prog)
			os.Exit(1)
		}
		lastState := ""
		onProg := func(j RestoreJob) {
			if j.Status == JobThawing {
				if lastState != JobThawing || !ttyOut {
					fmt.Fprintf(os.Stderr, "%s: [%s] %s — polling every 15m...\n",
						time.Now().Format(time.RFC3339), j.Archive.BaseName, j.Message)
				}
				lastState = j.Status
				return
			}
			lastState = j.Status
			if ttyOut {
				pct := 0.0
				if j.TotalBytes > 0 {
					pct = float64(j.DoneBytes) * 100.0 / float64(j.TotalBytes)
				}
				fmt.Fprintf(os.Stderr, "\r\033[K[%s] %5.1f%% (%s / %s) %.2f MiB/s — %s",
					j.Status, pct, FormatBytes(j.DoneBytes), FormatBytes(j.TotalBytes),
					j.SpeedBPS/1048576.0, j.Message)
			}
		}
		err := eng.RunCLIRestore(ctx, opts.RestoreObj, opts.Mount, opts.Tier, opts.RestoreDst, onProg)
		if ttyOut {
			fmt.Fprint(os.Stderr, "\r\033[K")
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: restore failed: %v\n", prog, err)
			os.Exit(1)
		}
		if ttyOut {
			fmt.Printf("%s: restore verified and committed to %s\n", prog, opts.Mount)
		}

	case opts.RunJobs:
		if err := eng.PollAndRunPendingJobs(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "%s: job poll error: %v\n", prog, err)
			os.Exit(1)
		}

	case opts.QueryMode != "":
		if err := runQuery(ctx, eng, opts.QueryMode); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", prog, err)
			os.Exit(1)
		}

	case opts.DeleteObj != "":
		if err := runDelete(ctx, eng, opts.DeleteObj); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", prog, err)
			os.Exit(1)
		}
	}
}
