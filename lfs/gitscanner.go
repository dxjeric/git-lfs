package lfs

import (
	"errors"
	"sync"
	"time"

	"github.com/git-lfs/git-lfs/v3/config"
	"github.com/git-lfs/git-lfs/v3/filepathfilter"
	"github.com/git-lfs/git-lfs/v3/tr"
	"github.com/rubyist/tracerx"
)

var missingCallbackErr = errors.New(tr.Tr.Get("no callback given"))

// IsCallbackMissing returns a boolean indicating whether the error is reporting
// that a GitScanner is missing a required GitScannerCallback.
func IsCallbackMissing(err error) bool {
	return err == missingCallbackErr
}

// GitScanner scans objects in a Git repository for LFS pointers.
type GitScanner struct {
	Filter             *filepathfilter.Filter
	FoundPointer       GitScannerFoundPointer
	FoundLockable      GitScannerFoundLockable
	PotentialLockables GitScannerSet
	remote             string
	skippedRefs        []string

	closed  bool
	started time.Time
	cfg     *config.Configuration
}

type GitScannerFoundPointer func(*WrappedPointer, error)
type GitScannerFoundLockable func(filename string)

type GitScannerSet interface {
	Contains(string) bool
}

// NewGitScanner initializes a *GitScanner for a Git repository in the current
// working directory.
func NewGitScanner(cfg *config.Configuration, cb GitScannerFoundPointer) *GitScanner {
	return &GitScanner{started: time.Now(), FoundPointer: cb, cfg: cfg}
}

// Close stops exits once all processing has stopped, and all resources are
// tracked and cleaned up.
func (s *GitScanner) Close() {
	if s.closed {
		return
	}

	s.closed = true
	tracerx.PerformanceSince("scan", s.started)
}

// RemoteForPush sets up this *GitScanner to scan for objects to push to the
// given remote. Needed for ScanMultiRangeToRemote().
func (s *GitScanner) RemoteForPush(r string) {
	if len(s.remote) > 0 && s.remote != r {
		return errors.New(tr.Tr.Get("trying to set remote to %q, already set to %q", r, s.remote))
	}

	s.remote = r
	s.skippedRefs = calcSkippedRefs(r)
}

// ScanMultiRangeToRemote scans through all unique objects reachable from the
// "include" ref but not reachable from any "exclude" refs and which the
// given remote does not have. See RemoteForPush().
func (s *GitScanner) ScanMultiRangeToRemote(include string, exclude []string, cb GitScannerFoundPointer) error {
	callback, err := firstGitScannerCallback(cb, s.FoundPointer)
	if err != nil {
		return err
	}

	if len(s.remote) == 0 {
		return errors.New(tr.Tr.Get("unable to scan starting at %q: no remote set", include))
	}

	return scanRefsToChanSingleIncludeMultiExclude(s, callback, include, exclude, s.cfg.GitEnv(), s.cfg.OSEnv(), s.opts(ScanRangeToRemoteMode))
}

// ScanRefs scans through all unique objects reachable from the "include" refs
// but not reachable from any "exclude" refs, including objects that have
// been modified or deleted.
func (s *GitScanner) ScanRefs(include, exclude []string, cb GitScannerFoundPointer) error {
	callback, err := firstGitScannerCallback(cb, s.FoundPointer)
	if err != nil {
		return err
	}

	opts := s.opts(ScanRefsMode)
	opts.SkipDeletedBlobs = false
	return scanRefsToChan(s, callback, include, exclude, s.cfg.GitEnv(), s.cfg.OSEnv(), opts)
}

// ScanRefRange scans through all unique objects reachable from the "include"
// ref but not reachable from the "exclude" ref, including objects that have
// been modified or deleted.
func (s *GitScanner) ScanRefRange(include, exclude string, cb GitScannerFoundPointer) error {
	callback, err := firstGitScannerCallback(cb, s.FoundPointer)
	if err != nil {
		return err
	}

	opts := s.opts(ScanRefsMode)
	opts.SkipDeletedBlobs = false
	return scanRefsToChanSingleIncludeExclude(s, callback, include, exclude, s.cfg.GitEnv(), s.cfg.OSEnv(), opts)
}

// ScanRefRangeByTree scans through all objects reachable from the "include"
// ref but not reachable from the "exclude" ref, including objects that have
// been modified or deleted.  Objects which appear in multiple trees will
// be visited once per tree.
func (s *GitScanner) ScanRefRangeByTree(include, exclude string, cb GitScannerFoundPointer) error {
	callback, err := firstGitScannerCallback(cb, s.FoundPointer)
	if err != nil {
		return err
	}

	opts := s.opts(ScanRefsMode)
	opts.SkipDeletedBlobs = false
	opts.CommitsOnly = true
	return scanRefsByTree(s, callback, []string{include}, []string{exclude}, s.cfg.GitEnv(), s.cfg.OSEnv(), opts)
}

// ScanRefWithDeleted scans through all unique objects in the given ref,
// including objects that have been modified or deleted.
func (s *GitScanner) ScanRefWithDeleted(ref string, cb GitScannerFoundPointer) error {
	return s.ScanRefRange(ref, "", cb)
}

