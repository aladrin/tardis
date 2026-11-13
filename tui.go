//go:build openbsd

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// ANSI palette & terminal control sequences modeled after bstat.
const (
	RESET    = "\033[0m"
	BOLD     = "\033[1m"
	DIM      = "\033[2m"
	RED      = "\033[31m"
	GREEN    = "\033[32m"
	LIME     = "\033[92m"
	YELLOW   = "\033[33m"
	BLUE     = "\033[34m"
	MAGENTA  = "\033[35m"
	CYAN     = "\033[36m"
	WHITE_BG = "\033[47;30m"
	RED_BG   = "\033[41;37m"

	ansiAltEnter   = "\033[?1049h\033[?25l\033[2J\033[H"
	ansiAltExit    = "\033[?25h\033[?1049l"
	ansiClearHome  = "\033[2J\033[H"
	ansiHome       = "\033[H"
	ansiClearBelow = "\033[J"
	ansiClearEOL   = "\033[K"
)

const (
	viewTimeline = 0
	viewBrowser  = 1
	viewJobs     = 2
	viewSettings = 3
	totalViews   = 4
)

type tuiInputKind int

const (
	keyRune tuiInputKind = iota
	keyUp
	keyDown
	keyLeft
	keyRight
	keyPageUp
	keyPageDown
	keyHome
	keyEnd
	keyEnter
	keyBackspace
	keyTab
	keyEsc
	keyCtrlU
	keyQuit
)

type tuiInputEvent struct {
	Kind tuiInputKind
	Ch   rune
}

type promptKind int

const (
	promptNone promptKind = iota
	promptFilter
	promptDestPath
	promptClientSwitch
	promptConfirmDelete
	promptEditSetting
)

type tuiState struct {
	ctx    context.Context
	eng    *Engine
	asyncC chan func(*tuiState)

	cols     int
	rows     int
	view     int
	lastView int

	// Crypto & mount environment
	libreSSLVer string
	keyFP       string
	localMounts []MountInfo

	// Status banner
	statusMsg   string
	statusIsErr bool
	statusTime  time.Time
	busyLabel   string

	// View 0: Dump Timeline
	activeClient     string
	knownClients     []string
	snapshots        []SnapshotRecord
	mountSnaps       []string // up to 9 mountpoints for 1-9 quick snaps
	mountFilter      string
	timelineSelected int
	timelineScroll   int

	// View 1: Snapshot Browser & Time-Travel
	browserMountSnaps []SnapshotRecord // chronological (newest-first) snapshots for active mount
	browserSnapIdx    int
	activeSnap        *SnapshotRecord
	catalogTree       *CatalogTree
	visibleNodes      []*CatalogNode
	browserSelected   int
	browserScroll     int
	expandedRelPaths  map[string]bool
	savedMarks        map[string]bool
	restoreMode       string // OVERWRITE | NEW_PATH
	restoreDest       string
	restoreTier       string

	// View 2: Jobs & Glacier Monitor
	jobsSelected int
	jobsScroll   int

	// View 3: S3 & System Config
	cfgDraft        *Config
	cfgDirty        bool
	settingSelected int

	// Shared filter & modal input prompt
	filterText  string
	promptMode  promptKind
	promptTitle string
	promptBuf   string
	pendingSnap *SnapshotRecord
}

func openRawTTY() (*os.File, *unix.Termios, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open /dev/tty: %w", err)
	}
	fd := int(tty.Fd())
	orig, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	if err != nil {
		_ = tty.Close()
		return nil, nil, fmt.Errorf("ioctl TIOCGETA: %w", err)
	}
	raw := *orig
	raw.Iflag &^= unix.BRKINT | unix.ICRNL | unix.INPCK | unix.ISTRIP | unix.IXON
	raw.Lflag &^= unix.ECHO | unix.ICANON | unix.IEXTEN | unix.ISIG
	raw.Cflag |= unix.CS8
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0

	if err := unix.IoctlSetTermios(fd, unix.TIOCSETA, &raw); err != nil {
		_ = tty.Close()
		return nil, nil, fmt.Errorf("ioctl TIOCSETA: %w", err)
	}
	return tty, orig, nil
}

func queryWinsize(fd int) (cols int, rows int) {
	ws, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
	if err != nil || ws.Col == 0 || ws.Row == 0 {
		return 100, 32
	}
	cols = int(ws.Col)
	rows = int(ws.Row)
	if cols < 80 {
		cols = 80
	}
	if rows < 22 {
		rows = 22
	}
	return cols, rows
}

func startTTYReader(tty *os.File, out chan<- tuiInputEvent, stop <-chan struct{}) {
	go func() {
		buf := make([]byte, 32)
		for {
			n, err := tty.Read(buf)
			if err != nil || n == 0 {
				select {
				case <-stop:
					return
				default:
					time.Sleep(20 * time.Millisecond)
					continue
				}
			}
			data := buf[:n]
			if data[0] == 0x03 { // Ctrl+C
				out <- tuiInputEvent{Kind: keyQuit}
				return
			}
			if data[0] == 0x1b {
				if len(data) == 1 {
					out <- tuiInputEvent{Kind: keyEsc}
					continue
				}
				if len(data) >= 3 && (data[1] == '[' || data[1] == 'O') {
					switch data[2] {
					case 'A':
						out <- tuiInputEvent{Kind: keyUp}
					case 'B':
						out <- tuiInputEvent{Kind: keyDown}
					case 'C':
						out <- tuiInputEvent{Kind: keyRight}
					case 'D':
						out <- tuiInputEvent{Kind: keyLeft}
					case 'H':
						out <- tuiInputEvent{Kind: keyHome}
					case 'F':
						out <- tuiInputEvent{Kind: keyEnd}
					case '5':
						if len(data) >= 4 && data[3] == '~' {
							out <- tuiInputEvent{Kind: keyPageUp}
						}
					case '6':
						if len(data) >= 4 && data[3] == '~' {
							out <- tuiInputEvent{Kind: keyPageDown}
						}
					case '1', '7':
						if len(data) >= 4 && data[3] == '~' {
							out <- tuiInputEvent{Kind: keyHome}
						}
					case '4', '8':
						if len(data) >= 4 && data[3] == '~' {
							out <- tuiInputEvent{Kind: keyEnd}
						}
					}
				}
				continue
			}

			switch data[0] {
			case '\r', '\n':
				out <- tuiInputEvent{Kind: keyEnter}
			case 127, '\b':
				out <- tuiInputEvent{Kind: keyBackspace}
			case '\t':
				out <- tuiInputEvent{Kind: keyTab}
			case 0x15: // Ctrl+U
				out <- tuiInputEvent{Kind: keyCtrlU}
			default:
				for len(data) > 0 {
					r, sz := utf8.DecodeRune(data)
					if r != utf8.RuneError && r >= 32 {
						out <- tuiInputEvent{Kind: keyRune, Ch: r}
					}
					data = data[sz:]
				}
			}
		}
	}()
}

// RunTUI launches the interactive 4-view bstat-inspired Tardis console on OpenBSD.
func RunTUI(ctx context.Context, eng *Engine) error {
	tty, origTerm, err := openRawTTY()
	if err != nil {
		return err
	}
	fd := int(tty.Fd())

	restoreTerminal := func() {
		_ = unix.IoctlSetTermios(fd, unix.TIOCSETA, origTerm)
		_, _ = tty.WriteString(ansiAltExit)
		_ = tty.Close()
	}
	defer func() {
		if r := recover(); r != nil {
			restoreTerminal()
			panic(r)
		}
	}()

	_, _ = tty.WriteString(ansiAltEnter)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	cfg := eng.ConfigSnapshot()
	cols, rows := queryWinsize(fd)

	st := &tuiState{
		ctx:              runCtx,
		eng:              eng,
		asyncC:           make(chan func(*tuiState), 64),
		cols:             cols,
		rows:             rows,
		view:             viewTimeline,
		lastView:         -1,
		activeClient:     cfg.EffectiveClient(),
		expandedRelPaths: make(map[string]bool),
		savedMarks:       make(map[string]bool),
		restoreMode:      RestoreModeOverwrite,
		restoreTier:      cfg.EffectiveTier(""),
		cfgDraft:         cfg.Clone(),
	}

	if ver, err := VerifyLibreSSL(); err == nil {
		st.libreSSLVer = ver
	} else {
		st.libreSSLVer = "MISSING"
		st.setStatus(err.Error(), true)
	}
	st.refreshKeyFingerprint()
	if mounts, err := ListMounts(); err == nil {
		st.localMounts = mounts
	}
	_ = eng.LoadJobs()

	// Start pending/thawing job checks in background if configured
	if cfg.ValidateS3() != nil {
		st.view = viewSettings
		st.setStatus("Configure S3 Bucket & Save Config to begin browsing snapshots.", false)
	} else {
		st.triggerRefreshSnapshots(true)
		st.triggerPollJobs(false)
	}

	stopReader := make(chan struct{})
	inputCh := make(chan tuiInputEvent, 32)
	startTTYReader(tty, inputCh, stopReader)

	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, unix.SIGWINCH, unix.SIGINT, unix.SIGTERM)
	defer signal.Stop(sigCh)

	renderTick := time.NewTicker(200 * time.Millisecond)
	defer renderTick.Stop()

	thawTick := time.NewTicker(60 * time.Second)
	defer thawTick.Stop()

	st.renderFrame(tty)

	for {
		select {
		case <-runCtx.Done():
			close(stopReader)
			restoreTerminal()
			return nil

		case sig := <-sigCh:
			if sig == unix.SIGWINCH {
				st.cols, st.rows = queryWinsize(fd)
				_, _ = tty.WriteString(ansiClearHome)
				st.renderFrame(tty)
			} else {
				close(stopReader)
				restoreTerminal()
				return nil
			}

		case fn := <-st.asyncC:
			fn(st)
			st.renderFrame(tty)

		case ev := <-inputCh:
			if st.handleInput(ev) {
				close(stopReader)
				restoreTerminal()
				return nil
			}
			st.renderFrame(tty)

		case <-thawTick.C:
			if st.eng.ConfigSnapshot().ValidateS3() == nil {
				st.triggerPollJobs(true)
			}

		case <-renderTick.C:
			st.renderFrame(tty)
		}
	}
}

