// SPDX-License-Identifier: MIT

package reconcile

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/store"
)

// --- fake backend ---------------------------------------------------------------

// fakeApp is the in-memory host state ONE backend "app" carries. It
// mutates the way the real adapter's verbs change the host, so a second
// identical run diffs to zero — the idempotency test lives on fake
// semantics deliberately.
type fakeApp struct {
	deployed      bool
	running       bool
	image         string
	revision      string
	env           map[string]string
	secret        map[string]string
	ports         []domain.Port
	domains       []string
	networks      []string
	restartPol    domain.RestartPolicy
	mounts        map[string]fakeMount
	ensuredStor   map[string]string // entry name → hostDir
	lastBuild     domain.BuildStatus
	failEnsure    error
	failDeploy    error
	readinessHold int // Remaining polls the readiness gate keeps failing
}

type fakeMount struct {
	entryName string
}

// fakeBackend implements the reconcile.Backend seam over an in-memory
// map.
type fakeBackend struct {
	mu       sync.Mutex
	apps     map[string]*fakeApp
	sequence []string // mutating call order: "verb:app[:detail]"
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{apps: map[string]*fakeApp{}}
}

func (fb *fakeBackend) app(ref domain.ServiceRef) *fakeApp {
	name := mustAppName(ref)
	fb.mu.Lock()
	defer fb.mu.Unlock()
	a, ok := fb.apps[name]
	if !ok {
		a = &fakeApp{
			env:         map[string]string{},
			secret:      map[string]string{},
			mounts:      map[string]fakeMount{},
			ensuredStor: map[string]string{},
		}
		fb.apps[name] = a
	}
	return a
}

func (fb *fakeBackend) logf(format string, args ...any) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.sequence = append(fb.sequence, fmt.Sprintf(format, args...))
}

func (fb *fakeBackend) EnsureApp(ctx context.Context, ref domain.ServiceRef) error {
	a := fb.app(ref)
	if a.failEnsure != nil {
		return a.failEnsure
	}
	a.deployed = true
	fb.logf("ensure:%s", ref)
	return nil
}

func (fb *fakeBackend) Inspect(ctx context.Context, ref domain.ServiceRef) (domain.ObservedState, error) {
	if err := ctx.Err(); err != nil {
		return domain.ObservedState{}, err
	}
	a := fb.app(ref)
	o := domain.ObservedState{
		Ref:             ref,
		AppName:         mustAppName(ref),
		Deployed:        a.deployed,
		Running:         a.running,
		Image:           a.image,
		Revision:        a.revision,
		EnvKeys:         sortedKeys(a.env),
		SecretChecksums: map[string]string{},
		Ports:           append([]domain.Port(nil), a.ports...),
		Domains:         append([]string(nil), a.domains...),
		StorageMounts:   []domain.StorageMount{},
		RestartPolicy:   a.restartPol,
		Networks:        append([]string(nil), a.networks...),
		LastBuild:       a.lastBuild,
	}
	if a.readinessHold > 0 {
		a.readinessHold--
		o.Running = false
	}
	for p, m := range a.mounts {
		o.StorageMounts = append(o.StorageMounts, domain.StorageMount{
			Name: m.entryName, MountPath: p,
		})
	}
	for name, v := range a.secret {
		o.SecretChecksums[name] = "sum-" + v
	}
	sort.Slice(o.StorageMounts, func(i, j int) bool {
		return o.StorageMounts[i].MountPath < o.StorageMounts[j].MountPath
	})
	return o, nil
}

func (fb *fakeBackend) LastGoodRevision(ctx context.Context, ref domain.ServiceRef) (string, error) {
	a := fb.app(ref)
	if a.image != "" {
		return a.image, nil
	}
	return a.revision, nil
}

func (fb *fakeBackend) Stop(ctx context.Context, ref domain.ServiceRef) error {
	fb.app(ref).running = false
	fb.logf("stop:%s", ref)
	return nil
}

func (fb *fakeBackend) Destroy(ctx context.Context, ref domain.ServiceRef) error {
	name := mustAppName(ref)
	fb.mu.Lock()
	delete(fb.apps, name)
	fb.mu.Unlock()
	fb.logf("destroy:%s", ref)
	return nil
}

func (fb *fakeBackend) DeployImage(ctx context.Context, ref domain.ServiceRef, imageRef string) error {
	a := fb.app(ref)
	if a.failDeploy != nil {
		return a.failDeploy
	}
	a.deployed = true
	a.image = imageRef
	if a.readinessHold <= 0 {
		a.running = true
	}
	fb.logf("deploy:%s:%s", ref, imageRef)
	return nil
}

