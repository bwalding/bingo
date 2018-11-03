package langserver

import (
	"context"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"go/types"
	"golang.org/x/tools/go/packages"
	"path"
	"path/filepath"
	"strings"

	"github.com/opentracing/opentracing-go"

	"github.com/saibing/bingo/langserver/util"

	"github.com/saibing/bingo/pkg/lsp"
	"github.com/sourcegraph/jsonrpc2"

	"golang.org/x/tools/go/buildutil"
	"golang.org/x/tools/go/loader"
)

func posForFileOffset(fset *token.FileSet, filename string, offset int) token.Pos {
	var f *token.File
	fset.Iterate(func(ff *token.File) bool {
		if util.PathEqual(ff.Name(), filename) {
			f = ff
			return false // break out of loop
		}
		return true
	})
	if f == nil {
		return token.NoPos
	}
	return f.Pos(offset)
}

// buildPackageForNamedFileInMultiPackageDir returns a package that
// refer to the package named by filename. If there are multiple
// (e.g.) main packages in a dir in separate files, this lets you
// synthesize a *packages.Package that just refers to one. It's necessary
// to handle that case.
func buildPackageForNamedFileInMultiPackageDir(bpkg *packages.Package, m *build.MultiplePackageError, filename string) (*packages.Package, error) {
	copy := *bpkg
	bpkg = &copy

	// First, find which package name each filename is in.
	fileToPkgName := make(map[string]string, len(m.Files))
	for i, f := range m.Files {
		fileToPkgName[f] = m.Packages[i]
	}

	pkgName := fileToPkgName[filename]
	if pkgName == "" {
		return nil, fmt.Errorf("package %q in %s has no file %q", bpkg.PkgPath, filepath.Dir(filename), filename)
	}

	filterToFilesInPackage := func(files []string, pkgName string) []string {
		var keep []string
		for _, f := range files {
			if fileToPkgName[f] == pkgName {
				keep = append(keep, f)
			}
		}
		return keep
	}

	// Trim the *GoFiles fields to only those files in the same
	// package.
	bpkg.Name = pkgName
	if pkgName == "main" {
		// TODO(sqs): If the package name is "main", and there are
		// multiple main packages that are separate programs (and,
		// e.g., expected to be run directly run `go run main1.go
		// main2.go`), then this will break because it will try to
		// compile them all together. There's no good way to handle
		// that case that I can think of, other than with heuristics.
	}
	var nonXTestPkgName string
	if strings.HasSuffix(pkgName, "_test") {
		nonXTestPkgName = strings.TrimSuffix(pkgName, "_test")
	} else {
		nonXTestPkgName = pkgName
	}
	bpkg.GoFiles = filterToFilesInPackage(bpkg.GoFiles, nonXTestPkgName)
	return bpkg, nil
}

type typecheckKey struct {
	importPath, srcDir, name string

	// TODO(sqs): needs to include a list of files in the key...there
	// can be multiple packages (e.g., build-tag-disabled main.go
	// files) with the same names

	// TODO(sqs): include build context in key
}

type typecheckResult struct {
	fset *token.FileSet
	prog *loader.Program
	err  error
}

func (h *LangHandler) cachedTypecheck(ctx context.Context, bctx *build.Context, bpkg *build.Package) (*token.FileSet, *loader.Program, diagnostics, error) {
	parentSpan := opentracing.SpanFromContext(ctx)
	span := parentSpan.Tracer().StartSpan("langserver-go: typecheck",
		opentracing.Tags{"pkg": bpkg.ImportPath},
		opentracing.ChildOf(parentSpan.Context()),
	)
	ctx = opentracing.ContextWithSpan(ctx, span)
	defer span.Finish()

	var diags diagnostics
	r := h.typecheckCache.Get(typecheckKey{bpkg.ImportPath, bpkg.Dir, bpkg.Name}, func() interface{} {
		res := &typecheckResult{
			fset: token.NewFileSet(),
		}
		res.prog, diags, res.err = typecheck(ctx, res.fset, bctx, bpkg, h.getFindPackageFunc())
		return res
	})
	if r == nil {
		// This can happen if we panic
		return nil, nil, diags, nil
	}
	res := r.(*typecheckResult)
	return res.fset, res.prog, diags, res.err
}

// TODO(sqs): allow typechecking just a specific file not in a package, too
func typecheck(ctx context.Context, fset *token.FileSet, bctx *build.Context, bpkg *build.Package, findPackage FindPackageFunc) (*loader.Program, diagnostics, error) {
	var typeErrs []error
	conf := loader.Config{
		Fset: fset,
		TypeChecker: types.Config{
			DisableUnusedImportCheck: true,
			FakeImportC:              true,
			Error: func(err error) {
				typeErrs = append(typeErrs, err)
			},
		},
		Build:       bctx,
		Cwd:         bpkg.Dir,
		AllowErrors: true,
		TypeCheckFuncBodies: func(p string) bool {
			return bpkg.ImportPath == p
		},
		ParserMode: parser.AllErrors | parser.ParseComments, // prevent parser from bailing out
		FindPackage: func(bctx *build.Context, importPath, fromDir string, mode build.ImportMode) (*build.Package, error) {
			// When importing a package, ignore any
			// MultipleGoErrors. This occurs, e.g., when you have a
			// main.go with "// +build ignore" that imports the
			// non-main package in the same dir.
			bpkg, err := findPackage(ctx, bctx, importPath, fromDir, mode)
			if err != nil && !isMultiplePackageError(err) {
				return bpkg, err
			}
			return bpkg, nil
		},
	}

	// Hover needs this info, otherwise we could zero out the unnecessary
	// results to save memory.
	//
	// TODO(sqs): investigate other ways to speed this up using
	// AfterTypeCheck; see
	// https://sourcegraph.com/github.com/golang/tools@5ffc3249d341c947aa65178abbf2253ed49c9e03/-/blob/cmd/guru/referrers.go#L148.
	//
	// 	conf.AfterTypeCheck = func(info *loader.PackageInfo, files []*ast.File) {
	// 		if !conf.TypeCheckFuncBodies(info.Pkg.Path()) {
	// 			clearInfoFields(info)
	// 		}
	// 	}
	//

	var goFiles []string
	goFiles = append(goFiles, bpkg.GoFiles...)
	goFiles = append(goFiles, bpkg.TestGoFiles...)
	if strings.HasSuffix(bpkg.Name, "_test") {
		goFiles = append(goFiles, bpkg.XTestGoFiles...)
	}
	for i, filename := range goFiles {
		goFiles[i] = buildutil.JoinPath(bctx, bpkg.Dir, filename)
	}
	conf.CreateFromFilenames(bpkg.ImportPath, goFiles...)
	prog, err := conf.Load()
	if err != nil && prog == nil {
		return nil, nil, err
	}
	diags, err := errsToDiagnostics(typeErrs, prog)
	if err != nil {
		return nil, nil, err
	}
	return prog, diags, nil
}