func (s *tuiState) setStatus(msg string, isErr bool) {
	s.statusMsg = msg
	s.statusIsErr = isErr
	s.statusTime = time.Now()
}

func (s *tuiState) refreshKeyFingerprint() {
	km, err := s.eng.EnsureKeyForClient(s.activeClient, false)
	if err != nil {
		s.keyFP = "NO-KEY"
		return
	}
	s.keyFP = km.Fingerprint()
	km.Wipe()
}

func (s *tuiState) post(fn func(*tuiState)) {
	select {
	case s.asyncC <- fn:
	case <-s.ctx.Done():
	}
}

// --- ASYNC ACTIONS ---

func (s *tuiState) triggerRefreshSnapshots(probeRestore bool) {
	cfg := s.eng.ConfigSnapshot()
	if err := cfg.ValidateS3(); err != nil {
		s.setStatus(err.Error(), true)
		return
	}
	client := s.activeClient
	mounts := append([]MountInfo(nil), s.localMounts...)
	s.busyLabel = fmt.Sprintf("Querying s3://%s/%s/ ...", cfg.Bucket, client)

	go func() {
		opCtx, cancel := context.WithTimeout(s.ctx, 45*time.Second)
		defer cancel()

		store, err := s.eng.NewStore(opCtx)
		if err != nil {
			s.post(func(st *tuiState) {
				st.busyLabel = ""
				st.setStatus(err.Error(), true)
			})
			return
		}
		clients, _ := store.ListClients(opCtx)
		snaps, err := store.ListSnapshots(opCtx, client, mounts, probeRestore)
		s.post(func(st *tuiState) {
			st.busyLabel = ""
			if err != nil {
				st.setStatus(err.Error(), true)
				return
			}
			if len(clients) > 0 {
				st.knownClients = clients
			}
			st.snapshots = snaps
			st.rebuildMountSnaps()
			st.refreshKeyFingerprint()
			st.setStatus(fmt.Sprintf("Loaded %d snapshots for client '%s'", len(snaps), client), false)
		})
	}()
}

func (s *tuiState) rebuildMountSnaps() {
	seen := make(map[string]bool)
	var mounts []string
	for _, snap := range s.snapshots {
		m := snap.Meta.MountGuess
		if m != "" && !seen[m] {
			seen[m] = true
			mounts = append(mounts, m)
		}
	}
	sort.Strings(mounts)
	if len(mounts) > 9 {
		mounts = mounts[:9]
	}
	s.mountSnaps = mounts
}

func (s *tuiState) filteredTimelineSnapshots() []SnapshotRecord {
	var out []SnapshotRecord
	for _, snap := range s.snapshots {
		if s.mountFilter != "" && !strings.EqualFold(snap.Meta.MountGuess, s.mountFilter) {
			continue
		}
		if s.filterText != "" && s.view == viewTimeline {
			haystack := snap.Meta.BaseName + " " + snap.Meta.MountGuess + " " + snap.GlacierState
			if rx, err := regexp.Compile("(?i)" + s.filterText); err == nil {
				if !rx.MatchString(haystack) {
					continue
				}
			} else if !strings.Contains(strings.ToUpper(haystack), strings.ToUpper(s.filterText)) {
				continue
			}
		}
		out = append(out, snap)
	}
	return out
}

func (s *tuiState) openSnapshotInBrowser(snap SnapshotRecord, anchorPath string) {
	// Rebuild chronological list of snapshots sharing this mountpoint
	var sameMount []SnapshotRecord
	for _, cand := range s.snapshots {
		if cand.Meta.MountSlug == snap.Meta.MountSlug {
			sameMount = append(sameMount, cand)
		}
	}
	if len(sameMount) == 0 {
		sameMount = []SnapshotRecord{snap}
	}
	idx := 0
	for i, cand := range sameMount {
		if cand.Meta.BaseName == snap.Meta.BaseName {
			idx = i
			break
		}
	}

	// Preserve current marks & expanded folders if staying on the same mountpoint
	sameMountSwitch := s.activeSnap != nil && s.activeSnap.Meta.MountSlug == snap.Meta.MountSlug
	if sameMountSwitch && s.catalogTree != nil {
		s.captureTreeState()
	} else {
		s.expandedRelPaths = make(map[string]bool)
		s.savedMarks = make(map[string]bool)
	}

	s.browserMountSnaps = sameMount
	s.browserSnapIdx = idx
	snapCopy := snap
	s.activeSnap = &snapCopy
	s.view = viewBrowser
	if s.promptMode != promptFilter {
		s.filterText = ""
	}

	if entries, ok := s.eng.CachedCatalogEntries(snap.Meta.BaseName); ok {
		s.applyLoadedCatalog(snap, entries, anchorPath)
		s.prefetchAdjacentCatalogs()
		return
	}

	s.busyLabel = fmt.Sprintf("Fetching & authenticating catalog %s ...", snap.Meta.ListFile)
	go func() {
		opCtx, cancel := context.WithTimeout(s.ctx, 45*time.Second)
		defer cancel()

		entries, err := s.eng.LoadCatalog(opCtx, snap.Meta)
		s.post(func(st *tuiState) {
			st.busyLabel = ""
			if err != nil {
				st.catalogTree = nil
				st.visibleNodes = nil
				st.setStatus("Catalog load error: "+err.Error(), true)
				return
			}
			if st.activeSnap != nil && st.activeSnap.Meta.BaseName == snap.Meta.BaseName {
				st.applyLoadedCatalog(snap, entries, anchorPath)
				st.setStatus(fmt.Sprintf("Verified HMAC & loaded %d catalog entries (%s)", len(entries), snap.Meta.BaseName), false)
				st.prefetchAdjacentCatalogs()
			}
		})
	}()
}

func (s *tuiState) captureTreeState() {
	if s.catalogTree == nil || s.catalogTree.Root == nil {
		return
	}
	exp := make(map[string]bool)
	marks := make(map[string]bool)
	var walk func(n *CatalogNode)
	walk = func(n *CatalogNode) {
		if n != s.catalogTree.Root {
			if n.IsDir && n.Expanded {
				exp[n.RelPath] = true
			}
			if n.Marked && len(n.Children) == 0 {
				marks[n.RelPath] = true
			}
		}
		for _, ch := range n.Children {
			walk(ch)
		}
	}
	walk(s.catalogTree.Root)
	s.expandedRelPaths = exp
	s.savedMarks = marks
}

func (s *tuiState) applyLoadedCatalog(snap SnapshotRecord, entries []CatalogEntry, anchorPath string) {
	tree := BuildCatalogTree(snap.Meta.MountGuess, entries)

	// Re-apply expanded folders & marks across dump-date switches
	for rel := range s.expandedRelPaths {
		if n, ok := tree.ByPath[rel]; ok && n.IsDir {
			n.Expanded = true
		}
	}
	for rel := range s.savedMarks {
		if n, ok := tree.ByPath[rel]; ok {
			tree.SetMark(n, true)
		}
	}

	if anchorPath != "" {
		// Walk up to nearest existing parent directory if anchorPath doesn't exist in this snapshot
		probe := anchorPath
		for probe != "" {
			if n, ok := tree.ByPath[probe]; ok {
				tree.ExpandAncestors(n)
				anchorPath = probe
				break
			}
			idx := strings.LastIndexByte(probe, '/')
			if idx == -1 {
				probe = ""
			} else {
				probe = probe[:idx]
			}
		}
	}

	s.catalogTree = tree
	s.refreshVisibleNodes(anchorPath)
}

