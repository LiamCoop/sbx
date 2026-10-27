package zfs

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// fake records the commands it is asked to run and replays canned output.
type fake struct {
	calls  [][]string
	stdout string
	stderr string
	err    error
}

func (f *fake) run(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	return []byte(f.stdout), []byte(f.stderr), f.err
}

func newFake(t *testing.T) (*Client, *fake) {
	t.Helper()
	c, err := New("tank/sbx")
	if err != nil {
		t.Fatal(err)
	}
	f := &fake{}
	return c.WithRunner(f.run), f
}

func TestValidateName(t *testing.T) {
	good := []string{"tank", "tank/sbx", "tank/sbx/templates/app@v1", "tank/a-b_c.d:e"}
	bad := []string{"", "-tank", "tank/-o", "tank//x", "tank/", "tank/x@", "tank/x@-v", "tank/a b", "tank/x\n", "/tank"}
	for _, n := range good {
		if err := ValidateName(n); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", n, err)
		}
	}
	for _, n := range bad {
		if err := ValidateName(n); err == nil {
			t.Errorf("ValidateName(%q) = nil, want error", n)
		}
	}
}

func TestRootGuard(t *testing.T) {
	c, f := newFake(t)
	ctx := context.Background()

	outside := []func() error{
		func() error { return c.Create(ctx, "tank/mssql-main/x", nil) },
		func() error { return c.Create(ctx, "tank/sbxother", nil) }, // prefix, not a child
		func() error { return c.Snapshot(ctx, "tank@x", nil) },
		func() error { return c.Clone(ctx, "tank/mssql-main@s", "tank/sbx/x", nil) },
		func() error { return c.Clone(ctx, "tank/sbx/t@v1", "tank/x", nil) },
		func() error { return c.Destroy(ctx, "tank/mssql-main", true) },
		func() error { return c.Set(ctx, "tank", map[string]string{"sbx:x": "1"}) },
		func() error { return c.Promote(ctx, "tank/mssql-branch1") },
	}
	for i, fn := range outside {
		if err := fn(); !errors.Is(err, ErrOutsideRoot) {
			t.Errorf("case %d: err = %v, want ErrOutsideRoot", i, err)
		}
	}
	if err := c.Destroy(ctx, "tank/sbx", true); err == nil {
		t.Error("Destroy(root) succeeded, want refusal")
	}
	if len(f.calls) != 0 {
		t.Errorf("guarded calls reached zfs: %v", f.calls)
	}
}

func TestCommandConstruction(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		do   func(*Client) error
		want []string
	}{
		{"create", func(c *Client) error {
			return c.Create(ctx, "tank/sbx/templates/app", map[string]string{"sbx:template": "app", "compression": "lz4"})
		}, []string{"zfs", "create", "-o", "compression=lz4", "-o", "sbx:template=app", "tank/sbx/templates/app"}},
		{"snapshot", func(c *Client) error {
			return c.Snapshot(ctx, "tank/sbx/templates/app@v1", nil)
		}, []string{"zfs", "snapshot", "tank/sbx/templates/app@v1"}},
		{"clone", func(c *Client) error {
			return c.Clone(ctx, "tank/sbx/templates/app@v1", "tank/sbx/sandboxes/01J", map[string]string{"refquota": "10G"})
		}, []string{"zfs", "clone", "-o", "refquota=10G", "tank/sbx/templates/app@v1", "tank/sbx/sandboxes/01J"}},
		{"destroy", func(c *Client) error {
			return c.Destroy(ctx, "tank/sbx/sandboxes/01J", false)
		}, []string{"zfs", "destroy", "tank/sbx/sandboxes/01J"}},
		{"destroy -r", func(c *Client) error {
			return c.Destroy(ctx, "tank/sbx/sandboxes/01J", true)
		}, []string{"zfs", "destroy", "-r", "tank/sbx/sandboxes/01J"}},
		{"set", func(c *Client) error {
			return c.Set(ctx, "tank/sbx/sandboxes/01J", map[string]string{"sbx:ttl": "3600", "sbx:parent": "x"})
		}, []string{"zfs", "set", "sbx:parent=x", "sbx:ttl=3600", "tank/sbx/sandboxes/01J"}},
		{"sync", func(c *Client) error {
			return c.SyncPool(ctx)
		}, []string{"zpool", "sync", "tank"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, f := newFake(t)
			if err := tt.do(c); err != nil {
				t.Fatal(err)
			}
			if len(f.calls) != 1 || !reflect.DeepEqual(f.calls[0], tt.want) {
				t.Errorf("ran %q, want %q", f.calls, tt.want)
			}
		})
	}
}

