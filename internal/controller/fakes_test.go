// SPDX-License-Identifier: MIT

package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/toledompm/top/internal/domain"
	"github.com/toledompm/top/internal/gitsource"
	"github.com/toledompm/top/internal/reconcile"
	"github.com/toledompm/top/internal/store"
)

// --- fake GitSource -------------------------------------------------------------

// fakeSource implements GitSource (method-for-method the *gitsource.Source
// surface): shas[i] answers the i-th ResolveLatest call (last repeats);
// FetchRevision materializes a tiny spec tree into a temp dir. Snapshot
// dirs are registered for cleanup (the real gitsource owns close
// semantics; here the test always cleans up explicitly).
type fakeSource struct {
	mu       sync.Mutex
	shas     []string
	calls    int
	fetches  []string
	fetchErr error
	dirs     []string
}

func (s *fakeSource) ResolveLatest(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.calls
	s.calls++
	if i >= len(s.shas) {
		i = len(s.shas) - 1
	}
	if i < 0 {
		return "", fmt.Errorf("fakeSource: no sha canned")
	}
	if s.shas[i] == "" { // "" = canned resolve outage
		return "", fmt.Errorf("fakeSource: ls-remote failed")
	}
	return s.shas[i], nil
}

func (s *fakeSource) FetchRevision(_ context.Context, sha string) (*gitsource.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fetches = append(s.fetches, sha)
	if s.fetchErr != nil {
		return nil, s.fetchErr
	}
	dir, err := os.MkdirTemp("", "top-fake-snap-*")
	if err != nil {
		return nil, err
	}
	s.dirs = append(s.dirs, dir)
	specYaml := `apiVersion: top/v1alpha1
kind: Service
metadata:
  name: demo
  namespace: default
spec:
  source:
    image: registry.example.com/demo/app
    tag: "1.2.3"
`
	if err := os.WriteFile(filepath.Join(dir, "demo.yaml"), []byte(specYaml), 0o600); err != nil {
		return nil, err
	}
	// A zero-literal Snapshot carries no release hook (Close is a benign
	// typed error in runTrigger); the test cleans the tree via cleanup.
	return &gitsource.Snapshot{SHA: sha, Dir: dir}, nil
}

// cleanup removes every snapshot tree the fake materialized.
func (s *fakeSource) cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.dirs {
		_ = os.RemoveAll(d)
	}
	s.dirs = nil
}

// --- fake store -------------------------------------------------------------------

// fakeStore implements exactly the store methods the daemon + reconciler
// use. The embedded nil store.Store makes any unexpected method panic
// loudly — silent store-appetite growth is discovered, never absorbed
// (same discipline as internal/reconcile's own fakes).
type fakeStore struct {
	store.Store

	mu           sync.Mutex
	ops          []*domain.Operation
	runs         []*domain.ReconciliationRun
	cps          []store.Checkpoint
	leaseHolders map[string]string
	leaseErr     error // canned AcquireLease error (lease-conflict path)
	deliveries   map[string]bool
	ledger       map[string]*store.Resource
}

func newFakeStore() *fakeStore {
	return &fakeStore{leaseHolders: map[string]string{}, deliveries: map[string]bool{}, ledger: map[string]*store.Resource{}}
}

func (fs *fakeStore) AcquireLease(_ context.Context, name, holderID string, _ time.Duration, _ bool) (bool, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if fs.leaseErr != nil {
		return false, fs.leaseErr
	}
	if cur, held := fs.leaseHolders[name]; held && cur != holderID {
		return false, domain.ErrLocked
	}
	fs.leaseHolders[name] = holderID
	return true, nil
}

func (fs *fakeStore) ReleaseLease(_ context.Context, name, holderID string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if cur, held := fs.leaseHolders[name]; held && cur != holderID {
		return domain.ErrLocked
	}
	delete(fs.leaseHolders, name)
	return nil
}

func (fs *fakeStore) InsertOperation(_ context.Context, op *domain.Operation) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.ops = append(fs.ops, op)
	return nil
}

func (fs *fakeStore) UpdateOperationState(_ context.Context, id uuid.UUID, next domain.OperationState, redactedErr string, at time.Time) error {
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

func (fs *fakeStore) RecordReconcileRun(_ context.Context, run *domain.ReconciliationRun, cp *store.Checkpoint) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.runs = append(fs.runs, run)
	if cp != nil {
		fs.cps = append(fs.cps, *cp)
	}
	return nil
}

func (fs *fakeStore) GetCheckpoint(context.Context) (*store.Checkpoint, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.cps) == 0 {
		return &store.Checkpoint{}, nil
	}
	cp := fs.cps[len(fs.cps)-1]
	return &cp, nil
}

func (fs *fakeStore) RecordDelivery(_ context.Context, deliveryID string, _ time.Time) (bool, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	first := !fs.deliveries[deliveryID]
	fs.deliveries[deliveryID] = true
	return first, nil
}

func (fs *fakeStore) InsertLedgerEntry(_ context.Context, r *store.Resource) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	key := r.Kind + "|" + r.Identifier
	if _, dup := fs.ledger[key]; dup {
		return fmt.Errorf("%w: fake ledger duplicate %s", domain.ErrConflict, key)
	}
	fs.ledger[key] = r
	return nil
}

func (fs *fakeStore) ListLedger(context.Context, string) ([]*store.Resource, error) {
	// The fake ledger is read only for orphan scans; created entries are
	// kept in fs.ledger via InsertLedgerEntry (unused by these runs).
	return nil, nil
}

func (fs *fakeStore) runsOf() []*domain.ReconciliationRun {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return append([]*domain.ReconciliationRun(nil), fs.runs...)
}