func (s *tuiState) refreshVisibleNodes(anchorPath string) {
	if s.catalogTree == nil {
		s.visibleNodes = nil
		s.browserSelected = 0
		s.browserScroll = 0
		return
	}
	filter := ""
	if s.view == viewBrowser {
		filter = s.filterText
	}
	s.visibleNodes = s.catalogTree.Flatten(filter)
	if anchorPath != "" {
		for i, n := range s.visibleNodes {
			if n.RelPath == anchorPath {
				s.browserSelected = i
				break
			}
		}
	}
	if s.browserSelected >= len(s.visibleNodes) {
		s.browserSelected = len(s.visibleNodes) - 1
	}
	if s.browserSelected < 0 {
		s.browserSelected = 0
	}
}

func (s *tuiState) prefetchAdjacentCatalogs() {
	var toLoad []ArchiveMeta
	for _, offset := range []int{-1, 1, -2, 2} {
		idx := s.browserSnapIdx + offset
		if idx >= 0 && idx < len(s.browserMountSnaps) {
			cand := s.browserMountSnaps[idx]
			if cand.HasAuthenticatedCatalog() {
				if _, ok := s.eng.CachedCatalogEntries(cand.Meta.BaseName); !ok {
					toLoad = append(toLoad, cand.Meta)
				}
			}
		}
	}
	if len(toLoad) == 0 {
		return
	}
	go func() {
		for _, m := range toLoad {
			opCtx, cancel := context.WithTimeout(s.ctx, 20*time.Second)
			_, _ = s.eng.LoadCatalog(opCtx, m)
			cancel()
		}
		s.post(func(_ *tuiState) {})
	}()
}

func (s *tuiState) preloadAllMountCatalogs() {
	if len(s.browserMountSnaps) == 0 {
		return
	}
	snaps := append([]SnapshotRecord(nil), s.browserMountSnaps...)
	s.busyLabel = fmt.Sprintf("Preloading %d catalogs from S3 STANDARD...", len(snaps))
	go func() {
		loaded := 0
		for _, sn := range snaps {
			if !sn.HasAuthenticatedCatalog() {
				continue
			}
			opCtx, cancel := context.WithTimeout(s.ctx, 25*time.Second)
			if _, err := s.eng.LoadCatalog(opCtx, sn.Meta); err == nil {
				loaded++
			}
			cancel()
		}
		s.post(func(st *tuiState) {
			st.busyLabel = ""
			st.setStatus(fmt.Sprintf("Cached & HMAC-verified %d/%d catalogs for %s", loaded, len(snaps), snaps[0].Meta.MountGuess), false)
		})
	}()
}

func (s *tuiState) stepSnapshotDate(delta int) {
	if len(s.browserMountSnaps) <= 1 {
		s.setStatus("Only 1 snapshot available for this mountpoint.", false)
		return
	}
	nextIdx := s.browserSnapIdx + delta
	if nextIdx < 0 {
		nextIdx = 0
		s.setStatus("Already at newest dump date for this mount.", false)
		return
	}
	if nextIdx >= len(s.browserMountSnaps) {
		nextIdx = len(s.browserMountSnaps) - 1
		s.setStatus("Already at oldest dump date for this mount.", false)
		return
	}
	anchor := ""
	if s.browserSelected >= 0 && s.browserSelected < len(s.visibleNodes) {
		anchor = s.visibleNodes[s.browserSelected].RelPath
	}
	s.openSnapshotInBrowser(s.browserMountSnaps[nextIdx], anchor)
}

func (s *tuiState) triggerThawSnapshot(snap SnapshotRecord) {
	tier := s.restoreTier
	s.busyLabel = fmt.Sprintf("Submitting Glacier %s thaw for %s ...", tier, snap.Meta.DataFile)
	go func() {
		opCtx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
		defer cancel()
		store, err := s.eng.NewStore(opCtx)
		if err != nil {
			s.post(func(st *tuiState) {
				st.busyLabel = ""
				st.setStatus(err.Error(), true)
			})
			return
		}
		state, err := store.RequestThaw(opCtx, snap.Meta.DataKey, tier, DefaultRestoreDays)
		s.post(func(st *tuiState) {
			st.busyLabel = ""
			if err != nil {
				st.setStatus(err.Error(), true)
				return
			}
			for i := range st.snapshots {
				if st.snapshots[i].Meta.BaseName == snap.Meta.BaseName {
					st.snapshots[i].GlacierState = state
				}
			}
			if st.activeSnap != nil && st.activeSnap.Meta.BaseName == snap.Meta.BaseName {
				st.activeSnap.GlacierState = state
			}
			st.setStatus(fmt.Sprintf("Archive %s Glacier state: %s (%s tier)", snap.Meta.BaseName, state, tier), false)
		})
	}()
}

func (s *tuiState) dispatchRestoreFromBrowser(includeFullChain bool) {
	if s.activeSnap == nil || s.catalogTree == nil {
		s.setStatus("No snapshot catalog loaded.", true)
		return
	}

	paths, fullSnap := s.catalogTree.EffectiveRestorePaths()
	if !fullSnap && len(paths) == 0 {
		// Fall back to highlighted item if nothing was explicitly marked with SPACE
		if s.browserSelected >= 0 && s.browserSelected < len(s.visibleNodes) {
			sel := s.visibleNodes[s.browserSelected]
			if sel == s.catalogTree.Root {
				fullSnap = true
			} else if sel.RelPath != "" {
				paths = []string{sel.RelPath}
			}
		}
	}

	if s.restoreMode == RestoreModeNewPath && strings.TrimSpace(s.restoreDest) == "" {
		// Open inline destination prompt pre-populated with a sensible default
		defDest := ".restored"
		if len(paths) == 1 && !fullSnap {
			defDest = filepath.Base(paths[0]) + ".restored"
		} else {
			defDest = filepath.Join(s.activeSnap.Meta.MountGuess, "restore_"+s.activeSnap.Meta.Timestamp.Format("20060102_150405"))
		}
		s.promptMode = promptDestPath
		s.promptTitle = "Enter NEW_PATH filename, directory, or suffix (e.g. .restored)"
		s.promptBuf = defDest
		return
	}

	var targets []SnapshotRecord
	if includeFullChain {
		targets = SnapshotChainForTarget(s.browserMountSnaps, *s.activeSnap)
	} else {
		targets = []SnapshotRecord{*s.activeSnap}
	}

	var queuedIDs []string
	for _, snap := range targets {
		job, err := s.eng.EnqueueRestoreJob(
			snap.Meta.Client,
			snap.Meta,
			snap.Meta.MountGuess,
			s.restoreTier,
			s.restoreMode,
			s.restoreDest,
			paths,
			fullSnap,
		)
		if err != nil {
			s.setStatus("Queue restore failed: "+err.Error(), true)
			return
		}
		queuedIDs = append(queuedIDs, job.ID)
		s.eng.StartJobAsync(s.ctx, job.ID)
	}

	s.view = viewJobs
	s.jobsSelected = 0
	s.setStatus(fmt.Sprintf("Dispatched %d restore job(s) [%s mode]", len(queuedIDs), s.restoreMode), false)
}

func (s *tuiState) triggerPollJobs(silent bool) {
	if !silent {
		s.busyLabel = "Polling pending S3 / Glacier restore jobs..."
	}
	go func() {
		err := s.eng.PollAndRunPendingJobs(s.ctx)
		s.post(func(st *tuiState) {
			if !silent {
				st.busyLabel = ""
			}
			if err != nil && !silent {
				st.setStatus("Job poll note: "+err.Error(), true)
			} else if !silent {
				st.setStatus("Polled all pending restore jobs.", false)
			}
		})
	}()
}

func (s *tuiState) triggerDeleteSnapshot(snap SnapshotRecord) {
	s.busyLabel = fmt.Sprintf("Deleting snapshot %s from S3...", snap.Meta.BaseName)
	go func() {
		opCtx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
		defer cancel()
		store, err := s.eng.NewStore(opCtx)
		if err != nil {
			s.post(func(st *tuiState) {
				st.busyLabel = ""
				st.setStatus(err.Error(), true)
			})
			return
		}
		err = store.DeleteSnapshot(opCtx, snap.Meta)
		s.post(func(st *tuiState) {
			st.busyLabel = ""
			if err != nil {
				st.setStatus("Delete error: "+err.Error(), true)
				return
			}
			st.setStatus("Deleted snapshot "+snap.Meta.BaseName+" from S3.", false)
			st.triggerRefreshSnapshots(false)
		})
	}()
}

