// sandpod-fuse: rule-aware passthrough FUSE. Vendored from
// claude-container/rp-fuse (github.com/hanwen/go-fuse-based shadow
// filesystem originally built for Apple `container`) - host_node.go and
// rules.go are unmodified; this file drops the lint/config/profile
// subcommands (sandpod resolves its config/ignore-list in bash, not here).
//
// Layout:
//   --backing <host>  : real host workspace bind mount (lower)
//   --shadow  <store> : container-local writable shadow store
//                       Mirrors FUSE paths: a matched rel path "a/b" lives at <store>/a/b.
//   --mount   <mnt>   : FUSE mount point exposed to the user/Claude
//   --rules   <file>  : path to the merged ignore list (gitignore-style patterns, one per line)
//
// Per-path semantics:
//   * Path NOT matched by any rule: passthrough to <host>/<path>. Edits propagate to host.
//   * Path matched by a rule       : routed to <store>/<path>.
//                                    Host's content is invisible to the container.
//                                    Container's create/write/delete touches the shadow only.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

func main() {
	backing := flag.String("backing", "", "backing host directory (absolute)")
	backingFd := flag.Int("backing-fd", -1, "backing as open fd inherited from caller; resolves /proc/self/fd/N. Used when the path cannot be reached by name (e.g. about to be overmounted)")
	shadow := flag.String("shadow", "", "shadow store directory (absolute)")
	mountpoint := flag.String("mount", "", "mount point (absolute)")
	rulesPath := flag.String("rules", "", "path to .rp/shadow (optional)")
	debug := flag.Bool("debug", false, "enable FUSE debug logging")
	cacheSec := flag.Float64("cache", 1.0, "attr/entry cache TTL in seconds")
	ownerUid := flag.Int("uid", -1, "if set (with --gid), report this uid as the owner of every host-tree entry, overriding the real backing owner")
	ownerGid := flag.Int("gid", -1, "if set (with --uid), report this gid as the owner of every host-tree entry, overriding the real backing owner")
	flag.Parse()

	if *backing == "" && *backingFd < 0 {
		log.Fatal("one of --backing or --backing-fd is required")
	}
	if *backing != "" && *backingFd >= 0 {
		log.Fatal("--backing and --backing-fd are mutually exclusive")
	}
	if *shadow == "" || *mountpoint == "" {
		log.Fatal("--shadow and --mount are required")
	}
	if *backingFd >= 0 {
		// /proc/self/fd/N is a "magic symlink" — the kernel resolves it via
		// the inode the fd already opens, not via path. So even if the path
		// it was originally opened from (/workspace-real) gets overmounted
		// with tmpfs immediately after this process started, lookups under
		// /proc/self/fd/N still reach the original host content.
		resolved := fmt.Sprintf("/proc/self/fd/%d", *backingFd)
		*backing = resolved
	}

	rules, err := ParseRulesFile(*rulesPath)
	if err != nil {
		log.Fatalf("parse rules %s: %v", *rulesPath, err)
	}
	if err := os.MkdirAll(*shadow, 0o755); err != nil {
		log.Fatalf("mkdir shadow root: %v", err)
	}

	cfg := &Config{Rules: rules}
	if *ownerUid >= 0 && *ownerGid >= 0 {
		cfg.OwnerOverride = true
		cfg.OwnerUid = uint32(*ownerUid)
		cfg.OwnerGid = uint32(*ownerGid)
	} else if *ownerUid >= 0 || *ownerGid >= 0 {
		log.Fatal("--uid and --gid must be set together")
	}

	var bst, sst syscall.Stat_t
	if statErr := syscall.Stat(*backing, &bst); statErr != nil {
		log.Fatalf("stat backing: %v", statErr)
	}
	if statErr := syscall.Stat(*shadow, &sst); statErr != nil {
		log.Fatalf("stat shadow: %v", statErr)
	}

	shadowRoot := &fs.LoopbackRoot{
		Path: *shadow,
		Dev:  uint64(sst.Dev),
		NewNode: func(rd *fs.LoopbackRoot, parent *fs.Inode, name string, st *syscall.Stat_t) fs.InodeEmbedder {
			return &ShadowNode{LoopbackNode: fs.LoopbackNode{RootData: rd}}
		},
	}
	hostRoot := &fs.LoopbackRoot{
		Path: *backing,
		Dev:  uint64(bst.Dev),
		NewNode: func(rd *fs.LoopbackRoot, parent *fs.Inode, name string, st *syscall.Stat_t) fs.InodeEmbedder {
			return &HostNode{
				LoopbackNode: fs.LoopbackNode{RootData: rd},
				cfg:          cfg,
			}
		},
	}
	cfg.HostRoot = hostRoot
	cfg.ShadowRoot = shadowRoot

	root := &HostNode{
		LoopbackNode: fs.LoopbackNode{RootData: hostRoot},
		cfg:          cfg,
	}

	ttl := time.Duration(*cacheSec * float64(time.Second))
	opts := &fs.Options{
		AttrTimeout:     &ttl,
		EntryTimeout:    &ttl,
		NegativeTimeout: &ttl,
		MountOptions: fuse.MountOptions{
			Debug:         *debug,
			AllowOther:    true,
			FsName:        "rp-fuse",
			Name:          "rp-fuse",
			MaxBackground: 32,
			MaxWrite:      1 << 20,
			DisableXAttrs: true,
		},
	}

	server, mountErr := fs.Mount(*mountpoint, root, opts)
	if mountErr != nil {
		log.Fatalf("mount %s: %v", *mountpoint, mountErr)
	}
	pats := rules.Patterns()
	log.Printf("mounted host=%s shadow=%s mnt=%s patterns=%d", *backing, *shadow, *mountpoint, len(pats))
	for _, p := range pats {
		log.Printf("  pattern: %s", p)
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigs
		log.Print("signal received; unmounting")
		if err := server.Unmount(); err != nil {
			log.Printf("unmount: %v", err)
		}
	}()
	server.Wait()
	log.Print("server exited")
}