// ScanRef scans through all unique objects in the current ref, excluding
// objects that have been modified or deleted before the ref.
func (s *GitScanner) ScanRef(ref string, cb GitScannerFoundPointer) error {
	callback, err := firstGitScannerCallback(cb, s.FoundPointer)
	if err != nil {
		return err
	}

	opts := s.opts(ScanRefsMode)
	opts.SkipDeletedBlobs = true
	return scanRefsToChanSingleIncludeExclude(s, callback, ref, "", s.cfg.GitEnv(), s.cfg.OSEnv(), opts)
}

// ScanRefByTree scans through all objects in the current ref, excluding
// objects that have been modified or deleted before the ref.  Objects which
// appear in multiple trees will be visited once per tree.
func (s *GitScanner) ScanRefByTree(ref string, cb GitScannerFoundPointer) error {
	callback, err := firstGitScannerCallback(cb, s.FoundPointer)
	if err != nil {
		return err
	}

	opts := s.opts(ScanRefsMode)
	opts.SkipDeletedBlobs = true
	opts.CommitsOnly = true
	return scanRefsByTree(s, callback, []string{ref}, []string{}, s.cfg.GitEnv(), s.cfg.OSEnv(), opts)
}

// ScanAll scans through all unique objects in the repository, including
// objects that have been modified or deleted.
func (s *GitScanner) ScanAll(cb GitScannerFoundPointer) error {
	callback, err := firstGitScannerCallback(cb, s.FoundPointer)
	if err != nil {
		return err
	}

	opts := s.opts(ScanAllMode)
	opts.SkipDeletedBlobs = false
	return scanRefsToChanSingleIncludeExclude(s, callback, "", "", s.cfg.GitEnv(), s.cfg.OSEnv(), opts)
}

// ScanTree takes a ref and returns WrappedPointer objects in the tree at that
// ref. Differs from ScanRefs in that multiple files in the tree with the same
// content are all reported.
func (s *GitScanner) ScanTree(ref string, cb GitScannerFoundPointer) error {
	callback, err := firstGitScannerCallback(cb, s.FoundPointer)
	if err != nil {
		return err
	}
	return runScanTree(callback, ref, s.Filter, s.cfg.GitEnv(), s.cfg.OSEnv())
}

// ScanUnpushed scans history for all LFS pointers which have been added but not
// pushed to the named remote. remote can be left blank to mean 'any remote'.
func (s *GitScanner) ScanUnpushed(remote string, cb GitScannerFoundPointer) error {
	callback, err := firstGitScannerCallback(cb, s.FoundPointer)
	if err != nil {
		return err
	}
	return scanUnpushed(callback, remote)
}

// ScanStashed scans for all LFS pointers referenced solely by a stash
func (s *GitScanner) ScanStashed(cb GitScannerFoundPointer) error {
	callback, err := firstGitScannerCallback(cb, s.FoundPointer)
	if err != nil {
		return err
	}

	return scanStashed(callback)
}

// ScanPreviousVersions scans changes reachable from ref (commit) back to since.
// Returns channel of pointers for *previous* versions that overlap that time.
// Does not include pointers which were still in use at ref (use ScanRefsToChan
// for that)
func (s *GitScanner) ScanPreviousVersions(ref string, since time.Time, cb GitScannerFoundPointer) error {
	callback, err := firstGitScannerCallback(cb, s.FoundPointer)
	if err != nil {
		return err
	}
	return logPreviousSHAs(callback, ref, s.Filter, since)
}

// ScanIndex scans the git index for modified LFS objects.
func (s *GitScanner) ScanIndex(ref string, cb GitScannerFoundPointer) error {
	callback, err := firstGitScannerCallback(cb, s.FoundPointer)
	if err != nil {
		return err
	}
	return scanIndex(callback, ref, s.Filter, s.cfg.GitEnv(), s.cfg.OSEnv())
}

func (s *GitScanner) opts(mode ScanningMode) *ScanRefsOptions {
	opts := newScanRefsOptions()
	opts.ScanMode = mode
	opts.RemoteName = s.remote
	opts.skippedRefs = s.skippedRefs
	return opts
}

func firstGitScannerCallback(callbacks ...GitScannerFoundPointer) (GitScannerFoundPointer, error) {
	for _, cb := range callbacks {
		if cb == nil {
			continue
		}
		return cb, nil
	}

	return nil, missingCallbackErr
}

type ScanningMode int

const (
	ScanRefsMode          = ScanningMode(iota) // 0 - or default scan mode
	ScanAllMode           = ScanningMode(iota)
	ScanRangeToRemoteMode = ScanningMode(iota)
)

type ScanRefsOptions struct {
	ScanMode         ScanningMode
	RemoteName       string
	SkipDeletedBlobs bool
	CommitsOnly      bool
	skippedRefs      []string
	nameMap          map[string]string
	mutex            *sync.Mutex
}

func (o *ScanRefsOptions) GetName(sha string) (string, bool) {
	o.mutex.Lock()
	name, ok := o.nameMap[sha]
	o.mutex.Unlock()
	return name, ok
}

func (o *ScanRefsOptions) SetName(sha, name string) {
	o.mutex.Lock()
	o.nameMap[sha] = name
	o.mutex.Unlock()
}

func newScanRefsOptions() *ScanRefsOptions {
	return &ScanRefsOptions{
		nameMap: make(map[string]string, 0),
		mutex:   &sync.Mutex{},
	}
}
