// Copyright © 2022 Meroxa, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package file

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// rotationOptions are the operator's choices for rotated files.
type rotationOptions struct {
	// pattern is a glob (relative to the directory of the path unless it is
	// absolute) that matches the rotated files, for example "app.log.*". With
	// it the follower finds the chain of rotated files itself and reads every
	// file in it, oldest first. Without it only the files it has seen at the
	// path (or finds by inode after a restart) are read, and anything it cannot
	// account for stops the source.
	pattern string
	// acceptLoss lets the source continue, with a warning, when the unread
	// tail of a rotated file cannot be read.
	acceptLoss bool
	// warnf logs a warning.
	warnf func(format string, args ...any)
	// checkDir, when set, is asked before every listing.
	checkDir DirChecker
}

// dirMeta are the glob characters that must not appear in the directory part.
// A backslash is the path separator on Windows, not an escape.
var dirMeta = func() string {
	if os.PathSeparator == '\\' {
		return "*?["
	}
	return "*?[\\"
}()

// PatternDir splits rotatedPattern into the directory it lists and the name
// pattern matched inside it. A relative pattern is relative to the directory of
// path. The directory part must be literal: wildcards are only allowed in the
// last component, so the directory an engine policy checks is the directory
// that is listed.
func PatternDir(path, pattern string) (dir, name string, err error) {
	if !filepath.IsAbs(pattern) {
		pattern = filepath.Join(filepath.Dir(path), pattern)
	}
	dir, name = filepath.Split(pattern)
	dir = filepath.Clean(dir)
	if name == "" || strings.ContainsAny(dir, dirMeta) {
		return "", "", &CodedError{
			Code: ErrCodeRotatedPatternInvalid, Path: path,
			Msg: fmt.Sprintf("rotatedPattern %q must end in a name pattern and may only use wildcards (* ? [) in that last part, not in a directory", pattern),
			Fix: "write the directory literally, for example /var/log/app/app.log.*",
		}
	}
	if _, err := filepath.Match(name, ""); err != nil {
		return "", "", &CodedError{Code: ErrCodeRotatedPatternInvalid, Path: path, Msg: fmt.Sprintf("rotatedPattern %q is not a valid glob", pattern), Fix: "fix the glob", Err: err}
	}
	return dir, name, nil
}

// listDirAllowed asks the engine's policy whether dir may be listed.
func (fl *follower) listDirAllowed(dir string) error {
	if fl.rot.checkDir == nil {
		return nil
	}
	if err := fl.rot.checkDir(dir); err != nil {
		return &CodedError{
			Code: ErrCodeRotatedPatternDenied, Path: fl.path,
			Msg: fmt.Sprintf("listing the directory %q to find rotated files is not allowed", dir),
			Fix: "put rotatedPattern (and path) in a directory the operator allows, or remove rotatedPattern",
			Err: err,
		}
	}
	return nil
}

// maxFamily bounds how many directory entries are looked at.
const maxFamily = 100000

type famFile struct {
	path string
	info os.FileInfo
}

// family lists the regular files that may belong to the rotation chain of the
// path. With a pattern those are its matches. Without one, with all=false,
// those are the files in the same directory whose name starts with the path's
// name and continues with something other than a letter (app.log.1,
// app.log-20261021, app.log20261021); with all=true every file in the
// directory. The path itself is never included. Symlinks are not followed.
func (fl *follower) family(all bool) ([]famFile, error) {
	dir, base := filepath.Dir(fl.path), filepath.Base(fl.path)
	var out []famFile
	add := func(p string) {
		if filepath.Clean(p) == filepath.Clean(fl.path) {
			return
		}
		info, err := os.Lstat(p)
		if err != nil || !info.Mode().IsRegular() {
			return
		}
		out = append(out, famFile{p, info})
	}
	if fl.rot.pattern != "" {
		pdir, name, err := PatternDir(fl.path, fl.rot.pattern)
		if err != nil {
			return nil, err
		}
		if err := fl.listDirAllowed(pdir); err != nil {
			return nil, err
		}
		entries, err := os.ReadDir(pdir)
		if err != nil {
			return nil, nil //nolint:nilnil // nothing to list is not a failure
		}
		for i, e := range entries {
			if i >= maxFamily {
				break
			}
			if ok, _ := filepath.Match(name, e.Name()); ok {
				add(filepath.Join(pdir, e.Name()))
			}
		}
		return out, nil
	}
	if err := fl.listDirAllowed(dir); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil //nolint:nilnil // nothing to list is not a failure
	}
	for i, e := range entries {
		if i >= maxFamily {
			break
		}
		name := e.Name()
		if !all {
			rest, ok := cutPrefix(name, base)
			if !ok || rest == "" {
				continue
			}
			if r, _ := utf8.DecodeRuneInString(rest); unicode.IsLetter(r) {
				continue
			}
		}
		add(filepath.Join(dir, name))
	}
	return out, nil
}

func cutPrefix(s, prefix string) (string, bool) {
	if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):], true
	}
	return s, false
}

