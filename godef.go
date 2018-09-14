package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/ioutil"
	"log"
	"os"
	"runtime"
	debugpkg "runtime/debug"
	"runtime/pprof"
	"runtime/trace"
	"sort"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/types/typeutil"
)

var readStdin = flag.Bool("i", false, "read file from stdin")
var offset = flag.Int("o", -1, "file offset of identifier in stdin")
var debug = flag.Bool("debug", false, "debug mode")
var tflag = flag.Bool("t", false, "print type information")
var aflag = flag.Bool("a", false, "print public type and member information")
var Aflag = flag.Bool("A", false, "print all type and members information")
var fflag = flag.String("f", "", "Go source filename")
var acmeFlag = flag.Bool("acme", false, "use current acme window")
var jsonFlag = flag.Bool("json", false, "output location in JSON format (-t flag is ignored)")

var cpuprofile = flag.String("cpuprofile", "", "write CPU profile to this file")
var memprofile = flag.String("memprofile", "", "write memory profile to this file")
var traceFlag = flag.String("trace", "", "write trace log to this file")

func fail(s string, a ...interface{}) {
	fmt.Fprint(os.Stderr, "godef: "+fmt.Sprintf(s, a...)+"\n")
	os.Exit(2)
}

func main() {
	debugpkg.SetGCPercent(1600)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: godef [flags] [expr]\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() > 1 {
		flag.Usage()
		os.Exit(2)
	}
	if flag.NArg() > 0 {
		fail("Expressions not yet supported `%v`", flag.Arg(0))
	}
	//TODO: types.Debug = *debug

	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			log.Fatal(err)
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			log.Fatal(err)
		}
		// NB: profile won't be written in case of error.
		defer pprof.StopCPUProfile()
	}

	if *traceFlag != "" {
		f, err := os.Create(*traceFlag)
		if err != nil {
			log.Fatal(err)
		}
		if err := trace.Start(f); err != nil {
			log.Fatal(err)
		}
		// NB: trace log won't be written in case of error.
		defer func() {
			trace.Stop()
			log.Printf("To view the trace, run:\n$ go tool trace view %s", *traceFlag)
		}()
	}

	if *memprofile != "" {
		f, err := os.Create(*memprofile)
		if err != nil {
			log.Fatal(err)
		}
		// NB: memprofile won't be written in case of error.
		defer func() {
			runtime.GC() // get up-to-date statistics
			if err := pprof.WriteHeapProfile(f); err != nil {
				log.Fatalf("Writing memory profile: %v", err)
			}
			f.Close()
		}()
	}

	*tflag = *tflag || *aflag || *Aflag
	searchpos := *offset
	filename := *fflag

	var afile *acmeFile
	var src []byte

	if *acmeFlag {
		var err error
		if afile, err = acmeCurrentFile(); err != nil {
			fail("%v", err)
		}
		filename, src, searchpos = afile.name, afile.body, afile.offset
	} else if filename == "" {
		// TODO if there's no filename, look in the current
		// directory and do something plausible.
		fail("A filename must be specified")
	} else if *readStdin {
		src, _ = ioutil.ReadAll(os.Stdin)
	}
	if searchpos < 0 {
		fmt.Fprintf(os.Stderr, "no expression or offset specified\n")
		flag.Usage()
		os.Exit(2)
	}
	parser, result := parseFile(filename, src, searchpos)
	// Load, parse, and type-check the packages named on the command line.
	cfg := &packages.Config{
		Mode:      packages.LoadSyntax,
		Tests:     strings.HasSuffix(filename, "_test.go"),
		ParseFile: parser,
	}
	lpkgs, err := packages.Load(cfg, "contains:"+filename)
	if err != nil {
		fail("%v", err)
	}
	if len(lpkgs) < 1 {
		fail("There must be at least one package that contains the file")
	}
	// get the node
	var o ast.Node
	select {
	case o = <-result:
	default:
		fail("no node found at search pos")
	}
	if o == nil {
		fail("Specified offset was not a valid location")
	}
	// print old source location to facilitate backtracking
	if *acmeFlag {
		fmt.Printf("\t%s:#%d\n", afile.name, afile.runeOffset)
	}
	ident, ok := o.(*ast.Ident)
	if !ok {
		return // DO NOT SUBMIT
		fail("no identifier found")
	}
	obj := lpkgs[0].TypesInfo.ObjectOf(ident)
	if obj == nil {
		fail("no object")
	}
	done(lpkgs[0].Fset, obj, func(p *types.Package) string {
		//TODO: this matches existing behaviour, but we can do better.
		//The previous code had the following TODO in it that now belongs here
		// TODO print path package when appropriate.
		// Current issues with using p.n.Pkg:
		//	- we should actually print the local package identifier
		//	rather than the package path when possible.
		//	- p.n.Pkg is non-empty even when
		//	the type is not relative to the package.
		return ""
	})
}

