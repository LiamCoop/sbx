// Package zfs is a thin wrapper over the zfs(8) command line for the handful
// of operations sbx needs: create, snapshot, clone, destroy, and reading and
// writing properties.
//
// It shells out rather than binding libzfs so it stays cgo-free and behaves
// exactly like the commands you'd type by hand. Every mutating call is
// confined to a root dataset, so a bug can never touch datasets sbx doesn't
// own (the daemon runs as root on a pool that may hold other data).
package zfs

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Sentinel errors, matched with errors.Is against the *Error returned by
// a failed command.
var (
	ErrNotExist      = errors.New("dataset does not exist")
	ErrExists        = errors.New("dataset already exists")
	ErrHasDependents = errors.New("dataset has dependent clones")
	ErrHasChildren   = errors.New("dataset has children")
	ErrBusy          = errors.New("dataset is busy")
	ErrPermission    = errors.New("permission denied")
	ErrOutsideRoot   = errors.New("dataset is outside the client's root")
)

// Error is a failed zfs command.
type Error struct {
	Args   []string
	Stderr string
	kind   error
	err    error
}

func (e *Error) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = e.err.Error()
	}
	return fmt.Sprintf("zfs %s: %s", strings.Join(e.Args, " "), msg)
}

func (e *Error) Is(target error) bool { return e.kind != nil && e.kind == target }
func (e *Error) Unwrap() error        { return e.err }

// classify maps zfs's stderr to a sentinel. The messages are stable across
// OpenZFS 2.x and are the only signal the CLI gives.
func classify(stderr string) error {
	switch {
	case strings.Contains(stderr, "dataset does not exist"),
		strings.Contains(stderr, "parent does not exist"):
		return ErrNotExist
	case strings.Contains(stderr, "dataset already exists"):
		return ErrExists
	case strings.Contains(stderr, "has dependent clones"):
		return ErrHasDependents
	case strings.Contains(stderr, "has children"):
		return ErrHasChildren
	case strings.Contains(stderr, "is busy"):
		return ErrBusy
	case strings.Contains(stderr, "permission denied"):
		return ErrPermission
	}
	return nil
}

// Runner executes a command and returns its stdout and stderr. Tests swap it
// out to check argument construction and output parsing without a pool.
type Runner func(ctx context.Context, name string, args ...string) (stdout, stderr []byte, err error)

func execRunner(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

// Client runs zfs commands confined to Root.
type Client struct {
	// Root is the dataset every mutating call must be at or under,
	// e.g. "tank/sbx". Required.
	Root string

	run Runner
}

// New returns a Client confined to root.
func New(root string) (*Client, error) {
	if err := ValidateName(root); err != nil {
		return nil, err
	}
	if strings.Contains(root, "@") {
		return nil, fmt.Errorf("root %q must be a filesystem, not a snapshot", root)
	}
	return &Client{Root: root, run: execRunner}, nil
}

// WithRunner returns a copy of c that executes commands with r.
func (c *Client) WithRunner(r Runner) *Client {
	cp := *c
	cp.run = r
	return &cp
}

var (
	componentRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]*$`)
	userPropRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*:[a-z0-9_.:-]+$`)
)

// ValidateName checks that name is a filesystem or snapshot name made of
// conservative characters. It is stricter than ZFS itself so names can never
// be mistaken for flags or carry whitespace into a command line.
func ValidateName(name string) error {
	fs, snap, hasSnap := strings.Cut(name, "@")
	if hasSnap && !componentRe.MatchString(snap) {
		return fmt.Errorf("invalid snapshot name %q", name)
	}
	if fs == "" {
		return fmt.Errorf("invalid dataset name %q", name)
	}
	for part := range strings.SplitSeq(fs, "/") {
		if !componentRe.MatchString(part) {
			return fmt.Errorf("invalid dataset name %q", name)
		}
	}
	return nil
}

func validateProps(props map[string]string) error {
	for k, v := range props {
		if strings.ContainsAny(k, "= \t\n") || k == "" || strings.HasPrefix(k, "-") {
			return fmt.Errorf("invalid property name %q", k)
		}
		if strings.ContainsAny(v, "\n\t") {
			return fmt.Errorf("invalid value for property %q", k)
		}
	}
	return nil
}

// IsUserProperty reports whether name is a ZFS user property (module:name).
func IsUserProperty(name string) bool { return userPropRe.MatchString(name) }

// inRoot reports whether name (filesystem or snapshot) is c.Root or below it.
func (c *Client) inRoot(name string) bool {
	fs, _, _ := strings.Cut(name, "@")
	return fs == c.Root || strings.HasPrefix(fs, c.Root+"/")
}