func (s *tuiState) triggerTestS3AndCrypto() {
	draft := s.cfgDraft.Clone()
	draft.Normalize()
	s.eng.ApplyRuntimeConfig(draft)
	s.activeClient = draft.EffectiveClient()
	s.restoreTier = draft.EffectiveTier("")

	s.busyLabel = "Running LibreSSL ChaCha20+HMAC self-test & S3 connectivity check..."
	go func() {
		opCtx, cancel := context.WithTimeout(s.ctx, 25*time.Second)
		defer cancel()

		km, err := s.eng.EnsureKeyForClient(draft.EffectiveClient(), true)
		if err != nil {
			s.post(func(st *tuiState) {
				st.busyLabel = ""
				st.setStatus("Key check failed: "+err.Error(), true)
			})
			return
		}
		fp := km.Fingerprint()
		ver, err := SelfTestLibreSSL(opCtx, km)
		km.Wipe()
		if err != nil {
			s.post(func(st *tuiState) {
				st.busyLabel = ""
				st.setStatus("Crypto self-test failed: "+err.Error(), true)
			})
			return
		}

		store, err := NewS3Store(opCtx, draft)
		if err != nil {
			s.post(func(st *tuiState) {
				st.busyLabel = ""
				st.libreSSLVer = ver
				st.keyFP = fp
				st.setStatus("S3 client init failed: "+err.Error(), true)
			})
			return
		}
		if err := store.TestConnection(opCtx, draft.EffectiveClient()); err != nil {
			s.post(func(st *tuiState) {
				st.busyLabel = ""
				st.libreSSLVer = ver
				st.keyFP = fp
				st.setStatus(err.Error(), true)
			})
			return
		}

		s.post(func(st *tuiState) {
			st.busyLabel = ""
			st.libreSSLVer = ver
			st.keyFP = fp
			st.setStatus(fmt.Sprintf("PASS: %s (key %s) + s3://%s/%s/ reachable", ver, fp, draft.Bucket, draft.EffectiveClient()), false)
		})
	}()
}

// --- INPUT HANDLING ---

func (s *tuiState) handleInput(ev tuiInputEvent) (quit bool) {
	if ev.Kind == keyQuit {
		return true
	}

	// Modal prompt handling (filter, path entry, client switch, delete confirm, config edit)
	if s.promptMode != promptNone {
		s.handlePromptInput(ev)
		return false
	}

	// Global navigation keys
	switch ev.Kind {
	case keyLeft:
		s.view = (s.view - 1 + totalViews) % totalViews
		return false
	case keyRight, keyTab:
		s.view = (s.view + 1) % totalViews
		return false
	case keyEsc:
		if s.filterText != "" {
			s.filterText = ""
			s.refreshVisibleNodes("")
		}
		return false
	case keyRune:
		if ev.Ch == 'q' || ev.Ch == 'Q' {
			return true
		}
	}

	switch s.view {
	case viewTimeline:
		s.handleTimelineInput(ev)
	case viewBrowser:
		s.handleBrowserInput(ev)
	case viewJobs:
		s.handleJobsInput(ev)
	case viewSettings:
		s.handleSettingsInput(ev)
	}
	return false
}

func (s *tuiState) handlePromptInput(ev tuiInputEvent) {
	if s.promptMode == promptConfirmDelete {
		if ev.Kind == keyRune && (ev.Ch == 'y' || ev.Ch == 'Y') && s.pendingSnap != nil {
			target := *s.pendingSnap
			s.pendingSnap = nil
			s.promptMode = promptNone
			s.triggerDeleteSnapshot(target)
			return
		}
		s.pendingSnap = nil
		s.promptMode = promptNone
		s.setStatus("Delete cancelled.", false)
		return
	}

	switch ev.Kind {
	case keyEsc:
		if s.promptMode == promptFilter {
			s.filterText = ""
			s.refreshVisibleNodes("")
		}
		s.promptMode = promptNone

	case keyCtrlU:
		s.promptBuf = ""
		if s.promptMode == promptFilter {
			s.filterText = ""
			s.refreshVisibleNodes("")
		}

	case keyBackspace:
		if len(s.promptBuf) > 0 {
			_, sz := utf8.DecodeLastRuneInString(s.promptBuf)
			s.promptBuf = s.promptBuf[:len(s.promptBuf)-sz]
		}
		if s.promptMode == promptFilter {
			s.filterText = s.promptBuf
			s.timelineSelected = 0
			s.refreshVisibleNodes("")
		}

	case keyRune:
		s.promptBuf += string(ev.Ch)
		if s.promptMode == promptFilter {
			s.filterText = s.promptBuf
			s.timelineSelected = 0
			s.refreshVisibleNodes("")
		}

	case keyEnter:
		val := strings.TrimSpace(s.promptBuf)
		mode := s.promptMode
		s.promptMode = promptNone

		switch mode {
		case promptFilter:
			s.filterText = val
			s.refreshVisibleNodes("")
		case promptDestPath:
			s.restoreDest = val
			if val != "" {
				s.restoreMode = RestoreModeNewPath
				s.setStatus("Set NEW_PATH target: "+val+" (press 'x' to dispatch restore)", false)
			}
		case promptClientSwitch:
			if val != "" {
				s.activeClient = val
				s.cfgDraft.Client = val
				s.eng.ApplyRuntimeConfig(s.cfgDraft)
				s.triggerRefreshSnapshots(true)
			}
		case promptEditSetting:
			s.applySettingValue(s.settingSelected, val)
		}
	}
}

func (s *tuiState) handleTimelineInput(ev tuiInputEvent) {
	list := s.filteredTimelineSnapshots()
	pageStep := s.viewportRows() - 2
	if pageStep < 5 {
		pageStep = 5
	}

	switch ev.Kind {
	case keyUp:
		if s.timelineSelected > 0 {
			s.timelineSelected--
		}
	case keyDown:
		if s.timelineSelected < len(list)-1 {
			s.timelineSelected++
		}
	case keyPageUp:
		s.timelineSelected -= pageStep
		if s.timelineSelected < 0 {
			s.timelineSelected = 0
		}
	case keyPageDown:
		s.timelineSelected += pageStep
		if s.timelineSelected >= len(list) {
			s.timelineSelected = len(list) - 1
		}
		if s.timelineSelected < 0 {
			s.timelineSelected = 0
		}
	case keyHome:
		s.timelineSelected = 0
	case keyEnd:
		if len(list) > 0 {
			s.timelineSelected = len(list) - 1
		}
	case keyEnter:
		if s.timelineSelected >= 0 && s.timelineSelected < len(list) {
			s.openSnapshotInBrowser(list[s.timelineSelected], "")
		}
	case keyRune:
		switch ev.Ch {
		case '/':
			s.promptMode = promptFilter
			s.promptTitle = "Filter snapshots (regex/substring)"
			s.promptBuf = s.filterText
		case 'w':
			s.timelineSelected -= pageStep
			if s.timelineSelected < 0 {
				s.timelineSelected = 0
			}
		case 's':
			s.timelineSelected += pageStep
			if s.timelineSelected >= len(list) {
				s.timelineSelected = len(list) - 1
			}
			if s.timelineSelected < 0 {
				s.timelineSelected = 0
			}
		case 'r':
			s.triggerRefreshSnapshots(true)
		case 't':
			if s.restoreTier == "Bulk" {
				s.restoreTier = "Standard"
			} else {
				s.restoreTier = "Bulk"
			}
			s.setStatus("Default Glacier thaw tier set to "+s.restoreTier, false)
		case 'T':
			if s.timelineSelected >= 0 && s.timelineSelected < len(list) {
				s.triggerThawSnapshot(list[s.timelineSelected])
			}
		case 'c':
			if len(s.knownClients) > 1 {
				idx := 0
				for i, c := range s.knownClients {
					if c == s.activeClient {
						idx = (i + 1) % len(s.knownClients)
						break
					}
				}
				s.activeClient = s.knownClients[idx]
				s.triggerRefreshSnapshots(true)
			} else {
				s.promptMode = promptClientSwitch
				s.promptTitle = "Switch S3 client prefix"
				s.promptBuf = s.activeClient
			}
		case '0':
			s.mountFilter = ""
			s.timelineSelected = 0
		case '1', '2', '3', '4', '5', '6', '7', '8', '9':
			idx := int(ev.Ch - '1')
			if idx < len(s.mountSnaps) {
				target := s.mountSnaps[idx]
				if strings.EqualFold(s.mountFilter, target) {
					s.mountFilter = ""
				} else {
					s.mountFilter = target
				}
				s.timelineSelected = 0
			}
		case 'D':
			if s.timelineSelected >= 0 && s.timelineSelected < len(list) {
				target := list[s.timelineSelected]
				s.pendingSnap = &target
				s.promptMode = promptConfirmDelete
				s.promptTitle = fmt.Sprintf("Delete S3 snapshot %s? Press 'y' to confirm, any other key to abort", target.Meta.BaseName)
			}
		}
	}
}

