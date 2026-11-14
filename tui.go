//go:build openbsd || linux

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

const (
	RESET    = "\033[0m"
	BOLD     = "\033[1m"
	DIM      = "\033[2m"
	RED      = "\033[31m"
	GREEN    = "\033[32m"
	LIME     = "\033[92m"
	YELLOW   = "\033[33m"
	BLUE     = "\033[34m"
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
	viewTimeline = iota
	viewBrowser
	viewJobs
	viewSettings
	totalViews
)

const totalSettingRows = 15

type tuiKey int

const (
	keyRune tuiKey = iota
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

type tuiEvent struct {
	Kind tuiKey
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
	ctx         context.Context
	eng         *Engine
	asyncC      chan func(*tuiState)
	cols, rows  int
	view        int
	lastView    int
	sslSummary  string
	keyFP       string
	localMounts []MountInfo

	statusMsg   string
	statusIsErr bool
	busyLabel   string

	// View 0: Dump Timeline
	activeClient     string
	knownClients     []string
	snapshots        []SnapshotRecord
	mountSnaps       []string
	mountFilter      string
	timelineSelected int
	timelineScroll   int

	// View 1: Snapshot Browser & Time-Travel
	browserMountSnaps []SnapshotRecord
	browserSnapIdx    int
	activeSnap        *SnapshotRecord
	catalogTree       *CatalogTree
	visibleNodes      []*CatalogNode
	browserSelected   int
	browserScroll     int
	expandedRelPaths  map[string]bool
	savedMarks        map[string]bool
	restoreMode       string
	restoreDest       string
	restoreTier       string

	// View 2: Jobs & Glacier Monitor
	jobsSelected int
	jobsScroll   int

	// View 3: S3 & System Config
	cfgDraft        *Config
	cfgDirty        bool
	settingSelected int

	// Shared filter & modal prompt
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
	orig, err := getTermios(fd)
	if err != nil {
		_ = tty.Close()
		return nil, nil, fmt.Errorf("termios get: %w", err)
	}
	raw := *orig
	raw.Iflag &^= unix.BRKINT | unix.ICRNL | unix.INPCK | unix.ISTRIP | unix.IXON
	raw.Lflag &^= unix.ECHO | unix.ICANON | unix.IEXTEN | unix.ISIG
	raw.Cflag |= unix.CS8
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	if err := setTermios(fd, &raw); err != nil {
		_ = tty.Close()
		return nil, nil, fmt.Errorf("termios set: %w", err)
	}
	return tty, orig, nil
}

func queryWinsize(fd int) (int, int) {
	ws, err := unix.IoctlGetWinsize(fd, unix.TIOCGWINSZ)
	if err != nil || ws.Col == 0 || ws.Row == 0 {
		return 100, 32
	}
	c, r := int(ws.Col), int(ws.Row)
	if c < 80 {
		c = 80
	}
	if r < 24 {
		r = 24
	}
	return c, r
}

func startTTYReader(tty *os.File, out chan<- tuiEvent, stop <-chan struct{}) {
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
			b := buf[:n]
			if b[0] == 0x03 {
				out <- tuiEvent{Kind: keyQuit}
				return
			}
			if b[0] == 0x1b {
				if len(b) == 1 {
					out <- tuiEvent{Kind: keyEsc}
					continue
				}
				if len(b) >= 3 && (b[1] == '[' || b[1] == 'O') {
					switch b[2] {
					case 'A':
						out <- tuiEvent{Kind: keyUp}
					case 'B':
						out <- tuiEvent{Kind: keyDown}
					case 'C':
						out <- tuiEvent{Kind: keyRight}
					case 'D':
						out <- tuiEvent{Kind: keyLeft}
					case 'H':
						out <- tuiEvent{Kind: keyHome}
					case 'F':
						out <- tuiEvent{Kind: keyEnd}
					case '5':
						if len(b) >= 4 && b[3] == '~' {
							out <- tuiEvent{Kind: keyPageUp}
						}
					case '6':
						if len(b) >= 4 && b[3] == '~' {
							out <- tuiEvent{Kind: keyPageDown}
						}
					case '1', '7':
						if len(b) >= 4 && b[3] == '~' {
							out <- tuiEvent{Kind: keyHome}
						}
					case '4', '8':
						if len(b) >= 4 && b[3] == '~' {
							out <- tuiEvent{Kind: keyEnd}
						}
					}
				}
				continue
			}
			switch b[0] {
			case '\r', '\n':
				out <- tuiEvent{Kind: keyEnter}
			case 127, '\b':
				out <- tuiEvent{Kind: keyBackspace}
			case '\t':
				out <- tuiEvent{Kind: keyTab}
			case 0x15:
				out <- tuiEvent{Kind: keyCtrlU}
			default:
				for len(b) > 0 {
					r, sz := utf8.DecodeRune(b)
					if r != utf8.RuneError && r >= 32 {
						out <- tuiEvent{Kind: keyRune, Ch: r}
					}
					b = b[sz:]
				}
			}
		}
	}()
}