func (c *Client) guard(names ...string) error {
	for _, n := range names {
		if err := ValidateName(n); err != nil {
			return err
		}
		if !c.inRoot(n) {
			return fmt.Errorf("%w: %q not under %q", ErrOutsideRoot, n, c.Root)
		}
	}
	return nil
}

func (c *Client) zfs(ctx context.Context, args ...string) ([]byte, error) {
	return c.cmd(ctx, "zfs", args...)
}

func (c *Client) cmd(ctx context.Context, name string, args ...string) ([]byte, error) {
	stdout, stderr, err := c.run(ctx, name, args...)
	if err != nil {
		return nil, &Error{Args: args, Stderr: string(stderr), kind: classify(string(stderr)), err: err}
	}
	return stdout, nil
}

// propArgs renders props as sorted "-o k=v" pairs so commands are
// deterministic.
func propArgs(props map[string]string) []string {
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	args := make([]string, 0, 2*len(keys))
	for _, k := range keys {
		args = append(args, "-o", k+"="+props[k])
	}
	return args
}

// Create creates a filesystem with props set. Its parent must exist, and it
// fails with ErrExists if name already does. (zfs create -p is avoided because
// it reports success when the target exists, silently ignoring props.)
func (c *Client) Create(ctx context.Context, name string, props map[string]string) error {
	if err := c.guard(name); err != nil {
		return err
	}
	if strings.Contains(name, "@") {
		return fmt.Errorf("create %q: use Snapshot for snapshots", name)
	}
	if err := validateProps(props); err != nil {
		return err
	}
	args := append([]string{"create"}, propArgs(props)...)
	_, err := c.zfs(ctx, append(args, name)...)
	return err
}

// Snapshot takes snapshot snap ("fs@name") with props set.
func (c *Client) Snapshot(ctx context.Context, snap string, props map[string]string) error {
	if err := c.guard(snap); err != nil {
		return err
	}
	if !strings.Contains(snap, "@") {
		return fmt.Errorf("snapshot %q: name must contain '@'", snap)
	}
	if err := validateProps(props); err != nil {
		return err
	}
	args := append([]string{"snapshot"}, propArgs(props)...)
	_, err := c.zfs(ctx, append(args, snap)...)
	return err
}

// Clone creates filesystem target as a clone of snap with props set. Its
// parent must exist, and it fails with ErrExists if target already does. (zfs
// clone -p is avoided because it reports success when the target exists, which
// would hand a caller someone else's sandbox.)
func (c *Client) Clone(ctx context.Context, snap, target string, props map[string]string) error {
	if err := c.guard(snap, target); err != nil {
		return err
	}
	if !strings.Contains(snap, "@") {
		return fmt.Errorf("clone source %q must be a snapshot", snap)
	}
	if strings.Contains(target, "@") {
		return fmt.Errorf("clone target %q must be a filesystem", target)
	}
	if err := validateProps(props); err != nil {
		return err
	}
	args := append([]string{"clone"}, propArgs(props)...)
	_, err := c.zfs(ctx, append(args, snap, target)...)
	return err
}

// Destroy destroys a filesystem or snapshot. With recursive, descendants
// (children and snapshots) go too. It never passes -R: destroying dependent
// clones elsewhere in the pool must be an explicit, separate decision.
func (c *Client) Destroy(ctx context.Context, name string, recursive bool) error {
	if err := c.guard(name); err != nil {
		return err
	}
	if name == c.Root {
		return fmt.Errorf("refusing to destroy root %q", name)
	}
	args := []string{"destroy"}
	if recursive {
		args = append(args, "-r")
	}
	_, err := c.zfs(ctx, append(args, name)...)
	return err
}

// Promote makes clone fs independent of its origin snapshot, reversing the
// dependency so the origin can be destroyed.
func (c *Client) Promote(ctx context.Context, fs string) error {
	if err := c.guard(fs); err != nil {
		return err
	}
	_, err := c.zfs(ctx, "promote", fs)
	return err
}

