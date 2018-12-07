package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/build"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	debugpkg "runtime/debug"
	"strconv"
	"strings"

	"github.com/rogpeppe/godef/go/ast"
	"github.com/rogpeppe/godef/go/parser"
	"github.com/rogpeppe/godef/go/token"
	"github.com/rogpeppe/godef/go/types"
	"github.com/rogpeppe/godef/tool"
	"golang.org/x/tools/go/packages"
)

func main() {
	tool.Main(context.Background(), &Application{}, os.Args[1:])
}

type Application struct {
	// Add the basic profiling flags
	tool.Profile
	// All the command line flags
	ReadStdin     bool    `flag:"i" help:"read file from stdin"`
	Offset        int     `flag:"o" help:"file offset of identifier in stdin"`
	Debug         bool    `flag:"debug" help:"debug mode"`
	Type          bool    `flag:"t" help:"print type information"`
	Members       bool    `flag:"a" help:"print public type and member information"`
	All           bool    `flag:"A" help:"print all type and members information"`
	Filename      string  `flag:"f" help:"source filename"`
	Acme          bool    `flag:"acme" help:"use current acme window"`
	JSON          bool    `flag:"json" help:"output location in JSON format (-t flag is ignored)"`
	ForcePackages triBool `flag:"new-implementation" help:"force godef to use the new go/packages implentation"`

	expr string // The zeroth command line argument if present
}

// Name implements tool.Application returning the binary name.
func (app *Application) Name() string { return "godef" }

// Usage implements tool.Application returning empty extra argument usage.
func (app *Application) Usage() string { return "[expr]" }

// ShortHelp implements tool.Application returning the main binary help.
func (app *Application) ShortHelp() string {
	return "Go to definition for identifiers or package paths."
}

// DetailedHelp implements tool.Application returning the main binary help.
// This includes the short help for all the sub commands.
func (app *Application) DetailedHelp(f *flag.FlagSet) {
	f.PrintDefaults()
}

func (app *Application) prepare() {
	app.Members = app.Members || app.All
	app.Type = app.Type || app.Members
}

func (app *Application) Run(ctx context.Context, args ...string) error {
	// store the expression so that oldGoDef can pick them up again
	if len(args) > 0 {
		app.expr = args[0]
		//TODO: what if more args were passed?
	}

	app.prepare()

	// for most godef invocations we want to produce the result and quit without
	// ever triggering the GC, but we don't want to outright disable it for the
	// rare case when we are asked to handle a truly huge data set, so we set it
	// to a very large ratio. This number was picked to be significantly bigger
	// than needed to prevent GC on a common very large build, but is essentially
	// a magic number not a calculated one
	debugpkg.SetGCPercent(1600)

	types.Debug = app.Debug
	searchpos := app.Offset
	filename := app.Filename

	var afile *acmeFile
	var src []byte
	if app.Acme {
		var err error
		if afile, err = acmeCurrentFile(); err != nil {
			return fmt.Errorf("%v", err)
		}
		filename, src, searchpos = afile.name, afile.body, afile.offset
	} else if app.ReadStdin {
		src, _ = ioutil.ReadAll(os.Stdin)
	} else {
		// TODO if there's no filename, look in the current
		// directory and do something plausible.
		b, err := ioutil.ReadFile(filename)
		if err != nil {
			return fmt.Errorf("cannot read %s: %v", filename, err)
		}
		src = b
	}
	// Load, parse, and type-check the packages named on the command line.
	cfg := &packages.Config{
		Context: ctx,
		Tests:   strings.HasSuffix(filename, "_test.go"),
	}
	obj, err := app.godef(cfg, filename, src, searchpos)
	if err != nil {
		return err
	}

	// print old source location to facilitate backtracking
	if app.Acme {
		fmt.Printf("\t%s:#%d\n", afile.name, afile.runeOffset)
	}

	return app.print(os.Stdout, obj)
}

