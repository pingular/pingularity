package logfilter

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"unicode"
)

// Every attribute the daemon logs is walked here and held to the rule the two
// lists implement: a value we do not control - free-form text, or an identifier
// the mask hides under another key - reaches the masked form only through
// piiKeys or errKeys. The walk reads what the compiler knows: the key's name,
// the value's type, and the names in the value's expression. A key or value
// named for an error, or typed as one, must be scrubbed. What no rule can read
// is whether a string built somewhere else is free-form - "detail" is one - so
// every string-valued key is classified: masked, scrubbed, or listed in
// plainKeys with the reason its value is the daemon's own. A new key fails
// this test until it is placed, which is the point.
func TestEveryLogKeyIsClassified(t *testing.T) {
	root, modPath := moduleRoot(t)
	sites := walkLogSites(t, root, modPath)
	if len(sites) < 200 {
		t.Fatalf("walked only %d attribute sites; the walk is not seeing the tree", len(sites))
	}
	seenPlain := map[string]bool{}
	var failures []string
	for _, s := range sites {
		if !s.freeForm {
			continue
		}
		errText := s.errText()
		switch {
		case piiKeys[s.key]:
			// Replaced whole: safe whatever it carries.
		case errText && errKeys[s.key]:
		case errText:
			failures = append(failures, fmt.Sprintf("%s: %q carries error text (%s) and is not in errKeys", s.pos, s.key, s.value))
		case errKeys[s.key]:
		case plainKeys[s.key] != "":
			seenPlain[s.key] = true
		default:
			why := "a string the walk cannot vouch for"
			if w := s.identWord(); w != "" {
				why = "named like an identifier (" + w + ")"
			}
			failures = append(failures, fmt.Sprintf("%s: %q = %s is %s; put it in piiKeys if it names a host, address, person or place, errKeys if it carries error text, or plainKeys with the reason it is neither", s.pos, s.key, s.value, why))
		}
	}
	for k := range plainKeys {
		if !seenPlain[k] {
			failures = append(failures, fmt.Sprintf("plainKeys lists %q but no log site uses it as a string; drop the entry", k))
		}
	}
	sort.Strings(failures)
	for _, f := range failures {
		t.Error(f)
	}
}

// plainKeys are the string-valued keys whose values the daemon controls - a word
// from a closed set, a count formatted as text, a name of its own - each with the
// reason, because the walk has no other way to tell a controlled string from a
// free-form one. An entry is held to its use: a key that stops being logged
// as a string must leave the list.
var plainKeys = map[string]string{
	"access":             "the -access mode word: local or network",
	"addr":               "the daemon's own bind address, from -listen; loopback unless the operator opened it",
	"listen":             "the daemon's own bind address, from -listen; loopback unless the operator opened it",
	"db":                 "the -db path, the operator's own flag",
	"centre":             "Origin.Kind, one of five fixed words; the place name beside it is the label, which is masked",
	"winner_city":        "Origin.Kind, one of five fixed words; the place name beside it is winner_label, which is masked",
	"dest":               "a webhook class word (discord, slack, ntfy, generic)",
	"event":              "a notification's event name",
	"failures":           "threshold verdict sentences built from the run's own figures",
	"families":           "per-family ok/total counts",
	"family":             "ipv4 or ipv6",
	"head_id":            "Ookla's catalogue number; it names a server only through the catalogue, and the label beside it is what the mask hides",
	"incumbent_id":       "Ookla's catalogue number; it names a server only through the catalogue, and the label beside it is what the mask hides",
	"server_id":          "Ookla's catalogue number; it names a server only through the catalogue, and the label beside it is what the mask hides",
	"head_reason":        "a win reason, a fixed word",
	"reason":             "a fixed word: a win reason, a dial-failure class, a scheduler's reason",
	"ipv4_mode":          "a mode word: auto, on or off",
	"ipv6_mode":          "a mode word: auto, on or off",
	"kind":               "an event kind word",
	"latest":             "the newest release version the update check found",
	"level":              "the log level word",
	"likely_cause":       "a fixed diagnostic sentence with no identifier in it",
	"method":             "the HTTP method",
	"os":                 "runtime.GOOS",
	"panic":              "a crash's own value; the raw form is what a crash report needs, and it is not error text about the network",
	"recovered":          "a crash's own value; the raw form is what a crash report needs, and it is not error text about the network",
	"stack":              "the crashing goroutine's stack: this program's own function names and files",
	"path":               "the request's route, capped; the client and host on the same line have keys of their own",
	"requested":          "the congestion-control name the operator asked iperf3 for",
	"speed_engine":       "ookla or iperf3",
	"table":              "a database table name",
	"target":             "the probe anchor's built-in name (google-v4 and friends); the exit trace's hostname logs under exit_target",
	"type":               "the deleted history's kind: latency, speed or downtime",
	"version":            "this build's version",
	"worker":             "a goroutine's name",
	"idle_ms":            "a bufferbloat figure, or nil when unmeasured",
	"loaded_down_ms":     "a bufferbloat figure, or nil when unmeasured",
	"loaded_down_p95_ms": "a bufferbloat figure, or nil when unmeasured",
	"loaded_up_ms":       "a bufferbloat figure, or nil when unmeasured",
	"loaded_up_p95_ms":   "a bufferbloat figure, or nil when unmeasured",
}