// Exists reports whether a filesystem or snapshot exists.
func (c *Client) Exists(ctx context.Context, name string) (bool, error) {
	if err := ValidateName(name); err != nil {
		return false, err
	}
	_, err := c.zfs(ctx, "list", "-H", "-o", "name", "-t", "filesystem,snapshot,volume", name)
	if errors.Is(err, ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// Set sets properties on a dataset.
func (c *Client) Set(ctx context.Context, name string, props map[string]string) error {
	if err := c.guard(name); err != nil {
		return err
	}
	if len(props) == 0 {
		return nil
	}
	if err := validateProps(props); err != nil {
		return err
	}
	args := []string{"set"}
	for i, a := range propArgs(props) {
		if i%2 == 1 { // skip the "-o" of each pair
			args = append(args, a)
		}
	}
	_, err := c.zfs(ctx, append(args, name)...)
	return err
}

// Get returns the requested properties of a dataset as parsable (exact)
// values. User properties that are not set are omitted from the result.
func (c *Client) Get(ctx context.Context, name string, props ...string) (map[string]string, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	if len(props) == 0 {
		return nil, errors.New("get: no properties requested")
	}
	out, err := c.zfs(ctx, "get", "-H", "-p", "-o", "property,value,source", strings.Join(props, ","), name)
	if err != nil {
		return nil, err
	}
	return parseGet(out)
}

func parseGet(out []byte) (map[string]string, error) {
	res := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Split(sc.Text(), "\t")
		if len(f) != 3 {
			return nil, fmt.Errorf("unexpected zfs get line %q", sc.Text())
		}
		if IsUserProperty(f[0]) && f[2] == "-" {
			continue // unset user property
		}
		res[f[0]] = f[1]
	}
	return res, sc.Err()
}

// Dataset is one row of a listing.
type Dataset struct {
	Name  string
	Props map[string]string
}

// List returns parent's direct children of the given types ("filesystem",
// "snapshot") with the requested properties. Unset user properties are
// omitted from Props. parent itself is not included.
func (c *Client) List(ctx context.Context, parent string, types []string, props ...string) ([]Dataset, error) {
	if err := ValidateName(parent); err != nil {
		return nil, err
	}
	if len(types) == 0 {
		types = []string{"filesystem"}
	}
	cols := append([]string{"name"}, props...)
	out, err := c.zfs(ctx, "list", "-H", "-p", "-r", "-d", "1",
		"-t", strings.Join(types, ","), "-o", strings.Join(cols, ","), parent)
	if err != nil {
		return nil, err
	}
	return parseList(out, parent, props)
}

func parseList(out []byte, parent string, props []string) ([]Dataset, error) {
	var res []Dataset
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Split(sc.Text(), "\t")
		if len(f) != len(props)+1 {
			return nil, fmt.Errorf("unexpected zfs list line %q", sc.Text())
		}
		if f[0] == parent {
			continue
		}
		d := Dataset{Name: f[0], Props: map[string]string{}}
		for i, p := range props {
			v := f[i+1]
			if IsUserProperty(p) && v == "-" {
				continue
			}
			d.Props[p] = v
		}
		res = append(res, d)
	}
	return res, sc.Err()
}

// Space is a dataset's space accounting, in bytes.
type Space struct {
	Used       uint64 // space consumed by the dataset and its descendants
	Referenced uint64 // data reachable from the dataset (shared with origin)
	Written    uint64 // data written since the previous snapshot (or origin, for a clone)
	Logical    uint64 // logicalreferenced: referenced before compression
}

// Space returns name's space accounting. Accounting reflects the last
// committed transaction group; call SyncPool first for up-to-date numbers.
func (c *Client) Space(ctx context.Context, name string) (Space, error) {
	m, err := c.Get(ctx, name, "used", "referenced", "written", "logicalreferenced")
	if err != nil {
		return Space{}, err
	}
	var s Space
	for prop, dst := range map[string]*uint64{
		"used": &s.Used, "referenced": &s.Referenced, "written": &s.Written, "logicalreferenced": &s.Logical,
	} {
		v, err := strconv.ParseUint(m[prop], 10, 64)
		if err != nil {
			return Space{}, fmt.Errorf("parse %s=%q: %w", prop, m[prop], err)
		}
		*dst = v
	}
	return s, nil
}

// GUID returns a dataset's or snapshot's guid: the identity that tells two
// hosts they hold the exact same snapshot.
func (c *Client) GUID(ctx context.Context, name string) (string, error) {
	m, err := c.Get(ctx, name, "guid")
	if err != nil {
		return "", err
	}
	return m["guid"], nil
}

// Mountpoint returns where a filesystem is mounted.
func (c *Client) Mountpoint(ctx context.Context, fs string) (string, error) {
	m, err := c.Get(ctx, fs, "mountpoint", "mounted")
	if err != nil {
		return "", err
	}
	if m["mounted"] != "yes" {
		return "", fmt.Errorf("%s is not mounted", fs)
	}
	return m["mountpoint"], nil
}

// SyncPool forces the pool holding Root to commit its pending transaction
// group so space accounting is current.
func (c *Client) SyncPool(ctx context.Context) error {
	pool, _, _ := strings.Cut(c.Root, "/")
	_, err := c.cmd(ctx, "zpool", "sync", pool)
	return err
}
