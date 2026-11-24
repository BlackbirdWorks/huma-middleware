// Package testrules checks the style of the tests of the repository. It is run by a test of its own package
// ([TestCheck]), so it is part of the test suite, of `task lint` and of CI. The rules are written out in AGENTS.md:
//
//   - every Test function is a table test: a range loop whose body calls t.Run;
//   - every Test function calls t.Parallel, unless it is allow-listed as serial (with the reason);
//   - the name of a Test function is the exported symbol it tests (TestNew, TestWithMode, TestType_Method), apart
//     from the allow-listed names, and from the unexported helpers that an internal test file may test;
//   - every exported function and method has a Test function of that name.
package testrules

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// whyName is the reason of a test name that is no symbol.
const whyName = "not the name of an exported symbol of the package (TestNew, TestType_Method)"

// Rules are the allow-lists, each entry with the reason it is there.
type Rules struct {
	// Serial lists the Test functions that do not call t.Parallel, keyed "dir/file_test.go:TestName".
	Serial map[string]string
	// Names lists the Test functions whose name is not an exported symbol, keyed "dir:TestName".
	Names map[string]string
	// Untested lists the exported functions and methods without a Test function of their own, keyed "dir:Name" or
	// "dir:Type_Method".
	Untested map[string]string
}

// Violation is a rule that is broken.
type Violation struct {
	File string
	Func string
	Why  string
}

func (v Violation) String() string { return fmt.Sprintf("%s: %s: %s", v.File, v.Func, v.Why) }

// Check parses the Go files under root and returns the violations, sorted.
func Check(root string, rules Rules) ([]Violation, error) {
	pkgs := map[string]*pkg{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); path != root &&
				(strings.HasPrefix(n, ".") || n == "coverage" || n == "bin" || n == "testdata") {
				return filepath.SkipDir
			}

			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		rel, _ := filepath.Rel(root, path)
		dir := filepath.ToSlash(filepath.Dir(rel))
		p := pkgs[dir]
		if p == nil {
			p = &pkg{dir: dir, exported: map[string]bool{}, unexported: map[string]bool{}}
			pkgs[dir] = p
		}
		p.add(file, filepath.ToSlash(rel), strings.HasSuffix(path, "_test.go"))

		return nil
	})
	if err != nil {
		return nil, err
	}

	used := map[string]bool{}
	var out []Violation
	dirs := make([]string, 0, len(pkgs))
	for d := range pkgs {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	for _, d := range dirs {
		out = append(out, pkgs[d].check(rules, used)...)
	}
	out = append(out, unused(rules, used)...)
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })

	return out, nil
}

type testFunc struct {
	file     string
	name     string
	table    bool
	parallel bool
	internal bool
}

type pkg struct {
	exported   map[string]bool
	unexported map[string]bool
	dir        string
	tests      []testFunc
	funcs      []string
	hasSource  bool
}

func (p *pkg) add(file *ast.File, rel string, isTest bool) {
	if !isTest {
		p.hasSource = true
		p.addSymbols(file)

		return
	}
	internal := !strings.HasSuffix(file.Name.Name, "_test")
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Body == nil || !isTestFunc(fn) {
			continue
		}
		param := paramName(fn)
		p.tests = append(p.tests, testFunc{
			file:     rel,
			name:     fn.Name.Name,
			table:    hasTableRun(fn.Body, param),
			parallel: callsParallel(fn.Body, param),
			internal: internal,
		})
	}
}

func (p *pkg) addSymbols(file *ast.File) {
	note := func(name string) {
		if ast.IsExported(name) {
			p.exported[name] = true
		} else {
			p.unexported[upper(name)] = true
		}
	}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			p.addFunc(d, note)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					note(s.Name.Name)
				case *ast.ValueSpec:
					for _, n := range s.Names {
						note(n.Name)
					}
				}
			}
		}
	}
}

func (p *pkg) addFunc(d *ast.FuncDecl, note func(string)) {
	name := d.Name.Name
	if d.Recv == nil {
		note(name)
		if ast.IsExported(name) {
			p.funcs = append(p.funcs, name)
		}

		return
	}
	recv := receiverName(d.Recv.List[0].Type)
	full := recv + "_" + name
	if ast.IsExported(recv) && ast.IsExported(name) {
		p.exported[full] = true
		p.funcs = append(p.funcs, full)
	} else {
		p.unexported[upper(recv)+"_"+name] = true
		p.unexported[upper(recv)+"_"+upper(name)] = true
	}
}

