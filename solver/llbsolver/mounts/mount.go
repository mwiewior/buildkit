package mounts

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/sys"
	"github.com/docker/docker/pkg/idtools"
	"github.com/docker/docker/pkg/locker"
	"github.com/moby/buildkit/cache"
	"github.com/moby/buildkit/cache/metadata"
	"github.com/moby/buildkit/client"
	"github.com/moby/buildkit/identity"
	"github.com/moby/buildkit/session"
	"github.com/moby/buildkit/session/secrets"
	"github.com/moby/buildkit/session/sshforward"
	"github.com/moby/buildkit/snapshot"
	"github.com/moby/buildkit/solver/pb"
	"github.com/moby/buildkit/util/grpcerrors"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	bolt "go.etcd.io/bbolt"
	"google.golang.org/grpc/codes"
)

func NewMountManager(name string, cm cache.Manager, sm *session.Manager, md *metadata.Store) *MountManager {
	return &MountManager{
		cm:          cm,
		sm:          sm,
		cacheMounts: map[string]*cacheRefShare{},
		md:          md,
		managerName: name,
	}
}

type MountManager struct {
	cm            cache.Manager
	sm            *session.Manager
	cacheMountsMu sync.Mutex
	cacheMounts   map[string]*cacheRefShare
	md            *metadata.Store
	managerName   string
}

func (mm *MountManager) getRefCacheDir(ctx context.Context, ref cache.ImmutableRef, id string, m *pb.Mount, sharing pb.CacheSharingOpt) (mref cache.MutableRef, err error) {
	g := &cacheRefGetter{
		locker:          &mm.cacheMountsMu,
		cacheMounts:     mm.cacheMounts,
		cm:              mm.cm,
		md:              mm.md,
		globalCacheRefs: sharedCacheRefs,
		name:            fmt.Sprintf("cached mount %s from %s", m.Dest, mm.managerName),
	}
	return g.getRefCacheDir(ctx, ref, id, sharing)
}

type cacheRefGetter struct {
	locker          sync.Locker
	cacheMounts     map[string]*cacheRefShare
	cm              cache.Manager
	md              *metadata.Store
	globalCacheRefs *cacheRefs
	name            string
}

func (g *cacheRefGetter) getRefCacheDir(ctx context.Context, ref cache.ImmutableRef, id string, sharing pb.CacheSharingOpt) (mref cache.MutableRef, err error) {
	key := "cache-dir:" + id
	if ref != nil {
		key += ":" + ref.ID()
	}
	mu := g.locker
	mu.Lock()
	defer mu.Unlock()

	if ref, ok := g.cacheMounts[key]; ok {
		return ref.clone(), nil
	}
	defer func() {
		if err == nil {
			share := &cacheRefShare{MutableRef: mref, refs: map[*cacheRef]struct{}{}}
			g.cacheMounts[key] = share
			mref = share.clone()
		}
	}()

	switch sharing {
	case pb.CacheSharingOpt_SHARED:
		return g.globalCacheRefs.get(key, func() (cache.MutableRef, error) {
			return g.getRefCacheDirNoCache(ctx, key, ref, id, false)
		})
	case pb.CacheSharingOpt_PRIVATE:
		return g.getRefCacheDirNoCache(ctx, key, ref, id, false)
	case pb.CacheSharingOpt_LOCKED:
		return g.getRefCacheDirNoCache(ctx, key, ref, id, true)
	default:
		return nil, errors.Errorf("invalid cache sharing option: %s", sharing.String())
	}
}