func (s *tuiState) handleBrowserInput(ev tuiInputEvent) {
	pageStep := s.browserTreeRows() - 2
	if pageStep < 5 {
		pageStep = 5
	}

	switch ev.Kind {
	case keyUp:
		if s.browserSelected > 0 {
			s.browserSelected--
		}
	case keyDown:
		if s.browserSelected < len(s.visibleNodes)-1 {
			s.browserSelected++
		}
	case keyPageUp:
		s.browserSelected -= pageStep
		if s.browserSelected < 0 {
			s.browserSelected = 0
		}
	case keyPageDown:
		s.browserSelected += pageStep
		if s.browserSelected >= len(s.visibleNodes) {
			s.browserSelected = len(s.visibleNodes) - 1
		}
		if s.browserSelected < 0 {
			s.browserSelected = 0
		}
	case keyHome:
		s.browserSelected = 0
	case keyEnd:
		if len(s.visibleNodes) > 0 {
			s.browserSelected = len(s.visibleNodes) - 1
		}
	case keyEnter:
		if s.catalogTree != nil && s.browserSelected >= 0 && s.browserSelected < len(s.visibleNodes) {
			node := s.visibleNodes[s.browserSelected]
			if s.filterText != "" {
				// Jump out of filtered flat list directly onto this node in the expanded directory tree
				s.filterText = ""
				s.catalogTree.ExpandAncestors(node)
				s.refreshVisibleNodes(node.RelPath)
				return
			}
			if node.IsDir && len(node.Children) > 0 {
				node.Expanded = !node.Expanded
				s.refreshVisibleNodes(node.RelPath)
			} else {
				s.catalogTree.ToggleMark(node)
			}
		}
	case keyRune:
		switch ev.Ch {
		case ' ':
			if s.catalogTree != nil && s.browserSelected >= 0 && s.browserSelected < len(s.visibleNodes) {
				node := s.visibleNodes[s.browserSelected]
				s.catalogTree.ToggleMark(node)
			}
		case 'u':
			if s.catalogTree != nil {
				s.catalogTree.ClearMarks()
				s.savedMarks = make(map[string]bool)
				s.setStatus("Cleared all restore marks.", false)
			}
		case '[':
			// Step backward in time (older dump date)
			s.stepSnapshotDate(+1)
		case ']':
			// Step forward in time (newer dump date)
			s.stepSnapshotDate(-1)
		case '/':
			s.promptMode = promptFilter
			s.promptTitle = "Filter snapshot files (regex/substring)"
			s.promptBuf = s.filterText
		case 'w':
			s.browserSelected -= pageStep
			if s.browserSelected < 0 {
				s.browserSelected = 0
			}
		case 's':
			s.browserSelected += pageStep
			if s.browserSelected >= len(s.visibleNodes) {
				s.browserSelected = len(s.visibleNodes) - 1
			}
			if s.browserSelected < 0 {
				s.browserSelected = 0
			}
		case 'm':
			if s.restoreMode == RestoreModeOverwrite {
				s.restoreMode = RestoreModeNewPath
				if s.restoreDest == "" {
					s.restoreDest = ".restored"
				}
			} else {
				s.restoreMode = RestoreModeOverwrite
			}
			s.setStatus("Restore mode switched to "+s.restoreMode, false)
		case 'o':
			s.promptMode = promptDestPath
			s.promptTitle = "Set NEW_PATH target filename, directory, or suffix (e.g. .restored)"
			s.promptBuf = s.restoreDest
		case 't':
			if s.restoreTier == "Bulk" {
				s.restoreTier = "Standard"
			} else {
				s.restoreTier = "Bulk"
			}
			s.setStatus("Glacier thaw tier set to "+s.restoreTier, false)
		case 'T':
			if s.activeSnap != nil {
				s.triggerThawSnapshot(*s.activeSnap)
			}
		case 'p':
			s.preloadAllMountCatalogs()
		case 'x':
			s.dispatchRestoreFromBrowser(false)
		case 'X':
			s.dispatchRestoreFromBrowser(true)
		}
	}
}

func (s *tuiState) handleJobsInput(ev tuiInputEvent) {
	jobs := s.eng.JobsSnapshot()
	switch ev.Kind {
	case keyUp:
		if s.jobsSelected > 0 {
			s.jobsSelected--
		}
	case keyDown:
		if s.jobsSelected < len(jobs)-1 {
			s.jobsSelected++
		}
	case keyRune:
		switch ev.Ch {
		case 'r':
			s.triggerPollJobs(false)
		case 'R':
			if s.jobsSelected >= 0 && s.jobsSelected < len(jobs) {
				id := jobs[s.jobsSelected].ID
				if err := s.eng.RetryJob(s.ctx, id); err != nil {
					s.setStatus(err.Error(), true)
				} else {
					s.setStatus("Retrying job "+id, false)
				}
			}
		case 'd':
			if s.jobsSelected >= 0 && s.jobsSelected < len(jobs) {
				id := jobs[s.jobsSelected].ID
				if err := s.eng.DeleteJob(id); err != nil {
					s.setStatus(err.Error(), true)
				} else {
					s.setStatus("Removed job record "+id, false)
				}
			}
		}
	}
}

const totalSettingRows = 13

func (s *tuiState) handleSettingsInput(ev tuiInputEvent) {
	switch ev.Kind {
	case keyUp:
		if s.settingSelected > 0 {
			s.settingSelected--
		}
	case keyDown:
		if s.settingSelected < totalSettingRows-1 {
			s.settingSelected++
		}
	case keyEnter:
		s.activateSettingRow(s.settingSelected)
	case keyRune:
		switch ev.Ch {
		case ' ':
			if s.settingSelected == 3 || s.settingSelected == 9 {
				s.activateSettingRow(s.settingSelected)
			}
		case 's', 'S':
			s.saveDraftConfig()
		case 't', 'T':
			s.triggerTestS3AndCrypto()
		}
	}
}

func (s *tuiState) activateSettingRow(idx int) {
	switch idx {
	case 0:
		s.openSettingPrompt("S3 Bucket Name", s.cfgDraft.Bucket)
	case 1:
		s.openSettingPrompt("AWS Region (e.g. ap-southeast-2)", s.cfgDraft.Region)
	case 2:
		s.openSettingPrompt("Custom S3 Endpoint URL (blank for AWS S3)", s.cfgDraft.Endpoint)
	case 3:
		s.cfgDraft.ForcePathStyle = !s.cfgDraft.ForcePathStyle
		s.cfgDirty = true
		s.eng.ApplyRuntimeConfig(s.cfgDraft)
	case 4:
		s.openSettingPrompt("AWS Shared Credentials Profile (optional)", s.cfgDraft.AWSProfile)
	case 5:
		s.openSettingPrompt("AWS Access Key ID (optional if using profile/IAM)", s.cfgDraft.AccessKeyID)
	case 6:
		s.openSettingPrompt("AWS Secret Access Key (stored 0600 in config.json)", s.cfgDraft.SecretAccessKey)
	case 7:
		s.openSettingPrompt("Client Identifier", s.cfgDraft.EffectiveClient())
	case 8:
		s.openSettingPrompt("LibreSSL Master Keyfile Path (0400)", s.cfgDraft.EffectiveKeyPath())
	case 9:
		if NormalizeTier(s.cfgDraft.DefaultTier) == "Bulk" {
			s.cfgDraft.DefaultTier = "Standard"
		} else {
			s.cfgDraft.DefaultTier = "Bulk"
		}
		s.restoreTier = s.cfgDraft.DefaultTier
		s.cfgDirty = true
		s.eng.ApplyRuntimeConfig(s.cfgDraft)
	case 10:
		s.triggerTestS3AndCrypto()
	case 11:
		s.saveDraftConfig()
	case 12:
		s.eng.ApplyRuntimeConfig(s.cfgDraft)
		km, err := s.eng.EnsureKeyForClient(s.cfgDraft.EffectiveClient(), true)
		if err != nil {
			s.setStatus("Keyfile error: "+err.Error(), true)
			return
		}
		fp := km.Fingerprint()
		created := km.Created
		path := km.Path
		km.Wipe()
		s.keyFP = fp
		if created {
			s.setStatus(fmt.Sprintf("Generated new 0400 keyfile %s (ID %s)", path, fp), false)
		} else {
			s.setStatus(fmt.Sprintf("Verified existing keyfile %s (ID %s)", path, fp), false)
		}
	}
}

func (s *tuiState) openSettingPrompt(label, current string) {
	s.promptMode = promptEditSetting
	s.promptTitle = label
	s.promptBuf = current
}