// parseFile returns a function that can be used as a Parser in packages.Config.
// It replaces the contents of a file that matches filename with the src.
// It also drops all function bodies that do not contain the searchpos.
// It also modifies the filename to be the canonical form that will appear in the fileset.
func parseFile(filename string, src []byte, searchpos int) (func(*token.FileSet, string) (*ast.File, error), chan ast.Node) {
	fstat, fstatErr := os.Stat(filename)
	result := make(chan ast.Node, 1)
	return func(fset *token.FileSet, fname string) (*ast.File, error) {
		var filedata []byte
		isInputFile := false
		if filename == fname {
			isInputFile = true
		} else if fstatErr != nil {
			isInputFile = false
		} else if s, err := os.Stat(fname); err == nil {
			isInputFile = os.SameFile(fstat, s)
		}
		if isInputFile && src != nil {
			filedata = src
		} else {
			var err error
			if filedata, err = ioutil.ReadFile(fname); err != nil {
				fail("cannot read %s: %v", fname, err)
			}
		}
		file, err := parser.ParseFile(fset, fname, filedata, 0)
		if file == nil {
			return nil, err
		}
		var keepFunc *ast.FuncDecl
		if isInputFile {
			pos := file.Pos() + token.Pos(searchpos)
			if pos > file.End() {
				return file, fmt.Errorf("cursor %d is beyond end of file %s (%d)", searchpos, fname, file.End()-file.Pos())
			}
			path, _ := astutil.PathEnclosingInterval(file, pos, pos)
			if len(path) < 1 {
				return nil, fmt.Errorf("Offest was not a valid token")
			}
			// report the base node we matched
			result <- path[0]
			// if we are inside a function, we need to retain that function body
			// start from the top not the bottom
			for i := len(path) - 1; i >= 0; i-- {
				if f, ok := path[i].(*ast.FuncDecl); ok {
					keepFunc = f
					break
				}
			}
		}
		// and drop all function bodies that are not relevant so they don't get
		// type checked
		for _, decl := range file.Decls {
			if f, ok := decl.(*ast.FuncDecl); ok && f != keepFunc {
				f.Body = nil
			}
		}
		return file, err
	}, result
}

type orderedObjects []types.Object

func (o orderedObjects) Less(i, j int) bool { return o[i].Name() < o[j].Name() }
func (o orderedObjects) Len() int           { return len(o) }
func (o orderedObjects) Swap(i, j int)      { o[i], o[j] = o[j], o[i] }

func done(fSet *token.FileSet, obj types.Object, q types.Qualifier) {
	pos := fSet.Position(obj.Pos())
	if *jsonFlag {
		p := struct {
			Filename string `json:"filename,omitempty"`
			Line     int    `json:"line,omitempty"`
			Column   int    `json:"column,omitempty"`
		}{
			Filename: pos.Filename,
			Line:     pos.Line,
			Column:   pos.Column,
		}
		jsonStr, err := json.Marshal(p)
		if err != nil {
			fail("JSON marshal error: %v", err)
		}
		fmt.Printf("%s\n", jsonStr)
		return
	} else {
		fmt.Printf("%v\n", posToString(pos))
	}
	if !*tflag {
		return
	}
	fmt.Printf("%s\n", typeStr(obj, q))
	if *aflag || *Aflag {
		m := orderedObjects(members(obj))
		sort.Sort(m)
		for _, obj := range m {
			// Ignore unexported members unless Aflag is set.
			if !*Aflag && !ast.IsExported(obj.Name()) {
				continue
			}
			fmt.Printf("\t%s\n", strings.Replace(typeStr(obj, q), "\n", "\n\t\t", -1))
			fmt.Printf("\t\t%v\n", posToString(fSet.Position(obj.Pos())))
		}
	}
}

func typeStr(obj types.Object, q types.Qualifier) string {
	buf := &bytes.Buffer{}
	switch obj := obj.(type) {
	case *types.Func:
		buf.WriteString(obj.Name())
		buf.WriteString(" ")
		types.WriteType(buf, obj.Type(), q)
	case *types.Var:
		buf.WriteString(obj.Name())
		buf.WriteString(" ")
		types.WriteType(buf, obj.Type(), q)
	case *types.PkgName:
		fmt.Fprintf(buf, "import (%v %q)", obj.Name(), obj.Imported().Path())
	case *types.Const:
		fmt.Fprintf(buf, "const %s ", obj.Name())
		types.WriteType(buf, obj.Type(), q)
		if obj.Val() != nil {
			buf.WriteString(" ")
			buf.WriteString(obj.Val().String())
		}
	case *types.Label:
		fmt.Fprintf(buf, "label %s ", obj.Name())
	case *types.TypeName:
		fmt.Fprintf(buf, "type %s ", obj.Name())
		types.WriteType(buf, obj.Type().Underlying(), q)
	default:
		fmt.Fprintf(buf, "unknown %v [%T] ", obj.Name(), obj)
		types.WriteType(buf, obj.Type(), q)
	}
	return buf.String()
}

func members(obj types.Object) []types.Object {
	var result []types.Object
	switch typ := obj.Type().Underlying().(type) {
	case *types.Struct:
		for i := 0; i < typ.NumFields(); i++ {
			result = append(result, typ.Field(i))
		}
	default:
	}
	mset := typeutil.IntuitiveMethodSet(obj.Type(), nil)
	for _, m := range mset {
		result = append(result, m.Obj())
	}
	return result
}

func posToString(pos token.Position) string {
	const prefix = "$GOROOT"
	filename := pos.Filename
	if strings.HasPrefix(filename, prefix) {
		suffix := strings.TrimPrefix(filename, prefix)
		filename = runtime.GOROOT() + suffix
	}
	return fmt.Sprintf("%v:%v:%v", filename, pos.Line, pos.Column)
}
