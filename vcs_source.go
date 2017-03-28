package gps

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Masterminds/semver"
	"github.com/Masterminds/vcs"
	"github.com/sdboyer/gps/internal/fs"
)

// gitSource is a generic git repository implementation that should work with
// all standard git remotes.
type gitSource struct {
	baseVCSSource
}

func (s *gitSource) exportVersionTo(v Version, to string) error {
	// Get away without syncing local, if we can
	r := s.crepo.r
	// ...but local repo does have to at least exist
	if err := s.ensureCacheExistence(); err != nil {
		return err
	}

	if err := os.MkdirAll(to, 0777); err != nil {
		return err
	}

	do := func() error {
		s.crepo.mut.Lock()
		defer s.crepo.mut.Unlock()

		// Back up original index
		idx, bak := filepath.Join(r.LocalPath(), ".git", "index"), filepath.Join(r.LocalPath(), ".git", "origindex")
		err := fs.RenameWithFallback(idx, bak)
		if err != nil {
			return err
		}

		// could have an err here...but it's hard to imagine how?
		defer fs.RenameWithFallback(bak, idx)

		vstr := v.String()
		if rv, ok := v.(PairedVersion); ok {
			vstr = rv.Underlying().String()
		}

		out, err := runFromRepoDir(r, "git", "read-tree", vstr)
		if err != nil {
			return fmt.Errorf("%s: %s", out, err)
		}

		// Ensure we have exactly one trailing slash
		to = strings.TrimSuffix(to, string(os.PathSeparator)) + string(os.PathSeparator)
		// Checkout from our temporary index to the desired target location on
		// disk; now it's git's job to make it fast.
		//
		// Sadly, this approach *does* also write out vendor dirs. There doesn't
		// appear to be a way to make checkout-index respect sparse checkout
		// rules (-a supercedes it). The alternative is using plain checkout,
		// though we have a bunch of housekeeping to do to set up, then tear
		// down, the sparse checkout controls, as well as restore the original
		// index and HEAD.
		out, err = runFromRepoDir(r, "git", "checkout-index", "-a", "--prefix="+to)
		if err != nil {
			return fmt.Errorf("%s: %s", out, err)
		}
		return nil
	}

	err := do()
	if err != nil && !s.crepo.synced {
		// If there was an err, and the repo cache is stale, it might've been
		// beacuse we were missing the rev/ref. Try syncing, then run the export
		// op again.
		err = s.syncLocal()
		if err != nil {
			return err
		}
		err = do()
	}

	return err
}

func (s *gitSource) listVersions() ([]Version, error) {
	s.baseVCSSource.lvmut.Lock()
	defer s.baseVCSSource.lvmut.Unlock()

	if s.cvsync {
		return s.dc.getAllVersions(), nil
	}

	vlist, err := s.doListVersions()
	if err != nil {
		return nil, err
	}
	// Process version data into the cache and mark cache as in sync
	s.dc.storeVersionMap(vlist, true)
	s.cvsync = true
	return s.dc.getAllVersions(), nil
}