func (fb *fakeBackend) EnsureNetwork(ctx context.Context, name string) error {
	fb.logf("network:%s", name)
	return nil
}

func (fb *fakeBackend) EnsureStorageEntry(ctx context.Context, name, hostDir string) error {
	fb.mu.Lock()
	ensuredEntries[name] = hostDir
	fb.mu.Unlock()
	fb.logf("storage-entry:%s:%s", name, hostDir)
	return nil
}

// ensuredEntries records every ensured entry name → hostDir (the fake
// keeps the Dokku storage registry globally; entry names are unique).
var ensuredEntries = map[string]string{}

func (fb *fakeBackend) AttachStorage(ctx context.Context, ref domain.ServiceRef, name, containerPath string) error {
	fb.app(ref).mounts[containerPath] = fakeMount{entryName: name}
	fb.logf("storage-attach:%s:%s:%s", ref, name, containerPath)
	return nil
}

func (fb *fakeBackend) DetachStorage(ctx context.Context, ref domain.ServiceRef, name, containerPath string) error {
	delete(fb.app(ref).mounts, containerPath)
	fb.logf("storage-detach:%s:%s", ref, containerPath)
	return nil
}

func (fb *fakeBackend) SetConfigEnv(ctx context.Context, ref domain.ServiceRef, env map[string]string) error {
	a := fb.app(ref)
	for k, v := range env {
		a.env[k] = v
	}
	fb.logf("env:%s:%d", ref, len(env))
	return nil
}

func (fb *fakeBackend) SetSecretEnv(ctx context.Context, ref domain.ServiceRef, name, value string) error {
	fb.app(ref).secret[name] = value
	fb.logf("secret:%s:%s", ref, name)
	return nil
}

func (fb *fakeBackend) SetPorts(ctx context.Context, ref domain.ServiceRef, ports []domain.Port) error {
	fb.app(ref).ports = append([]domain.Port(nil), ports...)
	fb.logf("ports:%s:%d", ref, len(ports))
	return nil
}

func (fb *fakeBackend) SetDomains(ctx context.Context, ref domain.ServiceRef, domains []string) error {
	fb.app(ref).domains = append([]string(nil), domains...)
	fb.logf("domains:%s:%d", ref, len(domains))
	return nil
}

func (fb *fakeBackend) SetRestartPolicy(ctx context.Context, ref domain.ServiceRef, policy domain.RestartPolicy) error {
	fb.app(ref).restartPol = policy
	fb.logf("restart:%s", ref)
	return nil
}

func (fb *fakeBackend) AttachNetwork(ctx context.Context, ref domain.ServiceRef, network string) error {
	a := fb.app(ref)
	for _, n := range a.networks {
		if n == network {
			return nil
		}
	}
	a.networks = append(a.networks, network)
	fb.logf("network-attach:%s:%s", ref, network)
	return nil
}

// sequenceOf renders the recorded mutating call order.
func (fb *fakeBackend) sequenceOf() []string {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	return append([]string(nil), fb.sequence...)
}

func (fb *fakeBackend) destroyCalled(ref domain.ServiceRef) bool {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	for _, s := range fb.sequence {
		if strings.HasPrefix(s, "destroy:"+ref.String()) {
			return true
		}
	}
	return false
}

// stoppedWithout resets the fake's readiness hold (tests pre-seed state).
func (fb *fakeBackend) seed(ref domain.ServiceRef, shape func(*fakeApp)) *fakeApp {
	a := fb.app(ref)
	shape(a)
	return a
}

func mustAppName(ref domain.ServiceRef) string {
	app, err := domain.AppName(ref.Namespace, ref.Name)
	if err != nil {
		panic(err)
	}
	return app
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// --- fake store -----------------------------------------------------------------

// fakeStore implements exactly the store-seam methods the reconciler
// uses. It embeds the nil store.Store so ANY unexpected method explodes
// loudly — the reconciler must not discover new store appetite silently.
type fakeStore struct {
	store.Store

	mu           sync.Mutex
	ops          []*domain.Operation
	runs         []*domain.ReconciliationRun
	cps          []store.Checkpoint
	leaseHolders map[string]string
	leaseErr     error // canned AcquireLease error (lease test)
	leaseCalls   int
	released     bool
	ledger       map[string]*store.Resource
	now          time.Time
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		leaseHolders: map[string]string{},
		ledger:       map[string]*store.Resource{},
		now:          newControlledClock().Now(),
	}
}

func (fs *fakeStore) InsertOperation(ctx context.Context, op *domain.Operation) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.ops = append(fs.ops, op)
	return nil
}

