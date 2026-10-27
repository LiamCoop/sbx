package zfs_test

// Integration tests against a real pool. They run only when SBX_TEST_ZFS_ROOT
// names a dataset the tests may own (created if missing; its parent must
// exist) and the test binary
// runs as root:
//
//	make test-zfs                       # SBX_TEST_ZFS_ROOT=tank/sbx-test
//
// Each run works under <root>/run-<timestamp>/<test name>/ with the same
// templates/ and sandboxes/ layout sbxd will use, and destroys it afterwards.

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/LiamCoop/sbx/internal/zfs"
)

const mib = 1 << 20

var (
	client  *zfs.Client
	runRoot string
)

func TestMain(m *testing.M) {
	root := os.Getenv("SBX_TEST_ZFS_ROOT")
	if root != "" && os.Geteuid() == 0 {
		var err error
		client, err = zfs.New(root)
		if err != nil {
			fmt.Fprintln(os.Stderr, "SBX_TEST_ZFS_ROOT:", err)
			os.Exit(2)
		}
		ctx := context.Background()
		if err := client.Create(ctx, root, nil); err != nil && !errors.Is(err, zfs.ErrExists) {
			fmt.Fprintln(os.Stderr, "create test root:", err)
			os.Exit(2)
		}
		runRoot = fmt.Sprintf("%s/run-%d", root, time.Now().UnixNano())
		if err := client.Create(ctx, runRoot, nil); err != nil {
			fmt.Fprintln(os.Stderr, "create run root:", err)
			os.Exit(2)
		}
	}
	code := m.Run()
	if client != nil {
		if err := destroyTree(context.Background(), runRoot); err != nil {
			fmt.Fprintf(os.Stderr, "cleanup failed, remove by hand with: zfs destroy -R %s\n%v\n", runRoot, err)
			if code == 0 {
				code = 1
			}
		}
	}
	os.Exit(code)
}