func (s *gitSource) doListVersions() (vlist []PairedVersion, err error) {
	r := s.crepo.r
	var out []byte
	c := exec.Command("git", "ls-remote", r.Remote())
	// Ensure no prompting for PWs
	c.Env = mergeEnvLists([]string{"GIT_ASKPASS=", "GIT_TERMINAL_PROMPT=0"}, os.Environ())
	out, err = c.CombinedOutput()

	all := bytes.Split(bytes.TrimSpace(out), []byte("\n"))
	if err != nil || len(all) == 0 {
		// TODO(sdboyer) remove this path? it really just complicates things, for
		// probably not much benefit

		// ls-remote failed, probably due to bad communication or a faulty
		// upstream implementation. So fetch updates, then build the list
		// locally
		s.crepo.mut.Lock()
		err = r.Update()
		s.crepo.mut.Unlock()
		if err != nil {
			// Definitely have a problem, now - bail out
			return
		}

		// Upstream and cache must exist for this to have worked, so add that to
		// searched and found
		s.ex.s |= existsUpstream | existsInCache
		s.ex.f |= existsUpstream | existsInCache
		// Also, local is definitely now synced
		s.crepo.synced = true

		s.crepo.mut.RLock()
		out, err = runFromRepoDir(r, "git", "show-ref", "--dereference")
		s.crepo.mut.RUnlock()
		if err != nil {
			// TODO(sdboyer) More-er proper-er error
			return
		}

		all = bytes.Split(bytes.TrimSpace(out), []byte("\n"))
		if len(all) == 0 {
			return nil, fmt.Errorf("no versions available for %s (this is weird)", r.Remote())
		}
	}

	// Local cache may not actually exist here, but upstream definitely does
	s.ex.s |= existsUpstream
	s.ex.f |= existsUpstream

	// Pull out the HEAD rev (it's always first) so we know what branches to
	// mark as default. This is, perhaps, not the best way to glean this, but it
	// was good enough for git itself until 1.8.5. Also, the alternative is
	// sniffing data out of the pack protocol, which is a separate request, and
	// also waaaay more than we want to do right now.
	//
	// The cost is that we could potentially have multiple branches marked as
	// the default. If that does occur, a later check (again, emulating git
	// <1.8.5 behavior) further narrows the failure mode by choosing master as
	// the sole default branch if a) master exists and b) master is one of the
	// branches marked as a default.
	//
	// This all reduces the failure mode to a very narrow range of
	// circumstances. Nevertheless, if we do end up emitting multiple
	// default branches, it is possible that a user could end up following a
	// non-default branch, IF:
	//
	// * Multiple branches match the HEAD rev
	// * None of them are master
	// * The solver makes it into the branch list in the version queue
	// * The user/tool has provided no constraint (so, anyConstraint)
	// * A branch that is not actually the default, but happens to share the
	//   rev, is lexicographically less than the true default branch
	//
	// If all of those conditions are met, then the user would end up with an
	// erroneous non-default branch in their lock file.
	headrev := Revision(all[0][:40])
	var onedef, multidef, defmaster bool

	smap := make(map[string]bool)
	uniq := 0
	vlist = make([]PairedVersion, len(all)-1) // less 1, because always ignore HEAD
	for _, pair := range all {
		var v PairedVersion
		if string(pair[46:51]) == "heads" {
			rev := Revision(pair[:40])

			isdef := rev == headrev
			n := string(pair[52:])
			if isdef {
				if onedef {
					multidef = true
				}
				onedef = true
				if n == "master" {
					defmaster = true
				}
			}
			v = branchVersion{
				name:      n,
				isDefault: isdef,
			}.Is(rev).(PairedVersion)

			vlist[uniq] = v
			uniq++
		} else if string(pair[46:50]) == "tags" {
			vstr := string(pair[51:])
			if strings.HasSuffix(vstr, "^{}") {
				// If the suffix is there, then we *know* this is the rev of
				// the underlying commit object that we actually want
				vstr = strings.TrimSuffix(vstr, "^{}")
			} else if smap[vstr] {
				// Already saw the deref'd version of this tag, if one
				// exists, so skip this.
				continue
				// Can only hit this branch if we somehow got the deref'd
				// version first. Which should be impossible, but this
				// covers us in case of weirdness, anyway.
			}
			v = NewVersion(vstr).Is(Revision(pair[:40])).(PairedVersion)
			smap[vstr] = true
			vlist[uniq] = v
			uniq++
		}
	}

	// Trim off excess from the slice
	vlist = vlist[:uniq]

	// There were multiple default branches, but one was master. So, go through
	// and strip the default flag from all the non-master branches.
	if multidef && defmaster {
		for k, v := range vlist {
			pv := v.(PairedVersion)
			if bv, ok := pv.Unpair().(branchVersion); ok {
				if bv.name != "master" && bv.isDefault == true {
					bv.isDefault = false
					vlist[k] = bv.Is(pv.Underlying())
				}
			}
		}
	}

	return
}