func (g *cacheRefGetter) getRefCacheDirNoCache(ctx context.Context, key string, ref cache.ImmutableRef, id string, block bool) (cache.MutableRef, error) {
	makeMutable := func(ref cache.ImmutableRef) (cache.MutableRef, error) {
		return g.cm.New(ctx, ref, cache.WithRecordType(client.UsageRecordTypeCacheMount), cache.WithDescription(g.name), cache.CachePolicyRetain)
	}

	cacheRefsLocker.Lock(key)
	defer cacheRefsLocker.Unlock(key)
	for {
		sis, err := g.md.Search(key)
		if err != nil {
			return nil, err
		}
		locked := false
		for _, si := range sis {
			if mRef, err := g.cm.GetMutable(ctx, si.ID()); err == nil {
				logrus.Debugf("reusing ref for cache dir: %s", mRef.ID())
				return mRef, nil
			} else if errors.Is(err, cache.ErrLocked) {
				locked = true
			}
		}
		if block && locked {
			cacheRefsLocker.Unlock(key)
			select {
			case <-ctx.Done():
				cacheRefsLocker.Lock(key)
				return nil, ctx.Err()
			case <-time.After(100 * time.Millisecond):
				cacheRefsLocker.Lock(key)
			}
		} else {
			break
		}
	}
	mRef, err := makeMutable(ref)
	if err != nil {
		return nil, err
	}

	si, _ := g.md.Get(mRef.ID())
	v, err := metadata.NewValue(key)
	if err != nil {
		mRef.Release(context.TODO())
		return nil, err
	}
	v.Index = key
	if err := si.Update(func(b *bolt.Bucket) error {
		return si.SetValue(b, key, v)
	}); err != nil {
		mRef.Release(context.TODO())
		return nil, err
	}
	return mRef, nil
}

func (mm *MountManager) getSSHMountable(ctx context.Context, m *pb.Mount, g session.Group) (cache.Mountable, error) {
	var caller session.Caller
	err := mm.sm.Any(ctx, g, func(ctx context.Context, _ string, c session.Caller) error {
		if err := sshforward.CheckSSHID(ctx, c, m.SSHOpt.ID); err != nil {
			if m.SSHOpt.Optional {
				return nil
			}
			if grpcerrors.Code(err) == codes.Unimplemented {
				return errors.Errorf("no SSH key %q forwarded from the client", m.SSHOpt.ID)
			}
			return err
		}
		caller = c
		return nil
	})
	if err != nil {
		return nil, err
	}
	// because ssh socket remains active, to actually handle session disconnecting ssh error
	// should restart the whole exec with new session
	return &sshMount{mount: m, caller: caller, idmap: mm.cm.IdentityMapping()}, nil
}

type sshMount struct {
	mount  *pb.Mount
	caller session.Caller
	idmap  *idtools.IdentityMapping
}

func (sm *sshMount) Mount(ctx context.Context, readonly bool) (snapshot.Mountable, error) {
	return &sshMountInstance{sm: sm, idmap: sm.idmap}, nil
}

type sshMountInstance struct {
	sm    *sshMount
	idmap *idtools.IdentityMapping
}

func (sm *sshMountInstance) Mount() ([]mount.Mount, func() error, error) {
	ctx, cancel := context.WithCancel(context.TODO())

	uid := int(sm.sm.mount.SSHOpt.Uid)
	gid := int(sm.sm.mount.SSHOpt.Gid)

	if sm.idmap != nil {
		identity, err := sm.idmap.ToHost(idtools.Identity{
			UID: uid,
			GID: gid,
		})
		if err != nil {
			cancel()
			return nil, nil, err
		}
		uid = identity.UID
		gid = identity.GID
	}

	sock, cleanup, err := sshforward.MountSSHSocket(ctx, sm.sm.caller, sshforward.SocketOpt{
		ID:   sm.sm.mount.SSHOpt.ID,
		UID:  uid,
		GID:  gid,
		Mode: int(sm.sm.mount.SSHOpt.Mode & 0777),
	})
	if err != nil {
		cancel()
		return nil, nil, err
	}
	release := func() error {
		var err error
		if cleanup != nil {
			err = cleanup()
		}
		cancel()
		return err
	}

	return []mount.Mount{{
		Type:    "bind",
		Source:  sock,
		Options: []string{"rbind"},
	}}, release, nil
}

func (sm *sshMountInstance) IdentityMapping() *idtools.IdentityMapping {
	return sm.idmap
}