func (app *Application) oldGodef(filename string, src []byte, searchpos int) (*ast.Object, types.Type, error) {
	pkgScope := ast.NewScope(parser.Universe)
	f, err := parser.ParseFile(types.FileSet, filename, src, 0, pkgScope, types.DefaultImportPathToName)
	if f == nil {
		return nil, types.Type{}, fmt.Errorf("cannot parse %s: %v", filename, err)
	}

	var o ast.Node
	switch {
	case app.expr != "":
		o, err = parseExpr(f.Scope, app.expr)
		if err != nil {
			return nil, types.Type{}, err
		}

	case searchpos >= 0:
		o, err = findIdentifier(f, searchpos)
		if err != nil {
			return nil, types.Type{}, err
		}

	default:
		return nil, types.Type{}, fmt.Errorf("no expression or offset specified")
	}
	switch e := o.(type) {
	case *ast.ImportSpec:
		path, err := importPath(e)
		if err != nil {
			return nil, types.Type{}, err
		}
		pkg, err := build.Default.Import(path, filepath.Dir(filename), build.FindOnly)
		if err != nil {
			return nil, types.Type{}, fmt.Errorf("error finding import path for %s: %s", path, err)
		}
		return &ast.Object{Kind: ast.Pkg, Data: pkg.Dir}, types.Type{}, nil
	case ast.Expr:
		if !app.Type {
			// try local declarations only
			if obj, typ := types.ExprType(e, types.DefaultImporter, types.FileSet); obj != nil {
				return obj, typ, nil
			}
		}
		// add declarations from other files in the local package and try again
		pkg, err := parseLocalPackage(filename, f, pkgScope, types.DefaultImportPathToName)
		if pkg == nil && !app.Type {
			fmt.Printf("parseLocalPackage error: %v\n", err)
		}
		if app.expr != "" {
			// Reading declarations in other files might have
			// resolved the original expression.
			e, err = parseExpr(f.Scope, app.expr)
			if err != nil {
				return nil, types.Type{}, err
			}
		}
		if obj, typ := types.ExprType(e, types.DefaultImporter, types.FileSet); obj != nil {
			return obj, typ, nil
		}
		return nil, types.Type{}, fmt.Errorf("no declaration found for %v", pretty{e})
	}
	return nil, types.Type{}, nil
}

func importPath(n *ast.ImportSpec) (string, error) {
	p, err := strconv.Unquote(n.Path.Value)
	if err != nil {
		return "", fmt.Errorf("invalid string literal %q in ast.ImportSpec", n.Path.Value)
	}
	return p, nil
}

type nodeResult struct {
	node ast.Node
	err  error
}

// findIdentifier looks for an identifier at byte-offset searchpos
// inside the parsed source represented by node.
// If it is part of a selector expression, it returns
// that expression rather than the identifier itself.
//
// As a special case, if it finds an import
// spec, it returns ImportSpec.
//
func findIdentifier(f *ast.File, searchpos int) (ast.Node, error) {
	ec := make(chan nodeResult)
	found := func(startPos, endPos token.Pos) bool {
		start := types.FileSet.Position(startPos).Offset
		end := start + int(endPos-startPos)
		return start <= searchpos && searchpos <= end
	}
	go func() {
		var visit func(ast.Node) bool
		visit = func(n ast.Node) bool {
			var startPos token.Pos
			switch n := n.(type) {
			default:
				return true
			case *ast.Ident:
				startPos = n.NamePos
			case *ast.SelectorExpr:
				startPos = n.Sel.NamePos
			case *ast.ImportSpec:
				startPos = n.Pos()
			case *ast.StructType:
				// If we find an anonymous bare field in a
				// struct type, its definition points to itself,
				// but we actually want to go elsewhere,
				// so assume (dubiously) that the expression
				// works globally and return a new node for it.
				for _, field := range n.Fields.List {
					if field.Names != nil {
						continue
					}
					t := field.Type
					if pt, ok := field.Type.(*ast.StarExpr); ok {
						t = pt.X
					}
					if id, ok := t.(*ast.Ident); ok {
						if found(id.NamePos, id.End()) {
							expr, err := parseExpr(f.Scope, id.Name)
							ec <- nodeResult{expr, err}
							runtime.Goexit()
						}
					}
				}
				return true
			}
			if found(startPos, n.End()) {
				ec <- nodeResult{n, nil}
				runtime.Goexit()
			}
			return true
		}
		ast.Walk(FVisitor(visit), f)
		ec <- nodeResult{nil, nil}
	}()
	ev := <-ec
	if ev.err != nil {
		return nil, ev.err
	}
	if ev.node == nil {
		return nil, fmt.Errorf("no identifier found")
	}
	return ev.node, nil
}

type Position struct {
	Filename string `json:"filename,omitempty"`
	Line     int    `json:"line,omitempty"`
	Column   int    `json:"column,omitempty"`
}

type Kind string

const (
	BadKind    Kind = "bad"
	FuncKind   Kind = "func"
	VarKind    Kind = "var"
	ImportKind Kind = "import"
	ConstKind  Kind = "const"
	LabelKind  Kind = "label"
	TypeKind   Kind = "type"
	PathKind   Kind = "path"
)

type Object struct {
	Name     string
	Kind     Kind
	Pkg      string
	Position Position
	Members  []*Object
	Type     interface{}
	Value    interface{}
}

type orderedObjects []*Object

func (o orderedObjects) Less(i, j int) bool { return o[i].Name < o[j].Name }
func (o orderedObjects) Len() int           { return len(o) }
func (o orderedObjects) Swap(i, j int)      { o[i], o[j] = o[j], o[i] }