// RunTUI launches the 4-view bstat-inspired Kerr console on OpenBSD or Linux.
func RunTUI(ctx context.Context, eng *Engine) error {
	tty, origTerm, err := openRawTTY()
	if err != nil {
		return err
	}
	fd := int(tty.Fd())
	restoreTerm := func() {
		_ = setTermios(fd, origTerm)
		_, _ = tty.WriteString(ansiAltExit)
		_ = tty.Close()
	}
	defer func() {
		if r := recover(); r != nil {
			restoreTerm()
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

	if rt, err := eng.ResolveSSL(); err == nil {
		st.sslSummary = rt.Summary()
	} else {
		st.sslSummary = "SSL:MISSING"
		st.setStatus(err.Error(), true)
	}
	st.refreshKeyFP()
	if mounts, err := ListMounts(); err == nil {
		st.localMounts = mounts
	}
	_ = eng.LoadJobs()

	if cfg.ValidateS3() != nil {
		st.view = viewSettings
		st.setStatus("Configure S3 Bucket & press 's' to save config.", false)
	} else {
		st.triggerRefreshSnapshots(true)
		st.triggerPollJobs(true)
	}

	stopReader := make(chan struct{})
	inputCh := make(chan tuiEvent, 32)
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
			restoreTerm()
			return nil
		case sig := <-sigCh:
			if sig == unix.SIGWINCH {
				st.cols, st.rows = queryWinsize(fd)
				_, _ = tty.WriteString(ansiClearHome)
				st.renderFrame(tty)
			} else {
				close(stopReader)
				restoreTerm()
				return nil
			}
		case fn := <-st.asyncC:
			fn(st)
			st.renderFrame(tty)
		case ev := <-inputCh:
			if st.handleInput(ev) {
				close(stopReader)
				restoreTerm()
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
}

func (s *tuiState) refreshKeyFP() {
	km, err := s.eng.EnsureKeyForClient(s.activeClient, false)
	if err != nil {
		s.keyFP = "NO-KEY"
		return
	}
	s.keyFP = km.Fingerprint()
	km.Wipe()
}

func (s *tuiState) refreshSSLSummary() {
	if rt, err := s.eng.ResolveSSL(); err == nil {
		s.sslSummary = rt.Summary()
	} else {
		s.sslSummary = "SSL:MISSING"
	}
}

func (s *tuiState) post(fn func(*tuiState)) {
	select {
	case s.asyncC <- fn:
	case <-s.ctx.Done():
	}
}

func moveCursor(cur, delta, total int) int {
	if total <= 0 {
		return 0
	}
	next := cur + delta
	if next < 0 {
		return 0
	}
	if next >= total {
		return total - 1
	}
	return next
}

func clampViewport(sel, scroll, total, height int) (int, int) {
	if total <= 0 {
		return 0, 0
	}
	if sel >= total {
		sel = total - 1
	}
	if sel < 0 {
		sel = 0
	}
	if sel < scroll {
		scroll = sel
	}
	if sel >= scroll+height {
		scroll = sel - height + 1
	}
	if scroll < 0 {
		scroll = 0
	}
	return sel, scroll
}

// --- ASYNC OPERATIONS ---

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
			s.post(func(st *tuiState) { st.busyLabel = ""; st.setStatus(err.Error(), true) })
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
			seen := make(map[string]bool)
			var ms []string
			for _, sn := range snaps {
				if sn.Meta.MountGuess != "" && !seen[sn.Meta.MountGuess] {
					seen[sn.Meta.MountGuess] = true
					ms = append(ms, sn.Meta.MountGuess)
				}
			}
			sort.Strings(ms)
			if len(ms) > 9 {
				ms = ms[:9]
			}
			st.mountSnaps = ms
			st.refreshKeyFP()
			st.setStatus(fmt.Sprintf("Loaded %d snapshots for client '%s'", len(snaps), client), false)
		})
	}()
}

func (s *tuiState) filteredTimelineSnapshots() []SnapshotRecord {
	var out []SnapshotRecord
	for _, snap := range s.snapshots {
		if s.mountFilter != "" && !strings.EqualFold(snap.Meta.MountGuess, s.mountFilter) {
			continue
		}
		if s.filterText != "" && s.view == viewTimeline {
			hay := snap.Meta.BaseName + " " + snap.Meta.MountGuess + " " + snap.GlacierState
			if rx, err := regexp.Compile("(?i)" + s.filterText); err == nil {
				if !rx.MatchString(hay) {
					continue
				}
			} else if !strings.Contains(strings.ToUpper(hay), strings.ToUpper(s.filterText)) {
				continue
			}
		}
		out = append(out, snap)
	}
	return out
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

func (s *tuiState) openSnapshotInBrowser(snap SnapshotRecord, anchorPath string) {
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

	if s.activeSnap != nil && s.activeSnap.Meta.MountSlug == snap.Meta.MountSlug && s.catalogTree != nil {
		s.captureTreeState()
	} else {
		s.expandedRelPaths = make(map[string]bool)
		s.savedMarks = make(map[string]bool)
	}

	s.browserMountSnaps = sameMount
	s.browserSnapIdx = idx
	cp := snap
	s.activeSnap = &cp
	s.view = viewBrowser
	if s.promptMode != promptFilter {
		s.filterText = ""
	}

	if entries, ok := s.eng.CachedCatalogEntries(snap.Meta.BaseName); ok {
		s.applyLoadedCatalog(snap, entries, anchorPath)
		s.prefetchAdjacentCatalogs()
		return
	}

	s.busyLabel = fmt.Sprintf("Fetching & verifying catalog %s ...", snap.Meta.ListFile)
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
				st.setStatus(fmt.Sprintf("HMAC verified & loaded %d entries (%s)", len(entries), snap.Meta.BaseName), false)
				st.prefetchAdjacentCatalogs()
			}
		})
	}()
}