// gopkginSource is a specialized git source that performs additional filtering
// according to the input URL.
type gopkginSource struct {
	gitSource
	major uint64
}

func (s *gopkginSource) listVersions() ([]Version, error) {
	s.baseVCSSource.lvmut.Lock()
	defer s.baseVCSSource.lvmut.Unlock()

	if s.cvsync {
		return s.dc.getAllVersions(), nil
	}

	ovlist, err := s.doListVersions()
	if err != nil {
		return nil, err
	}

	// Apply gopkg.in's filtering rules
	vlist := make([]PairedVersion, len(ovlist))
	k := 0
	var dbranch int // index of branch to be marked default
	var bsv *semver.Version
	for _, v := range ovlist {
		// all git versions will always be paired
		pv := v.(versionPair)
		switch tv := pv.v.(type) {
		case semVersion:
			if tv.sv.Major() == s.major {
				vlist[k] = v
				k++
			}
		case branchVersion:
			// The semver lib isn't exactly the same as gopkg.in's logic, but
			// it's close enough that it's probably fine to use. We can be more
			// exact if real problems crop up. The most obvious vector for
			// problems is that we totally ignore the "unstable" designation
			// right now.
			sv, err := semver.NewVersion(tv.name)
			if err != nil || sv.Major() != s.major {
				// not a semver-shaped branch name at all, or not the same major
				// version as specified in the import path constraint
				continue
			}

			// Turn off the default branch marker unconditionally; we can't know
			// which one to mark as default until we've seen them all
			tv.isDefault = false
			// Figure out if this is the current leader for default branch
			if bsv == nil || bsv.LessThan(sv) {
				bsv = sv
				dbranch = k
			}
			pv.v = tv
			vlist[k] = pv
			k++
		}
		// The switch skips plainVersions because they cannot possibly meet
		// gopkg.in's requirements
	}

	vlist = vlist[:k]
	if bsv != nil {
		dbv := vlist[dbranch].(versionPair)
		vlist[dbranch] = branchVersion{
			name:      dbv.v.(branchVersion).name,
			isDefault: true,
		}.Is(dbv.r)
	}

	// Process filtered version data into the cache and mark cache as in sync
	s.dc.storeVersionMap(vlist, true)
	s.cvsync = true
	return s.dc.getAllVersions(), nil
}

// bzrSource is a generic bzr repository implementation that should work with
// all standard bazaar remotes.
type bzrSource struct {
	baseVCSSource
}

func (s *bzrSource) update() error {
	r := s.crepo.r

	out, err := runFromRepoDir(r, "bzr", "pull")
	if err != nil {
		return vcs.NewRemoteError("Unable to update repository", err, string(out))
	}

	out, err = runFromRepoDir(r, "bzr", "update")
	if err != nil {
		return vcs.NewRemoteError("Unable to update repository", err, string(out))
	}

	return nil
}