func (app *Application) print(out io.Writer, obj *Object) error {
	if obj.Kind == PathKind {
		fmt.Fprintf(out, "%s\n", obj.Value)
		return nil
	}
	if app.JSON {
		jsonStr, err := json.Marshal(obj.Position)
		if err != nil {
			return fmt.Errorf("JSON marshal error: %v", err)
		}
		fmt.Fprintf(out, "%s\n", jsonStr)
		return nil
	} else {
		fmt.Fprintf(out, "%v\n", obj.Position)
	}
	if obj.Kind == BadKind || !app.Type {
		return nil
	}
	fmt.Fprintf(out, "%s\n", typeStr(obj))
	if app.Members {
		for _, obj := range obj.Members {
			// Ignore unexported members unless app.A is set.
			if !app.All && (obj.Pkg != "" || !ast.IsExported(obj.Name)) {
				continue
			}
			fmt.Fprintf(out, "\t%s\n", strings.Replace(typeStr(obj), "\n", "\n\t\t", -1))
			fmt.Fprintf(out, "\t\t%v\n", obj.Position)
		}
	}
	return nil
}

func typeStr(obj *Object) string {
	buf := &bytes.Buffer{}
	valueFmt := " = %v"
	switch obj.Kind {
	case VarKind, FuncKind:
		// don't print these
	case ImportKind:
		valueFmt = " %v)"
		fmt.Fprint(buf, obj.Kind)
		fmt.Fprint(buf, " (")
	default:
		fmt.Fprint(buf, obj.Kind)
		fmt.Fprint(buf, " ")
	}
	fmt.Fprint(buf, obj.Name)
	if obj.Type != nil {
		fmt.Fprintf(buf, " %v", pretty{obj.Type})
	}
	if obj.Value != nil {
		fmt.Fprintf(buf, valueFmt, pretty{obj.Value})
	}
	return buf.String()
}

func (pos Position) Format(f fmt.State, c rune) {
	switch {
	case pos.Filename != "" && pos.Line > 0:
		fmt.Fprintf(f, "%s:%d:%d", pos.Filename, pos.Line, pos.Column)
	case pos.Line > 0:
		fmt.Fprintf(f, "%d:%d", pos.Line, pos.Column)
	case pos.Filename != "":
		fmt.Fprint(f, pos.Filename)
	default:
		fmt.Fprint(f, "-")
	}
}

func parseExpr(s *ast.Scope, expr string) (ast.Expr, error) {
	n, err := parser.ParseExpr(types.FileSet, "<arg>", expr, s, types.DefaultImportPathToName)
	if err != nil {
		return nil, fmt.Errorf("cannot parse expression: %v", err)
	}
	switch n := n.(type) {
	case *ast.Ident, *ast.SelectorExpr:
		return n, nil
	}
	return nil, fmt.Errorf("no identifier found in expression")
}

type FVisitor func(n ast.Node) bool

func (f FVisitor) Visit(n ast.Node) ast.Visitor {
	if f(n) {
		return f
	}
	return nil
}

var errNoPkgFiles = errors.New("no more package files found")

// parseLocalPackage reads and parses all go files from the
// current directory that implement the same package name
// the principal source file, except the original source file
// itself, which will already have been parsed.
//
func parseLocalPackage(filename string, src *ast.File, pkgScope *ast.Scope, pathToName parser.ImportPathToName) (*ast.Package, error) {
	pkg := &ast.Package{src.Name.Name, pkgScope, nil, map[string]*ast.File{filename: src}}
	d, f := filepath.Split(filename)
	if d == "" {
		d = "./"
	}
	fd, err := os.Open(d)
	if err != nil {
		return nil, errNoPkgFiles
	}
	defer fd.Close()

	list, err := fd.Readdirnames(-1)
	if err != nil {
		return nil, errNoPkgFiles
	}

	for _, pf := range list {
		file := filepath.Join(d, pf)
		if !strings.HasSuffix(pf, ".go") ||
			pf == f ||
			pkgName(file) != pkg.Name {
			continue
		}
		src, err := parser.ParseFile(types.FileSet, file, nil, 0, pkg.Scope, types.DefaultImportPathToName)
		if err == nil {
			pkg.Files[file] = src
		}
	}
	if len(pkg.Files) == 1 {
		return nil, errNoPkgFiles
	}
	return pkg, nil
}

// pkgName returns the package name implemented by the
// go source filename.
//
func pkgName(filename string) string {
	prog, _ := parser.ParseFile(types.FileSet, filename, nil, parser.PackageClauseOnly, nil, types.DefaultImportPathToName)
	if prog != nil {
		return prog.Name.Name
	}
	return ""
}

func hasSuffix(s, suff string) bool {
	return len(s) >= len(suff) && s[len(s)-len(suff):] == suff
}