func (fs *fakeStore) UpdateOperationState(ctx context.Context, id uuid.UUID, next domain.OperationState, redactedErr string, at time.Time) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	for _, op := range fs.ops {
		if op.ID == id {
			op.State = next
			op.Err = redactedErr
			op.FinishedAt = at
			return nil
		}
	}
	return fmt.Errorf("fake store: no operation %x", id)
}

func (fs *fakeStore) AcquireLease(ctx context.Context, name, holderID string, ttl time.Duration, renew bool) (bool, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.leaseCalls++
	if fs.leaseErr != nil {
		return false, fs.leaseErr
	}
	if cur, held := fs.leaseHolders[name]; held && cur != holderID {
		var le domain.LockedError
		return false, le
	}
	fs.leaseHolders[name] = holderID
	return true, nil
}

func (fs *fakeStore) ReleaseLease(ctx context.Context, name, holderID string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if cur, held := fs.leaseHolders[name]; held && cur != holderID {
		var le domain.LockedError
		return le
	}
	delete(fs.leaseHolders, name)
	fs.released = true
	return nil
}

func (fs *fakeStore) RecordReconcileRun(ctx context.Context, run *domain.ReconciliationRun, cp *store.Checkpoint) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.runs = append(fs.runs, run)
	if cp != nil {
		fs.cps = append(fs.cps, *cp)
	}
	return nil
}

func (fs *fakeStore) GetCheckpoint(ctx context.Context) (*store.Checkpoint, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.cps) == 0 {
		return &store.Checkpoint{}, nil
	}
	cp := fs.cps[len(fs.cps)-1]
	return &cp, nil
}

func (fs *fakeStore) ListLedger(ctx context.Context, kind string) ([]*store.Resource, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	var out []*store.Resource
	for _, e := range fs.ledger {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Identifier < out[j].Identifier })
	return out, nil
}

func (fs *fakeStore) InsertLedgerEntry(ctx context.Context, r *store.Resource) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	key := r.Kind + "|" + r.Identifier
	if _, dup := fs.ledger[key]; dup {
		return fmt.Errorf("%w: ledger %s duplicate", domain.ErrConflict, key)
	}
	fs.ledger[key] = r
	return nil
}

func (fs *fakeStore) RemoveLedgerEntry(ctx context.Context, kind, identifier string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	key := kind + "|" + identifier
	if _, ok := fs.ledger[key]; !ok {
		return fmt.Errorf("%w: ledger %s absent", domain.ErrNotFound, key)
	}
	delete(fs.ledger, key)
	return nil
}

func (fs *fakeStore) lastCheckpoint() store.Checkpoint {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.cps) == 0 {
		return store.Checkpoint{}
	}
	return fs.cps[len(fs.cps)-1]
}

// --- fake secrets ----------------------------------------------------------------

type fakeSecrets struct {
	values map[string]string
}

func (s fakeSecrets) Resolve(ctx context.Context, name string) (value, version string, err error) {
	v, ok := s.values[name]
	if !ok {
		return "", "", fmt.Errorf("%w: secret %s", domain.ErrNotFound, name)
	}
	return v, "sum-" + v, nil
}

// --- fake prune gate -------------------------------------------------------------

type fakePruneGate struct {
	allowed map[domain.ServiceRef]bool
	asked   []domain.ServiceRef
	err     error
}

func (g *fakePruneGate) CanDelete(ctx context.Context, ref domain.ServiceRef) (bool, string, error) {
	g.asked = append(g.asked, ref)
	if g.err != nil {
		return false, "", g.err
	}
	if g.allowed[ref] {
		return true, "", nil
	}
	return false, "refused-by-fake-gate", nil
}

// --- controlled clock ------------------------------------------------------------

// controlledClock has instant, deterministic pacing: Wait advances the
// logical time by d. A run's readiness gate sees minutes pass without a
// single real sleep — perfect under -race.
type controlledClock struct {
	mu  sync.Mutex
	now time.Time
}

func newControlledClock() *controlledClock {
	return &controlledClock{now: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)}
}

func (c *controlledClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *controlledClock) Wait(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
	return ctx.Err()
}

// compile-time seam conformance: the fakes must satisfy the reconciler's
// locally defined structural seams exactly.
var (
	_ Backend        = (*fakeBackend)(nil)
	_ StateStore     = (*fakeStore)(nil)
	_ Clock          = (*controlledClock)(nil)
	_ SecretResolver = fakeSecrets{}
	_ PruneGate      = (*fakePruneGate)(nil)
)