// intermediates returns the files of the family that were written after prev
// was last written and no later than next, and that the follower does not
// already know (prev, next and the queued files). They are the files a rotation
// chain has between prev and next, oldest first (modification time, then inode).
func (fl *follower) intermediates(prev, next os.FileInfo) ([]famFile, error) {
	known := []os.FileInfo{prev, next}
	for _, q := range fl.queue {
		known = append(known, q.fi)
	}
	var out []famFile
	cands, err := fl.family(false)
	if err != nil {
		return nil, err
	}
outer:
	for _, c := range cands {
		for _, k := range known {
			if os.SameFile(c.info, k) {
				continue outer
			}
		}
		mt := c.info.ModTime()
		if mt.After(prev.ModTime()) && !mt.After(next.ModTime()) {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].info.ModTime(), out[j].info.ModTime()
		if !a.Equal(b) {
			return a.Before(b)
		}
		_, ia, _ := fileID(out[i].info)
		_, ib, _ := fileID(out[j].info)
		return ia < ib
	})
	return out, nil
}

// enter makes f the followed file after prev (the file just finished, if any)
// was left behind. Before it does, it accounts for the files in between.
func (fl *follower) enter(f *os.File) error {
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("could not stat file: %w", err)
	}
	prev := fl.hopFrom
	fl.hopFrom = nil
	if prev == nil {
		fl.install(f, fi, 0)
		return nil
	}
	mids, err := fl.intermediates(prev, fi)
	if err != nil {
		f.Close()
		return err
	}
	if len(mids) == 0 {
		fl.install(f, fi, 0)
		if len(fl.queue) > 0 {
			fl.switching = switchRotated
		}
		return nil
	}

	if fl.rot.pattern == "" {
		var size int64
		for _, m := range mids {
			size += m.info.Size()
		}
		msg := fmt.Sprintf("%d file(s) written between the rotated file and the current one (for example %q, %d bytes in total) were never read", len(mids), mids[0].path, size)
		if err := fl.tailUnreadable(msg); err != nil {
			f.Close()
			return err
		}
		fl.install(f, fi, 0)
		if len(fl.queue) > 0 {
			fl.switching = switchRotated
		}
		return nil
	}

	// With a pattern: read them, oldest first, then f.
	var chain []queuedFile
	for _, m := range mids {
		q, ok, err := fl.openChainFile(m)
		if err != nil {
			for _, c := range chain {
				c.f.Close()
			}
			f.Close()
			return err
		}
		if ok {
			chain = append(chain, q)
		}
	}
	chain = append(chain, queuedFile{f, fi})
	first := chain[0]
	fl.install(first.f, first.fi, 0)
	fl.queue = append(append([]queuedFile{}, chain[1:]...), fl.queue...)
	if len(fl.queue) > 0 {
		fl.switching = switchRotated
	}
	return nil
}

// openChainFile opens an intermediate through the opener. A compressed file
// cannot be read: that is an unreadable tail like any other.
func (fl *follower) openChainFile(m famFile) (queuedFile, bool, error) {
	f, err := fl.open(m.path, os.O_RDONLY, 0)
	if err != nil {
		if isNotExist(err) {
			return queuedFile{}, false, nil // removed meanwhile
		}
		return queuedFile{}, false, fmt.Errorf("could not open rotated file: %w", err)
	}
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() || !os.SameFile(fi, m.info) {
		f.Close()
		return queuedFile{}, false, nil
	}
	if isCompressed(f) {
		f.Close()
		msg := fmt.Sprintf("rotated file %q (%d bytes) is compressed and cannot be read", m.path, fi.Size())
		if err := fl.tailUnreadable(msg); err != nil {
			return queuedFile{}, false, err
		}
		return queuedFile{}, false, nil
	}
	return queuedFile{f, fi}, true, nil
}

var compressedMagic = [][]byte{
	{0x1f, 0x8b},                     // gzip
	[]byte("BZh"),                    // bzip2
	{0xfd, '7', 'z', 'X', 'Z', 0x00}, // xz
	{0x28, 0xb5, 0x2f, 0xfd},         // zstd
	{'P', 'K', 0x03, 0x04},           // zip
	{0x04, 0x22, 0x4d, 0x18},         // lz4
}

func isCompressed(f *os.File) bool {
	var b [6]byte
	n, _ := f.ReadAt(b[:], 0)
	for _, m := range compressedMagic {
		if n >= len(m) && bytes.Equal(b[:len(m)], m) {
			return true
		}
	}
	return false
}

// tailUnreadable reports that the unread tail of a rotated file cannot be
// accounted for. It is an error that stops the source, unless the operator
// accepted the loss, in which case it is a warning.
func (fl *follower) tailUnreadable(msg string) error {
	if fl.rot.acceptLoss {
		fl.rot.warnf("[%s] %s (path %q): continuing because rotation.acceptLoss is set; that data is skipped", ErrCodeRotationTailUnreadable, msg, fl.path)
		return nil
	}
	fix := "set rotatedPattern to a glob that matches the rotated files (for example " + filepath.Base(fl.path) + ".*) so they are read in order; a compressed, moved or deleted file cannot be read, restore it or set rotation.acceptLoss to true to skip it"
	if fl.rot.pattern != "" {
		fix = "restore the missing or compressed rotated file, widen rotatedPattern if it does not match it, or set rotation.acceptLoss to true to skip it"
	}
	return fl.coded(ErrCodeRotationTailUnreadable, msg, fix, nil)
}
