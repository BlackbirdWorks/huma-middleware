package testrules_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/blackbirdworks/huma-middleware/internal/testrules"
)

const (
	srcClient = "package p\n\ntype Client struct{}\n\nfunc (Client) Close() {}\n" +
		"\nfunc Client_Close() {}\n\nfunc New() {}\n"
	srcDotImport = "package p_test\n\nimport . \"testing\"\n\nfunc TestNew(tb *T) {\n\ttb.Parallel()\n" +
		"\tfor range 1 {\n\t\ttb.Run(\"x\", nil)\n\t}\n}\n"
	srcOtherT = testHead + "func TestNew(t *testing.T) {\n\tvar f *testing.T\n\tf.Parallel()\n" +
		"\tfor range 1 {\n\t\tf.Run(\"x\", nil)\n\t}\n}\n"
	badGo       = "package"
	whyParallel = "p_test.go: TestNew: does not call t.Parallel() and is not listed as serial"
	whyTable    = "p_test.go: TestNew: not a table test: no range loop whose body calls t.Run"
)

const (
	srcName    = "p.go"
	testName   = "p_test.go"
	srcNew     = "package p\n\nfunc New() {}\n"
	testHead   = "package p_test\n\nimport \"testing\"\n\n"
	tableBody  = "\tfor range 1 {\n\t\tt.Run(\"x\", nil)\n\t}\n}\n"
	passTest   = testHead + "func TestNew(t *testing.T) {\n\tt.Parallel()\n" + tableBody
	serialTest = testHead + "func TestNew(t *testing.T) {\n" + tableBody
)

const (
	reasonAllocs = "AllocsPerRun panics in parallel tests"
	reasonLogger = "sets the process wide default logger"
	reasonTotal  = "the allocations of the middleware as a whole, pinned in one serial test"
)

// repoRules are the allow-lists of this repository, each entry with its reason.
func repoRules() testrules.Rules {
	serial := map[string]string{
		"middleware/logging/options_test.go:TestWithLogger":      reasonLogger,
		"middleware/accesslog/options_test.go:TestWithLogger":    reasonLogger,
		"middleware/tracing/options_test.go:TestWithLogger":      reasonLogger,
		"middleware/tracing/options_test.go:TestWithPropagators": "sets the process wide propagator",
	}
	names := map[string]string{
		"middleware:TestStack":    "cross-package: the handlers stacked in front of one router",
		"middleware:TestLogs":     "cross-package: the request logger and the access log together, in both orders",
		"middleware:TestCombined": "cross-package: every middleware in one chain",
		"middleware:TestReadme":   "cross-package: the README snippets mirror the compiled examples",
	}
	for _, pkg := range []string{"accesslog", "cleanpath", "compression", "logging", "recovery", "tracing"} {
		names["middleware/"+pkg+":TestAllocs"] = reasonTotal
	}
	serial["middleware/cleanpath/cleanpath_test.go:TestAllocs"] = reasonAllocs
	for _, pkg := range []string{"accesslog", "compression", "logging", "recovery", "tracing"} {
		serial["middleware/"+pkg+"/allocs_test.go:TestAllocs"] = reasonAllocs
	}

	return testrules.Rules{Serial: serial, Names: names}
}

type checkCase struct {
	files   map[string]string
	rules   testrules.Rules
	name    string
	want    []string
	repo    bool
	wantErr bool
	missing bool
}

// styleCases are the cases of the table and parallel rules.
func styleCases() []checkCase {
	return []checkCase{
		{
			name:  "violations come sorted",
			files: map[string]string{srcName: srcNew, testName: testHead + "func TestNew(t *testing.T) {}\n"},
			want: []string{
				whyParallel,
				whyTable,
			},
		},
		{name: "this repository", repo: true, rules: repoRules()},
		{name: "a directory that is missing", files: map[string]string{"": ""}, wantErr: true, missing: true},
		{
			name: "hidden and generated directories are skipped",
			files: map[string]string{srcName: srcNew, testName: passTest, ".git/x.go": badGo, "bin/y.go": badGo,
				"coverage/z.go": badGo, "notes.txt": "not go"},
		},
		{
			name: "receivers of every shape",
			files: map[string]string{
				srcName: "package p\n\nfunc New() {}\n\ntype G[T any] struct{}\n\ntype H[K, V any] struct{}\n\n" +
					"type V struct{}\n\nfunc (*G[T]) Do() {}\n\nfunc (H[K, V]) Do() {}\n\nfunc (V) Do() {}\n\nfunc (v (V)) Go() {}\n",
				testName: passTest + "\n" + namedFunc(
					"TestG_Do",
				) + "\n" + namedFunc(
					"TestH_Do",
				) + "\n" + namedFunc(
					"TestV_Do",
				) +
					"\n" + namedFunc(
					"TestV_Go",
				),
			},
		},
		{
			name: "functions that are not tests",
			files: map[string]string{srcName: srcNew, testName: passTest + "\nfunc TestNoParams() {}\n\n" +
				"func TestValue(t testing.T) {}\n\nfunc TestBench(b *testing.B) {}\n\nfunc TestPlain(t *int) {}\n\n" +
				"func TestMain(m *testing.M) {}\n"},
		},
		{name: "a file that does not parse", files: map[string]string{srcName: badGo}, wantErr: true},
		{name: "clean", files: map[string]string{srcName: srcNew, testName: passTest}},
		{
			name: "not a table",
			files: map[string]string{
				srcName:  srcNew,
				testName: testHead + "func TestNew(t *testing.T) { t.Parallel() }\n",
			},
			want: []string{whyTable},
		},
	}
}

