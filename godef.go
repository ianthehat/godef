package main

import (
	"bufio"
	"bytes"
	"context"
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

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "godef: %v\n", err)
		os.Exit(2)
	}
}

func run(ctx context.Context) error {
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
		return fmt.Errorf("Expressions not yet supported `%v`", flag.Arg(0))
	}
	//TODO: types.Debug = *debug

	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err != nil {
			return err
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			return err
		}
		// NB: profile won't be written in case of error.
		defer pprof.StopCPUProfile()
	}

	if *traceFlag != "" {
		f, err := os.Create(*traceFlag)
		if err != nil {
			return err
		}
		if err := trace.Start(f); err != nil {
			return err
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
			return err
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
			return fmt.Errorf("%v", err)
		}
		filename, src, searchpos = afile.name, afile.body, afile.offset
	} else if filename == "" {
		// TODO if there's no filename, look in the current
		// directory and do something plausible.
		return fmt.Errorf("A filename must be specified")
	} else if *readStdin {
		src, _ = ioutil.ReadAll(os.Stdin)
	}
	if searchpos < 0 {
		fmt.Fprintf(os.Stderr, "no expression or offset specified\n")
		flag.Usage()
		os.Exit(2)
	}
	// Load, parse, and type-check the packages named on the command line.
	cfg := &packages.Config{
		Context: ctx,
		Tests:   strings.HasSuffix(filename, "_test.go"),
	}
	fset, obj, err := godef(cfg, filename, src, searchpos)
	if err != nil {
		return err
	}
	// print old source location to facilitate backtracking
	if *acmeFlag {
		fmt.Printf("\t%s:#%d\n", afile.name, afile.runeOffset)
	}

	return done(fset, obj, func(p *types.Package) string {
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

func godef(cfg *packages.Config, filename string, src []byte, searchpos int) (*token.FileSet, types.Object, error) {
	parser, result := parseFile(filename, searchpos)
	// Load, parse, and type-check the packages named on the command line.
	if src != nil {
		cfg.Overlay = map[string][]byte{
			filename: src,
		}
	}
	cfg.Mode = packages.LoadSyntax
	cfg.ParseFile = parser
	lpkgs, err := packages.Load(cfg, "file="+filename)
	if err != nil {
		return nil, nil, err
	}
	if len(lpkgs) < 1 {
		return nil, nil, fmt.Errorf("There must be at least one package that contains the file")
	}
	// get the node
	var m match
	select {
	case m = <-result:
	default:
		return nil, nil, fmt.Errorf("no file found at search pos %d", searchpos)
	}
	if m.ident == nil {
		return nil, nil, fmt.Errorf("Offset %d was not a valid identifier", searchpos)
	}
	obj := lpkgs[0].TypesInfo.ObjectOf(m.ident)
	if obj == nil {
		return nil, nil, fmt.Errorf("no object")
	}
	if m.wasEmbeddedField {
		// the original position was on the embedded field declaration
		// so we try to dig out the type and jump to that instead
		if v, ok := obj.(*types.Var); ok {
			if n, ok := v.Type().(*types.Named); ok {
				obj = n.Obj()
			}
		}
	}
	return lpkgs[0].Fset, obj, nil
}

// match returns the ident plus any extra information needed
type match struct {
	ident            *ast.Ident
	wasEmbeddedField bool
}

// parseFile returns a function that can be used as a Parser in packages.Config.
// It replaces the contents of a file that matches filename with the src.
// It also drops all function bodies that do not contain the searchpos.
// It also modifies the filename to be the canonical form that will appear in the fileset.
func parseFile(filename string, searchpos int) (func(*token.FileSet, string, []byte) (*ast.File, error), chan match) {
	result := make(chan match, 1)
	isInputFile := newFileCompare(filename)
	return func(fset *token.FileSet, fname string, filedata []byte) (*ast.File, error) {
		isInput := isInputFile(fname)
		file, err := parser.ParseFile(fset, fname, filedata, 0)
		if file == nil {
			return nil, err
		}
		pos := token.Pos(-1)
		if isInput {
			tfile := fset.File(file.Pos())
			if tfile == nil {
				return file, fmt.Errorf("cursor %d is beyond end of file %s (%d)", searchpos, fname, file.End()-file.Pos())
			}
			if searchpos > tfile.Size() {
				return file, fmt.Errorf("cursor %d is beyond end of file %s (%d)", searchpos, fname, tfile.Size())
			}
			pos = tfile.Pos(searchpos)
			m, err := findMatch(file, pos)
			if err != nil {
				return nil, err
			}
			result <- m
		}
		trimAST(file, pos)
		return file, err
	}, result
}

func newFileCompare(filename string) func(string) bool {
	fstat, fstatErr := os.Stat(filename)
	return func(compare string) bool {
		if filename == compare {
			return true
		}
		if fstatErr != nil {
			return false
		}
		if s, err := os.Stat(compare); err == nil {
			return os.SameFile(fstat, s)
		}
		return false
	}
}

func findMatch(f *ast.File, pos token.Pos) (match, error) {
	m, err := checkMatch(f, pos)
	if err != nil {
		return match{}, err
	}
	if m.ident != nil {
		return m, nil
	}
	// If the position is not an identifier but immediately follows
	// an identifier or selector period (as is common when
	// requesting a completion), use the path to the preceding node.
	return checkMatch(f, pos-1)
}

// checkMatch checks a single position for a potential identifier.
func checkMatch(f *ast.File, pos token.Pos) (match, error) {
	path, _ := astutil.PathEnclosingInterval(f, pos, pos)
	result := match{}
	if path == nil {
		return result, fmt.Errorf("can't find node enclosing position")
	}
	switch node := path[0].(type) {
	case *ast.Ident:
		result.ident = node
	case *ast.SelectorExpr:
		result.ident = node.Sel
	}
	if result.ident != nil {
		for _, n := range path[1:] {
			if field, ok := n.(*ast.Field); ok {
				result.wasEmbeddedField = len(field.Names) == 0
			}
		}
	}
	return result, nil
}

func trimAST(file *ast.File, pos token.Pos) {
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			return false
		}
		if pos < n.Pos() || pos >= n.End() {
			switch n := n.(type) {
			case *ast.FuncDecl:
				n.Body = nil
			case *ast.BlockStmt:
				n.List = nil
			case *ast.CaseClause:
				n.Body = nil
			case *ast.CommClause:
				n.Body = nil
			case *ast.CompositeLit:
				// Leave elts in place for [...]T
				// array literals, because they can
				// affect the expression's type.
				if !isEllipsisArray(n.Type) {
					n.Elts = nil
				}
			}
		}
		return true
	})
}

