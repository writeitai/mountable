package nfs

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/seaweedfs/seaweedfs/weed/filer"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// The memory filer's mutations, as the filer answers them: exclusive and
// conditional creates and updates, deletes that refuse a directory with
// children, and renames reported as one event.

func (f *memFiler) CreateEntry(_ context.Context, r *filer_pb.CreateEntryRequest) (*filer_pb.CreateEntryResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := strings.TrimSuffix(r.Directory, "/") + "/" + r.Entry.Name
	current := f.entries[key]
	if r.OExcl && current != nil {
		return &filer_pb.CreateEntryResponse{Error: "entry already exists", ErrorCode: filer_pb.FilerError_ENTRY_ALREADY_EXISTS}, nil
	}
	if !conditionHolds(r.Condition, current) {
		return &filer_pb.CreateEntryResponse{Error: "precondition failed", ErrorCode: filer_pb.FilerError_PRECONDITION_FAILED}, nil
	}
	stored := proto.Clone(r.Entry).(*filer_pb.Entry)
	f.entries[key] = stored
	return &filer_pb.CreateEntryResponse{MetadataEvent: &filer_pb.SubscribeMetadataResponse{Directory: r.Directory, EventNotification: &filer_pb.EventNotification{
		NewEntry: proto.Clone(stored).(*filer_pb.Entry), NewParentPath: r.Directory,
	}}}, nil
}

func (f *memFiler) UpdateEntry(_ context.Context, r *filer_pb.UpdateEntryRequest) (*filer_pb.UpdateEntryResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := strings.TrimSuffix(r.Directory, "/") + "/" + r.Entry.Name
	current := f.entries[key]
	if current == nil {
		if r.Entry.IsDirectory {
			return &filer_pb.UpdateEntryResponse{}, nil // a directory's times, such as the root's
		}
		return nil, status.Errorf(codes.Unknown, "not found %s", key)
	}
	if !conditionHolds(r.Condition, current) {
		return nil, status.Errorf(codes.FailedPrecondition, "precondition failed: %s", key)
	}
	f.entries[key] = proto.Clone(r.Entry).(*filer_pb.Entry)
	return &filer_pb.UpdateEntryResponse{}, nil
}

func (f *memFiler) DeleteEntry(_ context.Context, r *filer_pb.DeleteEntryRequest) (*filer_pb.DeleteEntryResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := strings.TrimSuffix(r.Directory, "/") + "/" + r.Name
	if f.entries[key] == nil {
		return &filer_pb.DeleteEntryResponse{Error: "not found " + key}, nil
	}
	for other := range f.entries {
		if strings.HasPrefix(other, key+"/") {
			return &filer_pb.DeleteEntryResponse{Error: "fail to delete non-empty folder: " + key}, nil
		}
	}
	delete(f.entries, key)
	return &filer_pb.DeleteEntryResponse{}, nil
}

func (f *memFiler) StreamRenameEntry(r *filer_pb.StreamRenameEntryRequest, stream grpc.ServerStreamingServer[filer_pb.StreamRenameEntryResponse]) error {
	f.mu.Lock()
	from := strings.TrimSuffix(r.OldDirectory, "/") + "/" + r.OldName
	to := strings.TrimSuffix(r.NewDirectory, "/") + "/" + r.NewName
	e := f.entries[from]
	if e == nil {
		f.mu.Unlock()
		return status.Errorf(codes.NotFound, "%s not found", from)
	}
	delete(f.entries, from)
	moved := proto.Clone(e).(*filer_pb.Entry)
	moved.Name = r.NewName
	f.entries[to] = moved
	f.mu.Unlock()
	return stream.Send(&filer_pb.StreamRenameEntryResponse{Directory: r.OldDirectory, EventNotification: &filer_pb.EventNotification{
		OldEntry: &filer_pb.Entry{Name: r.OldName}, NewEntry: proto.Clone(moved).(*filer_pb.Entry), NewParentPath: r.NewDirectory,
	}})
}

// conditionHolds evaluates IF_ENTRY_EQUAL as the filer does.
func conditionHolds(c *filer_pb.WriteCondition, current *filer_pb.Entry) bool {
	for _, clause := range c.GetClauses() {
		if clause.Kind != filer_pb.WriteCondition_IF_ENTRY_EQUAL {
			continue
		}
		expected := clause.GetExpectedEntry()
		if (current == nil) != (expected == nil) {
			return false
		}
		if current != nil && !proto.Equal(filer.FromPbEntry("", current).ToProtoEntry(), filer.FromPbEntry("", expected).ToProtoEntry()) {
			return false
		}
	}
	return true
}