// symbolCases are the cases of the name and exported function rules, and of the walk.
func symbolCases() []checkCase {
	return []checkCase{
		{
			name:  "serial and listed",
			files: map[string]string{srcName: srcNew, testName: serialTest},
			rules: testrules.Rules{Serial: map[string]string{"p_test.go:TestNew": "global state"}},
		},
		{
			name:  "serial and not listed",
			files: map[string]string{srcName: srcNew, testName: serialTest},
			want:  []string{whyParallel},
		},
		{
			name:  "a name that is no symbol",
			files: map[string]string{srcName: srcNew, testName: passTest + "\n" + namedFunc("TestEverythingWorks")},
			want: []string{
				"p_test.go: TestEverythingWorks: not the name of an exported symbol of the package (TestNew, TestType_Method)",
			},
		},
		{
			name:  "a name that is listed",
			files: map[string]string{srcName: srcNew, testName: passTest + "\n" + namedFunc("TestStack")},
			rules: testrules.Rules{Names: map[string]string{".:TestStack": "cross-package"}},
		},
		{
			name: "an exported method without a test",
			files: map[string]string{
				srcName:  "package p\n\ntype T struct{}\n\nfunc (*T) Do() {}\n\nfunc New() {}\n",
				testName: passTest,
			},
			want: []string{".: T_Do: exported function or method without a Test function of that name"},
		},
		{
			name: "an unexported method is not asked for",
			files: map[string]string{
				srcName:  "package p\n\ntype T struct{}\n\nfunc (*T) do() {}\n\nfunc New() {}\n",
				testName: passTest,
			},
		},
		{
			name: "an internal test may name an unexported symbol",
			files: map[string]string{
				srcName: srcNew + "\nfunc helper() {}\n", testName: passTest,
				"i_test.go": "package p\n\nimport \"testing\"\n\n" + namedFunc("TestHelper"),
			},
		},
		{
			name:  "an allow-list entry that matches nothing",
			files: map[string]string{srcName: srcNew, testName: passTest},
			rules: testrules.Rules{Untested: map[string]string{".:Gone": "left over"}},
			want:  []string{"allow-list: .:Gone: the untested entry matches nothing: remove it"},
		},
		{
			name:  "a listed untested function",
			files: map[string]string{srcName: srcNew + "\nfunc Extra() {}\n", testName: passTest},
			rules: testrules.Rules{Untested: map[string]string{".:Extra": "trivial"}},
		},
	}
}

// shapeCases are the cases of what the checker reads: which directories, which functions, which parameters.
func shapeCases() []checkCase {
	return []checkCase{
		{
			name:  "testdata is skipped",
			files: map[string]string{srcName: srcNew, testName: passTest, "testdata/broken.go": badGo},
		},
		{
			name: "a function and a method with one test name",
			files: map[string]string{
				srcName:  srcClient,
				testName: passTest + "\n" + namedFunc("TestClient_Close"),
			},
			want: []string{
				".: Client_Close: ambiguous: a function and a method (or two methods) map to the same test name",
			},
		},
		{
			name:  "an entry without a reason",
			files: map[string]string{srcName: srcNew + "\nfunc Extra() {}\n", testName: passTest},
			rules: testrules.Rules{Untested: map[string]string{".:Extra": " "}},
			want:  []string{"allow-list: .:Extra: the untested entry has no reason"},
		},
		{
			name:  "a lower case letter after Test is no test",
			files: map[string]string{srcName: srcNew, testName: passTest + "\nfunc Testfixture(t *testing.T) {}\n"},
		},
		{
			name: "a dot-imported T and another parameter name",
			files: map[string]string{
				srcName:  srcNew,
				testName: srcDotImport,
			},
		},
		{
			name: "Run and Parallel on something else than the test parameter",
			files: map[string]string{
				srcName:  srcNew,
				testName: srcOtherT,
			},
			want: []string{
				whyParallel,
				whyTable,
			},
		},
		{
			name:  "a blank parameter cannot run anything",
			files: map[string]string{srcName: srcNew, testName: testHead + "func TestNew(_ *testing.T) {}\n"},
			want: []string{
				whyParallel,
				whyTable,
			},
		},
		{
			name: "a parameter of another type",
			files: map[string]string{
				srcName:  srcNew,
				testName: passTest + "\nfunc TestOther(t *int) {}\n\nfunc TestSlice(t *[]int) {}\n",
			},
		},
	}
}

// TestCheck covers the checker on small packages, and on this repository.
func TestCheck(t *testing.T) {
	t.Parallel()

	tests := append(append(styleCases(), symbolCases()...), shapeCases()...)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if tt.repo {
				dir = filepath.Join("..", "..")
			}
			if tt.missing {
				dir = filepath.Join(dir, "missing")
			}
			for name, src := range tt.files {
				if name == "" {
					continue
				}
				require.NoError(t, os.MkdirAll(filepath.Join(dir, filepath.Dir(name)), 0o750))
				require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(src), 0o600))
			}
			got, err := testrules.Check(dir, tt.rules)
			if tt.wantErr {
				require.Error(t, err)

				return
			}
			require.NoError(t, err)
			var lines []string
			for _, v := range got {
				lines = append(lines, v.String())
			}
			assert.Equal(t, tt.want, lines)
		})
	}
}

// namedFunc is a parallel table test of the name given.
func namedFunc(name string) string {
	return "func " + name + "(t *testing.T) {\n\tt.Parallel()\n" + tableBody
}

// TestViolation_String covers the text of a violation.
func TestViolation_String(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		v    testrules.Violation
		want string
	}{
		{
			name: "all fields",
			v:    testrules.Violation{File: "a_test.go", Func: "TestA", Why: "because"},
			want: "a_test.go: TestA: because",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, tt.v.String())
		})
	}
}