func isEllipsisArray(n ast.Expr) bool {
	at, ok := n.(*ast.ArrayType)
	if !ok {
		return false
	}
	_, ok = at.Len.(*ast.Ellipsis)
	return ok
}

func objToPos(fSet *token.FileSet, obj types.Object) token.Position {
	p := obj.Pos()
	f := fSet.File(p)
	pos := f.Position(p)
	if pos.Column != 1 {
		return pos
	}
	// currently exportdata does not store the column
	// until it does, we have a hacky fix to attempt to find the name within
	// the line and patch the column to match
	named, ok := obj.(interface{ Name() string })
	if !ok {
		return pos
	}
	in, err := os.Open(f.Name())
	if err != nil {
		return pos
	}
	for l, scanner := 1, bufio.NewScanner(in); scanner.Scan(); l++ {
		if l < pos.Line {
			continue
		}
		col := bytes.Index([]byte(scanner.Text()), []byte(named.Name()))
		if col >= 0 {
			pos.Column = col + 1
		}
		break
	}
	return pos
}

type orderedObjects []types.Object

func (o orderedObjects) Less(i, j int) bool { return o[i].Name() < o[j].Name() }
func (o orderedObjects) Len() int           { return len(o) }
func (o orderedObjects) Swap(i, j int)      { o[i], o[j] = o[j], o[i] }

func done(fSet *token.FileSet, obj types.Object, q types.Qualifier) error {
	pos := objToPos(fSet, obj)
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
			return fmt.Errorf("JSON marshal error: %v", err)
		}
		fmt.Printf("%s\n", jsonStr)
		return nil
	} else {
		fmt.Printf("%v\n", pos)
	}
	if !*tflag {
		return nil
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
	return nil
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
