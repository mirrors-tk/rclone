//go:build linux || freebsd
// +build linux freebsd

package mount

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"bazil.org/fuse"
	fusefs "bazil.org/fuse/fs"
	"github.com/rclone/rclone/cmd/mountlib"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/log"
	"github.com/rclone/rclone/vfs"
)

// Dir represents a directory entry
type Dir struct {
	*vfs.Dir
	fsys *FS
}

// Check interface satisfied
var _ fusefs.Node = (*Dir)(nil)

// Attr updates the attributes of a directory
func (d *Dir) Attr(ctx context.Context, a *fuse.Attr) (err error) {
	defer log.Trace(d, "")("attr=%+v, err=%v", a, &err)
	a.Valid = d.fsys.opt.AttrTimeout
	a.Gid = d.VFS().Opt.GID
	a.Uid = d.VFS().Opt.UID
	a.Mode = os.ModeDir | d.VFS().Opt.DirPerms
	modTime := d.ModTime()
	a.Atime = modTime
	a.Mtime = modTime
	a.Ctime = modTime
	a.Crtime = modTime
	// FIXME include Valid so get some caching?
	// FIXME fs.Debugf(d.path, "Dir.Attr %+v", a)
	return nil
}

// Check interface satisfied
var _ fusefs.NodeSetattrer = (*Dir)(nil)

// Setattr handles attribute changes from FUSE. Currently supports ModTime only.
func (d *Dir) Setattr(ctx context.Context, req *fuse.SetattrRequest, resp *fuse.SetattrResponse) (err error) {
	defer log.Trace(d, "stat=%+v", req)("err=%v", &err)
	if d.VFS().Opt.NoModTime {
		return nil
	}

	if req.Valid.MtimeNow() {
		err = d.SetModTime(time.Now())
	} else if req.Valid.Mtime() {
		err = d.SetModTime(req.Mtime)
	}

	return translateError(err)
}

// Check interface satisfied
var _ fusefs.NodeRequestLookuper = (*Dir)(nil)

// Lookup looks up a specific entry in the receiver.
//
// Lookup should return a Node corresponding to the entry.  If the
// name does not exist in the directory, Lookup should return ENOENT.
//
// Lookup need not to handle the names "." and "..".
func (d *Dir) Lookup(ctx context.Context, req *fuse.LookupRequest, resp *fuse.LookupResponse) (node fusefs.Node, err error) {
	defer log.Trace(d, "name=%q", req.Name)("node=%+v, err=%v", &node, &err)
	mnode, err := d.Dir.Stat(req.Name)
	if err != nil {
		// might be a symlink; so check to see if it is
		symlink_mnode, err := d.Dir.Stat(req.Name + fs.LinkSuffix)
		if err != nil {
			return nil, translateError(err)
		}
		// symlink found
		mnode = symlink_mnode
	}
	resp.EntryValid = d.fsys.opt.AttrTimeout
	// Check the mnode to see if it has a fuse Node cached
	// We must return the same fuse nodes for vfs Nodes
	node, ok := mnode.Sys().(fusefs.Node)
	if ok {
		return node, nil
	}
	switch x := mnode.(type) {
	case *vfs.File:
		node = &File{x, d.fsys}
	case *vfs.Dir:
		node = &Dir{x, d.fsys}
	default:
		panic("bad type")
	}
	// Cache the node for later
	mnode.SetSys(node)
	return node, nil
}

// Check interface satisfied
var _ fusefs.HandleReadDirAller = (*Dir)(nil)

