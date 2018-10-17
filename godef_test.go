package main

import (
	"go/token"
	"io/ioutil"
	"os"
	"path/filepath"
	"testing"
	"go/types"

	"golang.org/x/tools/go/packages/packagestest"
)

func TestGoDef(t *testing.T) { packagestest.TestAll(t, testGoDef) }
func testGoDef(t *testing.T, exporter packagestest.Exporter) {
	runGoDefTest(t, exporter, 1, []packagestest.Module{{
		Name:  "github.com/rogpeppe/godef",
		Files: packagestest.MustCopyFileTree("testdata"),
	}})
}

func BenchmarkGoDef(b *testing.B) { packagestest.BenchmarkAll(b, benchGoDef) }
func benchGoDef(b *testing.B, exporter packagestest.Exporter) {
	runGoDefTest(b, exporter, b.N, []packagestest.Module{{
		Name:  "github.com/rogpeppe/godef",
		Files: packagestest.MustCopyFileTree("testdata"),
	}})
}

func runGoDefTest(t testing.TB, exporter packagestest.Exporter, runCount int, modules []packagestest.Module) {
	const expectedGodefCount = 16
	exported := packagestest.Export(t, exporter, modules)
	defer exported.Cleanup()
	posStr := func(p token.Position) string {
		return localPos(p, exported, modules)
	}
	count := 0
	if err := exported.Expect(map[string]interface{}{
		"godef": func(src, target token.Position) {
			count++
			input, err := ioutil.ReadFile(src.Filename)
			if err != nil {
				t.Fatalf("cannot read source: %v", err)
				return
			}
			// There's a "saved" version of the file, so
			// copy it to the original version; we want the
			// Expect method to see the in-editor-buffer
			// versions of the files, but we want the godef
			// function to see the files as they should
			// be on disk, so that we're actually testing the
			// define-in-buffer functionality.
			savedFile := src.Filename + ".saved"
			if _, err := os.Stat(savedFile); err == nil {
				savedData, err := ioutil.ReadFile(savedFile)
				if err != nil {
					t.Fatalf("cannot read saved file: %v", err)
				}
				if err := ioutil.WriteFile(src.Filename, savedData, 0666); err != nil {
					t.Fatalf("cannot write saved file: %v", err)
				}
				defer ioutil.WriteFile(src.Filename, input, 0666)
			}
			var obj types.Object
			var fSet *token.FileSet
			for i := 0; i < runCount; i++ {
				fSet, obj, err = godef(exported.Config, src.Filename, input, src.Offset)
				if err != nil {
					t.Errorf("godef error %v: %v", posStr(src), err)
					return
				}
			}
			pos := objToPos(fSet, obj)
			if pos.String() != target.String() {
				t.Errorf("unexpected result %v -> %v want %v", posStr(src), posStr(pos), posStr(target))
			}
		},
	}); err != nil {
		t.Fatal(err)
	}
	if count != expectedGodefCount {
		t.Fatalf("expected %d godef tests, got %d", expectedGodefCount, count)
	}
}

var cwd, _ = os.Getwd()

func localPos(pos token.Position, e *packagestest.Exported, modules []packagestest.Module) string {
	fstat, fstatErr := os.Stat(pos.Filename)
	if fstatErr != nil {
		return pos.String()
	}
	for _, m := range modules {
		for fragment := range m.Files {
			fname := e.File(m.Name, fragment)
			if s, err := os.Stat(fname); err == nil && os.SameFile(s, fstat) {
				pos.Filename = filepath.Join(cwd, "testdata", filepath.FromSlash(fragment))
				return pos.String()
			}
		}
	}
	return pos.String()
}