func (s *bzrSource) listVersions() ([]Version, error) {
	s.baseVCSSource.lvmut.Lock()
	defer s.baseVCSSource.lvmut.Unlock()

	if s.cvsync {
		return s.dc.getAllVersions(), nil
	}

	// Must first ensure cache checkout's existence
	err := s.ensureCacheExistence()
	if err != nil {
		return nil, err
	}
	r := s.crepo.r

	// Local repo won't have all the latest refs if ensureCacheExistence()
	// didn't create it
	if !s.crepo.synced {
		s.crepo.mut.Lock()
		err = s.update()
		s.crepo.mut.Unlock()
		if err != nil {
			return nil, err
		}

		s.crepo.synced = true
	}

	var out []byte
	// Now, list all the tags
	out, err = runFromRepoDir(r, "bzr", "tags", "--show-ids", "-v")
	if err != nil {
		return nil, fmt.Errorf("%s: %s", err, string(out))
	}

	all := bytes.Split(bytes.TrimSpace(out), []byte("\n"))

	var branchrev []byte
	branchrev, err = runFromRepoDir(r, "bzr", "version-info", "--custom", "--template={revision_id}", "--revision=branch:.")
	br := string(branchrev)
	if err != nil {
		return nil, fmt.Errorf("%s: %s", err, br)
	}

	vlist := make([]PairedVersion, 0, len(all)+1)

	// Now, all the tags.
	for _, line := range all {
		idx := bytes.IndexByte(line, 32) // space
		v := NewVersion(string(line[:idx]))
		r := Revision(bytes.TrimSpace(line[idx:]))
		vlist = append(vlist, v.Is(r))
	}

	// Last, add the default branch, hardcoding the visual representation of it
	// that bzr uses when operating in the workflow mode we're using.
	v := newDefaultBranch("(default)")
	vlist = append(vlist, v.Is(Revision(string(branchrev))))

	// Process version data into the cache and mark cache as in sync
	s.dc.storeVersionMap(vlist, true)
	s.cvsync = true
	return s.dc.getAllVersions(), nil
}

// hgSource is a generic hg repository implementation that should work with
// all standard mercurial servers.
type hgSource struct {
	baseVCSSource
}

func (s *hgSource) update() error {
	r := s.crepo.r

	out, err := runFromRepoDir(r, "hg", "pull")
	if err != nil {
		return vcs.NewLocalError("Unable to update checked out version", err, string(out))
	}

	out, err = runFromRepoDir(r, "hg", "update")
	if err != nil {
		return vcs.NewLocalError("Unable to update checked out version", err, string(out))
	}

	return nil
}

