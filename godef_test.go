package main

import (
	"bytes"
	"fmt"
	"go/build"
	"go/token"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages/packagestest"
)

func TestGoDef(t *testing.T) { packagestest.TestAll(t, testGoDef) }
func testGoDef(t *testing.T, exporter packagestest.Exporter) {
	const expectGodefCount = 15
	const expectGodefPrintCount = 11

	exported := godefExport(t, exporter)
	defer exported.Cleanup()

	var godefCount, godefPrintCount int
	if err := exported.Expect(map[string]interface{}{
		"godef": func(src, target token.Position) {
			godefCount++
			obj, err := invokeGodef(exported, src)
			if err != nil {
				t.Error(err)
				return
			}
			check := token.Position{
				Filename: obj.Position.Filename,
				Line:     obj.Position.Line,
				Column:   obj.Position.Column,
			}
			if check, target := localPos(check, exported), localPos(target, exported); check != target {
				t.Errorf("Got %v expected %v", check, target)
			}
		},
		"godefPrint": func(src token.Position, mode string, re *regexp.Regexp) {
			godefPrintCount++
			obj, err := invokeGodef(exported, src)
			if err != nil {
				t.Error(err)
				return
			}
			buf := &bytes.Buffer{}
			switch mode {
			case "json":
				*jsonFlag = true
				*tflag = false
				*aflag = false
				*Aflag = false
			case "all":
				*jsonFlag = false
				*tflag = true
				*aflag = true
				*Aflag = true
			case "public":
				*jsonFlag = false
				*tflag = true
				*aflag = true
				*Aflag = false
			case "type":
				*jsonFlag = false
				*tflag = true
				*aflag = false
				*Aflag = false
			default:
				t.Fatalf("Invalid print mode %v", mode)
			}

			print(buf, obj)
			if !re.Match(buf.Bytes()) {
				t.Errorf("in mode %q got %v want %v", mode, buf, re)
			}
		},
	}); err != nil {
		t.Fatal(err)
	}
	if godefCount != expectGodefCount {
		t.Errorf("ran %d godef tests, expected %v", godefCount, expectGodefCount)
	}
	if godefPrintCount != expectGodefPrintCount {
		t.Errorf("ran %d godefPrint tests, expected %v", godefPrintCount, expectGodefPrintCount)
	}
}

func BenchmarkGoDef(b *testing.B) { packagestest.BenchmarkAll(b, benchGoDef) }
func benchGoDef(b *testing.B, exporter packagestest.Exporter) {
	const expectGodefBenchCount = 1

	exported := godefExport(b, exporter)
	defer exported.Cleanup()

	for _, forced := range []triBool{unset, off, on} {
		forcePackages = forced
		b.Run(forced.String(), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				var godefBenchCount int
				if err := exported.Expect(map[string]interface{}{
					"godef": func(src token.Position, target string) {
						if !strings.Contains(target, "Bench") {
							return
						}
						godefBenchCount++
						invokeGodef(exported, src)
					},
				}); err != nil {
					b.Fatal(err)
				}
				if godefBenchCount != expectGodefBenchCount {
					b.Errorf("ran %d godef tests, expected %v", godefBenchCount, expectGodefBenchCount)
				}
			}
		})
	}
}

func godefExport(t testing.TB, exporter packagestest.Exporter) *packagestest.Exported {
	// build the file and overlay sets
	files := packagestest.MustCopyFileTree("testdata")
	overlay := make(map[string][]byte)
	for fragment := range files {
		if trimmed := strings.TrimSuffix(fragment, ".overlay"); trimmed != fragment {
			delete(files, fragment)
			content, err := ioutil.ReadFile(filepath.Join("testdata", fragment))
			if err == nil {
				overlay[trimmed] = content
			}
		}
	}
	// do the export
	exported := packagestest.Export(t, exporter, []packagestest.Module{{
		Name:  "github.com/rogpeppe/godef",
		Files: files,
		Overlay: overlay,
	}})
	// process the resulting configuration
	const gopathPrefix = "GOPATH="
	const gorootPrefix = "GOROOT="
	for _, v := range exported.Config.Env {
		if strings.HasPrefix(v, gopathPrefix) {
			build.Default.GOPATH = v[len(gopathPrefix):]
		}
		if strings.HasPrefix(v, gorootPrefix) {
			build.Default.GOROOT = v[len(gorootPrefix):]
		}
	}
	return exported
}

var cwd, _ = os.Getwd()

func invokeGodef(e *packagestest.Exported, src token.Position) (*Object, error) {
	input, err := e.FileContents(src.Filename)
	if err != nil {
		return nil, fmt.Errorf("Failed %v: %v", src, err)
	}
	obj, err := adaptGodef(e.Config, src.Filename, input, src.Offset)
	if err != nil {
		return nil, fmt.Errorf("Failed %v: %v", src, err)
	}
	return obj, nil
}

func localPos(pos token.Position, e *packagestest.Exported) string {
	fstat, fstatErr := os.Stat(pos.Filename)
	if fstatErr != nil {
		return pos.String()
	}
	for _, m := range e.Modules {
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
