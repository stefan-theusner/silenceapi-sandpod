package main

import (
	"context"
	"os"
	"path/filepath"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// Config is shared by every HostNode in the FUSE tree.
type Config struct {
	Rules      *Rules
	HostRoot   *fs.LoopbackRoot
	ShadowRoot *fs.LoopbackRoot

	// OwnerOverride, when set, forces every HostNode entry's reported
	// owner to OwnerUid:OwnerGid regardless of what the real backing
	// file is owned by. sandpod-specific: on Podman/macOS the host tree
	// is reached through virtiofs, which reports all backing content as
	// owned by root no matter who actually owns it on the Mac side -
	// confusing in `ls -l` and (before a separate git safe.directory fix)
	// tripped git's dubious-ownership check. This only changes what's
	// *reported*; it never touches real permission enforcement, which for
	// the host tree already happens via the underlying syscalls running
	// as whatever the FUSE server itself is (root), not the caller.
	OwnerOverride bool
	OwnerUid      uint32
	OwnerGid      uint32
}

// applyOwnerOverride rewrites attr.Owner in place per Config.OwnerOverride.
// No-op (and cheap) when unconfigured.
func (n *HostNode) applyOwnerOverride(attr *fuse.Attr) {
	if !n.cfg.OwnerOverride {
		return
	}
	attr.Owner.Uid = n.cfg.OwnerUid
	attr.Owner.Gid = n.cfg.OwnerGid
}

// HostNode is a rule-aware loopback node rooted at HostRoot.Path.
// When a child path matches a rule it routes to ShadowRoot.
type HostNode struct {
	fs.LoopbackNode
	cfg *Config
}

func (n *HostNode) relPath() string { return n.Path(n.Root()) }

// shadowPhase is XOR'd into every StableAttr.Ino assigned to nodes under the
// shadow tree, keeping their kernel-cache identity disjoint from the backing
// tree. Without this, a backing inode N and a shadow inode N (both arrive
// after npm install populates the shadow store with low Ino numbers) hash to
// the same StableAttr.Ino in go-fuse; the kernel inode cache then aliases
// them and serves the wrong subtree to OpenDir/Readdir. Backing-tree nodes
// keep phase 0 — go-fuse's own Lookups don't know about phases, so we only
// shift the side we control.
const shadowPhase uint64 = 1 << 63

// idFromStat replicates LoopbackRoot.idFromStat (unexported in go-fuse).
func idFromStat(rootDev uint64, st *syscall.Stat_t) fs.StableAttr {
	swapped := (uint64(st.Dev) << 32) | (uint64(st.Dev) >> 32)
	swappedRootDev := (rootDev << 32) | (rootDev >> 32)
	return fs.StableAttr{
		Mode: uint32(st.Mode),
		Gen:  1,
		Ino:  (swapped ^ swappedRootDev) ^ st.Ino,
	}
}

// idFromShadowStat returns a StableAttr namespaced under shadowPhase. Use for
// any node whose backing storage is /var/lib/rp/shadow.
func idFromShadowStat(rootDev uint64, st *syscall.Stat_t) fs.StableAttr {
	attr := idFromStat(rootDev, st)
	attr.Ino ^= shadowPhase
	return attr
}

func joinRel(parent, name string) string {
	if parent == "" || parent == "." {
		return name
	}
	return parent + "/" + name
}

// rulesFileName is the workspace-relative path of the shadow rules file. Writes
// to this path from inside the container are rejected with EROFS — only the
// host is allowed to modify the ruleset.
const rulesFileName = ".rp/shadow"

func isWriteAccess(flags uint32) bool {
	mode := int(flags) & syscall.O_ACCMODE
	return mode == syscall.O_WRONLY || mode == syscall.O_RDWR
}

// shadowPath returns the shadow backing path for a workspace-relative path.
func (n *HostNode) shadowPath(rel string) string {
	return filepath.Join(n.cfg.ShadowRoot.Path, rel)
}

// ensureShadowParent creates the shadow parent directory (recursively) for a
// to-be-created rule-matched child. No-op if it already exists. Each newly-
// created intermediate is chowned to the FUSE caller so that subsequent
// caller-owned operations (fchmod, chmod, rmdir) don't get EPERM from the
// kernel's permission check.
func (n *HostNode) ensureShadowParent(ctx context.Context, childRel string) error {
	parent := filepath.Dir(childRel)
	if parent == "." || parent == "" {
		return nil
	}
	// Walk up the chain, mkdir each missing component, and chown it to the
	// caller. We can't use os.MkdirAll here because it doesn't tell us which
	// components it created, so we'd over-chown existing dirs.
	full := n.shadowPath(parent)
	var toCreate []string
	for cur := full; cur != n.cfg.ShadowRoot.Path && cur != "/" && cur != "."; cur = filepath.Dir(cur) {
		if _, err := os.Stat(cur); err == nil {
			break
		} else if !os.IsNotExist(err) {
			return err
		}
		toCreate = append(toCreate, cur)
	}
	// Create in root-to-leaf order.
	for i := len(toCreate) - 1; i >= 0; i-- {
		if err := os.Mkdir(toCreate[i], 0o755); err != nil && !os.IsExist(err) {
			return err
		}
		chownToCaller(ctx, toCreate[i])
	}
	return nil
}

// chownToCaller chowns the given path to the FUSE caller's uid/gid. Used after
// shadow-side create operations so the file/dir ends up owned by the user that
// triggered the FUSE op (rather than the FUSE driver process, which runs as
// root). Without this, the kernel rejects subsequent caller-owned operations
// (fchmod, fchown, rmdir from non-empty dir) with EPERM before the request
// ever reaches FUSE. Silently no-ops if there's no Caller in the context
// (initial mount setup, internal calls).
func chownToCaller(ctx context.Context, path string) {
	caller, ok := fuse.FromContext(ctx)
	if !ok || caller == nil {
		return
	}
	_ = syscall.Lchown(path, int(caller.Uid), int(caller.Gid))
}

// fchownToCaller is the FD-based variant for callers that already have the fd
// open (e.g. straight after syscall.Open with O_CREAT).
func fchownToCaller(ctx context.Context, fd int) {
	caller, ok := fuse.FromContext(ctx)
	if !ok || caller == nil {
		return
	}
	_ = syscall.Fchown(fd, int(caller.Uid), int(caller.Gid))
}

func (n *HostNode) shadowChild(ctx context.Context, rel string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	p := n.shadowPath(rel)
	var st syscall.Stat_t
	if err := syscall.Lstat(p, &st); err != nil {
		return nil, fs.ToErrno(err)
	}
	if out != nil {
		out.Attr.FromStat(&st)
	}
	node := &ShadowNode{LoopbackNode: fs.LoopbackNode{RootData: n.cfg.ShadowRoot}}
	ch := n.NewInode(ctx, node, idFromShadowStat(n.cfg.ShadowRoot.Dev, &st))
	return ch, 0
}

// --- Read ops ---

func (n *HostNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	childRel := joinRel(n.relPath(), name)
	if n.cfg.Rules.Match(childRel) {
		return n.shadowChild(ctx, childRel, out)
	}
	inode, errno := n.LoopbackNode.Lookup(ctx, name, out)
	if errno == 0 {
		n.applyOwnerOverride(&out.Attr)
	}
	return inode, errno
}

// Getattr applies the same owner override as Lookup - needed because the
// kernel calls Getattr independently (e.g. on cache expiry, or explicit
// stat() calls), not just once via Lookup's initial EntryOut.
func (n *HostNode) Getattr(ctx context.Context, f fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	errno := n.LoopbackNode.Getattr(ctx, f, out)
	if errno == 0 {
		n.applyOwnerOverride(&out.Attr)
	}
	return errno
}

func (n *HostNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	parentRel := n.relPath()

	stream, errno := n.LoopbackNode.Readdir(ctx)
	if errno != 0 {
		return nil, errno
	}
	var entries []fuse.DirEntry
	seen := map[string]bool{}
	for stream.HasNext() {
		e, _ := stream.Next()
		if n.cfg.Rules.Match(joinRel(parentRel, e.Name)) {
			continue
		}
		entries = append(entries, e)
		seen[e.Name] = true
	}
	stream.Close()

	// Merge in shadow-side entries that exist at this directory level.
	shadowDir := n.shadowPath(parentRel)
	if dh, err := os.Open(shadowDir); err == nil {
		names, _ := dh.Readdirnames(-1)
		dh.Close()
		for _, name := range names {
			if seen[name] {
				continue
			}
			childRel := joinRel(parentRel, name)
			if !n.cfg.Rules.Match(childRel) {
				continue
			}
			var st syscall.Stat_t
			if err := syscall.Lstat(filepath.Join(shadowDir, name), &st); err == nil {
				entries = append(entries, fuse.DirEntry{
					Name: name,
					Mode: uint32(st.Mode) & syscall.S_IFMT,
					Ino:  st.Ino ^ shadowPhase,
				})
			}
		}
	}
	return fs.NewListDirStream(entries), 0
}

// --- .rp/shadow read-only enforcement ---

// Open: writes to .rp/shadow return EROFS; container cannot modify the ruleset.
func (n *HostNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if n.relPath() == rulesFileName && isWriteAccess(flags) {
		return nil, 0, syscall.EROFS
	}
	return n.LoopbackNode.Open(ctx, flags)
}

// Setattr: chmod, chown, truncate, etc. on .rp/shadow return EROFS.
func (n *HostNode) Setattr(ctx context.Context, fh fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	if n.relPath() == rulesFileName {
		return syscall.EROFS
	}
	return n.LoopbackNode.Setattr(ctx, fh, in, out)
}

// --- Write ops on rule-matching children: route to shadow ---

func (n *HostNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	childRel := joinRel(n.relPath(), name)
	if childRel == rulesFileName {
		return nil, syscall.EROFS
	}
	if n.cfg.Rules.Match(childRel) {
		if err := n.ensureShadowParent(ctx, childRel); err != nil {
			return nil, fs.ToErrno(err)
		}
		p := n.shadowPath(childRel)
		if err := os.Mkdir(p, os.FileMode(mode)); err != nil {
			return nil, fs.ToErrno(err)
		}
		chownToCaller(ctx, p)
		return n.shadowChild(ctx, childRel, out)
	}
	return n.LoopbackNode.Mkdir(ctx, name, mode, out)
}

func (n *HostNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	childRel := joinRel(n.relPath(), name)
	if childRel == rulesFileName {
		return nil, nil, 0, syscall.EROFS
	}
	if n.cfg.Rules.Match(childRel) {
		if err := n.ensureShadowParent(ctx, childRel); err != nil {
			return nil, nil, 0, fs.ToErrno(err)
		}
		p := n.shadowPath(childRel)
		flags &^= syscall.O_APPEND
		fd, err := syscall.Open(p, int(flags)|os.O_CREATE, mode)
		if err != nil {
			return nil, nil, 0, fs.ToErrno(err)
		}
		fchownToCaller(ctx, fd)
		var st syscall.Stat_t
		if err := syscall.Fstat(fd, &st); err != nil {
			syscall.Close(fd)
			return nil, nil, 0, fs.ToErrno(err)
		}
		out.FromStat(&st)
		node := &ShadowNode{LoopbackNode: fs.LoopbackNode{RootData: n.cfg.ShadowRoot}}
		ch := n.NewInode(ctx, node, idFromShadowStat(n.cfg.ShadowRoot.Dev, &st))
		return ch, fs.NewLoopbackFile(fd), 0, 0
	}
	return n.LoopbackNode.Create(ctx, name, flags, mode, out)
}

func (n *HostNode) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	childRel := joinRel(n.relPath(), name)
	if childRel == rulesFileName {
		return nil, syscall.EROFS
	}
	if n.cfg.Rules.Match(childRel) {
		if err := n.ensureShadowParent(ctx, childRel); err != nil {
			return nil, fs.ToErrno(err)
		}
		p := n.shadowPath(childRel)
		if err := syscall.Symlink(target, p); err != nil {
			return nil, fs.ToErrno(err)
		}
		chownToCaller(ctx, p)
		return n.shadowChild(ctx, childRel, out)
	}
	return n.LoopbackNode.Symlink(ctx, target, name, out)
}