// errWords mark a key or a value's name as carrying error text; identWords mark
// one as carrying an identifier. Both are matched against the words of a name,
// so clientIP, AllowedHosts and errByID all count.
var (
	errWords   = map[string]bool{"err": true, "error": true, "errs": true, "errors": true}
	identWords = map[string]bool{
		"ip": true, "ipv4": true, "ipv6": true, "addr": true, "address": true,
		"host": true, "hosts": true, "hostname": true, "url": true, "domain": true,
		"dns": true, "resolver": true, "isp": true, "asn": true,
		"user": true, "username": true, "email": true, "peer": true,
		"server": true, "router": true, "gateway": true, "hop": true, "handoff": true,
		"exit": true, "target": true, "colo": true, "city": true, "label": true, "mac": true,
	}
)

// logSite is one key/value pair at one logging call.
type logSite struct {
	pos      string
	key      string
	value    string // the value expression, as written
	freeForm bool   // a string, an interface, a slice of strings, or a type the walk could not resolve
	isError  bool   // the value's static type is an error
	names    []string
}

func (s logSite) errText() bool {
	if s.isError {
		return true
	}
	kw := words(s.key)
	if len(kw) > 0 && errWords[kw[len(kw)-1]] {
		return true
	}
	for _, n := range s.names {
		for _, w := range words(n) {
			if errWords[w] {
				return true
			}
		}
	}
	return false
}

func (s logSite) identWord() string {
	for _, n := range append([]string{s.key}, s.names...) {
		for _, w := range words(n) {
			if identWords[w] {
				return w
			}
		}
	}
	return ""
}

// words splits snake_case and camelCase into lower-case words. IPv4/IPv6 are
// kept whole so they read as one word rather than "i" and "pv6".
func words(ident string) []string {
	rs := []rune(strings.NewReplacer("IPv4", "Ipv4", "IPv6", "Ipv6").Replace(ident))
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	for i, r := range rs {
		if r == '_' || r == '.' || r == '-' {
			flush()
			continue
		}
		if unicode.IsUpper(r) && i > 0 {
			prev := rs[i-1]
			nextLower := i+1 < len(rs) && unicode.IsLower(rs[i+1])
			if unicode.IsLower(prev) || unicode.IsDigit(prev) || (unicode.IsUpper(prev) && nextLower) {
				flush()
			}
		}
		cur = append(cur, r)
	}
	flush()
	return out
}

// moduleRoot finds go.mod above the package directory and reads the module path.
func moduleRoot(t *testing.T) (root, modPath string) {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		f, err := os.Open(filepath.Join(dir, "go.mod"))
		if err == nil {
			defer f.Close()
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				if strings.HasPrefix(sc.Text(), "module ") {
					return dir, strings.TrimSpace(strings.TrimPrefix(sc.Text(), "module "))
				}
			}
			t.Fatalf("%s/go.mod has no module line", dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the package directory")
		}
		dir = parent
	}
}

// moduleChecker type-checks the module's own packages from source. The standard
// library comes from the compiler's export data; a third-party import is stood
// in for by an empty package, which leaves a value drawn from one unresolved -
// and unresolved counts as free-form, so it must be classified like a string.
type moduleChecker struct {
	fset    *token.FileSet
	root    string
	modPath string
	std     types.Importer
	pkgs    map[string]*types.Package
	infos   map[string]*types.Info
	files   map[string][]*ast.File
}