func (mm *MountManager) getSecretMountable(ctx context.Context, m *pb.Mount, g session.Group) (cache.Mountable, error) {
	if m.SecretOpt == nil {
		return nil, errors.Errorf("invalid secret mount options")
	}
	sopt := *m.SecretOpt

	id := sopt.ID
	if id == "" {
		return nil, errors.Errorf("secret ID missing from mount options")
	}
	var dt []byte
	var err error
	err = mm.sm.Any(ctx, g, func(ctx context.Context, _ string, caller session.Caller) error {
		dt, err = secrets.GetSecret(ctx, caller, id)
		if err != nil {
			if errors.Is(err, secrets.ErrNotFound) && m.SecretOpt.Optional {
				return nil
			}
			return err
		}
		return nil
	})
	if err != nil || dt == nil {
		return nil, err
	}
	return &secretMount{mount: m, data: dt, idmap: mm.cm.IdentityMapping()}, nil
}

type secretMount struct {
	mount *pb.Mount
	data  []byte
	idmap *idtools.IdentityMapping
}

func (sm *secretMount) Mount(ctx context.Context, readonly bool) (snapshot.Mountable, error) {
	return &secretMountInstance{sm: sm, idmap: sm.idmap}, nil
}

type secretMountInstance struct {
	sm    *secretMount
	root  string
	idmap *idtools.IdentityMapping
}

func (sm *secretMountInstance) Mount() ([]mount.Mount, func() error, error) {
	dir, err := ioutil.TempDir("", "buildkit-secrets")
	if err != nil {
		return nil, nil, errors.Wrap(err, "failed to create temp dir")
	}
	cleanupDir := func() error {
		return os.RemoveAll(dir)
	}

	if err := os.Chmod(dir, 0711); err != nil {
		cleanupDir()
		return nil, nil, err
	}

	tmpMount := mount.Mount{
		Type:    "tmpfs",
		Source:  "tmpfs",
		Options: []string{"nodev", "nosuid", "noexec", fmt.Sprintf("uid=%d,gid=%d", os.Geteuid(), os.Getegid())},
	}

	if sys.RunningInUserNS() {
		tmpMount.Options = nil
	}

	if err := mount.All([]mount.Mount{tmpMount}, dir); err != nil {
		cleanupDir()
		return nil, nil, errors.Wrap(err, "unable to setup secret mount")
	}
	sm.root = dir

	cleanup := func() error {
		if err := mount.Unmount(dir, 0); err != nil {
			return err
		}
		return cleanupDir()
	}

	randID := identity.NewID()
	fp := filepath.Join(dir, randID)
	if err := ioutil.WriteFile(fp, sm.sm.data, 0600); err != nil {
		cleanup()
		return nil, nil, err
	}

	uid := int(sm.sm.mount.SecretOpt.Uid)
	gid := int(sm.sm.mount.SecretOpt.Gid)

	if sm.idmap != nil {
		identity, err := sm.idmap.ToHost(idtools.Identity{
			UID: uid,
			GID: gid,
		})
		if err != nil {
			cleanup()
			return nil, nil, err
		}
		uid = identity.UID
		gid = identity.GID
	}

	if err := os.Chown(fp, uid, gid); err != nil {
		cleanup()
		return nil, nil, err
	}

	if err := os.Chmod(fp, os.FileMode(sm.sm.mount.SecretOpt.Mode&0777)); err != nil {
		cleanup()
		return nil, nil, err
	}

	return []mount.Mount{{
		Type:    "bind",
		Source:  fp,
		Options: []string{"ro", "rbind", "nodev", "nosuid", "noexec"},
	}}, cleanup, nil
}

func (sm *secretMountInstance) IdentityMapping() *idtools.IdentityMapping {
	return sm.idmap
}