func (s *tuiState) applySettingValue(idx int, val string) {
	switch idx {
	case 0:
		s.cfgDraft.Bucket = val
	case 1:
		s.cfgDraft.Region = val
	case 2:
		s.cfgDraft.Endpoint = val
	case 4:
		s.cfgDraft.AWSProfile = val
	case 5:
		s.cfgDraft.AccessKeyID = val
	case 6:
		s.cfgDraft.SecretAccessKey = val
	case 7:
		s.cfgDraft.Client = val
		if val != "" {
			s.activeClient = val
		}
	case 8:
		s.cfgDraft.KeyPath = val
	}
	s.cfgDraft.Normalize()
	s.cfgDirty = true
	s.eng.ApplyRuntimeConfig(s.cfgDraft)
	s.refreshKeyFingerprint()
}

func (s *tuiState) saveDraftConfig() {
	if err := s.eng.UpdateAndSaveConfig(s.cfgDraft); err != nil {
		s.setStatus("Save config failed: "+err.Error(), true)
		return
	}
	s.cfgDirty = false
	s.activeClient = s.cfgDraft.EffectiveClient()
	s.restoreTier = s.cfgDraft.EffectiveTier("")
	s.refreshKeyFingerprint()
	s.setStatus("Saved configuration to "+s.eng.ConfigPath()+" (mode 0600)", false)
	if s.cfgDraft.ValidateS3() == nil {
		s.triggerRefreshSnapshots(true)
	}
}

// --- RENDERING ---

func (s *tuiState) viewportRows() int {
	r := s.rows - 13
	if r < 6 {
		r = 6
	}
	return r
}

func (s *tuiState) browserTreeRows() int {
	r := s.rows - 15
	if r < 6 {
		r = 6
	}
	return r
}

func padOrTrim(str string, maxWidth int) string {
	if maxWidth <= 3 {
		return ""
	}
	if utf8.RuneCountInString(str) <= maxWidth {
		return str
	}
	runes := []rune(str)
	return string(runes[:maxWidth-3]) + "..."
}

func formatGlacierBadge(state string) string {
	switch state {
	case GlacierReady:
		return GREEN + BOLD + "READY  " + RESET
	case GlacierThawing:
		return YELLOW + BOLD + "THAWING" + RESET
	case GlacierFrozen:
		return CYAN + "FROZEN " + RESET
	default:
		return DIM + "UNKNOWN" + RESET
	}
}

func formatProgressBar(done, total int64, width int) string {
	if width < 8 {
		width = 8
	}
	if total <= 0 {
		return DIM + strings.Repeat("░", width) + RESET
	}
	ratio := float64(done) / float64(total)
	if ratio < 0 {
		ratio = 0
	}
	if ratio > 1 {
		ratio = 1
	}
	filled := int(ratio * float64(width))
	return fmt.Sprintf("%s%s%s%s%s %5.1f%%",
		CYAN, strings.Repeat("█", filled),
		DIM, strings.Repeat("░", width-filled), RESET,
		ratio*100.0)
}

func drawTUIHeader(out *strings.Builder, title string, viewIdx int, cfg *Config, client, libreVer, keyFP string, cols int) {
	bucket := cfg.Bucket
	if bucket == "" {
		bucket = "(unconfigured)"
	}
	out.WriteString(fmt.Sprintf("%s[ ◄ %s (%d/%d) ► ]%s  %ss3://%s/%s/%s  │  %s%s%s  │  %sKey:%s %s\n",
		BOLD, title, viewIdx+1, totalViews, RESET,
		CYAN, bucket, client, RESET,
		LIME, libreVer, RESET,
		DIM, RESET, keyFP))
	out.WriteString(strings.Repeat("─", cols-1) + "\n")
}

func (s *tuiState) renderFrame(tty *os.File) {
	var out strings.Builder
	cfg := s.eng.ConfigSnapshot()
	cols := s.cols

	switch s.view {
	case viewTimeline:
		s.renderTimeline(&out, cfg, cols)
	case viewBrowser:
		s.renderBrowser(&out, cfg, cols)
	case viewJobs:
		s.renderJobs(&out, cfg, cols)
	case viewSettings:
		s.renderSettings(&out, cfg, cols)
	}

	out.WriteString(strings.Repeat("─", cols-1) + "\n")

	if s.promptMode != promptNone {
		cursor := "█"
		if s.promptMode == promptConfirmDelete {
			out.WriteString(fmt.Sprintf("%s%s %s %s\n", RED_BG, BOLD, s.promptTitle, RESET))
		} else {
			dispVal := s.promptBuf
			if s.promptMode == promptEditSetting && s.settingSelected == 6 {
				dispVal = strings.Repeat("*", len(s.promptBuf))
			}
			out.WriteString(fmt.Sprintf("%s%s:%s %s%s\n", YELLOW+BOLD, s.promptTitle, RESET, dispVal, cursor))
			out.WriteString(fmt.Sprintf("%s[ENTER: Apply │ ESC: Cancel │ CTRL+U: Clear]%s", DIM, RESET))
		}
	} else {
		if s.busyLabel != "" {
			out.WriteString(fmt.Sprintf("%s⏳ %s%s\n", YELLOW, padOrTrim(s.busyLabel, cols-5), RESET))
		} else if s.statusMsg != "" {
			col := LIME
			if s.statusIsErr {
				col = RED + BOLD
			}
			out.WriteString(fmt.Sprintf("%s%s%s\n", col, padOrTrim(s.statusMsg, cols-2), RESET))
		} else {
			out.WriteString("\n")
		}
		out.WriteString(DIM + padOrTrim(s.footerHint(), cols-1) + RESET)
	}

	if s.view != s.lastView {
		_, _ = tty.WriteString(ansiClearHome)
		s.lastView = s.view
	}
	frame := ansiHome + strings.ReplaceAll(out.String(), "\n", ansiClearEOL+"\n") + ansiClearBelow
	_, _ = tty.WriteString(frame)
}

func (s *tuiState) footerHint() string {
	switch s.view {
	case viewTimeline:
		return "[ ◄/►: Views │ ▲/▼: Select │ ENTER: Browse Files │ 1-9/0: Mount Snap │ /: Filter │ T: Thaw │ c: Client │ r: Refresh │ D: Del │ q: Quit ]"
	case viewBrowser:
		return "[ [ / ]: Older/Newer Dump Date │ SPACE: Mark │ u: Unmark │ m: Overwrite/NewPath │ o: Dest │ x: Restore (X: Chain) │ /: Filter │ p: Preload ]"
	case viewJobs:
		return "[ ◄/►: Views │ ▲/▼: Select Job │ r: Poll S3/Glacier Now │ R: Retry Failed │ d: Remove Record │ q: Quit ]"
	case viewSettings:
		return "[ ◄/►: Views │ ▲/▼: Field │ ENTER: Edit/Toggle │ t: Test S3 & LibreSSL │ s: Save Config (0600) │ q: Quit ]"
	default:
		return ""
	}
}