func (fs *fakeStore) lastCheckpoint() store.Checkpoint {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.cps) == 0 {
		return store.Checkpoint{}
	}
	return fs.cps[len(fs.cps)-1]
}

func (fs *fakeStore) leaseHolder(name string) string {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.leaseHolders[name]
}

var _ reconcile.StateStore = (*fakeStore)(nil)

// --- fake backend -------------------------------------------------------------------

// fakeBackend implements the reconcile.Backend seam over an in-memory
// host map: EnsureApp creates, DeployImage deploys+starts, Inspect
// mirrors the state (ErrNotFound while never created — the reconciler's
// "absence is create" path).
type fakeBackend struct {
	mu         sync.Mutex
	apps       map[string]*fakeApp
	failDeploy error
	// blockCh, when non-nil, makes DeployImage block (shutdown-drain
	// tests hold the reconcile "busy" until they release it).
	blockCh chan struct{}
}

type fakeApp struct {
	deployed bool
	running  bool
	image    string
}

func newFakeBackend() *fakeBackend { return &fakeBackend{apps: map[string]*fakeApp{}} }

func mustAppName(ref domain.ServiceRef) string {
	app, err := domain.AppName(ref.Namespace, ref.Name)
	if err != nil {
		panic(err)
	}
	return app
}

func (fb *fakeBackend) get(app string) *fakeApp {
	a, ok := fb.apps[app]
	if !ok {
		a = &fakeApp{}
		fb.apps[app] = a
	}
	return a
}

func (fb *fakeBackend) EnsureApp(_ context.Context, ref domain.ServiceRef) error {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	fb.get(mustAppName(ref)).deployed = true
	return nil
}

func (fb *fakeBackend) Inspect(_ context.Context, ref domain.ServiceRef) (domain.ObservedState, error) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	app := mustAppName(ref)
	a, ok := fb.apps[app]
	if !ok {
		return domain.ObservedState{}, fmt.Errorf("%w: fake app %s absent", domain.ErrNotFound, app)
	}
	o := domain.ObservedState{
		Ref: ref, AppName: app,
		Deployed:      a.deployed,
		Running:       a.running,
		Image:         a.image,
		EnvKeys:       []string{},
		Ports:         []domain.Port{},
		Domains:       []string{},
		Networks:      []string{},
		StorageMounts: []domain.StorageMount{},
		LastBuild:     domain.BuildSucceeded,
	}
	return o, nil
}

func (fb *fakeBackend) DeployImage(ctx context.Context, ref domain.ServiceRef, imageRef string) error {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if fb.failDeploy != nil {
		return fb.failDeploy
	}
	if fb.blockCh != nil {
		ch := fb.blockCh
		fb.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			fb.mu.Lock()
			return ctx.Err()
		}
		fb.mu.Lock()
	}
	a := fb.get(mustAppName(ref))
	a.image = imageRef
	a.running = true
	return nil
}

// The remaining convergence verbs are faithful no-ops: this fake's runs
// converge on plain image services and never cross these families.
func (fb *fakeBackend) LastGoodRevision(context.Context, domain.ServiceRef) (string, error) {
	return "", fmt.Errorf("%w: fake last-good unset", domain.ErrNotFound)
}
func (fb *fakeBackend) Stop(context.Context, domain.ServiceRef) error            { return nil }
func (fb *fakeBackend) Destroy(context.Context, domain.ServiceRef) error         { return nil }
func (fb *fakeBackend) EnsureNetwork(context.Context, string) error              { return nil }
func (fb *fakeBackend) EnsureStorageEntry(context.Context, string, string) error { return nil }
func (fb *fakeBackend) AttachStorage(context.Context, domain.ServiceRef, string, string) error {
	return nil
}

func (fb *fakeBackend) DetachStorage(context.Context, domain.ServiceRef, string, string) error {
	return nil
}

func (fb *fakeBackend) SetConfigEnv(context.Context, domain.ServiceRef, map[string]string) error {
	return nil
}

func (fb *fakeBackend) SetSecretEnv(context.Context, domain.ServiceRef, string, string) error {
	return nil
}
func (fb *fakeBackend) SetPorts(context.Context, domain.ServiceRef, []domain.Port) error { return nil }
func (fb *fakeBackend) SetDomains(context.Context, domain.ServiceRef, []string) error    { return nil }
func (fb *fakeBackend) SetRestartPolicy(context.Context, domain.ServiceRef, domain.RestartPolicy) error {
	return nil
}
func (fb *fakeBackend) AttachNetwork(context.Context, domain.ServiceRef, string) error { return nil }

// --- daemon fabric ------------------------------------------------------------------

// newTestDaemon wires a daemon over the fakes. The HTTP server is off
// (Addr == "") unless the caller sets it; cleanup owns the snapshot dirs.
func newTestDaemon(t *testing.T, src *fakeSource, st *fakeStore, be *fakeBackend, o Options) *Daemon {
	t.Helper()
	o.Reconciler = mustReconciler(t, be, st)
	o.Store = st
	o.Source = src
	if o.Branch == "" {
		o.Branch = "main"
	}
	t.Cleanup(src.cleanup)
	d, err := New(o)
	if err != nil {
		t.Fatalf("controller.New: %v", err)
	}
	return d
}

// mustReconciler builds the real reconciler over fakes (the daemon must
// own the REAL Run pipeline; only its seams are faked).
func mustReconciler(t *testing.T, be *fakeBackend, st *fakeStore) *reconcile.Reconciler {
	t.Helper()
	r, err := reconcile.New(reconcile.Options{Backend: be, Store: st})
	if err != nil {
		t.Fatalf("reconcile.New: %v", err)
	}
	return r
}