func clearInfoFields(info *loader.PackageInfo) {
	// TODO(adonovan): opt: save memory by eliminating unneeded scopes/objects.
	// (Requires go/types change for Go 1.7.)
	//   info.Pkg.Scope().ClearChildren()

	// Discard the file ASTs and their accumulated type
	// information to save memory.
	info.Files = nil
	info.Defs = make(map[*ast.Ident]types.Object)
	info.Uses = make(map[*ast.Ident]types.Object)
	info.Implicits = make(map[ast.Node]types.Object)

	// Also, disable future collection of wholly unneeded
	// type information for the package in case there is
	// more type-checking to do (augmentation).
	info.Types = nil
	info.Scopes = nil
	info.Selections = nil
}

func isMultiplePackageError(err error) bool {
	_, ok := err.(*build.MultiplePackageError)
	return ok
}

func fsetToFiles(fset *token.FileSet) (files []string) {
	fset.Iterate(func(f *token.File) bool {
		files = append(files, f.Name())
		return true
	})
	return files
}


func (h *LangHandler) loadPackage(ctx context.Context, conn jsonrpc2.JSONRPC2, fileURI lsp.DocumentURI, position lsp.Position) (*packages.Package, token.Pos, error) {
	parentSpan := opentracing.SpanFromContext(ctx)
	span := parentSpan.Tracer().StartSpan("langserver-go: load program",
		opentracing.Tags{"fileURI": fileURI},
		opentracing.ChildOf(parentSpan.Context()),
	)
	ctx = opentracing.ContextWithSpan(ctx, span)
	defer span.Finish()

	start := token.NoPos
	if !util.IsURI(fileURI) {
		return nil, start, fmt.Errorf("typechecking of out-of-workspace URI (%q) is not yet supported", fileURI)
	}

	filename := h.FilePath(fileURI)

	bctx := h.BuildContext(ctx)
	pkg, err := h.load(ctx, bctx, conn, filename)
	if mpErr, ok := err.(*build.MultiplePackageError); ok {
		pkg, err = buildPackageForNamedFileInMultiPackageDir(pkg, mpErr, path.Base(filename))
		if err != nil {
			return nil, start, err
		}
	} else if err != nil {
		return nil, start, err
	}

	//isIgnoredFile := true
	//for _, f := range pkg.CompiledGoFiles {
	//	if path.Base(filename) == path.Base(f) {
	//		isIgnoredFile = false
	//		break
	//	}
	//}
	//
	//if isIgnoredFile {
	//	return nil, start, fmt.Errorf("file %s is ignored by the build", filename)
	//}

	// collect all loaded files, required to remove existing diagnostics from our cache
	//files := fsetToFiles(pkg.Fset)
	//if err := h.publishDiagnostics(ctx, conn, error2Diagnostics(pkg.Errors), files); err != nil {
	//	log.Printf("warning: failed to send diagnostics: %s.", err)
	//}

	contents, err := h.readFile(ctx, fileURI)
	if err != nil {
		return nil, start, err
	}
	offset, valid, why := offsetForPosition(contents, position)
	if !valid {
		return nil, start, fmt.Errorf("invalid position: %s:%d:%d (%s)", filename, position.Line, position.Character, why)
	}

	start = posForFileOffset(pkg.Fset, filename, offset)
	if start == token.NoPos {
		return nil, start, fmt.Errorf("invalid location: %s:#%d", filename, offset)
	}

	return pkg, start, nil
}

// ContainingPackageModule returns the package that contains the given
// filename. It is like buildutil.ContainingPackage, except that:
//
// * it returns the whole package (i.e., it doesn't use build.FindOnly)
// * it does not perform FS calls that are unnecessary for us (such
//   as searching the GOROOT; this is only called on the main
//   workspace's code, not its deps).
// * if the file is in the xtest package (package p_test not package p),
//   it returns build.Package only representing that xtest package
func (h *LangHandler) load(ctx context.Context, bctx *build.Context, conn jsonrpc2.JSONRPC2, filename string) (*packages.Package, error) {
	pkgDir := filename
	if !bctx.IsDir(filename) {
		pkgDir = path.Dir(filename)
	}

	return h.packageCache.Load(ctx, conn, pkgDir)
}

func error2Diagnostics(errorList []packages.Error) (diags diagnostics) {
	return
}