func (s *tuiState) renderTimeline(out *strings.Builder, cfg *Config, cols int) {
	drawTUIHeader(out, "DUMP TIMELINE", viewTimeline, cfg, s.activeClient, s.libreSSLVer, s.keyFP, cols)

	// Mountpoint quick-snap bar (bstat SNAPS style)
	out.WriteString(BOLD + "[ MOUNTS ] " + RESET)
	allCol := DIM
	if s.mountFilter == "" {
		allCol = GREEN + BOLD
	}
	out.WriteString(fmt.Sprintf("%s[0] ALL%s  ", allCol, RESET))
	for i, m := range s.mountSnaps {
		c := DIM
		if strings.EqualFold(s.mountFilter, m) {
			c = GREEN + BOLD
		}
		out.WriteString(fmt.Sprintf("%s[%d] %s%s  ", c, i+1, m, RESET))
	}
	if s.filterText != "" {
		out.WriteString(fmt.Sprintf("│ %s[FILTER: /%s]%s", YELLOW, s.filterText, RESET))
	}
	out.WriteString(fmt.Sprintf("│ %sTier: %s%s\n", DIM, s.restoreTier, RESET))
	out.WriteString(strings.Repeat("─", cols-1) + "\n")

	out.WriteString(fmt.Sprintf("%s  %-19s │ %-14s │ %-4s │ %-10s │ %-10s │ %-7s │ %s%s\n",
		BOLD, "DUMP TIMESTAMP", "MOUNT", "LVL", "ARCHIVE", "CATALOG", "GLACIER", "AUTH", RESET))
	out.WriteString(strings.Repeat("─", cols-1) + "\n")

	list := s.filteredTimelineSnapshots()
	maxRows := s.viewportRows()

	if len(list) == 0 {
		out.WriteString(fmt.Sprintf("%s  No snapshots matching current client/filter.%s\n", DIM, RESET))
		for i := 1; i < maxRows; i++ {
			out.WriteString("\n")
		}
		out.WriteString("\n")
		return
	}

	if s.timelineSelected >= len(list) {
		s.timelineSelected = len(list) - 1
	}
	if s.timelineSelected < 0 {
		s.timelineSelected = 0
	}
	if s.timelineSelected < s.timelineScroll {
		s.timelineScroll = s.timelineSelected
	}
	if s.timelineSelected >= s.timelineScroll+maxRows {
		s.timelineScroll = s.timelineSelected - maxRows + 1
	}

	end := s.timelineScroll + maxRows
	if end > len(list) {
		end = len(list)
	}

	for i := s.timelineScroll; i < end; i++ {
		rec := list[i]
		prefix := "  "
		tsStyle := ""
		if i == s.timelineSelected {
			prefix = YELLOW + ">" + RESET + " "
			tsStyle = WHITE_BG + BOLD
		}

		lvlStr := fmt.Sprintf("L%d  ", rec.Meta.Level)
		if rec.Meta.Level == 0 {
			lvlStr = LIME + BOLD + "L0  " + RESET
		} else {
			lvlStr = CYAN + fmt.Sprintf("+L%-2d", rec.Meta.Level) + RESET
		}

		catStr := RED + "MISSING   " + RESET
		if _, cached := s.eng.CachedCatalogEntries(rec.Meta.BaseName); cached {
			catStr = LIME + "CACHED    " + RESET
		} else if rec.HasAuthenticatedCatalog() {
			catStr = GREEN + fmt.Sprintf("%-10s", FormatBytes(rec.ListSize)) + RESET
		}

		authStr := RED + "NO-HMAC" + RESET
		if rec.IsComplete() && rec.HasAuthenticatedCatalog() {
			authStr = GREEN + "HMAC-OK" + RESET
		} else if rec.IsComplete() {
			authStr = YELLOW + "DATA-OK" + RESET
		}

		tsText := rec.Meta.Timestamp.Format("2006-01-02 15:04:05")
		out.WriteString(fmt.Sprintf("%s%s%-19s%s │ %-14s │ %s │ %-10s │ %s │ %s │ %s\n",
			prefix,
			tsStyle, tsText, RESET,
			padOrTrim(rec.Meta.MountGuess, 14),
			lvlStr,
			FormatBytes(rec.DataSize),
			catStr,
			formatGlacierBadge(rec.GlacierState),
			authStr,
		))
	}
	for i := end - s.timelineScroll; i < maxRows; i++ {
		out.WriteString("\n")
	}

	// Render BSD dump chain summary for highlighted snapshot
	sel := list[s.timelineSelected]
	chain := SnapshotChainForTarget(s.snapshots, sel)
	var chainParts []string
	for _, c := range chain {
		chainParts = append(chainParts, fmt.Sprintf("L%d(%s)", c.Meta.Level, c.Meta.Timestamp.Format("01-02 15:04")))
	}
	out.WriteString(fmt.Sprintf("%s[ DUMP CHAIN ]%s %s\n", BOLD, RESET, strings.Join(chainParts, " -> ")))
}

func (s *tuiState) renderBrowser(out *strings.Builder, cfg *Config, cols int) {
	drawTUIHeader(out, "SNAPSHOT BROWSER", viewBrowser, cfg, s.activeClient, s.libreSSLVer, s.keyFP, cols)

	if s.activeSnap == nil {
		out.WriteString(fmt.Sprintf("%s  Select a snapshot on View 1 (DUMP TIMELINE) and press ENTER to browse files.%s\n", DIM, RESET))
		for i := 0; i < s.rows-7; i++ {
			out.WriteString("\n")
		}
		return
	}

	snap := s.activeSnap
	modeBadge := YELLOW + BOLD + "OVERWRITE" + RESET
	if s.restoreMode == RestoreModeNewPath {
		destDisp := s.restoreDest
		if destDisp == "" {
			destDisp = "(prompt on restore)"
		}
		modeBadge = LIME + BOLD + "NEW_PATH -> " + padOrTrim(destDisp, 26) + RESET
	}

	markedCnt, markedBytes := 0, int64(0)
	if s.catalogTree != nil {
		markedCnt, markedBytes = s.catalogTree.MarkedSummary()
	}

	out.WriteString(fmt.Sprintf("%sMount:%s %-10s │ %sDate [%d/%d]:%s %s%s (L%d)%s │ %s │ %sTier:%s %s\n",
		BOLD, RESET, snap.Meta.MountGuess,
		BOLD, s.browserSnapIdx+1, len(s.browserMountSnaps), RESET,
		CYAN+BOLD, snap.Meta.Timestamp.Format("2006-01-02 15:04:05"), snap.Meta.Level, RESET,
		formatGlacierBadge(snap.GlacierState),
		DIM, RESET, s.restoreTier,
	))
	out.WriteString(fmt.Sprintf("%sMode :%s %s │ %sMarked:%s %s%d items (%s)%s",
		BOLD, RESET, modeBadge,
		BOLD, RESET, LIME+BOLD, markedCnt, FormatBytes(markedBytes), RESET))
	if s.filterText != "" {
		out.WriteString(fmt.Sprintf(" │ %s[FILTER: /%s]%s", YELLOW, s.filterText, RESET))
	}
	out.WriteString("\n" + strings.Repeat("─", cols-1) + "\n")

	treeRows := s.browserTreeRows()
	if s.catalogTree == nil || len(s.visibleNodes) == 0 {
		msg := "No files match filter."
		if s.catalogTree == nil {
			msg = "Loading catalog from S3 STANDARD..."
		}
		out.WriteString("  " + DIM + msg + RESET + "\n")
		for i := 1; i < treeRows; i++ {
			out.WriteString("\n")
		}
	} else {
		if s.browserSelected >= len(s.visibleNodes) {
			s.browserSelected = len(s.visibleNodes) - 1
		}
		if s.browserSelected < 0 {
			s.browserSelected = 0
		}
		if s.browserSelected < s.browserScroll {
			s.browserScroll = s.browserSelected
		}
		if s.browserSelected >= s.browserScroll+treeRows {
			s.browserScroll = s.browserSelected - treeRows + 1
		}

		end := s.browserScroll + treeRows
		if end > len(s.visibleNodes) {
			end = len(s.visibleNodes)
		}

		nameWidth := cols - 44
		if nameWidth < 24 {
			nameWidth = 24
		}

		for i := s.browserScroll; i < end; i++ {
			node := s.visibleNodes[i]
			cursor := " "
			rowHi := ""
			if i == s.browserSelected {
				cursor = YELLOW + ">" + RESET
				rowHi = WHITE_BG + BOLD
			}

			markBox := DIM + "[ ]" + RESET
			if node.Marked {
				markBox = LIME + BOLD + "[✓]" + RESET
			} else if node.PartialMark {
				markBox = YELLOW + "[~]" + RESET
			}

			indent := ""
			if s.filterText == "" && node.Level > 0 {
				rep := node.Level
				if rep > 10 {
					rep = 10
				}
				indent = strings.Repeat("  ", rep)
			}

			sym := " "
			nameCol := RESET
			dispLabel := node.Name
			if s.filterText != "" && node.RelPath != "" {
				dispLabel = node.RelPath
			}
			if node.IsDir {
				nameCol = BOLD + BLUE
				if len(node.Children) > 0 {
					if node.Expanded {
						sym = "▼"
					} else {
						sym = "►"
					}
				}
				if node != s.catalogTree.Root && !strings.HasSuffix(dispLabel, "/") {
					dispLabel += "/"
				}
			} else if node.Entry.Type == "l" {
				nameCol = CYAN
				if node.Entry.LinkTarget != "" {
					dispLabel += " -> " + node.Entry.LinkTarget
				}
			}

			if rowHi != "" {
				nameCol = rowHi
			}

			treeCell := padOrTrim(indent+sym+" "+dispLabel, nameWidth)
			padSpaces := nameWidth - utf8.RuneCountInString(treeCell)
			if padSpaces < 1 {
				padSpaces = 1
			}

			sz := node.Entry.Size
			if node.IsDir {
				sz = node.SubtreeSize
			}
			mtimeStr := "—"
			if node.Entry.Mtime > 0 {
				mtimeStr = time.Unix(node.Entry.Mtime, 0).Format("2006-01-02 15:04")
			}

			out.WriteString(fmt.Sprintf("%s %s %s%s%s%s %10s  %10s  %s\n",
				cursor,
				markBox,
				nameCol, treeCell, RESET,
				strings.Repeat(" ", padSpaces),
				node.Entry.ModeString(),
				FormatBytes(sz),
				DIM+mtimeStr+RESET,
			))
		}
		for i := end - s.browserScroll; i < treeRows; i++ {
			out.WriteString("\n")
		}
	}

	out.WriteString(strings.Repeat("─", cols-1) + "\n")

	// Selected Path Inspector + Time-Travel Version Strip across mount snapshots
	var selNode *CatalogNode
	if s.browserSelected >= 0 && s.browserSelected < len(s.visibleNodes) {
		selNode = s.visibleNodes[s.browserSelected]
	}
	if selNode == nil {
		out.WriteString("\n\n")
		return
	}

	fullOrig := filepath.Join(snap.Meta.MountGuess, selNode.RelPath)
	out.WriteString(fmt.Sprintf("%s[ PATH ]%s %s  %s(ino:%d uid:%d gid:%d)%s\n",
		BOLD, RESET, padOrTrim(fullOrig, cols-36), DIM, selNode.Entry.Inode, selNode.Entry.UID, selNode.Entry.GID, RESET))

	history := s.eng.FileHistory(s.browserMountSnaps, selNode.RelPath)
	var chips []string
	maxChips := 5
	startIdx := s.browserSnapIdx - 2
	if startIdx < 0 {
		startIdx = 0
	}
	if startIdx+maxChips > len(history) {
		startIdx = len(history) - maxChips
		if startIdx < 0 {
			startIdx = 0
		}
	}
	endIdx := startIdx + maxChips
	if endIdx > len(history) {
		endIdx = len(history)
	}

	for i := startIdx; i < endIdx; i++ {
		h := history[i]
		dateTag := fmt.Sprintf("%s L%d", h.Snapshot.Meta.Timestamp.Format("01-02 15:04"), h.Snapshot.Meta.Level)
		var stateTag string
		if !h.CatalogLoaded {
			stateTag = "?"
		} else if !h.Present {
			stateTag = "--"
		} else if h.Entry.Type == "d" {
			stateTag = "dir"
		} else {
			stateTag = FormatBytes(h.Entry.Size)
		}
		chip := fmt.Sprintf("[%s: %s]", dateTag, stateTag)
		if i == s.browserSnapIdx {
			chip = WHITE_BG + BOLD + chip + RESET
		} else if h.CatalogLoaded && h.Present {
			chip = LIME + chip + RESET
		} else {
			chip = DIM + chip + RESET
		}
		chips = append(chips, chip)
	}
	out.WriteString(fmt.Sprintf("%s[ VERSIONS ([/]) ]%s %s\n", BOLD, RESET, strings.Join(chips, " ")))
}

