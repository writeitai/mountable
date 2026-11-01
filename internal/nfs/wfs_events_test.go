package nfs

import (
	"os"
	"testing"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"google.golang.org/grpc"
)

// eventFiler is a memFiler that also sends change events, as the filer's
// metadata subscription does.
type eventFiler struct {
	*memFiler
	events chan *filer_pb.SubscribeMetadataResponse
}

func (f *eventFiler) SubscribeMetadata(_ *filer_pb.SubscribeMetadataRequest, stream grpc.ServerStreamingServer[filer_pb.SubscribeMetadataResponse]) error {
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case e := <-f.events:
			if err := stream.Send(e); err != nil {
				return err
			}
		}
	}
}

// A file another client deletes, or moves away, changes its directory's
// mtime here: the macOS client keeps a name it looked up, answering ESTALE
// for it, until the directory's mtime changes, and the filer leaves that
// mtime alone (board ticket d67c643e).
func TestRealEngineRemoteRemovalMovesDirectoryMtime(t *testing.T) {
	for _, moved := range []bool{false, true} {
		filer := &eventFiler{memFiler: &memFiler{entries: map[string]*filer_pb.Entry{}}, events: make(chan *filer_pb.SubscribeMetadataResponse, 1)}
		filer.put("/c/fs1", entry("d", os.ModeDir|0o755, 700, ""))
		filer.put("/c/fs1/d", entry("f", 0o644, 701, "data"))
		engine := realEngine(t, filer, 100000)
		if err := engine.StartBackgroundTasks(); err != nil {
			t.Fatal(err)
		}
		_, c := startServer(t, engine)
		dir, before := c.lookup(c.root(), "d")
		c.lookup(dir, "f")

		gone := filer.entries["/c/fs1/d/f"]
		filer.remove("/c/fs1/d", "f")
		event := &filer_pb.EventNotification{OldEntry: gone}
		if moved {
			filer.put("/c/fs1", entry("f", 0o644, 701, "data"))
			event.NewEntry, event.NewParentPath = filer.entries["/c/fs1/f"], "/c/fs1"
		}
		filer.events <- &filer_pb.SubscribeMetadataResponse{Directory: "/c/fs1/d", EventNotification: event, TsNs: time.Now().UnixNano()}

		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			r := c.call(procGetAttr, wire{}.opaque(dir))
			if s := r.u32(); s != 0 {
				t.Fatalf("GETATTR of the directory: %d", s)
			}
			if a := r.fattr(); a.mtimeSec != before.mtimeSec || a.mtimeNsec != before.mtimeNsec {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("moved %v: the directory's mtime stayed %d.%09d", moved, before.mtimeSec, before.mtimeNsec)
			}
		}
		if s := c.status(procLookup, wire{}.opaque(dir).str("f")); s != uint32(nfsErrNoEnt) {
			t.Fatalf("moved %v: lookup of the removed name: %d", moved, s)
		}
	}
}
