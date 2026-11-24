package middleware_test

import (
	"io/fs"
	"os"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var readmeExample = regexp.MustCompile("(?s)<!-- example: (\\w+) (\\w+) -->\n```go\n(.*?)\n```")

// TestReadme checks that every README snippet is the text of an Example
// function of its package.
func TestReadme(t *testing.T) {
	t.Parallel()

	readme, err := os.ReadFile("../README.md")
	require.NoError(t, err)
	snippets := readmeExample.FindAllStringSubmatch(string(readme), -1)
	require.NotEmpty(t, snippets)

	wantPackages := map[string]bool{
		"tracing":     false,
		nameLogging:   false,
		nameAccess:    false,
		"recovery":    false,
		"compression": false,
		"cleanpath":   false,
	}
	for _, s := range snippets {
		wantPackages[s[1]] = true
	}
	for pkg, found := range wantPackages {
		assert.True(t, found, "the README has a snippet for %s", pkg)
	}

	for _, s := range snippets {
		pkg, name, snippet := s[1], s[2], s[3]
		t.Run(pkg+"/"+name, func(t *testing.T) {
			t.Parallel()
			src, readErr := fs.ReadFile(os.DirFS("."), pkg+"/example_test.go")
			require.NoError(t, readErr)
			fn := regexp.MustCompile("(?ms)^func " + name + `\(\) \{\n.*?^\}$`).Find(src)
			require.NotNil(t, fn, "%s exists in %s/example_test.go", name, pkg)
			assert.Equal(t, string(fn), snippet)
		})
	}
}
