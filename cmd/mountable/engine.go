package main

// The mount engine runs in this process: SeaweedFS's mount filesystem, wired
// the way upstream `weed mount` (weed/command/mount_std.go) does, keeping
// only the options Mountable uses. It is served over FUSE on Linux
// (engine_fuse.go) and to the Mac's own NFS client on macOS
// (engine_darwin.go).

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/user"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	weedmount "github.com/seaweedfs/seaweedfs/weed/mount"
	"github.com/seaweedfs/seaweedfs/weed/mount/meta_cache"
	"github.com/seaweedfs/seaweedfs/weed/operation"
	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"github.com/seaweedfs/seaweedfs/weed/util"
	"github.com/seaweedfs/seaweedfs/weed/util/fla9"
	util_http "github.com/seaweedfs/seaweedfs/weed/util/http"
	"google.golang.org/grpc"
	grpccredentials "google.golang.org/grpc/credentials"
)

// The filesystem owner's uid/gid as stored by the Mountable gateway; the mount
// shows them as the local user.
const ownerID = 1000

// startupTimeout bounds mounting, from the first call to the gateway until
// the kernel has the mount.
const startupTimeout = 2 * time.Minute

// Upstream defaults for the options Mountable does not change.
const (
	chunkSizeMB = 2
	umask       = 0o022
)

type mountConfig struct {
	gateway  string // host:port of the gateway's HTTP side; gRPC is port+10000
	root     string // the filesystem's path in the cell
	dir      string // local mount point, absolute and symlink-free
	readOnly bool
	tls      *tls.Config
	cacheDir string
	verbose  bool // the engine's whole log reaches stderr
}