func (mm *MountManager) MountableCache(ctx context.Context, m *pb.Mount, ref cache.ImmutableRef) (cache.MutableRef, error) {
	if m.CacheOpt == nil {
		return nil, errors.Errorf("missing cache mount options")
	}
	return mm.getRefCacheDir(ctx, ref, m.CacheOpt.ID, m, m.CacheOpt.Sharing)
}

func (mm *MountManager) MountableTmpFS() cache.Mountable {
	return newTmpfs(mm.cm.IdentityMapping())
}

func (mm *MountManager) MountableSecret(ctx context.Context, m *pb.Mount, g session.Group) (cache.Mountable, error) {
	return mm.getSecretMountable(ctx, m, g)
}

func (mm *MountManager) MountableSSH(ctx context.Context, m *pb.Mount, g session.Group) (cache.Mountable, error) {
	return mm.getSSHMountable(ctx, m, g)
}

func newTmpfs(idmap *idtools.IdentityMapping) cache.Mountable {
	return &tmpfs{idmap: idmap}
}

type tmpfs struct {
	idmap *idtools.IdentityMapping
}

func (f *tmpfs) Mount(ctx context.Context, readonly bool) (snapshot.Mountable, error) {
	return &tmpfsMount{readonly: readonly, idmap: f.idmap}, nil
}

type tmpfsMount struct {
	readonly bool
	idmap    *idtools.IdentityMapping
}

func (m *tmpfsMount) Mount() ([]mount.Mount, func() error, error) {
	opt := []string{"nosuid"}
	if m.readonly {
		opt = append(opt, "ro")
	}
	return []mount.Mount{{
		Type:    "tmpfs",
		Source:  "tmpfs",
		Options: opt,
	}}, func() error { return nil }, nil
}

func (m *tmpfsMount) IdentityMapping() *idtools.IdentityMapping {
	return m.idmap
}

var cacheRefsLocker = locker.New()
var sharedCacheRefs = &cacheRefs{}

type cacheRefs struct {
	mu     sync.Mutex
	shares map[string]*cacheRefShare
}

// ClearActiveCacheMounts clears shared cache mounts currently in use.
// Caller needs to hold CacheMountsLocker before calling
func ClearActiveCacheMounts() {
	sharedCacheRefs.shares = nil
}

func CacheMountsLocker() sync.Locker {
	return &sharedCacheRefs.mu
}

func (r *cacheRefs) get(key string, fn func() (cache.MutableRef, error)) (cache.MutableRef, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.shares == nil {
		r.shares = map[string]*cacheRefShare{}
	}

	share, ok := r.shares[key]
	if ok {
		return share.clone(), nil
	}

	mref, err := fn()
	if err != nil {
		return nil, err
	}

	share = &cacheRefShare{MutableRef: mref, main: r, key: key, refs: map[*cacheRef]struct{}{}}
	r.shares[key] = share
	return share.clone(), nil
}

type cacheRefShare struct {
	cache.MutableRef
	mu   sync.Mutex
	refs map[*cacheRef]struct{}
	main *cacheRefs
	key  string
}

func (r *cacheRefShare) clone() cache.MutableRef {
	cacheRef := &cacheRef{cacheRefShare: r}
	if cacheRefCloneHijack != nil {
		cacheRefCloneHijack()
	}
	r.mu.Lock()
	r.refs[cacheRef] = struct{}{}
	r.mu.Unlock()
	return cacheRef
}

func (r *cacheRefShare) release(ctx context.Context) error {
	if r.main != nil {
		delete(r.main.shares, r.key)
	}
	return r.MutableRef.Release(ctx)
}

var cacheRefReleaseHijack func()
var cacheRefCloneHijack func()

type cacheRef struct {
	*cacheRefShare
}

func (r *cacheRef) Release(ctx context.Context) error {
	if r.main != nil {
		r.main.mu.Lock()
		defer r.main.mu.Unlock()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.refs, r)
	if len(r.refs) == 0 {
		if cacheRefReleaseHijack != nil {
			cacheRefReleaseHijack()
		}
		return r.release(ctx)
	}
	return nil
}