// ReadDirAll reads the contents of the directory
func (d *Dir) ReadDirAll(ctx context.Context) (dirents []fuse.Dirent, err error) {
	itemsRead := -1
	defer log.Trace(d, "")("item=%d, err=%v", &itemsRead, &err)
	items, err := d.Dir.ReadDirAll()
	if err != nil {
		return nil, translateError(err)
	}
	dirents = append(dirents, fuse.Dirent{
		Type: fuse.DT_Dir,
		Name: ".",
	}, fuse.Dirent{
		Type: fuse.DT_Dir,
		Name: "..",
	})
	for _, node := range items {
		name := node.Name()
		if len(name) > mountlib.MaxLeafSize {
			fs.Errorf(d, "Name too long (%d bytes) for FUSE, skipping: %s", len(name), name)
			continue
		}
		var dirent = fuse.Dirent{
			// Inode FIXME ???
			Type: fuse.DT_File,
			Name: name,
		}
		if node.IsDir() {
			dirent.Type = fuse.DT_Dir
		} else if strings.HasSuffix(name, fs.LinkSuffix) {
			// node represents a symlink; mark it as such, and remove the suffix from the node name
			dirent.Type = fuse.DT_Link
			dirent.Name = name[:len(name)-len(fs.LinkSuffix)]
			log.Trace(d, "symlink %s => %s", name, dirent.Name)("")
		}
		dirents = append(dirents, dirent)
	}
	itemsRead = len(dirents)
	return dirents, nil
}

var _ fusefs.NodeCreater = (*Dir)(nil)

// Create makes a new file
func (d *Dir) Create(ctx context.Context, req *fuse.CreateRequest, resp *fuse.CreateResponse) (node fusefs.Node, handle fusefs.Handle, err error) {
	defer log.Trace(d, "name=%q", req.Name)("node=%v, handle=%v, err=%v", &node, &handle, &err)
	file, err := d.Dir.Create(req.Name, int(req.Flags))
	if err != nil {
		return nil, nil, translateError(err)
	}
	fh, err := file.Open(int(req.Flags) | os.O_CREATE)
	if err != nil {
		return nil, nil, translateError(err)
	}
	node = &File{file, d.fsys}
	file.SetSys(node) // cache the FUSE node for later
	return node, &FileHandle{fh}, err
}

var _ fusefs.NodeMkdirer = (*Dir)(nil)

// Mkdir creates a new directory
func (d *Dir) Mkdir(ctx context.Context, req *fuse.MkdirRequest) (node fusefs.Node, err error) {
	defer log.Trace(d, "name=%q", req.Name)("node=%+v, err=%v", &node, &err)
	dir, err := d.Dir.Mkdir(req.Name)
	if err != nil {
		return nil, translateError(err)
	}
	node = &Dir{dir, d.fsys}
	dir.SetSys(node) // cache the FUSE node for later
	return node, nil
}

var _ fusefs.NodeRemover = (*Dir)(nil)

// Remove removes the entry with the given name from
// the receiver, which must be a directory.  The entry to be removed
// may correspond to a file (unlink) or to a directory (rmdir).
func (d *Dir) Remove(ctx context.Context, req *fuse.RemoveRequest) (err error) {
	defer log.Trace(d, "name=%q", req.Name)("err=%v", &err)

	_, err = d.Dir.Stat(req.Name)
	if err != nil && err == vfs.ENOENT {
		// check for removing symlink
		err = d.Dir.RemoveName(req.Name + fs.LinkSuffix)
		if err != nil {
			return translateError(err)
		}
		return nil
	}

	err = d.Dir.RemoveName(req.Name)
	if err != nil {
		return translateError(err)
	}
	return nil
}

// Invalidate a leaf in a directory
func (d *Dir) invalidateEntry(dirNode fusefs.Node, leaf string) {
	fs.Debugf(dirNode, "Invalidating %q", leaf)
	err := d.fsys.server.InvalidateEntry(dirNode, leaf)
	if err != nil {
		fs.Debugf(dirNode, "Failed to invalidate %q: %v", leaf, err)
	}
}

// Check interface satisfied
var _ fusefs.NodeRenamer = (*Dir)(nil)