func (p *pkg) check(rules Rules, used map[string]bool) []Violation {
	var out []Violation
	tested := map[string]bool{}
	for _, tf := range p.tests {
		tested[strings.TrimPrefix(tf.name, "Test")] = true
		if !tf.table {
			out = append(out, Violation{tf.file, tf.name, "not a table test: no range loop whose body calls t.Run"})
		}
		if !tf.parallel {
			out = append(
				out,
				listed(
					rules.Serial,
					"serial",
					tf.file+":"+tf.name,
					tf,
					"does not call t.Parallel() and is not listed as serial",
					used,
				)...)
		}
		if !p.validName(tf) {
			out = append(out, listed(rules.Names, "name", p.dir+":"+tf.name, tf, whyName, used)...)
		}
	}
	seen := map[string]bool{}
	for _, f := range p.funcs {
		if seen[f] {
			out = append(
				out,
				Violation{p.dir, f, "ambiguous: a function and a method (or two methods) map to the same test name"},
			)

			continue
		}
		seen[f] = true
		if tested[f] {
			continue
		}
		key := p.dir + ":" + f
		out = append(
			out,
			listed(
				rules.Untested,
				"untested",
				key,
				testFunc{file: p.dir, name: f},
				"exported function or method without a Test function of that name",
				used,
			)...)
	}

	return out
}

// listed is the violation why for tf, unless the allow-list has a reason for key; an entry without a reason is a
// violation too.
func listed(list map[string]string, kind, key string, tf testFunc, why string, used map[string]bool) []Violation {
	reason, ok := list[key]
	if !ok {
		return []Violation{{tf.file, tf.name, why}}
	}
	used[kind+" "+key] = true
	if strings.TrimSpace(reason) == "" {
		return []Violation{{"allow-list", key, "the " + kind + " entry has no reason"}}
	}

	return nil
}

func (p *pkg) validName(tf testFunc) bool {
	sym := strings.TrimPrefix(tf.name, "Test")

	return p.exported[sym] || (tf.internal && p.unexported[sym])
}

// unused reports allow-list entries that no longer match anything, so the lists cannot rot.
func unused(rules Rules, used map[string]bool) []Violation {
	var out []Violation
	lists := map[string]map[string]string{"serial": rules.Serial, "name": rules.Names, "untested": rules.Untested}
	for kind, list := range lists {
		for key := range list {
			if !used[kind+" "+key] {
				out = append(out, Violation{"allow-list", key, "the " + kind + " entry matches nothing: remove it"})
			}
		}
	}

	return out
}

func receiverName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return receiverName(t.X)
	case *ast.ParenExpr:
		return receiverName(t.X)
	case *ast.IndexExpr:
		return receiverName(t.X)
	case *ast.IndexListExpr:
		return receiverName(t.X)
	default:
		id, _ := t.(*ast.Ident)

		return id.Name
	}
}

func upper(s string) string {
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])

	return string(r)
}

// isTestFunc reports whether fn is a test function the go tool runs: Test followed by anything but a lower case
// letter, with one parameter, a pointer to a [testing.T] (qualified, or dot-imported).
func isTestFunc(fn *ast.FuncDecl) bool {
	rest, found := strings.CutPrefix(fn.Name.Name, "Test")
	if !found || fn.Type.Params == nil || len(fn.Type.Params.List) != 1 {
		return false
	}
	if r, _ := utf8.DecodeRuneInString(rest); unicode.IsLower(r) {
		return false
	}
	star, ok := fn.Type.Params.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	switch t := star.X.(type) {
	case *ast.SelectorExpr:
		return t.Sel.Name == "T"
	case *ast.Ident:
		return t.Name == "T"
	}

	return false
}

// paramName is the name of the [testing.T] parameter, empty when it is blank or missing.
func paramName(fn *ast.FuncDecl) string {
	names := fn.Type.Params.List[0].Names
	if len(names) == 0 || names[0].Name == "_" {
		return ""
	}

	return names[0].Name
}

// calls reports whether n is a call of the method name on the identifier recv.
func calls(n ast.Node, recv, name string) bool {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	id, ok := sel.X.(*ast.Ident)

	return ok && id.Name == recv
}

// hasTableRun reports whether the body has a range statement whose body calls Run on the test parameter.
func hasTableRun(body *ast.BlockStmt, param string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		rs, ok := n.(*ast.RangeStmt)
		if !ok || found {
			return !found
		}
		ast.Inspect(rs.Body, func(m ast.Node) bool {
			if calls(m, param, "Run") {
				found = true
			}

			return !found
		})

		return !found
	})

	return found
}

// callsParallel reports whether the function itself (not a subtest closure) calls Parallel on the test parameter.
func callsParallel(body *ast.BlockStmt, param string) bool {
	for _, st := range body.List {
		if es, ok := st.(*ast.ExprStmt); ok && calls(es.X, param, "Parallel") {
			return true
		}
	}

	return false
}
