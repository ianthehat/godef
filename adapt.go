package main

// The contents of this file are designed to adapt between the two implementations
// of godef, and should be removed when we fully switch to the go/pacakges
// implementation for all cases

import (
	"flag"
	"fmt"
	"golang.org/x/tools/go/packages"

	rpast "github.com/rogpeppe/godef/go/ast"
	//rpparser "github.com/rogpeppe/godef/go/parser"
	//rpprinter "github.com/rogpeppe/godef/go/printer"
	//rptoken "github.com/rogpeppe/godef/go/token"
	rptypes "github.com/rogpeppe/godef/go/types"

	//goast "go/ast"
	//goparser "go/parser"
	//goprinter "go/printer"
	gotoken "go/token"
	gotypes "go/types"
)

var forcePackages = flag.Bool("force-packages", false, "force godef to use the go/packages implentation")

func adaptGodef(cfg *packages.Config, filename string, src []byte, searchpos int) (*rpast.Object, rptypes.Type, error) {
	if *forcePackages {
		fset, obj, err := godefPackages(cfg, filename, src, searchpos)
		if err != nil {
			return nil, rptypes.Type{}, err
		}
		return adaptObject(fset, obj)
	}
	return godef(filename, src, searchpos)
}

func adaptObject(fset *gotoken.FileSet, obj gotypes.Object) (*rpast.Object, rptypes.Type, error) {
	return nil, rptypes.Type{}, fmt.Errorf("adapter not written yet")
}