func (n *HostNode) Mknod(ctx context.Context, name string, mode, rdev uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	childRel := joinRel(n.relPath(), name)
	if childRel == rulesFileName {
		return nil, syscall.EROFS
	}
	if n.cfg.Rules.Match(childRel) {
		if err := n.ensureShadowParent(ctx, childRel); err != nil {
			return nil, fs.ToErrno(err)
		}
		p := n.shadowPath(childRel)
		if err := syscall.Mknod(p, mode, int(rdev)); err != nil {
			return nil, fs.ToErrno(err)
		}
		chownToCaller(ctx, p)
		return n.shadowChild(ctx, childRel, out)
	}
	return n.LoopbackNode.Mknod(ctx, name, mode, rdev, out)
}

func (n *HostNode) Unlink(ctx context.Context, name string) syscall.Errno {
	childRel := joinRel(n.relPath(), name)
	if childRel == rulesFileName {
		return syscall.EROFS
	}
	if n.cfg.Rules.Match(childRel) {
		return fs.ToErrno(syscall.Unlink(n.shadowPath(childRel)))
	}
	return n.LoopbackNode.Unlink(ctx, name)
}

func (n *HostNode) Rmdir(ctx context.Context, name string) syscall.Errno {
	childRel := joinRel(n.relPath(), name)
	if childRel == rulesFileName {
		return syscall.EROFS
	}
	if n.cfg.Rules.Match(childRel) {
		return fs.ToErrno(syscall.Rmdir(n.shadowPath(childRel)))
	}
	return n.LoopbackNode.Rmdir(ctx, name)
}