func (s *tuiState) applyLoadedCatalog(snap SnapshotRecord, entries []CatalogEntry, anchorPath string) {
	tree := BuildCatalogTree(snap.Meta.MountGuess, entries)
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
	for probe := anchorPath; probe != ""; {
		if n, ok := tree.ByPath[probe]; ok {
			tree.ExpandAncestors(n)
			anchorPath = probe
			break
		}
		if idx := strings.LastIndexByte(probe, '/'); idx != -1 {
			probe = probe[:idx]
		} else {
			probe = ""
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
	f := ""
	if s.view == viewBrowser {
		f = s.filterText
	}
	s.visibleNodes = s.catalogTree.Flatten(f)
	if anchorPath != "" {
		for i, n := range s.visibleNodes {
			if n.RelPath == anchorPath {
				s.browserSelected = i
				break
			}
		}
	}
	s.browserSelected, s.browserScroll = clampViewport(s.browserSelected, s.browserScroll, len(s.visibleNodes), s.browserTreeRows())
}

func (s *tuiState) prefetchAdjacentCatalogs() {
	var metas []ArchiveMeta
	for _, off := range []int{-1, 1, -2, 2} {
		idx := s.browserSnapIdx + off
		if idx >= 0 && idx < len(s.browserMountSnaps) {
			c := s.browserMountSnaps[idx]
			if c.HasAuthenticatedCatalog() {
				if _, ok := s.eng.CachedCatalogEntries(c.Meta.BaseName); !ok {
					metas = append(metas, c.Meta)
				}
			}
		}
	}
	if len(metas) == 0 {
		return
	}
	go func() {
		for _, m := range metas {
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
	s.busyLabel = fmt.Sprintf("Preloading %d S3 STANDARD catalogs...", len(snaps))
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
	next := s.browserSnapIdx + delta
	if next < 0 {
		s.setStatus("Already at newest dump date for this mount.", false)
		return
	}
	if next >= len(s.browserMountSnaps) {
		s.setStatus("Already at oldest dump date for this mount.", false)
		return
	}
	anchor := ""
	if s.browserSelected >= 0 && s.browserSelected < len(s.visibleNodes) {
		anchor = s.visibleNodes[s.browserSelected].RelPath
	}
	s.openSnapshotInBrowser(s.browserMountSnaps[next], anchor)
}

func (s *tuiState) triggerThawSnapshot(snap SnapshotRecord) {
	tier := s.restoreTier
	s.busyLabel = fmt.Sprintf("Submitting Glacier %s thaw for %s ...", tier, snap.Meta.DataFile)
	go func() {
		opCtx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
		defer cancel()
		store, err := s.eng.NewStore(opCtx)
		if err != nil {
			s.post(func(st *tuiState) { st.busyLabel = ""; st.setStatus(err.Error(), true) })
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

func (s *tuiState) dispatchRestoreFromBrowser(includeChain bool) {
	if s.activeSnap == nil || s.catalogTree == nil {
		s.setStatus("No snapshot catalog loaded.", true)
		return
	}
	paths, fullSnap := s.catalogTree.EffectiveRestorePaths()
	if !fullSnap && len(paths) == 0 && s.browserSelected >= 0 && s.browserSelected < len(s.visibleNodes) {
		sel := s.visibleNodes[s.browserSelected]
		if sel == s.catalogTree.Root {
			fullSnap = true
		} else if sel.RelPath != "" {
			paths = []string{sel.RelPath}
		}
	}

	if s.restoreMode == RestoreModeNewPath && strings.TrimSpace(s.restoreDest) == "" {
		defDest := filepath.Join(s.activeSnap.Meta.MountGuess, "restore_"+s.activeSnap.Meta.Timestamp.Format("20060102_150405"))
		if len(paths) == 1 && !fullSnap {
			defDest = filepath.Base(paths[0]) + ".restored"
		}
		s.promptMode = promptDestPath
		s.promptTitle = "Enter NEW_PATH filename, directory, or suffix (e.g. .restored)"
		s.promptBuf = defDest
		return
	}

	targets := []SnapshotRecord{*s.activeSnap}
	if includeChain {
		targets = SnapshotChainForTarget(s.browserMountSnaps, *s.activeSnap)
	}
	for _, snap := range targets {
		job, err := s.eng.EnqueueRestoreJob(snap.Meta.Client, snap.Meta, snap.Meta.MountGuess, s.restoreTier, s.restoreMode, s.restoreDest, paths, fullSnap)
		if err != nil {
			s.setStatus("Queue restore failed: "+err.Error(), true)
			return
		}
		s.eng.StartJobAsync(s.ctx, job.ID)
	}
	s.view = viewJobs
	s.jobsSelected = 0
	s.setStatus(fmt.Sprintf("Dispatched %d restore job(s) [%s mode]", len(targets), s.restoreMode), false)
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
				if err != nil {
					st.setStatus("Job poll: "+err.Error(), true)
				} else {
					st.setStatus("Polled all pending restore jobs.", false)
				}
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
			s.post(func(st *tuiState) { st.busyLabel = ""; st.setStatus(err.Error(), true) })
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
	s.busyLabel = "Running SSL ChaCha20+HMAC self-test & S3 check..."

	go func() {
		opCtx, cancel := context.WithTimeout(s.ctx, 25*time.Second)
		defer cancel()
		km, err := s.eng.EnsureKeyForClient(draft.EffectiveClient(), true)
		if err != nil {
			s.post(func(st *tuiState) { st.busyLabel = ""; st.setStatus("Key error: "+err.Error(), true) })
			return
		}
		fp := km.Fingerprint()
		rt, err := SelfTestSSL(opCtx, draft, km)
		km.Wipe()
		if err != nil {
			s.post(func(st *tuiState) {
				st.busyLabel = ""
				st.refreshSSLSummary()
				st.setStatus("Crypto self-test failed: "+err.Error(), true)
			})
			return
		}
		store, err := NewS3Store(opCtx, draft)
		if err == nil {
			err = store.TestConnection(opCtx, draft.EffectiveClient())
		}
		s.post(func(st *tuiState) {
			st.busyLabel = ""
			st.sslSummary = rt.Summary()
			st.keyFP = fp
			if err != nil {
				st.setStatus(err.Error(), true)
			} else {
				st.setStatus(fmt.Sprintf("PASS: %s (key %s) + s3://%s/%s/ reachable", rt.Summary(), fp, draft.Bucket, draft.EffectiveClient()), false)
			}
		})
	}()
}

// --- INPUT DISPATCH ---

func (s *tuiState) handleInput(ev tuiEvent) bool {
	if ev.Kind == keyQuit {
		return true
	}
	if s.promptMode != promptNone {
		s.handlePromptInput(ev)
		return false
	}
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

func (s *tuiState) handlePromptInput(ev tuiEvent) {
	if s.promptMode == promptConfirmDelete {
		if ev.Kind == keyRune && (ev.Ch == 'y' || ev.Ch == 'Y') && s.pendingSnap != nil {
			t := *s.pendingSnap
			s.pendingSnap = nil
			s.promptMode = promptNone
			s.triggerDeleteSnapshot(t)
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
				s.setStatus("Set NEW_PATH target: "+val+" (press 'x' to restore)", false)
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

func (s *tuiState) handleTimelineInput(ev tuiEvent) {
	list := s.filteredTimelineSnapshots()
	step := max(5, s.viewportRows()-2)
	switch ev.Kind {
	case keyUp:
		s.timelineSelected = moveCursor(s.timelineSelected, -1, len(list))
	case keyDown:
		s.timelineSelected = moveCursor(s.timelineSelected, 1, len(list))
	case keyPageUp:
		s.timelineSelected = moveCursor(s.timelineSelected, -step, len(list))
	case keyPageDown:
		s.timelineSelected = moveCursor(s.timelineSelected, step, len(list))
	case keyHome:
		s.timelineSelected = 0
	case keyEnd:
		s.timelineSelected = max(0, len(list)-1)
	case keyEnter:
		if s.timelineSelected >= 0 && s.timelineSelected < len(list) {
			s.openSnapshotInBrowser(list[s.timelineSelected], "")
		}
	case keyRune:
		switch ev.Ch {
		case '/':
			s.promptMode, s.promptTitle, s.promptBuf = promptFilter, "Filter snapshots (regex/substring)", s.filterText
		case 'w':
			s.timelineSelected = moveCursor(s.timelineSelected, -step, len(list))
		case 's':
			s.timelineSelected = moveCursor(s.timelineSelected, step, len(list))
		case 'r':
			s.triggerRefreshSnapshots(true)
		case 't':
			if s.restoreTier == "Bulk" {
				s.restoreTier = "Standard"
			} else {
				s.restoreTier = "Bulk"
			}
			s.setStatus("Glacier thaw tier set to "+s.restoreTier, false)
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
				s.promptMode, s.promptTitle, s.promptBuf = promptClientSwitch, "Switch S3 client prefix", s.activeClient
			}
		case '0':
			s.mountFilter, s.timelineSelected = "", 0
		case '1', '2', '3', '4', '5', '6', '7', '8', '9':
			if idx := int(ev.Ch - '1'); idx < len(s.mountSnaps) {
				if strings.EqualFold(s.mountFilter, s.mountSnaps[idx]) {
					s.mountFilter = ""
				} else {
					s.mountFilter = s.mountSnaps[idx]
				}
				s.timelineSelected = 0
			}
		case 'D':
			if s.timelineSelected >= 0 && s.timelineSelected < len(list) {
				t := list[s.timelineSelected]
				s.pendingSnap = &t
				s.promptMode = promptConfirmDelete
				s.promptTitle = fmt.Sprintf("Delete S3 snapshot %s? Press 'y' to confirm, any other key to abort", t.Meta.BaseName)
			}
		}
	}
}

func (s *tuiState) handleBrowserInput(ev tuiEvent) {
	total := len(s.visibleNodes)
	step := max(5, s.browserTreeRows()-2)
	switch ev.Kind {
	case keyUp:
		s.browserSelected = moveCursor(s.browserSelected, -1, total)
	case keyDown:
		s.browserSelected = moveCursor(s.browserSelected, 1, total)
	case keyPageUp:
		s.browserSelected = moveCursor(s.browserSelected, -step, total)
	case keyPageDown:
		s.browserSelected = moveCursor(s.browserSelected, step, total)
	case keyHome:
		s.browserSelected = 0
	case keyEnd:
		s.browserSelected = max(0, total-1)
	case keyEnter:
		if s.catalogTree != nil && s.browserSelected >= 0 && s.browserSelected < total {
			node := s.visibleNodes[s.browserSelected]
			if s.filterText != "" {
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
			if s.catalogTree != nil && s.browserSelected >= 0 && s.browserSelected < total {
				s.catalogTree.ToggleMark(s.visibleNodes[s.browserSelected])
			}
		case 'u':
			if s.catalogTree != nil {
				s.catalogTree.ClearMarks()
				s.savedMarks = make(map[string]bool)
				s.setStatus("Cleared all restore marks.", false)
			}
		case '[':
			s.stepSnapshotDate(+1)
		case ']':
			s.stepSnapshotDate(-1)
		case '/':
			s.promptMode, s.promptTitle, s.promptBuf = promptFilter, "Filter snapshot files (regex/substring)", s.filterText
		case 'w':
			s.browserSelected = moveCursor(s.browserSelected, -step, total)
		case 's':
			s.browserSelected = moveCursor(s.browserSelected, step, total)
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
			s.promptMode, s.promptTitle, s.promptBuf = promptDestPath, "Set NEW_PATH target filename, directory, or suffix", s.restoreDest
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

func (s *tuiState) handleJobsInput(ev tuiEvent) {
	jobs := s.eng.JobsSnapshot()
	switch ev.Kind {
	case keyUp:
		s.jobsSelected = moveCursor(s.jobsSelected, -1, len(jobs))
	case keyDown:
		s.jobsSelected = moveCursor(s.jobsSelected, 1, len(jobs))
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

func (s *tuiState) handleSettingsInput(ev tuiEvent) {
	switch ev.Kind {
	case keyUp:
		s.settingSelected = moveCursor(s.settingSelected, -1, totalSettingRows)
	case keyDown:
		s.settingSelected = moveCursor(s.settingSelected, 1, totalSettingRows)
	case keyEnter:
		s.activateSettingRow(s.settingSelected)
	case keyRune:
		switch ev.Ch {
		case ' ':
			if s.settingSelected == 3 || s.settingSelected == 11 {
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
	prompts := map[int][2]string{
		0:  {"S3 Bucket Name", s.cfgDraft.Bucket},
		1:  {"AWS Region (e.g. ap-southeast-2)", s.cfgDraft.Region},
		2:  {"Custom S3 Endpoint URL (blank for AWS S3)", s.cfgDraft.Endpoint},
		4:  {"AWS Shared Credentials Profile", s.cfgDraft.AWSProfile},
		5:  {"AWS Access Key ID", s.cfgDraft.AccessKeyID},
		6:  {"AWS Secret Access Key (stored 0600)", s.cfgDraft.SecretAccessKey},
		7:  {"Client Identifier", s.cfgDraft.EffectiveClient()},
		8:  {"Master Keyfile Path (0400)", s.cfgDraft.EffectiveKeyPath()},
		9:  {"SSL Binary Path (blank = OS auto-detect)", s.cfgDraft.OpenSSLBin},
		10: {"SSL LD_LIBRARY_PATH (Linux custom prefix, e.g. /opt/kerr/lib)", s.cfgDraft.LDLibraryPath},
	}
	if p, ok := prompts[idx]; ok {
		s.promptMode, s.promptTitle, s.promptBuf = promptEditSetting, p[0], p[1]
		return
	}
	switch idx {
	case 3:
		s.cfgDraft.ForcePathStyle = !s.cfgDraft.ForcePathStyle
		s.cfgDirty = true
		s.eng.ApplyRuntimeConfig(s.cfgDraft)
	case 11:
		if NormalizeTier(s.cfgDraft.DefaultTier) == "Bulk" {
			s.cfgDraft.DefaultTier = "Standard"
		} else {
			s.cfgDraft.DefaultTier = "Bulk"
		}
		s.restoreTier = s.cfgDraft.DefaultTier
		s.cfgDirty = true
		s.eng.ApplyRuntimeConfig(s.cfgDraft)
	case 12:
		s.triggerTestS3AndCrypto()
	case 13:
		s.saveDraftConfig()
	case 14:
		s.eng.ApplyRuntimeConfig(s.cfgDraft)
		km, err := s.eng.EnsureKeyForClient(s.cfgDraft.EffectiveClient(), true)
		if err != nil {
			s.setStatus("Keyfile error: "+err.Error(), true)
			return
		}
		fp, created, path := km.Fingerprint(), km.Created, km.Path
		km.Wipe()
		s.keyFP = fp
		if created {
			s.setStatus(fmt.Sprintf("Generated new 0400 keyfile %s (ID %s)", path, fp), false)
		} else {
			s.setStatus(fmt.Sprintf("Verified existing keyfile %s (ID %s)", path, fp), false)
		}
	}
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
	case 9:
		s.cfgDraft.OpenSSLBin = val
	case 10:
		s.cfgDraft.LDLibraryPath = val
	}
	s.cfgDraft.Normalize()
	s.cfgDirty = true
	s.eng.ApplyRuntimeConfig(s.cfgDraft)
	s.refreshKeyFP()
	s.refreshSSLSummary()
}

func (s *tuiState) saveDraftConfig() {
	if err := s.eng.UpdateAndSaveConfig(s.cfgDraft); err != nil {
		s.setStatus("Save config failed: "+err.Error(), true)
		return
	}
	s.cfgDirty = false
	s.activeClient = s.cfgDraft.EffectiveClient()
	s.restoreTier = s.cfgDraft.EffectiveTier("")
	s.refreshKeyFP()
	s.refreshSSLSummary()
	s.setStatus("Saved configuration to "+s.eng.ConfigPath()+" (mode 0600)", false)
	if s.cfgDraft.ValidateS3() == nil {
		s.triggerRefreshSnapshots(true)
	}
}

// --- FRAME RENDERING ---

func (s *tuiState) viewportRows() int    { return max(6, s.rows-13) }
func (s *tuiState) browserTreeRows() int { return max(6, s.rows-15) }

func padOrTrim(str string, maxWidth int) string {
	if maxWidth <= 3 {
		return ""
	}
	if utf8.RuneCountInString(str) <= maxWidth {
		return str
	}
	r := []rune(str)
	return string(r[:maxWidth-3]) + "..."
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
	width = max(8, width)
	if total <= 0 {
		return DIM + strings.Repeat("░", width) + RESET
	}
	ratio := min(1.0, max(0.0, float64(done)/float64(total)))
	filled := int(ratio * float64(width))
	return fmt.Sprintf("%s%s%s%s%s %5.1f%%", CYAN, strings.Repeat("█", filled), DIM, strings.Repeat("░", width-filled), RESET, ratio*100.0)
}

func drawTUIHeader(out *strings.Builder, title string, viewIdx int, cfg *Config, client, sslSum, keyFP string, cols int) {
	b := cfg.Bucket
	if b == "" {
		b = "(unconfigured)"
	}
	out.WriteString(fmt.Sprintf("%s[ ◄ %s (%d/%d) ► ]%s  %ss3://%s/%s/%s  │  %s%s%s  │  %sKey:%s %s\n",
		BOLD, title, viewIdx+1, totalViews, RESET, CYAN, b, client, RESET, LIME, sslSum, RESET, DIM, RESET, keyFP))
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
		if s.promptMode == promptConfirmDelete {
			out.WriteString(fmt.Sprintf("%s%s %s %s\n", RED_BG, BOLD, s.promptTitle, RESET))
		} else {
			disp := s.promptBuf
			if s.promptMode == promptEditSetting && s.settingSelected == 6 {
				disp = strings.Repeat("*", len(s.promptBuf))
			}
			out.WriteString(fmt.Sprintf("%s%s:%s %s█\n", YELLOW+BOLD, s.promptTitle, RESET, disp))
			out.WriteString(DIM + "[ENTER: Apply │ ESC: Cancel │ CTRL+U: Clear]" + RESET)
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
		hints := [4]string{
			"[ ◄/►: Views │ ▲/▼: Select │ ENTER: Browse Files │ 1-9/0: Mount Snap │ /: Filter │ T: Thaw │ c: Client │ r: Refresh │ D: Del │ q: Quit ]",
			"[ [ / ]: Older/Newer Dump Date │ SPACE: Mark │ u: Unmark │ m: Overwrite/NewPath │ o: Dest │ x: Restore (X: Chain) │ /: Filter │ p: Preload ]",
			"[ ◄/►: Views │ ▲/▼: Select Job │ r: Poll S3/Glacier Now │ R: Retry Failed │ d: Remove Record │ q: Quit ]",
			"[ ◄/►: Views │ ▲/▼: Field │ ENTER: Edit/Toggle │ t: Test S3 & SSL Crypto │ s: Save Config (0600) │ q: Quit ]",
		}
		out.WriteString(DIM + padOrTrim(hints[s.view], cols-1) + RESET)
	}

	if s.view != s.lastView {
		_, _ = tty.WriteString(ansiClearHome)
		s.lastView = s.view
	}
	_, _ = tty.WriteString(ansiHome + strings.ReplaceAll(out.String(), "\n", ansiClearEOL+"\n") + ansiClearBelow)
}

func (s *tuiState) renderTimeline(out *strings.Builder, cfg *Config, cols int) {
	drawTUIHeader(out, "DUMP TIMELINE", viewTimeline, cfg, s.activeClient, s.sslSummary, s.keyFP, cols)

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
		out.WriteString(fmt.Sprintf("│ %s[FILTER: /%s]%s ", YELLOW, s.filterText, RESET))
	}
	out.WriteString(fmt.Sprintf("│ %sTier: %s%s\n%s\n", DIM, s.restoreTier, RESET, strings.Repeat("─", cols-1)))
	out.WriteString(fmt.Sprintf("%s  %-19s │ %-14s │ %-4s │ %-10s │ %-10s │ %-7s │ %s%s\n%s\n",
		BOLD, "DUMP TIMESTAMP", "MOUNT", "LVL", "ARCHIVE", "CATALOG", "GLACIER", "AUTH", RESET, strings.Repeat("─", cols-1)))

	list := s.filteredTimelineSnapshots()
	maxRows := s.viewportRows()
	if len(list) == 0 {
		out.WriteString(DIM + "  No snapshots matching current client/filter." + RESET + strings.Repeat("\n", maxRows+1))
		return
	}

	s.timelineSelected, s.timelineScroll = clampViewport(s.timelineSelected, s.timelineScroll, len(list), maxRows)
	end := min(len(list), s.timelineScroll+maxRows)

	for i := s.timelineScroll; i < end; i++ {
		rec := list[i]
		prefix, tsStyle := "  ", ""
		if i == s.timelineSelected {
			prefix, tsStyle = YELLOW+">"+RESET+" ", WHITE_BG+BOLD
		}
		lvlStr := CYAN + fmt.Sprintf("+L%-2d", rec.Meta.Level) + RESET
		if rec.Meta.Level == 0 {
			lvlStr = LIME + BOLD + "L0  " + RESET
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
		out.WriteString(fmt.Sprintf("%s%s%-19s%s │ %-14s │ %s │ %-10s │ %s │ %s │ %s\n",
			prefix, tsStyle, rec.Meta.Timestamp.Format("2006-01-02 15:04:05"), RESET,
			padOrTrim(rec.Meta.MountGuess, 14), lvlStr, FormatBytes(rec.DataSize),
			catStr, formatGlacierBadge(rec.GlacierState), authStr))
	}
	out.WriteString(strings.Repeat("\n", maxRows-(end-s.timelineScroll)))

	chain := SnapshotChainForTarget(s.snapshots, list[s.timelineSelected])
	var parts []string
	for _, c := range chain {
		parts = append(parts, fmt.Sprintf("L%d(%s)", c.Meta.Level, c.Meta.Timestamp.Format("01-02 15:04")))
	}
	out.WriteString(fmt.Sprintf("%s[ DUMP CHAIN ]%s %s\n", BOLD, RESET, strings.Join(parts, " -> ")))
}

func (s *tuiState) renderBrowser(out *strings.Builder, cfg *Config, cols int) {
	drawTUIHeader(out, "SNAPSHOT BROWSER", viewBrowser, cfg, s.activeClient, s.sslSummary, s.keyFP, cols)
	if s.activeSnap == nil {
		out.WriteString(DIM + "  Select a snapshot on View 1 (DUMP TIMELINE) and press ENTER to browse files." + RESET + strings.Repeat("\n", max(1, s.rows-7)))
		return
	}

	snap := s.activeSnap
	modeBadge := YELLOW + BOLD + "OVERWRITE" + RESET
	if s.restoreMode == RestoreModeNewPath {
		d := s.restoreDest
		if d == "" {
			d = "(prompt on restore)"
		}
		modeBadge = LIME + BOLD + "NEW_PATH -> " + padOrTrim(d, 26) + RESET
	}
	markedCnt, markedBytes := 0, int64(0)
	if s.catalogTree != nil {
		markedCnt, markedBytes = s.catalogTree.MarkedSummary()
	}

	out.WriteString(fmt.Sprintf("%sMount:%s %-10s │ %sDate [%d/%d]:%s %s%s (L%d)%s │ %s │ %sTier:%s %s\n",
		BOLD, RESET, snap.Meta.MountGuess, BOLD, s.browserSnapIdx+1, len(s.browserMountSnaps), RESET,
		CYAN+BOLD, snap.Meta.Timestamp.Format("2006-01-02 15:04:05"), snap.Meta.Level, RESET,
		formatGlacierBadge(snap.GlacierState), DIM, RESET, s.restoreTier))
	out.WriteString(fmt.Sprintf("%sMode :%s %s │ %sMarked:%s %s%d items (%s)%s",
		BOLD, RESET, modeBadge, BOLD, RESET, LIME+BOLD, markedCnt, FormatBytes(markedBytes), RESET))
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
		out.WriteString("  " + DIM + msg + RESET + strings.Repeat("\n", treeRows))
	} else {
		s.browserSelected, s.browserScroll = clampViewport(s.browserSelected, s.browserScroll, len(s.visibleNodes), treeRows)
		end := min(len(s.visibleNodes), s.browserScroll+treeRows)
		nameWidth := max(24, cols-44)

		for i := s.browserScroll; i < end; i++ {
			node := s.visibleNodes[i]
			cursor, rowHi := " ", ""
			if i == s.browserSelected {
				cursor, rowHi = YELLOW+">"+RESET, WHITE_BG+BOLD
			}
			markBox := DIM + "[ ]" + RESET
			if node.Marked {
				markBox = LIME + BOLD + "[✓]" + RESET
			} else if node.PartialMark {
				markBox = YELLOW + "[~]" + RESET
			}

			indent := ""
			if s.filterText == "" && node.Level > 0 {
				indent = strings.Repeat("  ", min(10, node.Level))
			}
			sym, nameCol, disp := " ", RESET, node.Name
			if s.filterText != "" && node.RelPath != "" {
				disp = node.RelPath
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
				if node != s.catalogTree.Root && !strings.HasSuffix(disp, "/") {
					disp += "/"
				}
			} else if node.Entry.Type == "l" {
				nameCol = CYAN
				if node.Entry.LinkTarget != "" {
					disp += " -> " + node.Entry.LinkTarget
				}
			}
			if rowHi != "" {
				nameCol = rowHi
			}

			cell := padOrTrim(indent+sym+" "+disp, nameWidth)
			padSpaces := max(1, nameWidth-utf8.RuneCountInString(cell))
			sz := node.Entry.Size
			if node.IsDir {
				sz = node.SubtreeSize
			}
			mtimeStr := "—"
			if node.Entry.Mtime > 0 {
				mtimeStr = time.Unix(node.Entry.Mtime, 0).Format("2006-01-02 15:04")
			}
			out.WriteString(fmt.Sprintf("%s %s %s%s%s%s %10s  %10s  %s\n",
				cursor, markBox, nameCol, cell, RESET, strings.Repeat(" ", padSpaces),
				node.Entry.ModeString(), FormatBytes(sz), DIM+mtimeStr+RESET))
		}
		out.WriteString(strings.Repeat("\n", treeRows-(end-s.browserScroll)))
	}

	out.WriteString(strings.Repeat("─", cols-1) + "\n")
	if s.browserSelected < 0 || s.browserSelected >= len(s.visibleNodes) {
		out.WriteString("\n\n")
		return
	}

	sel := s.visibleNodes[s.browserSelected]
	out.WriteString(fmt.Sprintf("%s[ PATH ]%s %s  %s(ino:%d uid:%d gid:%d)%s\n",
		BOLD, RESET, padOrTrim(filepath.Join(snap.Meta.MountGuess, sel.RelPath), cols-36),
		DIM, sel.Entry.Inode, sel.Entry.UID, sel.Entry.GID, RESET))

	history := s.eng.FileHistory(s.browserMountSnaps, sel.RelPath)
	startIdx := max(0, min(s.browserSnapIdx-2, len(history)-5))
	endIdx := min(len(history), startIdx+5)
	var chips []string
	for i := startIdx; i < endIdx; i++ {
		h := history[i]
		stTag := "?"
		if h.CatalogLoaded {
			if !h.Present {
				stTag = "--"
			} else if h.Entry.Type == "d" {
				stTag = "dir"
			} else {
				stTag = FormatBytes(h.Entry.Size)
			}
		}
		chip := fmt.Sprintf("[%s L%d: %s]", h.Snapshot.Meta.Timestamp.Format("01-02 15:04"), h.Snapshot.Meta.Level, stTag)
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
	drawTUIHeader(out, "JOBS & GLACIER MONITOR", viewJobs, cfg, s.activeClient, s.sslSummary, s.keyFP, cols)
	jobs := s.eng.JobsSnapshot()
	maxRows := s.viewportRows()

	out.WriteString(fmt.Sprintf("%s  %-18s │ %-11s │ %-9s │ %-24s │ %s%s\n%s\n",
		BOLD, "JOB ID", "STATUS", "MODE", "PROGRESS", "ARCHIVE / SPEED", RESET, strings.Repeat("─", cols-1)))
	if len(jobs) == 0 {
		out.WriteString(DIM + "  No active or historical restore jobs." + RESET + strings.Repeat("\n", maxRows+2))
		return
	}

	s.jobsSelected, s.jobsScroll = clampViewport(s.jobsSelected, s.jobsScroll, len(jobs), maxRows)
	end := min(len(jobs), s.jobsScroll+maxRows)

	for i := s.jobsScroll; i < end; i++ {
		j := jobs[i]
		prefix, idCol := "  ", ""
		if i == s.jobsSelected {
			prefix, idCol = YELLOW+">"+RESET+" ", WHITE_BG+BOLD
		}
		stCol := CYAN + BOLD
		switch j.Status {
		case JobCompleted:
			stCol = LIME + BOLD
		case JobFailed:
			stCol = RED_BG + BOLD
		case JobThawing:
			stCol = YELLOW + BOLD
		}
		speed := ""
		if j.SpeedBPS > 0 {
			speed = fmt.Sprintf(" (%.2f MiB/s)", j.SpeedBPS/1048576.0)
		}
		out.WriteString(fmt.Sprintf("%s%s%-18s%s │ %s%-11s%s │ %-9s │ %s │ %s%s\n",
			prefix, idCol, padOrTrim(j.ID, 18), RESET, stCol, j.Status, RESET,
			j.Mode, formatProgressBar(j.DoneBytes, j.TotalBytes, 14),
			padOrTrim(j.Archive.BaseName, 32), DIM+speed+RESET))
	}
	out.WriteString(strings.Repeat("\n", maxRows-(end-s.jobsScroll)))

	sel := jobs[s.jobsSelected]
	dest := sel.MountPoint
	if sel.Mode == RestoreModeNewPath {
		dest = sel.TargetDest + " (base mount " + sel.MountPoint + ")"
	}
	detail := sel.Message
	if sel.Error != "" {
		detail = sel.Error
	}
	out.WriteString(strings.Repeat("─", cols-1) + "\n")
	out.WriteString(fmt.Sprintf("%s[ TARGET ]%s %s │ %sTier:%s %s │ %sUpdated:%s %s\n",
		BOLD, RESET, padOrTrim(dest, cols-44), DIM, RESET, sel.Tier, DIM, RESET, sel.UpdatedAt.Format("15:04:05")))
	out.WriteString(fmt.Sprintf("%s[ DETAIL ]%s %s\n", BOLD, RESET, padOrTrim(detail, cols-13)))
}

func (s *tuiState) renderSettings(out *strings.Builder, cfg *Config, cols int) {
	drawTUIHeader(out, "S3 & SYSTEM CONFIG", viewSettings, cfg, s.activeClient, s.sslSummary, s.keyFP, cols)
	dirty := GREEN + "SAVED" + RESET
	if s.cfgDirty {
		dirty = YELLOW + BOLD + "UNSAVED CHANGES (press 's' to save)" + RESET
	}
	out.WriteString(fmt.Sprintf("%sPlatform:%s %s  │  %sConfig File:%s %s (0600)  │  %s\n\n",
		BOLD, RESET, PlatformName, BOLD, RESET, s.eng.ConfigPath(), dirty))

	secMask := "(empty — using AWS profile / IAM / env)"
	if s.cfgDraft.SecretAccessKey != "" {
		secMask = strings.Repeat("*", 16) + " (configured)"
	}
	sslDisp := s.cfgDraft.OpenSSLBin
	if sslDisp == "" {
		sslDisp = "(auto-detect: " + s.sslSummary + ")"
	}
	ldDisp := s.cfgDraft.LDLibraryPath
	if ldDisp == "" && DefaultLDLibraryPath != "" {
		ldDisp = "(auto if present: " + DefaultLDLibraryPath + ")"
	}

	rows := [totalSettingRows][2]string{
		{"S3 Bucket", s.cfgDraft.Bucket},
		{"AWS Region", s.cfgDraft.Region},
		{"Custom S3 Endpoint", s.cfgDraft.Endpoint},
		{"Force S3 Path-Style", fmt.Sprintf("%v", s.cfgDraft.ForcePathStyle)},
		{"AWS Shared Profile", s.cfgDraft.AWSProfile},
		{"AWS Access Key ID", s.cfgDraft.AccessKeyID},
		{"AWS Secret Access Key", secMask},
		{"Client Identifier", s.cfgDraft.EffectiveClient()},
		{"Master Keyfile (0400)", s.cfgDraft.EffectiveKeyPath()},
		{"SSL Binary Override", sslDisp},
		{"SSL LD_LIBRARY_PATH", ldDisp},
		{"Default Glacier Tier", s.cfgDraft.EffectiveTier("")},
		{"[ Test S3 Connectivity & SSL ChaCha20+HMAC Self-Test ]", ""},
		{"[ Save Configuration to Disk (0600) ]", ""},
		{"[ Generate / Verify Client 0400 Keyfile ]", ""},
	}
	for i, r := range rows {
		if i == 12 {
			out.WriteString("\n")
		}
		prefix, lblStyle := "  ", BOLD
		if i == s.settingSelected {
			prefix, lblStyle = YELLOW+">"+RESET+" ", WHITE_BG+BOLD
		}
		if i >= 12 {
			col := CYAN + BOLD
			if i == s.settingSelected {
				col = WHITE_BG + BOLD
			}
			out.WriteString(fmt.Sprintf("%s%s%s%s\n", prefix, col, r[0], RESET))
			continue
		}
		val := r[1]
		if val == "" {
			val = DIM + "(unset)" + RESET
		}
		out.WriteString(fmt.Sprintf("%s%s%-26s%s : %s\n", prefix, lblStyle, r[0], RESET, val))
	}
}