func (s *hgSource) listVersions() ([]Version, error) {
	s.baseVCSSource.lvmut.Lock()
	defer s.baseVCSSource.lvmut.Unlock()

	if s.cvsync {
		return s.dc.getAllVersions(), nil
	}

	// Must first ensure cache checkout's existence
	err := s.ensureCacheExistence()
	if err != nil {
		return nil, err
	}
	r := s.crepo.r

	// Local repo won't have all the latest refs if ensureCacheExistence()
	// didn't create it
	if !s.crepo.synced {
		s.crepo.mut.Lock()
		err = unwrapVcsErr(s.update())
		s.crepo.mut.Unlock()
		if err != nil {
			return nil, err
		}

		s.crepo.synced = true
	}

	var out []byte
	var vlist []PairedVersion

	// Now, list all the tags
	out, err = runFromRepoDir(r, "hg", "tags", "--debug", "--verbose")
	if err != nil {
		return nil, fmt.Errorf("%s: %s", err, string(out))
	}

	all := bytes.Split(bytes.TrimSpace(out), []byte("\n"))
	lbyt := []byte("local")
	nulrev := []byte("0000000000000000000000000000000000000000")
	for _, line := range all {
		if bytes.Equal(lbyt, line[len(line)-len(lbyt):]) {
			// Skip local tags
			continue
		}

		// tip is magic, don't include it
		if bytes.HasPrefix(line, []byte("tip")) {
			continue
		}

		// Split on colon; this gets us the rev and the tag plus local revno
		pair := bytes.Split(line, []byte(":"))
		if bytes.Equal(nulrev, pair[1]) {
			// null rev indicates this tag is marked for deletion
			continue
		}

		idx := bytes.IndexByte(pair[0], 32) // space
		v := NewVersion(string(pair[0][:idx])).Is(Revision(pair[1])).(PairedVersion)
		vlist = append(vlist, v)
	}

	// bookmarks next, because the presence of the magic @ bookmark has to
	// determine how we handle the branches
	var magicAt bool
	out, err = runFromRepoDir(r, "hg", "bookmarks", "--debug")
	if err != nil {
		// better nothing than partial and misleading
		return nil, fmt.Errorf("%s: %s", err, string(out))
	}

	out = bytes.TrimSpace(out)
	if !bytes.Equal(out, []byte("no bookmarks set")) {
		all = bytes.Split(out, []byte("\n"))
		for _, line := range all {
			// Trim leading spaces, and * marker if present
			line = bytes.TrimLeft(line, " *")
			pair := bytes.Split(line, []byte(":"))
			// if this doesn't split exactly once, we have something weird
			if len(pair) != 2 {
				continue
			}

			// Split on colon; this gets us the rev and the branch plus local revno
			idx := bytes.IndexByte(pair[0], 32) // space
			// if it's the magic @ marker, make that the default branch
			str := string(pair[0][:idx])
			var v PairedVersion
			if str == "@" {
				magicAt = true
				v = newDefaultBranch(str).Is(Revision(pair[1])).(PairedVersion)
			} else {
				v = NewBranch(str).Is(Revision(pair[1])).(PairedVersion)
			}
			vlist = append(vlist, v)
		}
	}

	out, err = runFromRepoDir(r, "hg", "branches", "-c", "--debug")
	if err != nil {
		// better nothing than partial and misleading
		return nil, fmt.Errorf("%s: %s", err, string(out))
	}

	all = bytes.Split(bytes.TrimSpace(out), []byte("\n"))
	for _, line := range all {
		// Trim inactive and closed suffixes, if present; we represent these
		// anyway
		line = bytes.TrimSuffix(line, []byte(" (inactive)"))
		line = bytes.TrimSuffix(line, []byte(" (closed)"))

		// Split on colon; this gets us the rev and the branch plus local revno
		pair := bytes.Split(line, []byte(":"))
		idx := bytes.IndexByte(pair[0], 32) // space
		str := string(pair[0][:idx])
		// if there was no magic @ bookmark, and this is mercurial's magic
		// "default" branch, then mark it as default branch
		var v PairedVersion
		if !magicAt && str == "default" {
			v = newDefaultBranch(str).Is(Revision(pair[1])).(PairedVersion)
		} else {
			v = NewBranch(str).Is(Revision(pair[1])).(PairedVersion)
		}
		vlist = append(vlist, v)
	}

	// Process version data into the cache and mark cache as in sync
	s.dc.storeVersionMap(vlist, true)
	s.cvsync = true
	return s.dc.getAllVersions(), nil
}

type repo struct {
	// Path to the root of the default working copy (NOT the repo itself)
	rpath string

	// Mutex controlling general access to the repo
	mut sync.RWMutex

	// Object for direct repo interaction
	r vcs.Repo

	// Whether or not the cache repo is in sync (think dvcs) with upstream
	synced bool
}

func (r *repo) exportVersionTo(v Version, to string) error {
	r.mut.Lock()
	defer r.mut.Unlock()

	// TODO(sdboyer) sloppy - this update may not be necessary
	if !r.synced {
		err := r.r.Update()
		if err != nil {
			return fmt.Errorf("err on attempting to update repo: %s", unwrapVcsErr(err))
		}
	}

	r.r.UpdateVersion(v.String())

	// TODO(sdboyer) this is a simplistic approach and relying on the tools
	// themselves might make it faster, but git's the overwhelming case (and has
	// its own method) so fine for now
	return fs.CopyDir(r.rpath, to)
}

// This func copied from Masterminds/vcs so we can exec our own commands
func mergeEnvLists(in, out []string) []string {
NextVar:
	for _, inkv := range in {
		k := strings.SplitAfterN(inkv, "=", 2)[0]
		for i, outkv := range out {
			if strings.HasPrefix(outkv, k) {
				out[i] = inkv
				continue NextVar
			}
		}
		out = append(out, inkv)
	}
	return out
}

func stripVendor(path string, info os.FileInfo, err error) error {
	if info.Name() == "vendor" {
		if _, err := os.Lstat(path); err == nil {
			if info.IsDir() {
				return removeAll(path)
			}
		}
	}

	return nil
}