// ShadowNode wraps go-fuse's LoopbackNode for the shadow tree so that every
// child node it creates carries a shadowPhase-XOR'd StableAttr.Ino. Without
// the override, deep paths inside /var/lib/rp/shadow would go through
// go-fuse's default LoopbackNode.Lookup → idFromStat → unphased Ino, and the
// kernel cache would alias them with backing inodes that happen to share the
// raw Ino number (very common — both filesystems frequently have low inode
// numbers like 159).
type ShadowNode struct {
	fs.LoopbackNode
}

// shadowBackingPath returns the absolute /var/lib/rp/shadow path for a child
// named `name` of this node.
func (n *ShadowNode) shadowBackingPath(name string) string {
	return filepath.Join(n.RootData.Path, n.Path(n.Root()), name)
}

// registerShadowChild builds the StableAttr for a freshly-stat'd child and
// returns a ShadowNode-wrapped inode. Centralises the phase-XOR so every
// create path in ShadowNode shares one code path.
func (n *ShadowNode) registerShadowChild(ctx context.Context, st *syscall.Stat_t) *fs.Inode {
	child := &ShadowNode{LoopbackNode: fs.LoopbackNode{RootData: n.RootData}}
	return n.NewInode(ctx, child, idFromShadowStat(n.RootData.Dev, st))
}