// env is one test's slice of the pool: a template dataset with a v1 snapshot
// and an empty sandboxes parent.
type env struct {
	t         *testing.T
	ctx       context.Context
	base      string
	templates string
	sandboxes string
	template  string // templates/app
	snap      string // templates/app@v1
}

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9_.:-]`)

func newEnv(t *testing.T) *env {
	t.Helper()
	if client == nil {
		t.Skip("set SBX_TEST_ZFS_ROOT and run as root (make test-zfs) to run ZFS integration tests")
	}
	t.Parallel()
	ctx := t.Context()
	base := runRoot + "/" + unsafeChars.ReplaceAllString(t.Name(), "_")
	e := &env{
		t: t, ctx: ctx, base: base,
		templates: base + "/templates",
		sandboxes: base + "/sandboxes",
		template:  base + "/templates/app",
		snap:      base + "/templates/app@v1",
	}
	for _, ds := range []string{e.base, e.templates, e.sandboxes, e.template} {
		must(t, client.Create(ctx, ds, nil))
	}
	t.Cleanup(func() {
		if err := destroyTree(context.Background(), e.base); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	// A stand-in for an unpacked root filesystem with a repo and its
	// dependencies installed.
	dir := e.mountpoint(e.template)
	e.writeFile(dir, "etc/os-release", []byte("ID=sbx-test\n"))
	e.writeFile(dir, "app/package.json", []byte(`{"name":"app"}`))
	e.writeFile(dir, "app/node_modules/dep/blob", randomBytes(t, 4*mib))
	must(t, client.Snapshot(ctx, e.snap, nil))
	return e
}

func (e *env) sandbox(id string) string { return e.sandboxes + "/" + id }

// clone creates sandbox id from the template snapshot and returns its dataset.
func (e *env) clone(id string, props map[string]string) string {
	e.t.Helper()
	ds := e.sandbox(id)
	must(e.t, client.Clone(e.ctx, e.snap, ds, props))
	return ds
}

func (e *env) mountpoint(ds string) string {
	e.t.Helper()
	mp, err := client.Mountpoint(e.ctx, ds)
	must(e.t, err)
	return mp
}

func (e *env) writeFile(dir, rel string, data []byte) {
	e.t.Helper()
	p := filepath.Join(dir, rel)
	must(e.t, os.MkdirAll(filepath.Dir(p), 0o755))
	f, err := os.Create(p)
	must(e.t, err)
	_, err = f.Write(data)
	must(e.t, err)
	must(e.t, f.Sync())
	must(e.t, f.Close())
}

func (e *env) readFile(dir, rel string) []byte {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, rel))
	must(e.t, err)
	return b
}

func (e *env) space(ds string) zfs.Space {
	e.t.Helper()
	must(e.t, client.SyncPool(e.ctx))
	s, err := client.Space(e.ctx, ds)
	must(e.t, err)
	return s
}

// destroyTree destroys ds and everything under it. Clones pin their origin
// snapshots, and forks are clones of sibling sandboxes, so it destroys
// children in passes, retrying the ones still pinned by a sibling, rather than
// reaching for -R (which would follow dependents anywhere in the pool).
func destroyTree(ctx context.Context, ds string) error {
	for {
		kids, err := client.List(ctx, ds, nil)
		if errors.Is(err, zfs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if len(kids) == 0 {
			break
		}
		progress := false
		var pinned error
		for _, k := range kids {
			err := destroyTree(ctx, k.Name)
			switch {
			case err == nil:
				progress = true
			case errors.Is(err, zfs.ErrHasDependents):
				pinned = err
			default:
				return err
			}
		}
		if !progress {
			return pinned
		}
	}
	err := client.Destroy(ctx, ds, true)
	if errors.Is(err, zfs.ErrNotExist) {
		return nil
	}
	return err
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func randomBytes(t testing.TB, n int) []byte {
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCloneHasTemplateContents(t *testing.T) {
	e := newEnv(t)
	tmpl := e.mountpoint(e.template)
	sb := e.mountpoint(e.clone("a", nil))

	for _, rel := range []string{"etc/os-release", "app/package.json", "app/node_modules/dep/blob"} {
		if !bytes.Equal(e.readFile(tmpl, rel), e.readFile(sb, rel)) {
			t.Errorf("%s differs between template and clone", rel)
		}
	}
}

func TestClonesAreIsolated(t *testing.T) {
	e := newEnv(t)
	tmpl := e.mountpoint(e.template)
	a := e.mountpoint(e.clone("a", nil))
	b := e.mountpoint(e.clone("b", nil))
	c := e.mountpoint(e.clone("c", nil))

	e.writeFile(a, "app/new.txt", []byte("from a"))
	e.writeFile(b, "app/package.json", []byte(`{"name":"changed-in-b"}`))
	must(t, os.RemoveAll(filepath.Join(c, "app/node_modules")))
	// Writes to the live template after the snapshot reach no clone.
	e.writeFile(tmpl, "etc/os-release", []byte("ID=changed-after-snapshot\n"))

	for name, dir := range map[string]string{"template": tmpl, "b": b, "c": c} {
		if _, err := os.Stat(filepath.Join(dir, "app/new.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a's new file is visible in %s", name)
		}
	}
	for name, dir := range map[string]string{"template": tmpl, "a": a, "c": c} {
		if got := string(e.readFile(dir, "app/package.json")); got != `{"name":"app"}` {
			t.Errorf("b's edit leaked into %s: %s", name, got)
		}
	}
	for name, dir := range map[string]string{"template": tmpl, "a": a, "b": b} {
		if _, err := os.Stat(filepath.Join(dir, "app/node_modules/dep/blob")); err != nil {
			t.Errorf("c's delete leaked into %s: %v", name, err)
		}
	}
	for name, dir := range map[string]string{"a": a, "b": b, "c": c} {
		if got := string(e.readFile(dir, "etc/os-release")); got != "ID=sbx-test\n" {
			t.Errorf("post-snapshot template write visible in %s: %q", name, got)
		}
	}
}

func TestCloneOriginAndDependents(t *testing.T) {
	e := newEnv(t)
	a := e.clone("a", nil)
	b := e.clone("b", nil)

	m, err := client.Get(e.ctx, a, "origin")
	must(t, err)
	if m["origin"] != e.snap {
		t.Errorf("origin = %q, want %q", m["origin"], e.snap)
	}

	m, err = client.Get(e.ctx, e.snap, "clones")
	must(t, err)
	got := strings.Split(m["clones"], ",")
	slices.Sort(got)
	if want := []string{a, b}; !slices.Equal(got, want) {
		t.Errorf("clones = %v, want %v", got, want)
	}
}

func TestCloneCostsOnlyItsWrites(t *testing.T) {
	e := newEnv(t)
	tmplRef := e.space(e.template).Referenced
	snapUsedBefore := e.space(e.snap).Used
	sb := e.clone("a", nil)

	fresh := e.space(sb)
	t.Logf("fresh clone: used=%d referenced=%d (template referenced=%d)", fresh.Used, fresh.Referenced, tmplRef)
	if fresh.Used > mib {
		t.Errorf("fresh clone uses %d bytes, want under 1 MiB", fresh.Used)
	}
	if fresh.Referenced < tmplRef-mib {
		t.Errorf("fresh clone references %d bytes, want about the template's %d", fresh.Referenced, tmplRef)
	}

	const n = 8 * mib
	e.writeFile(e.mountpoint(sb), "work/output.bin", randomBytes(t, n))
	after := e.space(sb)
	t.Logf("after writing %d bytes: used=%d written=%d", n, after.Used, after.Written)
	if after.Used < n || after.Used > n+n/10+mib {
		t.Errorf("clone uses %d bytes after writing %d, want about %d", after.Used, n, n)
	}
	if after.Written < n {
		t.Errorf("written = %d, want at least %d", after.Written, n)
	}
	if got := e.space(e.snap).Used; got != snapUsedBefore {
		t.Errorf("template snapshot used changed from %d to %d", snapUsedBefore, got)
	}
}

func TestParallelClones(t *testing.T) {
	e := newEnv(t)
	const n = 5
	var wg sync.WaitGroup
	errs := make([]error, n)
	took := make([]time.Duration, n)
	for i := range n {
		wg.Go(func() {
			start := time.Now()
			errs[i] = client.Clone(e.ctx, e.snap, e.sandbox(fmt.Sprint(i)), nil)
			took[i] = time.Since(start)
		})
	}
	wg.Wait()
	t.Logf("%d parallel clones took %v", n, took)
	must(t, errors.Join(errs...))

	ds, err := client.List(e.ctx, e.sandboxes, nil, "origin")
	must(t, err)
	if len(ds) != n {
		t.Fatalf("listed %d sandboxes, want %d", len(ds), n)
	}
	// Each writes its own marker and sees only its own.
	for i := range n {
		e.writeFile(e.mountpoint(e.sandbox(fmt.Sprint(i))), "marker", []byte(fmt.Sprint(i)))
	}
	for i := range n {
		if got := string(e.readFile(e.mountpoint(e.sandbox(fmt.Sprint(i))), "marker")); got != fmt.Sprint(i) {
			t.Errorf("sandbox %d sees marker %q", i, got)
		}
	}
}

func TestSnapshotPinnedByClones(t *testing.T) {
	e := newEnv(t)
	a := e.clone("a", nil)

	if err := client.Destroy(e.ctx, e.snap, false); !errors.Is(err, zfs.ErrHasDependents) {
		t.Fatalf("destroying a snapshot with clones: err = %v, want ErrHasDependents", err)
	}
	if err := client.Destroy(e.ctx, e.template, true); !errors.Is(err, zfs.ErrHasDependents) {
		t.Fatalf("destroying a template with clones: err = %v, want ErrHasDependents", err)
	}
	must(t, client.Destroy(e.ctx, a, false))
	must(t, client.Destroy(e.ctx, e.snap, false))
	if ok, err := client.Exists(e.ctx, e.snap); ok || err != nil {
		t.Errorf("Exists(snapshot) = %v, %v after destroy", ok, err)
	}
}

func TestPromoteFreesTemplate(t *testing.T) {
	e := newEnv(t)
	a := e.clone("a", nil)
	dir := e.mountpoint(a)
	e.writeFile(dir, "kept.txt", []byte("keep me"))

	must(t, client.Promote(e.ctx, a))
	// The snapshot moved to the promoted clone; the old template is now the
	// dependent one and can go.
	if ok, _ := client.Exists(e.ctx, a+"@v1"); !ok {
		t.Errorf("%s@v1 missing after promote", a)
	}
	must(t, client.Destroy(e.ctx, e.template, true))

	if got := string(e.readFile(dir, "kept.txt")); got != "keep me" {
		t.Errorf("kept.txt = %q after promote and template destroy", got)
	}
	if got := string(e.readFile(dir, "app/package.json")); got != `{"name":"app"}` {
		t.Errorf("template data lost after promote: %q", got)
	}
}

func TestForkOfSandbox(t *testing.T) {
	e := newEnv(t)
	parent := e.clone("parent", nil)
	e.writeFile(e.mountpoint(parent), "progress.txt", []byte("halfway"))

	fork := parent + "@fork1"
	must(t, client.Snapshot(e.ctx, fork, nil))
	var kids []string
	for i := range 3 {
		kid := e.sandbox(fmt.Sprintf("kid%d", i))
		must(t, client.Clone(e.ctx, fork, kid, map[string]string{"sbx:parent": "parent"}))
		kids = append(kids, kid)
	}
	e.writeFile(e.mountpoint(parent), "progress.txt", []byte("parent moved on"))

	for _, kid := range kids {
		dir := e.mountpoint(kid)
		if got := string(e.readFile(dir, "progress.txt")); got != "halfway" {
			t.Errorf("%s progress.txt = %q, want the state at fork time", kid, got)
		}
		if got := string(e.readFile(dir, "etc/os-release")); got != "ID=sbx-test\n" {
			t.Errorf("%s lost template data: %q", kid, got)
		}
		m, err := client.Get(e.ctx, kid, "origin", "sbx:parent")
		must(t, err)
		if m["origin"] != fork || m["sbx:parent"] != "parent" {
			t.Errorf("%s props = %v", kid, m)
		}
	}
	// Forks pin the parent's snapshot just as sandboxes pin the template.
	if err := client.Destroy(e.ctx, parent, true); !errors.Is(err, zfs.ErrHasDependents) {
		t.Errorf("destroying a forked parent: err = %v, want ErrHasDependents", err)
	}
}

// ZFS is the source of truth: a host must be able to rebuild its view of
// every sandbox from a listing alone.
func TestStateRebuildsFromList(t *testing.T) {
	e := newEnv(t)
	guid, err := client.GUID(e.ctx, e.snap)
	must(t, err)

	created := time.Now().UTC().Format(time.RFC3339)
	want := map[string]map[string]string{}
	for _, id := range []string{"01a", "01b", "01c"} {
		props := map[string]string{
			"sbx:template":      "app",
			"sbx:template-guid": guid,
			"sbx:created":       created,
		}
		if id == "01b" {
			props["sbx:ttl"] = "3600"
		}
		e.clone(id, props)
		want[e.sandbox(id)] = props
	}

	ds, err := client.List(e.ctx, e.sandboxes, nil,
		"origin", "sbx:template", "sbx:template-guid", "sbx:created", "sbx:ttl")
	must(t, err)
	if len(ds) != len(want) {
		t.Fatalf("listed %d sandboxes, want %d", len(ds), len(want))
	}
	for _, d := range ds {
		w, ok := want[d.Name]
		if !ok {
			t.Errorf("unexpected dataset %s", d.Name)
			continue
		}
		if d.Props["origin"] != e.snap {
			t.Errorf("%s origin = %q", d.Name, d.Props["origin"])
		}
		for k, v := range w {
			if d.Props[k] != v {
				t.Errorf("%s %s = %q, want %q", d.Name, k, d.Props[k], v)
			}
		}
		if _, has := d.Props["sbx:ttl"]; has != (w["sbx:ttl"] != "") {
			t.Errorf("%s sbx:ttl presence = %v, want %v", d.Name, has, !has)
		}
	}
}

func TestSnapshotGUIDs(t *testing.T) {
	e := newEnv(t)
	g1, err := client.GUID(e.ctx, e.snap)
	must(t, err)
	again, err := client.GUID(e.ctx, e.snap)
	must(t, err)
	if g1 == "" || g1 != again {
		t.Errorf("guid not stable: %q then %q", g1, again)
	}

	v2 := e.template + "@v2"
	must(t, client.Snapshot(e.ctx, v2, nil))
	g2, err := client.GUID(e.ctx, v2)
	must(t, err)
	if g2 == g1 {
		t.Errorf("v1 and v2 share guid %s", g1)
	}
}

func TestRefquotaCapsSandbox(t *testing.T) {
	e := newEnv(t)
	tmplRef := e.space(e.template).Referenced
	// refquota counts everything the clone references, template data included.
	quota := tmplRef + 16*mib
	sb := e.clone("a", map[string]string{"refquota": fmt.Sprint(quota)})

	f, err := os.Create(filepath.Join(e.mountpoint(sb), "fill.bin"))
	must(t, err)
	defer f.Close()
	chunk := randomBytes(t, mib)
	var written int
	for written < 64*mib {
		if _, err = f.Write(chunk); err == nil {
			err = f.Sync()
		}
		if err != nil {
			break
		}
		written += len(chunk)
	}
	if !errors.Is(err, syscall.EDQUOT) {
		t.Fatalf("after writing %d MiB: err = %v, want EDQUOT", written/mib, err)
	}
	t.Logf("quota hit after %d MiB of a 16 MiB allowance", written/mib)
	if written > 24*mib {
		t.Errorf("wrote %d MiB before EDQUOT, want about 16", written/mib)
	}
}

func TestErrors(t *testing.T) {
	e := newEnv(t)
	if err := client.Clone(e.ctx, e.template+"@nope", e.sandbox("x"), nil); !errors.Is(err, zfs.ErrNotExist) {
		t.Errorf("clone of missing snapshot: err = %v, want ErrNotExist", err)
	}
	e.clone("a", nil)
	if err := client.Clone(e.ctx, e.snap, e.sandbox("a"), nil); !errors.Is(err, zfs.ErrExists) {
		t.Errorf("duplicate clone: err = %v, want ErrExists", err)
	}
	if err := client.Create(e.ctx, e.template, nil); !errors.Is(err, zfs.ErrExists) {
		t.Errorf("duplicate create: err = %v, want ErrExists", err)
	}
	if err := client.Clone(e.ctx, e.snap, e.base+"/no-such-parent/x", nil); !errors.Is(err, zfs.ErrNotExist) {
		t.Errorf("clone under a missing parent: err = %v, want ErrNotExist", err)
	}
	if err := client.Destroy(e.ctx, e.sandbox("missing"), false); !errors.Is(err, zfs.ErrNotExist) {
		t.Errorf("destroy missing: err = %v, want ErrNotExist", err)
	}
	if ok, err := client.Exists(e.ctx, e.sandbox("missing")); ok || err != nil {
		t.Errorf("Exists(missing) = %v, %v", ok, err)
	}
}

// BenchmarkClone measures a clone plus its destroy, the two ZFS steps of a
// sandbox's life.
func BenchmarkClone(b *testing.B) {
	if client == nil {
		b.Skip("set SBX_TEST_ZFS_ROOT and run as root (make bench-zfs)")
	}
	ctx := b.Context()
	base := runRoot + "/bench-clone"
	snap := base + "/templates/app@v1"
	for _, ds := range []string{base, base + "/templates", base + "/templates/app", base + "/sandboxes"} {
		must(b, client.Create(ctx, ds, nil))
	}
	must(b, client.Snapshot(ctx, snap, nil))
	b.Cleanup(func() { _ = destroyTree(context.Background(), base) })

	var cloneTime, destroyTime time.Duration
	i := 0
	for b.Loop() {
		ds := fmt.Sprintf("%s/sandboxes/%d", base, i)
		i++
		start := time.Now()
		must(b, client.Clone(ctx, snap, ds, nil))
		mid := time.Now()
		must(b, client.Destroy(ctx, ds, false))
		cloneTime += mid.Sub(start)
		destroyTime += time.Since(mid)
	}
	b.ReportMetric(float64(cloneTime.Microseconds())/float64(b.N), "clone-µs/op")
	b.ReportMetric(float64(destroyTime.Microseconds())/float64(b.N), "destroy-µs/op")
}