// Rename the file
func (d *Dir) Rename(ctx context.Context, req *fuse.RenameRequest, newDir fusefs.Node) (err error) {
	defer log.Trace(d, "oldName=%q, newName=%q, newDir=%+v", req.OldName, req.NewName, newDir)("err=%v", &err)
	destDir, ok := newDir.(*Dir)
	if !ok {
		return fmt.Errorf("Unknown Dir type %T", newDir)
	}

	_, err = d.Dir.Stat(req.OldName)
	// if the file cannot be found, it might be because it is a symbolic link
	if err != nil && err == vfs.ENOENT {
		log.Trace(d, "Could not find file %s; checking for symbolic link", req.OldName)("")
		err = d.Dir.Rename(req.OldName+fs.LinkSuffix, req.NewName+fs.LinkSuffix, destDir.Dir)
		if err != nil {
			return translateError(err)
		}
		return nil
	}

	err = d.Dir.Rename(req.OldName, req.NewName, destDir.Dir)
	if err != nil {
		return translateError(err)
	}

	// Invalidate the new directory entry so it gets re-read (in
	// the background otherwise we cause a deadlock)
	//
	// See https://github.com/rclone/rclone/issues/4977 for why
	go d.invalidateEntry(newDir, req.NewName)
	//go d.invalidateEntry(d, req.OldName)

	return nil
}

// Check interface satisfied
var _ fusefs.NodeFsyncer = (*Dir)(nil)

// Fsync the directory
func (d *Dir) Fsync(ctx context.Context, req *fuse.FsyncRequest) (err error) {
	defer log.Trace(d, "")("err=%v", &err)
	err = d.Dir.Sync()
	if err != nil {
		return translateError(err)
	}
	return nil
}

// Check interface satisfied
var _ fusefs.NodeLinker = (*Dir)(nil)

// Link creates a new directory entry in the receiver based on an
// existing Node. Receiver must be a directory.
func (d *Dir) Link(ctx context.Context, req *fuse.LinkRequest, old fusefs.Node) (newNode fusefs.Node, err error) {
	defer log.Trace(d, "req=%v, old=%v", req, old)("new=%v, err=%v", &newNode, &err)
	return nil, fuse.ENOSYS
}

// Check interface satisfied
var _ fusefs.NodeMknoder = (*Dir)(nil)

// Mknod is called to create a file. Since we define create this will
// be called in preference, however NFS likes to call it for some
// reason. We don't actually create a file here just the Node.
func (d *Dir) Mknod(ctx context.Context, req *fuse.MknodRequest) (node fusefs.Node, err error) {
	defer log.Trace(d, "name=%v, mode=%d, rdev=%d", req.Name, req.Mode, req.Rdev)("node=%v, err=%v", &node, &err)
	if req.Rdev != 0 {
		fs.Errorf(d, "Can't create device node %q", req.Name)
		return nil, fuse.EIO
	}
	var cReq = fuse.CreateRequest{
		Name:  req.Name,
		Flags: fuse.OpenFlags(os.O_CREATE | os.O_WRONLY),
		Mode:  req.Mode,
		Umask: req.Umask,
	}
	var cResp fuse.CreateResponse
	node, handle, err := d.Create(ctx, &cReq, &cResp)
	if err != nil {
		return nil, err
	}
	err = handle.(io.Closer).Close()
	if err != nil {
		return nil, err
	}
	return node, nil
}

var _ fusefs.NodeSymlinker = (*Dir)(nil)

// create symbolic link
func (d *Dir) Symlink(ctx context.Context, req *fuse.SymlinkRequest) (node fusefs.Node, err error) {
	defer log.Trace(d, "target=%q; new_name=%q", req.Target, req.NewName)("node=%v, err=%v", &node, &err)

	symlinkName := req.NewName + fs.LinkSuffix
	file, err := d.Dir.Create(symlinkName, os.O_RDWR)
	if err != nil {
		log.Trace(d, "failed to create symlink file %s", symlinkName)
		return nil, translateError(err)
	}
	fh, err := file.Open(os.O_RDWR | os.O_CREATE)
	if err != nil {
		log.Trace(d, "failed to open symlink file %s", symlinkName)
		return nil, translateError(err)
	}
	node = &File{file, d.fsys}

	_, err2 := fh.WriteAt([]byte(req.Target), 0)
	if err2 != nil {
		log.Trace(d, "failed to write to symlink file %s", symlinkName)
		return nil, translateError(err2)
	}

	err = fh.Release()
	if err != nil {
		log.Trace(d, "failed to close symlink file %s", symlinkName)
		return nil, translateError(err)
	}

	return node, err
}