func (n *ShadowNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	full := n.shadowBackingPath(name)
	var st syscall.Stat_t
	if err := syscall.Lstat(full, &st); err != nil {
		return nil, fs.ToErrno(err)
	}
	if out != nil {
		out.Attr.FromStat(&st)
	}
	return n.registerShadowChild(ctx, &st), 0
}

func (n *ShadowNode) Mkdir(ctx context.Context, name string, mode uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	p := n.shadowBackingPath(name)
	if err := syscall.Mkdir(p, mode); err != nil {
		return nil, fs.ToErrno(err)
	}
	chownToCaller(ctx, p)
	var st syscall.Stat_t
	if err := syscall.Lstat(p, &st); err != nil {
		return nil, fs.ToErrno(err)
	}
	out.Attr.FromStat(&st)
	return n.registerShadowChild(ctx, &st), 0
}

func (n *ShadowNode) Create(ctx context.Context, name string, flags uint32, mode uint32, out *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	p := n.shadowBackingPath(name)
	flags &^= syscall.O_APPEND
	fd, err := syscall.Open(p, int(flags)|os.O_CREATE, mode)
	if err != nil {
		return nil, nil, 0, fs.ToErrno(err)
	}
	fchownToCaller(ctx, fd)
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		syscall.Close(fd)
		return nil, nil, 0, fs.ToErrno(err)
	}
	out.FromStat(&st)
	return n.registerShadowChild(ctx, &st), fs.NewLoopbackFile(fd), 0, 0
}

func (n *ShadowNode) Symlink(ctx context.Context, target, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	p := n.shadowBackingPath(name)
	if err := syscall.Symlink(target, p); err != nil {
		return nil, fs.ToErrno(err)
	}
	chownToCaller(ctx, p)
	var st syscall.Stat_t
	if err := syscall.Lstat(p, &st); err != nil {
		return nil, fs.ToErrno(err)
	}
	out.Attr.FromStat(&st)
	return n.registerShadowChild(ctx, &st), 0
}

func (n *ShadowNode) Mknod(ctx context.Context, name string, mode, rdev uint32, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	p := n.shadowBackingPath(name)
	if err := syscall.Mknod(p, mode, int(rdev)); err != nil {
		return nil, fs.ToErrno(err)
	}
	chownToCaller(ctx, p)
	var st syscall.Stat_t
	if err := syscall.Lstat(p, &st); err != nil {
		return nil, fs.ToErrno(err)
	}
	out.Attr.FromStat(&st)
	return n.registerShadowChild(ctx, &st), 0
}

// Rename: same-region only. Cross-region returns EXDEV. Rename touching
// .rp/shadow on either side returns EROFS (only the host can modify the rules).
func (n *HostNode) Rename(ctx context.Context, name string, newParent fs.InodeEmbedder, newName string, flags uint32) syscall.Errno {
	srcRel := joinRel(n.relPath(), name)

	dstParent, ok := newParent.(*HostNode)
	if !ok {
		return syscall.EXDEV
	}
	dstRel := joinRel(dstParent.relPath(), newName)

	if srcRel == rulesFileName || dstRel == rulesFileName {
		return syscall.EROFS
	}

	srcRule := n.cfg.Rules.Match(srcRel)
	dstRule := n.cfg.Rules.Match(dstRel)
	if srcRule != dstRule {
		return syscall.EXDEV
	}
	if srcRule {
		if err := n.ensureShadowParent(ctx, dstRel); err != nil {
			return fs.ToErrno(err)
		}
		return fs.ToErrno(syscall.Rename(n.shadowPath(srcRel), n.shadowPath(dstRel)))
	}
	return n.LoopbackNode.Rename(ctx, name, newParent, newName, flags)
}