func (m *moduleChecker) Import(path string) (*types.Package, error) {
	if p, ok := m.pkgs[path]; ok {
		return p, nil
	}
	if path == m.modPath || strings.HasPrefix(path, m.modPath+"/") {
		return m.check(filepath.Join(m.root, strings.TrimPrefix(strings.TrimPrefix(path, m.modPath), "/")), path)
	}
	if p, err := m.std.Import(path); err == nil {
		m.pkgs[path] = p
		return p, nil
	}
	p := types.NewPackage(path, filepath.Base(path))
	p.MarkComplete()
	m.pkgs[path] = p
	return p, nil
}

func (m *moduleChecker) check(dir, path string) (*types.Package, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []*ast.File
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(m.fset, filepath.Join(dir, n), nil, 0)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Uses: map[*ast.Ident]types.Object{}, Defs: map[*ast.Ident]types.Object{}}
	conf := types.Config{Importer: m, Error: func(error) {}, FakeImportC: true}
	pkg, _ := conf.Check(path, m.fset, files, info) // errors are expected wherever a stub stands in
	m.pkgs[path] = pkg
	m.infos[path] = info
	m.files[path] = files
	return pkg, nil
}

// walkLogSites type-checks every package in the module and collects the key/value
// pairs of every logging call: the slog.Logger methods and slog functions, and
// anything of the module's own shaped like one - (msg string, args ...any) -
// which covers the speedtest wrappers and the boot-time log callbacks alike.
func walkLogSites(t *testing.T, root, modPath string) []logSite {
	t.Helper()
	m := &moduleChecker{fset: token.NewFileSet(), root: root, modPath: modPath, std: importer.Default(),
		pkgs: map[string]*types.Package{}, infos: map[string]*types.Info{}, files: map[string][]*ast.File{}}
	var dirs []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			n := d.Name()
			if p != root && (strings.HasPrefix(n, ".") || n == "node_modules" || n == "testdata") {
				return filepath.SkipDir
			}
			if p != root {
				if _, err := os.Stat(filepath.Join(p, "go.mod")); err == nil {
					return filepath.SkipDir // a nested module is not this one
				}
			}
			return nil
		}
		if strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
			if d := filepath.Dir(p); len(dirs) == 0 || dirs[len(dirs)-1] != d {
				dirs = append(dirs, d)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dirs {
		rel, _ := filepath.Rel(root, d)
		path := modPath
		if rel != "." {
			path += "/" + filepath.ToSlash(rel)
		}
		if _, ok := m.pkgs[path]; !ok {
			if _, err := m.check(d, path); err != nil {
				t.Fatal(err)
			}
		}
	}
	errType := types.Universe.Lookup("error").Type()
	var sites []logSite
	for path, files := range m.files {
		info := m.infos[path]
		for _, f := range files {
			ast.Inspect(f, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				args, ok := logArgs(call, info, modPath)
				if !ok {
					return true
				}
				pos := m.fset.Position(call.Pos())
				rel, _ := filepath.Rel(root, pos.Filename)
				for i := 0; i < len(args); i++ {
					if call.Ellipsis.IsValid() && i == len(args)-1 {
						break // args... forwarded from a wrapper; its own callers are walked
					}
					lit, ok := args[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						t.Errorf("%s:%d: log key %s is not a string literal; the walk cannot classify it", rel, pos.Line, exprString(m.fset, args[i]))
						continue
					}
					if i+1 >= len(args) {
						t.Errorf("%s:%d: log key %s has no value", rel, pos.Line, lit.Value)
						break
					}
					v := args[i+1]
					i++
					s := logSite{pos: fmt.Sprintf("%s:%d", rel, pos.Line), key: strings.Trim(lit.Value, "`\""), value: exprString(m.fset, v)}
					tv, known := info.Types[v]
					switch {
					case known && tv.Value != nil:
						// A literal or a constant: ours by construction.
					case known && tv.Type != nil && tv.Type != types.Typ[types.Invalid]:
						s.isError = types.AssignableTo(tv.Type, errType)
						s.freeForm = s.isError || freeFormType(tv.Type)
					default:
						s.freeForm = !numericShape(v)
					}
					if s.freeForm && timeFormatting(v, info) {
						s.freeForm = false
					}
					ast.Inspect(v, func(n ast.Node) bool {
						if id, ok := n.(*ast.Ident); ok {
							s.names = append(s.names, id.Name)
						}
						return true
					})
					sites = append(sites, s)
				}
				return true
			})
		}
	}
	return sites
}

