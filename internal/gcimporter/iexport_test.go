// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This is a copy of bexport_test.go for iexport.go.

//go:build go1.11
// +build go1.11

package gcimporter_test

import (
	"bufio"
	"bytes"
	"fmt"
	"go/ast"
	"go/build"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"golang.org/x/tools/go/ast/inspector"
	"golang.org/x/tools/go/buildutil"
	"golang.org/x/tools/go/gcexportdata"
	"golang.org/x/tools/go/loader"
	"github.com/gnoverse/gnopls/internal/aliases"
	"github.com/gnoverse/gnopls/internal/gcimporter"
	"github.com/gnoverse/gnopls/internal/testenv"
	"github.com/gnoverse/gnopls/internal/typeparams/genericfeatures"
)

func readExportFile(filename string) ([]byte, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	buf := bufio.NewReader(f)
	if _, _, err := gcimporter.FindExportData(buf); err != nil {
		return nil, err
	}

	if ch, err := buf.ReadByte(); err != nil {
		return nil, err
	} else if ch != 'i' {
		return nil, fmt.Errorf("unexpected byte: %v", ch)
	}

	return io.ReadAll(buf)
}

func iexport(fset *token.FileSet, version int, pkg *types.Package) ([]byte, error) {
	var buf bytes.Buffer
	const bundle, shallow = false, false
	if err := gcimporter.IExportCommon(&buf, fset, bundle, shallow, version, []*types.Package{pkg}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// isUnifiedBuilder reports whether we are executing on a go builder that uses
// unified export data.
func isUnifiedBuilder() bool {
	return os.Getenv("GO_BUILDER_NAME") == "linux-amd64-unified"
}

const minStdlibPackages = 248

func TestIExportData_stdlib(t *testing.T) {
	if runtime.Compiler == "gccgo" {
		t.Skip("gccgo standard library is inaccessible")
	}
	testenv.NeedsGoBuild(t)
	if isRace {
		t.Skipf("stdlib tests take too long in race mode and flake on builders")
	}
	if testing.Short() {
		t.Skip("skipping RAM hungry test in -short mode")
	}

	// Load, parse and type-check the program.
	ctxt := build.Default // copy
	ctxt.GOPATH = ""      // disable GOPATH
	conf := loader.Config{
		Build:       &ctxt,
		AllowErrors: true,
		TypeChecker: types.Config{
			Sizes: types.SizesFor(ctxt.Compiler, ctxt.GOARCH),
			Error: func(err error) { t.Log(err) },
		},
	}
	for _, path := range buildutil.AllPackages(conf.Build) {
		conf.Import(path)
	}

	// Create a package containing type and value errors to ensure
	// they are properly encoded/decoded.
	f, err := conf.ParseFile("haserrors/haserrors.go", `package haserrors
const UnknownValue = "" + 0
type UnknownType undefined
`)
	if err != nil {
		t.Fatal(err)
	}
	conf.CreateFromFiles("haserrors", f)

	prog, err := conf.Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	var sorted []*types.Package
	isUnified := isUnifiedBuilder()
	for pkg, info := range prog.AllPackages {
		// Temporarily skip packages that use generics on the unified builder, to
		// fix TryBots.
		//
		// TODO(#48595): fix this test with GOEXPERIMENT=unified.
		inspect := inspector.New(info.Files)
		features := genericfeatures.ForPackage(inspect, &info.Info)
		if isUnified && features != 0 {
			t.Logf("skipping package %q which uses generics", pkg.Path())
			continue
		}
		if info.Files != nil { // non-empty directory
			sorted = append(sorted, pkg)
		}
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Path() < sorted[j].Path()
	})

	version := gcimporter.IExportVersion
	numPkgs := len(sorted)
	if want := minStdlibPackages; numPkgs < want {
		t.Errorf("Loaded only %d packages, want at least %d", numPkgs, want)
	}

	// TODO(adonovan): opt: parallelize this slow loop.
	for _, pkg := range sorted {
		if exportdata, err := iexport(conf.Fset, version, pkg); err != nil {
			t.Error(err)
		} else {
			testPkgData(t, conf.Fset, version, pkg, exportdata)
		}

		if pkg.Name() == "main" || pkg.Name() == "haserrors" {
			// skip; no export data
		} else if bp, err := ctxt.Import(pkg.Path(), "", build.FindOnly); err != nil {
			t.Log("warning:", err)
		} else if exportdata, err := readExportFile(bp.PkgObj); err != nil {
			t.Log("warning:", err)
		} else {
			testPkgData(t, conf.Fset, version, pkg, exportdata)
		}
	}

	var bundle bytes.Buffer
	if err := gcimporter.IExportBundle(&bundle, conf.Fset, sorted); err != nil {
		t.Fatal(err)
	}
	fset2 := token.NewFileSet()
	imports := make(map[string]*types.Package)
	pkgs2, err := gcimporter.IImportBundle(fset2, imports, bundle.Bytes())
	if err != nil {
		t.Fatal(err)
	}

	for i, pkg := range sorted {
		testPkg(t, conf.Fset, version, pkg, fset2, pkgs2[i])
	}
}

func testPkgData(t *testing.T, fset *token.FileSet, version int, pkg *types.Package, exportdata []byte) {
	imports := make(map[string]*types.Package)
	fset2 := token.NewFileSet()
	_, pkg2, err := gcimporter.IImportData(fset2, imports, exportdata, pkg.Path())
	if err != nil {
		t.Errorf("IImportData(%s): %v", pkg.Path(), err)
	}

	testPkg(t, fset, version, pkg, fset2, pkg2)
}

func testPkg(t *testing.T, fset *token.FileSet, version int, pkg *types.Package, fset2 *token.FileSet, pkg2 *types.Package) {
	if _, err := iexport(fset2, version, pkg2); err != nil {
		t.Errorf("reexport %q: %v", pkg.Path(), err)
	}

	// Compare the packages' corresponding members.
	for _, name := range pkg.Scope().Names() {
		if !token.IsExported(name) {
			continue
		}
		obj1 := pkg.Scope().Lookup(name)
		obj2 := pkg2.Scope().Lookup(name)
		if obj2 == nil {
			t.Errorf("%s.%s not found, want %s", pkg.Path(), name, obj1)
			continue
		}

		fl1 := fileLine(fset, obj1)
		fl2 := fileLine(fset2, obj2)
		if fl1 != fl2 {
			t.Errorf("%s.%s: got posn %s, want %s",
				pkg.Path(), name, fl2, fl1)
		}

		if err := cmpObj(obj1, obj2); err != nil {
			t.Errorf("%s.%s: %s\ngot:  %s\nwant: %s",
				pkg.Path(), name, err, obj2, obj1)
		}
	}
}

// TestIExportData_long tests the position of an import object declared in
// a very long input file.  Line numbers greater than maxlines are
// reported as line 1, not garbage or token.NoPos.
func TestIExportData_long(t *testing.T) {
	// parse and typecheck
	longFile := "package foo" + strings.Repeat("\n", 123456) + "var X int"
	fset1 := token.NewFileSet()
	f, err := parser.ParseFile(fset1, "foo.go", longFile, 0)
	if err != nil {
		t.Fatal(err)
	}
	var conf types.Config
	pkg, err := conf.Check("foo", fset1, []*ast.File{f}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// export
	exportdata, err := iexport(fset1, gcimporter.IExportVersion, pkg)
	if err != nil {
		t.Fatal(err)
	}

	// import
	imports := make(map[string]*types.Package)
	fset2 := token.NewFileSet()
	_, pkg2, err := gcimporter.IImportData(fset2, imports, exportdata, pkg.Path())
	if err != nil {
		t.Fatalf("IImportData(%s): %v", pkg.Path(), err)
	}

	// compare
	posn1 := fset1.Position(pkg.Scope().Lookup("X").Pos())
	posn2 := fset2.Position(pkg2.Scope().Lookup("X").Pos())
	if want := "foo.go:1:1"; posn2.String() != want {
		t.Errorf("X position = %s, want %s (orig was %s)",
			posn2, want, posn1)
	}
}

func TestIExportData_typealiases(t *testing.T) {
	// parse and typecheck
	fset1 := token.NewFileSet()
	f, err := parser.ParseFile(fset1, "p.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var conf types.Config
	pkg1, err := conf.Check("p", fset1, []*ast.File{f}, nil)
	if err == nil {
		// foo in undeclared in src; we should see an error
		t.Fatal("invalid source type-checked without error")
	}
	if pkg1 == nil {
		// despite incorrect src we should see a (partially) type-checked package
		t.Fatal("nil package returned")
	}
	checkPkg(t, pkg1, "export")

	// export
	// use a nil fileset here to confirm that it doesn't panic
	exportdata, err := iexport(nil, gcimporter.IExportVersion, pkg1)
	if err != nil {
		t.Fatal(err)
	}

	// import
	imports := make(map[string]*types.Package)
	fset2 := token.NewFileSet()
	_, pkg2, err := gcimporter.IImportData(fset2, imports, exportdata, pkg1.Path())
	if err != nil {
		t.Fatalf("IImportData(%s): %v", pkg1.Path(), err)
	}
	checkPkg(t, pkg2, "import")
}

// cmpObj reports how x and y differ. They are assumed to belong to different
// universes so cannot be compared directly. It is an adapted version of
// equalObj in bexport_test.go.
func cmpObj(x, y types.Object) error {
	if reflect.TypeOf(x) != reflect.TypeOf(y) {
		return fmt.Errorf("%T vs %T", x, y)
	}
	xt := x.Type()
	yt := y.Type()
	switch x := x.(type) {
	case *types.Var, *types.Func:
		// ok
	case *types.Const:
		xval := x.Val()
		yval := y.(*types.Const).Val()
		equal := constant.Compare(xval, token.EQL, yval)
		if !equal {
			// try approx. comparison
			xkind := xval.Kind()
			ykind := yval.Kind()
			if xkind == constant.Complex || ykind == constant.Complex {
				equal = same(constant.Real(xval), constant.Real(yval)) &&
					same(constant.Imag(xval), constant.Imag(yval))
			} else if xkind == constant.Float || ykind == constant.Float {
				equal = same(xval, yval)
			} else if xkind == constant.Unknown && ykind == constant.Unknown {
				equal = true
			}
		}
		if !equal {
			return fmt.Errorf("unequal constants %s vs %s", xval, yval)
		}
	case *types.TypeName:
		if xalias, yalias := x.IsAlias(), y.(*types.TypeName).IsAlias(); xalias != yalias {
			return fmt.Errorf("mismatching IsAlias(): %s vs %s", x, y)
		}

		// equalType does not recurse into the underlying types of named types, so
		// we must pass the underlying type explicitly here. However, in doing this
		// we may skip checking the features of the named types themselves, in
		// situations where the type name is not referenced by the underlying or
		// any other top-level declarations. Therefore, we must explicitly compare
		// named types here, before passing their underlying types into equalType.
		xn, _ := aliases.Unalias(xt).(*types.Named)
		yn, _ := aliases.Unalias(yt).(*types.Named)
		if (xn == nil) != (yn == nil) {
			return fmt.Errorf("mismatching types: %T vs %T", xt, yt)
		}
		if xn != nil {
			if err := cmpNamed(xn, yn); err != nil {
				return err
			}
		}
		xt = xt.Underlying()
		yt = yt.Underlying()
	default:
		return fmt.Errorf("unexpected %T", x)
	}
	return equalType(xt, yt)
}

// Use the same floating-point precision (512) as cmd/compile
// (see Mpprec in cmd/compile/internal/gc/mpfloat.go).
const mpprec = 512

// same compares non-complex numeric values and reports if they are approximately equal.
func same(x, y constant.Value) bool {
	xf := constantToFloat(x)
	yf := constantToFloat(y)
	d := new(big.Float).Sub(xf, yf)
	d.Abs(d)
	eps := big.NewFloat(1.0 / (1 << (mpprec - 1))) // allow for 1 bit of error
	return d.Cmp(eps) < 0
}

// copy of the function with the same name in iexport.go.
func constantToFloat(x constant.Value) *big.Float {
	var f big.Float
	f.SetPrec(mpprec)
	if v, exact := constant.Float64Val(x); exact {
		// float64
		f.SetFloat64(v)
	} else if num, denom := constant.Num(x), constant.Denom(x); num.Kind() == constant.Int {
		// TODO(gri): add big.Rat accessor to constant.Value.
		n := valueToRat(num)
		d := valueToRat(denom)
		f.SetRat(n.Quo(n, d))
	} else {
		// Value too large to represent as a fraction => inaccessible.
		// TODO(gri): add big.Float accessor to constant.Value.
		_, ok := f.SetString(x.ExactString())
		if !ok {
			panic("should not reach here")
		}
	}
	return &f
}

// copy of the function with the same name in iexport.go.
func valueToRat(x constant.Value) *big.Rat {
	// Convert little-endian to big-endian.
	// I can't believe this is necessary.
	bytes := constant.Bytes(x)
	for i := 0; i < len(bytes)/2; i++ {
		bytes[i], bytes[len(bytes)-1-i] = bytes[len(bytes)-1-i], bytes[i]
	}
	return new(big.Rat).SetInt(new(big.Int).SetBytes(bytes))
}

// This is a regression test for a bug in iexport of types.Struct:
// unexported fields were losing their implicit package qualifier.
func TestUnexportedStructFields(t *testing.T) {
	fset := token.NewFileSet()
	export := make(map[string][]byte)

	// process parses and type-checks a single-file
	// package and saves its export data.
	process := func(path, content string) {
		syntax, err := parser.ParseFile(fset, path+"/x.go", content, 0)
		if err != nil {
			t.Fatal(err)
		}
		packages := make(map[string]*types.Package) // keys are package paths
		cfg := &types.Config{
			Importer: importerFunc(func(path string) (*types.Package, error) {
				data, ok := export[path]
				if !ok {
					return nil, fmt.Errorf("missing export data for %s", path)
				}
				return gcexportdata.Read(bytes.NewReader(data), fset, packages, path)
			}),
		}
		pkg := types.NewPackage(path, syntax.Name.Name)
		check := types.NewChecker(cfg, fset, pkg, nil)
		if err := check.Files([]*ast.File{syntax}); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := gcexportdata.Write(&out, fset, pkg); err != nil {
			t.Fatal(err)
		}
		export[path] = out.Bytes()
	}

	// Historically this led to a spurious error:
	// "cannot convert a.M (variable of type a.MyTime) to type time.Time"
	// because the private fields of Time and MyTime were not identical.
	process("time", `package time; type Time struct { x, y int }`)
	process("a", `package a; import "time"; type MyTime time.Time; var M MyTime`)
	process("b", `package b; import ("a"; "time"); var _ = time.Time(a.M)`)
}

type importerFunc func(path string) (*types.Package, error)

func (f importerFunc) Import(path string) (*types.Package, error) { return f(path) }

// TestIExportDataTypeParameterizedAliases tests IExportData
// on both declarations and uses of type parameterized aliases.
func TestIExportDataTypeParameterizedAliases(t *testing.T) {
	testenv.NeedsGo1Point(t, 23)

	testenv.NeedsGoExperiment(t, "aliastypeparams")
	t.Setenv("GODEBUG", "gotypesalias=1")

	// High level steps:
	// * parse  and typecheck
	// * export the data for the importer (via IExportData),
	// * import the data (via either x/tools or GOROOT's gcimporter), and
	// * check the imported types.

	const src = `package a

type A[T any] = *T
type B[R any, S *R] = []S
type C[U any] = B[U, A[U]]

type Named int
type Chained = C[Named] // B[Named, A[Named]] = B[Named, *Named] = []*Named
`

	// parse and typecheck
	fset1 := token.NewFileSet()
	f, err := parser.ParseFile(fset1, "a", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var conf types.Config
	pkg1, err := conf.Check("a", fset1, []*ast.File{f}, nil)
	if err != nil {
		t.Fatal(err)
	}

	testcases := map[string]func(t *testing.T) *types.Package{
		// Read the result of IExportData through x/tools/internal/gcimporter.IImportData.
		"tools": func(t *testing.T) *types.Package {
			// export
			exportdata, err := iexport(fset1, gcimporter.IExportVersion, pkg1)
			if err != nil {
				t.Fatal(err)
			}

			// import
			imports := make(map[string]*types.Package)
			fset2 := token.NewFileSet()
			_, pkg2, err := gcimporter.IImportData(fset2, imports, exportdata, pkg1.Path())
			if err != nil {
				t.Fatalf("IImportData(%s): %v", pkg1.Path(), err)
			}
			return pkg2
		},
		// Read the result of IExportData through $GOROOT/src/internal/gcimporter.IImportData.
		//
		// This test fakes creating an old go object file in indexed format.
		// This means that it can be loaded by go/importer or go/types.
		// This step is not supported, but it does give test coverage for stdlib.
		"goroot": func(t *testing.T) *types.Package {
			// Write indexed export data file contents.
			//
			// TODO(taking): Slightly unclear to what extent this step should be supported by go/importer.
			var buf bytes.Buffer
			buf.WriteString("go object \n$$B\n") // object file header
			if err := gcexportdata.Write(&buf, fset1, pkg1); err != nil {
				t.Fatal(err)
			}

			// Write export data to temporary file
			out := t.TempDir()
			name := filepath.Join(out, "a.out")
			if err := os.WriteFile(name+".a", buf.Bytes(), 0644); err != nil {
				t.Fatal(err)
			}
			pkg2, err := importer.Default().Import(name)
			if err != nil {
				t.Fatal(err)
			}
			return pkg2
		},
	}

	for name, importer := range testcases {
		t.Run(name, func(t *testing.T) {
			pkg := importer(t)

			obj := pkg.Scope().Lookup("A")
			if obj == nil {
				t.Fatalf("failed to find %q in package %s", "A", pkg)
			}

			// Check that A is type A[T any] = *T.
			// TODO(taking): fix how go/types prints parameterized aliases to simplify tests.
			alias, ok := obj.Type().(*aliases.Alias)
			if !ok {
				t.Fatalf("Obj %s is not an Alias", obj)
			}

			targs := aliases.TypeArgs(alias)
			if targs.Len() != 0 {
				t.Errorf("%s has %d type arguments. expected 0", alias, targs.Len())
			}

			tparams := aliases.TypeParams(alias)
			if tparams.Len() != 1 {
				t.Fatalf("%s has %d type arguments. expected 1", alias, targs.Len())
			}
			tparam := tparams.At(0)
			if got, want := tparam.String(), "T"; got != want {
				t.Errorf("(%q).TypeParams().At(0)=%q. want %q", alias, got, want)
			}

			anyt := types.Universe.Lookup("any").Type()
			if c := tparam.Constraint(); !types.Identical(anyt, c) {
				t.Errorf("(%q).Constraint()=%q. expected %q", tparam, c, anyt)
			}

			ptparam := types.NewPointer(tparam)
			if rhs := aliases.Rhs(alias); !types.Identical(ptparam, rhs) {
				t.Errorf("(%q).Rhs()=%q. expected %q", alias, rhs, ptparam)
			}

			// TODO(taking): add tests for B and C once it is simpler to write tests.

			chained := pkg.Scope().Lookup("Chained")
			if chained == nil {
				t.Fatalf("failed to find %q in package %s", "Chained", pkg)
			}

			named, _ := pkg.Scope().Lookup("Named").(*types.TypeName)
			if named == nil {
				t.Fatalf("failed to find %q in package %s", "Named", pkg)
			}

			want := types.NewSlice(types.NewPointer(named.Type()))
			if got := chained.Type(); !types.Identical(got, want) {
				t.Errorf("(%q).Type()=%q which should be identical to %q", chained, got, want)
			}
		})
	}
}