// startEngine mounts. Every step that talks to the gateway runs before the
// mount is attached and ends with ctx or startupTimeout, even where the
// engine's own calls ignore their context; once attached, a failure or
// cancellation aborts the mount before returning.
func startEngine(ctx context.Context, c mountConfig) (*engine, error) {
	ctx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	engineVerbose.Store(c.verbose)
	if err := logToStderr(); err != nil {
		return nil, err
	}
	if err := useHTTPClientTLS(c.tls); err != nil {
		return nil, err
	}
	dialOption := grpcDialOption(c.tls)
	filers := []pb.ServerAddress{pb.ServerAddress(c.gateway)}
	var cipher bool
	err := unlessCancelled(ctx, func() (err error) {
		cipher, err = filerCipher(ctx, filers, dialOption)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("reaching the gateway %s: %w", c.gateway, err)
	}

	dir := c.dir
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	uid, gid := util.GetFileUidGid(info)
	if uid == 0 {
		if u, err := user.Current(); err == nil {
			if id, err := strconv.ParseUint(u.Uid, 10, 32); err == nil {
				uid = uint32(id)
			}
			if id, err := strconv.ParseUint(u.Gid, 10, 32); err == nil {
				gid = uint32(id)
			}
		}
	}
	mapper, err := meta_cache.NewUidGidMapper(
		fmt.Sprintf("%d:%d", os.Getuid(), ownerID), fmt.Sprintf("%d:%d", os.Getgid(), ownerID))
	if err != nil {
		return nil, err
	}

	wfs := weedmount.NewSeaweedFileSystem(&weedmount.Option{
		MountDirectory:        dir,
		FilerAddresses:        filers,
		GrpcDialOption:        dialOption,
		FilerMountRootPath:    c.root,
		ChunkSizeLimit:        chunkSizeMB * 1024 * 1024,
		ConcurrentWriters:     128,
		ConcurrentReaders:     128,
		ReaderCacheSizeMB:     256,
		CacheDirForRead:       c.cacheDir,
		CacheSizeMBForRead:    128,
		CacheDirForWrite:      c.cacheDir,
		CacheMetaTTlSec:       60,
		CacheDirMaxEntries:    100000,
		MountUid:              uid,
		MountGid:              gid,
		MountMode:             os.ModeDir | 0o777&^umask,
		MountCtime:            info.ModTime(),
		MountMtime:            time.Now(),
		Umask:                 umask,
		VolumeServerAccess:    "filerProxy",
		Cipher:                cipher,
		UidGidMapper:          mapper,
		DefaultPermissions:    true,
		IsMacOs:               runtime.GOOS == "darwin",
		MetadataFlushSeconds:  120,
		DirIdleEvictSec:       600,
		EnableDistributedLock: true,
		// Creates reach the filer before they are reported, so a create
		// adopts the inode the filer stored, on every platform; the
		// writeback cache stays off (D43, architecture §4 obligation 13).
		EagerFilerCreate: true,
		// A macOS WRITE is write-through (D45): a refusal the gateway
		// made final is answered at once instead of retried for seconds
		// while the file's other writes wait. Linux keeps the engine's
		// retries.
		RefusalsAreFinal: runtime.GOOS == "darwin",
	})
	parent, name := util.FullPath(c.root).DirAndName()
	if err := unlessCancelled(ctx, func() error {
		return filer_pb.Mkdir(ctx, wfs, parent, name, nil)
	}); err != nil {
		return nil, fmt.Errorf("creating the filesystem root: %w", err)
	}
	// Reads the cell's configuration (without a deadline of its own) and
	// starts the metadata subscription; nothing here needs the mount.
	if err := unlessCancelled(ctx, wfs.StartBackgroundTasks); err != nil {
		return nil, fmt.Errorf("starting the mount engine: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return attach(ctx, wfs, c)
}

// unlessCancelled runs fn and returns its error, or ctx's error as soon as
// ctx ends, whether or not fn honours ctx (it then finishes in the
// background).
func unlessCancelled(ctx context.Context, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	result := make(chan error, 1)
	go func() { result <- fn() }()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// engineLog carries what the engine writes to os.Stderr through a pipe, so
// its lines reach the real stderr with secrets removed, and only its fatal
// errors unless engineVerbose.
var engineLog struct {
	mu      sync.Mutex
	pipe    *os.File      // os.Stderr once redirected
	flushed chan struct{} // receives when a flush marker is read
}

const engineLogFlush = "\x00mountable: flush\n"

// engineVerbose is mount --verbose: the engine's whole log reaches stderr.
var engineVerbose atomic.Bool

// logToStderr sends the engine's log to stderr, never to files, with
// secrets removed. The engine writes to os.Stderr directly, so os.Stderr
// becomes a pipe whose lines are redacted onto the process's stderr.
func logToStderr() error {
	engineLog.mu.Lock()
	defer engineLog.mu.Unlock()
	if engineLog.pipe != nil {
		return nil
	}
	// Once: the engine reads the flag unlocked while it logs.
	if err := fla9.Set("logtostderr", "true"); err != nil {
		return err
	}
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	engineLog.pipe, engineLog.flushed = w, make(chan struct{}, 1)
	flushed := engineLog.flushed
	os.Stderr = w
	log.SetOutput(w) // the standard logger kept the original stderr
	go func() {
		lines := bufio.NewReader(r)
		for {
			line, err := lines.ReadString('\n')
			if line == engineLogFlush {
				select {
				case flushed <- struct{}{}:
				default:
				}
				continue
			}
			if shown := engineLine(line, engineVerbose.Load()); shown != "" {
				writeDiagnostic(shown)
			}
			if err != nil {
				return
			}
		}
	}()
	return nil
}

// glogLine is a line of the engine's log: its severity (Info, Warning,
// Error, Fatal), date, time, source file and line, then the message.
var glogLine = regexp.MustCompile(`^([IWEF])\d{4} \d\d:\d\d:\d\d\.\d{6} +\S+:\d+\]? (.*)`)

// engineLine is what reaches stderr of one line the engine wrote: the line
// itself when verbose, otherwise only a fatal error's message. The engine
// logs routine progress, and every retry, at the other levels; what a user
// needs from a running mount, the mount reports in its own words, and a
// failed file operation fails in the program that made it.
func engineLine(line string, verbose bool) string {
	if verbose {
		return line
	}
	m := glogLine.FindStringSubmatch(line)
	if m == nil || m[1] != "F" {
		return ""
	}
	return "mountable: fatal error: " + strings.TrimSpace(m[2]) + "\n"
}

// flushEngineLog waits, briefly, until what the engine logged so far is
// written out.
func flushEngineLog() {
	engineLog.mu.Lock()
	defer engineLog.mu.Unlock()
	if engineLog.pipe == nil {
		return
	}
	select {
	case <-engineLog.flushed: // a stale marker from a flush that timed out
	default:
	}
	if _, err := io.WriteString(engineLog.pipe, engineLogFlush); err != nil {
		return
	}
	select {
	case <-engineLog.flushed:
	case <-time.After(time.Second):
	}
}

func grpcDialOption(config *tls.Config) grpc.DialOption {
	return grpc.WithTransportCredentials(grpccredentials.NewTLS(config))
}

// useHTTPClientTLS makes the engine's HTTP clients (chunk reads and writes
// through the gateway) use https with config. It must run before their first
// use.
func useHTTPClientTLS(config *tls.Config) error {
	v := util.GetViper()
	v.Set("https.client.enabled", true)
	// Ignore any security.toml lying around: the certificate comes from memory.
	for _, key := range []string{"cert", "key", "ca"} {
		v.Set("https.client."+key, "")
	}
	v.Set("https.client.insecure_skip_verify", false)
	client := util_http.GetGlobalHttpClient()
	if client.GetHttpScheme() != "https" {
		return errors.New("the mount engine's HTTP client was initialised without TLS")
	}
	client.Transport.TLSClientConfig = config
	// Chunk uploads go through a second, shared client; make it this one.
	uploader, err := operation.NewUploader()
	if err != nil {
		return err
	}
	*uploader = *operation.NewUploaderWithHttpClient(client)
	return nil
}

// filerCipher asks the gateway for the cell's configuration, retrying while
// it is unreachable, and returns whether chunks are encrypted.
func filerCipher(ctx context.Context, filers []pb.ServerAddress, dialOption grpc.DialOption) (cipher bool, err error) {
	for attempt := 1; attempt <= 5; attempt++ {
		err = pb.WithOneOfGrpcFilerClients(false, filers, dialOption, func(client filer_pb.SeaweedFilerClient) error {
			callCtx, cancel := context.WithTimeout(ctx, requestTimeout)
			defer cancel()
			resp, err := client.GetFilerConfiguration(callCtx, &filer_pb.GetFilerConfigurationRequest{})
			if err != nil {
				return err
			}
			cipher = resp.Cipher
			return nil
		})
		if err == nil {
			return cipher, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(time.Duration(attempt) * time.Second):
		}
	}
	return false, err
}