// logArgs returns the attribute arguments of a logging call, or false when the
// call is not one.
func logArgs(call *ast.CallExpr, info *types.Info, modPath string) ([]ast.Expr, bool) {
	var obj types.Object
	switch fn := call.Fun.(type) {
	case *ast.SelectorExpr:
		obj = info.Uses[fn.Sel]
	case *ast.Ident:
		obj = info.Uses[fn]
	}
	if obj == nil || obj.Pkg() == nil {
		return nil, false
	}
	sig, ok := obj.Type().(*types.Signature)
	if !ok {
		return nil, false
	}
	start := -1
	switch {
	case obj.Pkg().Path() == "log/slog":
		switch obj.Name() {
		case "Debug", "Info", "Warn", "Error":
			start = 1
		case "DebugContext", "InfoContext", "WarnContext", "ErrorContext":
			start = 2
		case "Log":
			start = 3
		case "With":
			start = 0
		}
	case obj.Pkg().Path() == modPath || strings.HasPrefix(obj.Pkg().Path(), modPath+"/"):
		if p := sig.Params(); sig.Variadic() && p.Len() == 2 && isString(p.At(0).Type()) && isAnySlice(p.At(1).Type()) {
			start = 1
		}
	}
	if start < 0 || len(call.Args) < start {
		return nil, false
	}
	return call.Args[start:], true
}

func isString(t types.Type) bool {
	b, ok := t.Underlying().(*types.Basic)
	return ok && b.Kind() == types.String
}

func isAnySlice(t types.Type) bool {
	sl, ok := t.Underlying().(*types.Slice)
	if !ok {
		return false
	}
	i, ok := sl.Elem().Underlying().(*types.Interface)
	return ok && i.Empty()
}

// freeFormType reports whether a value of this type can carry text we did not
// write: strings, interfaces, and slices or maps of either. Numbers, booleans and
// the time types cannot.
func freeFormType(t types.Type) bool {
	switch u := t.Underlying().(type) {
	case *types.Basic:
		return u.Kind() == types.String || u.Kind() == types.UntypedString
	case *types.Interface:
		return true
	case *types.Slice:
		return freeFormType(u.Elem())
	case *types.Map:
		return freeFormType(u.Elem())
	case *types.Pointer:
		return freeFormType(u.Elem())
	}
	return false
}

// numericShape recognises a value the checker could not type but whose shape is
// plainly a number: len(x) over a third-party slice, or arithmetic on it.
func numericShape(v ast.Expr) bool {
	switch e := v.(type) {
	case *ast.CallExpr:
		id, ok := e.Fun.(*ast.Ident)
		return ok && id.Name == "len"
	case *ast.BinaryExpr:
		return numericShape(e.X) || numericShape(e.Y)
	case *ast.BasicLit:
		return e.Kind == token.INT || e.Kind == token.FLOAT
	}
	return false
}

// timeFormatting recognises a time.Time or time.Duration rendered as text - a
// value the clock wrote, not a person.
func timeFormatting(v ast.Expr, info *types.Info) bool {
	call, ok := v.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || (sel.Sel.Name != "String" && sel.Sel.Name != "Format") {
		return false
	}
	t := info.TypeOf(sel.X)
	if t == nil {
		return false
	}
	named, ok := t.(*types.Named)
	if !ok || named.Obj().Pkg() == nil || named.Obj().Pkg().Path() != "time" {
		return false
	}
	return named.Obj().Name() == "Time" || named.Obj().Name() == "Duration"
}

func exprString(fset *token.FileSet, e ast.Expr) string {
	var b strings.Builder
	start, end := fset.Position(e.Pos()), fset.Position(e.End())
	src, err := os.ReadFile(start.Filename)
	if err != nil || start.Offset > end.Offset || end.Offset > len(src) {
		return fmt.Sprintf("%T", e)
	}
	b.Write(src[start.Offset:end.Offset])
	return strings.Join(strings.Fields(b.String()), " ")
}