func (s *tuiState) renderJobs(out *strings.Builder, cfg *Config, cols int) {
	drawTUIHeader(out, "JOBS & GLACIER MONITOR", viewJobs, cfg, s.activeClient, s.libreSSLVer, s.keyFP, cols)

	jobs := s.eng.JobsSnapshot()
	maxRows := s.viewportRows()

	out.WriteString(fmt.Sprintf("%s  %-18s │ %-11s │ %-9s │ %-24s │ %s%s\n",
		BOLD, "JOB ID", "STATUS", "MODE", "PROGRESS", "ARCHIVE / SPEED", RESET))
	out.WriteString(strings.Repeat("─", cols-1) + "\n")

	if len(jobs) == 0 {
		out.WriteString(fmt.Sprintf("%s  No active or historical restore jobs.%s\n", DIM, RESET))
		for i := 1; i < maxRows; i++ {
			out.WriteString("\n")
		}
		out.WriteString("\n\n")
		return
	}

	if s.jobsSelected >= len(jobs) {
		s.jobsSelected = len(jobs) - 1
	}
	if s.jobsSelected < 0 {
		s.jobsSelected = 0
	}
	if s.jobsSelected < s.jobsScroll {
		s.jobsScroll = s.jobsSelected
	}
	if s.jobsSelected >= s.jobsScroll+maxRows {
		s.jobsScroll = s.jobsSelected - maxRows + 1
	}

	end := s.jobsScroll + maxRows
	if end > len(jobs) {
		end = len(jobs)
	}

	for i := s.jobsScroll; i < end; i++ {
		j := jobs[i]
		prefix := "  "
		idCol := ""
		if i == s.jobsSelected {
			prefix = YELLOW + ">" + RESET + " "
			idCol = WHITE_BG + BOLD
		}

		var stCol string
		switch j.Status {
		case JobCompleted:
			stCol = LIME + BOLD
		case JobFailed:
			stCol = RED_BG + BOLD
		case JobThawing:
			stCol = YELLOW + BOLD
		default:
			stCol = CYAN + BOLD
		}

		bar := formatProgressBar(j.DoneBytes, j.TotalBytes, 14)
		speed := ""
		if j.SpeedBPS > 0 {
			speed = fmt.Sprintf(" (%.2f MiB/s)", j.SpeedBPS/1048576.0)
		}

		out.WriteString(fmt.Sprintf("%s%s%-18s%s │ %s%-11s%s │ %-9s │ %s │ %s%s\n",
			prefix,
			idCol, padOrTrim(j.ID, 18), RESET,
			stCol, j.Status, RESET,
			j.Mode,
			bar,
			padOrTrim(j.Archive.BaseName, 32),
			DIM+speed+RESET,
		))
	}
	for i := end - s.jobsScroll; i < maxRows; i++ {
		out.WriteString("\n")
	}

	out.WriteString(strings.Repeat("─", cols-1) + "\n")
	sel := jobs[s.jobsSelected]
	destInfo := sel.MountPoint
	if sel.Mode == RestoreModeNewPath {
		destInfo = sel.TargetDest + " (base mount " + sel.MountPoint + ")"
	}
	out.WriteString(fmt.Sprintf("%s[ TARGET ]%s %s │ %sTier:%s %s │ %sUpdated:%s %s\n",
		BOLD, RESET, padOrTrim(destInfo, cols-44), DIM, RESET, sel.Tier, DIM, RESET, sel.UpdatedAt.Format("15:04:05")))
	detail := sel.Message
	if sel.Error != "" {
		detail = sel.Error
	}
	out.WriteString(fmt.Sprintf("%s[ DETAIL ]%s %s\n", BOLD, RESET, padOrTrim(detail, cols-13)))
}

func (s *tuiState) renderSettings(out *strings.Builder, cfg *Config, cols int) {
	drawTUIHeader(out, "S3 & SYSTEM CONFIG", viewSettings, cfg, s.activeClient, s.libreSSLVer, s.keyFP, cols)

	dirtyBadge := GREEN + "SAVED" + RESET
	if s.cfgDirty {
		dirtyBadge = YELLOW + BOLD + "UNSAVED CHANGES (press 's' to save)" + RESET
	}
	out.WriteString(fmt.Sprintf("%sConfig File:%s %s (mode 0600)  │  %s\n\n",
		BOLD, RESET, s.eng.ConfigPath(), dirtyBadge))

	secretMask := "(empty — using AWS profile / IAM / env)"
	if s.cfgDraft.SecretAccessKey != "" {
		secretMask = strings.Repeat("*", 16) + " (configured)"
	}

	type row struct {
		label string
		val   string
		isAct bool
	}
	rows := []row{
		{"S3 Bucket", s.cfgDraft.Bucket, false},
		{"AWS Region", s.cfgDraft.Region, false},
		{"Custom S3 Endpoint", s.cfgDraft.Endpoint, false},
		{"Force S3 Path-Style", fmt.Sprintf("%v", s.cfgDraft.ForcePathStyle), false},
		{"AWS Shared Profile", s.cfgDraft.AWSProfile, false},
		{"AWS Access Key ID", s.cfgDraft.AccessKeyID, false},
		{"AWS Secret Access Key", secretMask, false},
		{"Client Identifier", s.cfgDraft.EffectiveClient(), false},
		{"LibreSSL Keyfile (0400)", s.cfgDraft.EffectiveKeyPath(), false},
		{"Default Glacier Tier", s.cfgDraft.EffectiveTier(""), false},
		{"[ Test S3 Connectivity & LibreSSL ChaCha20+HMAC Self-Test ]", "", true},
		{"[ Save Configuration to Disk (0600) ]", "", true},
		{"[ Generate / Verify Client 0400 Keyfile ]", "", true},
	}

	for i, r := range rows {
		if i == 10 {
			out.WriteString("\n")
		}
		prefix := "  "
		labelStyle := BOLD
		if i == s.settingSelected {
			prefix = YELLOW + ">" + RESET + " "
			labelStyle = WHITE_BG + BOLD
		}
		if r.isAct {
			col := CYAN + BOLD
			if i == s.settingSelected {
				col = WHITE_BG + BOLD
			}
			out.WriteString(fmt.Sprintf("%s%s%s%s\n", prefix, col, r.label, RESET))
			continue
		}
		disp := r.val
		if disp == "" {
			disp = DIM + "(unset)" + RESET
		}
		out.WriteString(fmt.Sprintf("%s%s%-26s%s : %s\n", prefix, labelStyle, r.label, RESET, disp))
	}
}