func TestRejectsBadArguments(t *testing.T) {
	c, f := newFake(t)
	ctx := context.Background()
	cases := []error{
		c.Create(ctx, "tank/sbx/x@v1", nil),
		c.Snapshot(ctx, "tank/sbx/x", nil),
		c.Clone(ctx, "tank/sbx/x", "tank/sbx/y", nil),
		c.Clone(ctx, "tank/sbx/x@v1", "tank/sbx/y@v1", nil),
		c.Create(ctx, "tank/sbx/x", map[string]string{"bad=key": "v"}),
		c.Create(ctx, "tank/sbx/x", map[string]string{"-o": "v"}),
		c.Create(ctx, "tank/sbx/x", map[string]string{"sbx:x": "multi\nline"}),
	}
	for i, err := range cases {
		if err == nil {
			t.Errorf("case %d: want error", i)
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("bad arguments reached zfs: %v", f.calls)
	}
}

func TestErrorClassification(t *testing.T) {
	tests := []struct {
		stderr string
		want   error
	}{
		{"cannot open 'tank/sbx/nope': dataset does not exist\n", ErrNotExist},
		{"cannot create 'tank/sbx/nope/x': parent does not exist\n", ErrNotExist},
		{"cannot create 'tank/sbx/x': dataset already exists\n", ErrExists},
		{"cannot destroy 'tank/sbx/t@v1': snapshot has dependent clones\nuse '-R' to destroy the following datasets:\ntank/sbx/s/1\n", ErrHasDependents},
		{"cannot destroy 'tank/sbx/t': filesystem has children\nuse '-r' to destroy the following datasets:\n", ErrHasChildren},
		{"cannot destroy 'tank/sbx/s': dataset is busy\n", ErrBusy},
		{"cannot create 'tank/sbx/x': permission denied\n", ErrPermission},
	}
	for _, tt := range tests {
		c, f := newFake(t)
		f.stderr, f.err = tt.stderr, errors.New("exit status 1")
		err := c.Destroy(context.Background(), "tank/sbx/x", false)
		if !errors.Is(err, tt.want) {
			t.Errorf("stderr %q: err = %v, want %v", tt.stderr, err, tt.want)
		}
		if !strings.Contains(err.Error(), strings.TrimSpace(strings.SplitN(tt.stderr, "\n", 2)[0])) {
			t.Errorf("error %q does not include zfs's message", err)
		}
	}
}

func TestExists(t *testing.T) {
	c, f := newFake(t)
	f.stderr, f.err = "cannot open 'tank/sbx/x': dataset does not exist\n", errors.New("exit status 1")
	ok, err := c.Exists(context.Background(), "tank/sbx/x")
	if ok || err != nil {
		t.Errorf("Exists = %v, %v; want false, nil", ok, err)
	}

	c, f = newFake(t)
	f.stdout = "tank/sbx/x\n"
	ok, err = c.Exists(context.Background(), "tank/sbx/x")
	if !ok || err != nil {
		t.Errorf("Exists = %v, %v; want true, nil", ok, err)
	}
}

func TestParseGet(t *testing.T) {
	out := "guid\t8068395070731787220\t-\n" +
		"sbx:template\tapp\tlocal\n" +
		"sbx:ttl\t-\t-\n" +
		"origin\t-\t-\n"
	got, err := parseGet([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"guid": "8068395070731787220", "sbx:template": "app", "origin": "-"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseGet = %v, want %v", got, want)
	}
}

func TestParseList(t *testing.T) {
	out := "tank/sbx/sandboxes\t-\t-\n" +
		"tank/sbx/sandboxes/a\ttank/sbx/templates/app@v1\tapp\n" +
		"tank/sbx/sandboxes/b\ttank/sbx/templates/app@v1\t-\n"
	got, err := parseList([]byte(out), "tank/sbx/sandboxes", []string{"origin", "sbx:template"})
	if err != nil {
		t.Fatal(err)
	}
	want := []Dataset{
		{Name: "tank/sbx/sandboxes/a", Props: map[string]string{"origin": "tank/sbx/templates/app@v1", "sbx:template": "app"}},
		{Name: "tank/sbx/sandboxes/b", Props: map[string]string{"origin": "tank/sbx/templates/app@v1"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseList = %+v, want %+v", got, want)
	}
}

func TestSpaceParsing(t *testing.T) {
	c, f := newFake(t)
	f.stdout = "used\t1048576\t-\nreferenced\t2097152\t-\nwritten\t4096\t-\nlogicalreferenced\t3145728\t-\n"
	s, err := c.Space(context.Background(), "tank/sbx/x")
	if err != nil {
		t.Fatal(err)
	}
	want := Space{Used: 1 << 20, Referenced: 2 << 20, Written: 4096, Logical: 3 << 20}
	if s != want {
		t.Errorf("Space = %+v, want %+v", s, want)
	}
}