// The namespace and attribute procedures against the real engine: what
// the NFS client creates, changes, links, moves and removes is what the
// filer then holds, and handles follow the objects.
func TestRealEngineMutations(t *testing.T) {
	filer := &memFiler{entries: map[string]*filer_pb.Entry{
		"/c/fs1/a.txt": entry("a.txt", 0o644, 9001, "hello"),
	}}
	_, c := startWritable(t, realEngine(t, filer, 1000))
	root := c.root()
	stored := func(name string) *filer_pb.Entry {
		filer.mu.Lock()
		defer filer.mu.Unlock()
		return filer.entries["/c/fs1/"+name]
	}

	fh, a := c.call(procCreate, wire{}.opaque(root).str("new").u32(createGuarded).append(modeAttrs(0o640))).created()
	if e := stored("new"); e == nil || e.Attributes.FileMode&0o777 != 0o640 || a.fileid != fhInode(fh) {
		t.Fatalf("CREATE: stored %v, fileid %d", e, a.fileid)
	}
	if s := c.status(procCreate, wire{}.opaque(root).str("new").u32(createGuarded).append(noAttrs)); s != uint32(nfsErrExist) {
		t.Fatalf("CREATE GUARDED again: %d", s)
	}
	verifier := wire{}.u32(1).u32(2)
	lock, _ := c.call(procCreate, wire{}.opaque(root).str("lock").u32(createExclusive).append(verifier)).created()
	if again, _ := c.call(procCreate, wire{}.opaque(root).str("lock").u32(createExclusive).append(verifier)).created(); !bytes.Equal(again, lock) {
		t.Fatal("EXCLUSIVE retried: another handle")
	}
	if s := c.status(procSetAttr, wire{}.opaque(lock).append(modeAttrs(0o600)).u32(0)); s != uint32(nfsOK) {
		t.Fatalf("SETATTR: %d", s)
	}
	if e := stored("lock"); e.Attributes.AtimeNs == 999999999 || e.Attributes.FileMode&0o777 != 0o600 {
		t.Fatalf("after the SETATTR: %v", e.Attributes)
	}
	if _, a := c.call(procMkdir, wire{}.opaque(root).str("d").append(noAttrs)).created(); a.ftype != typeDir || stored("d") == nil || !stored("d").IsDirectory {
		t.Fatalf("MKDIR: %+v", a)
	}
	a1, _ := c.lookup(root, "a.txt")
	if s := c.status(procLink, wire{}.opaque(a1).opaque(root).str("a2")); s != uint32(nfsOK) {
		t.Fatalf("LINK: %d", s)
	}
	if a2, _ := c.lookup(root, "a2"); !bytes.Equal(a2, a1) || len(stored("a2").GetHardLinkId()) == 0 {
		t.Fatal("LINK: the new name is another object")
	}
	if s := c.status(procRename, wire{}.opaque(root).str("new").opaque(root).str("moved")); s != uint32(nfsOK) {
		t.Fatalf("RENAME: %d", s)
	}
	if moved, _ := c.lookup(root, "moved"); !bytes.Equal(moved, fh) || stored("new") != nil || stored("moved") == nil {
		t.Fatal("RENAME: the file did not move with its handle")
	}
	if s := c.status(procRemove, wire{}.opaque(root).str("moved")); s != uint32(nfsOK) || stored("moved") != nil {
		t.Fatalf("REMOVE: %d", s)
	}
	if s := c.status(procGetAttr, wire{}.opaque(fh)); s != uint32(nfsErrStale) {
		t.Fatalf("the removed file's handle: %d", s)
	}
	if s := c.status(procRmdir, wire{}.opaque(root).str("d")); s != uint32(nfsOK) || stored("d") != nil {
		t.Fatalf("RMDIR: %d", s)
	}
}

// fhInode is the WFS inode a handle names.
func fhInode(fh []byte) uint64 {
	var inode uint64
	for _, b := range fh[12:20] {
		inode = inode<<8 | uint64(b)
	}
	return inode
}
