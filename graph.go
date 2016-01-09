package main

import (
	"encoding/json"
	"fmt"
	"go/build"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/tools/go/loader"

	"sourcegraph.com/sourcegraph/srclib-go/gog"
	"sourcegraph.com/sourcegraph/srclib-go/gog/definfo"
	defpkg "sourcegraph.com/sourcegraph/srclib-go/golang_def"
	"sourcegraph.com/sourcegraph/srclib/graph"
	"sourcegraph.com/sourcegraph/srclib/unit"
)

func init() {
	_, err := parser.AddCommand("graph",
		"graph a Go package",
		"Graph a Go package, producing all defs, refs, and docs.",
		&graphCmd,
	)
	if err != nil {
		log.Fatal(err)
	}

	// Check that we have the '-i' flag.
	cmd := exec.Command("go", "help", "build")
	o, err := cmd.Output()
	if err != nil {
		log.Fatal(err)
	}
	usage := strings.Split(string(o), "\n")[0] // The usage is on the first line.
	matched, err := regexp.MatchString("-i", usage)
	if err != nil {
		log.Fatal(err)
	}
	if !matched {
		log.Fatal("'go build' does not have the '-i' flag. Please upgrade to go1.3+.")
	}
}

type GraphCmd struct{}

var graphCmd GraphCmd

// allowErrorsInGoGet is whether the grapher should continue after
// if `go get` fails.
var allowErrorsInGoGet = true

func (c *GraphCmd) Execute(args []string) error {
	var unit *unit.SourceUnit
	if err := json.NewDecoder(os.Stdin).Decode(&unit); err != nil {
		return err
	}
	if err := os.Stdin.Close(); err != nil {
		return err
	}

	if err := unmarshalTypedConfig(unit.Config); err != nil {
		return err
	}
	if err := config.apply(); err != nil {
		return err
	}

	out, err := Graph(unit)
	if err != nil {
		return err
	}

	// Make paths relative to repo.
	for _, gs := range out.Defs {
		if gs.File == "" {
			log.Printf("no file %+v", gs)
		}
		if gs.File != "" {
			gs.File = relPath(cwd, gs.File)
		}
	}
	for _, gr := range out.Refs {
		if gr.File != "" {
			gr.File = relPath(cwd, gr.File)
		}
	}
	for _, gd := range out.Docs {
		if gd.File != "" {
			gd.File = relPath(cwd, gd.File)
		}
	}

	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		return err
	}
	return nil
}

func relPath(base, path string) string {
	rp, err := filepath.Rel(evalSymlinks(base), evalSymlinks(path))
	if err != nil {
		log.Fatalf("Failed to make path %q relative to %q: %s", path, base, err)
	}
	return filepath.ToSlash(rp)
}

func Graph(unit *unit.SourceUnit) (*graph.Output, error) {
	pkg, err := UnitDataAsBuildPackage(unit)
	if err != nil {
		return nil, err
	}

	o, err := doGraph(pkg)
	if err != nil {
		return nil, err
	}

	o2 := graph.Output{}

	uri := string(unit.Repo)

	for _, gs := range o.Defs {
		d, err := convertGoDef(gs, uri)
		if err != nil {
			return nil, err
		}
		if d != nil {
			o2.Defs = append(o2.Defs, d)
		}
	}
	for _, gr := range o.Refs {
		r, err := convertGoRef(gr, uri)
		if err != nil {
			return nil, err
		}
		if r != nil {
			o2.Refs = append(o2.Refs, r)
		}
	}
	for _, gd := range o.Docs {
		d, err := convertGoDoc(gd, uri)
		if err != nil {
			return nil, err
		}
		if d != nil {
			o2.Docs = append(o2.Docs, d)
		}
	}

	return &o2, nil
}

func convertGoDef(gs *gog.Def, repoURI string) (*graph.Def, error) {
	resolvedTarget, err := ResolveDep(gs.DefKey.PackageImportPath, repoURI)
	if err != nil {
		return nil, err
	}
	path := filepath.ToSlash(pathOrDot(filepath.Join(gs.Path...)))
	treePath := treePath(strings.Replace(string(path), ".go", "", -1))
	if !graph.IsValidTreePath(treePath) {
		return nil, fmt.Errorf("'%s' is not a valid tree-path", treePath)
	}

	def := &graph.Def{
		DefKey: graph.DefKey{
			Unit:     resolvedTarget.ToUnit,
			UnitType: resolvedTarget.ToUnitType,
			Path:     path,
		},
		TreePath: treePath,

		Name: gs.Name,
		Kind: definfo.GeneralKindMap[gs.Kind],

		File:     filepath.ToSlash(gs.File),
		DefStart: gs.DeclSpan[0],
		DefEnd:   gs.DeclSpan[1],

		Exported: gs.DefInfo.Exported,
		Local:    !gs.DefInfo.Exported && !gs.DefInfo.PkgScope,
		Test:     strings.HasSuffix(gs.File, "_test.go"),
	}

	d := defpkg.DefData{
		PackageImportPath: gs.DefKey.PackageImportPath,
		DefInfo:           gs.DefInfo,
	}
	def.Data, err = json.Marshal(d)
	if err != nil {
		return nil, err
	}

	if def.File == "" {
		// some cgo defs have empty File; omit them
		return nil, nil
	}

	return def, nil
}

func convertGoRef(gr *gog.Ref, repoURI string) (*graph.Ref, error) {
	resolvedTarget, err := ResolveDep(gr.Def.PackageImportPath, repoURI)
	if err != nil {
		return nil, err
	}
	if resolvedTarget == nil {
		return nil, nil
	}

	return &graph.Ref{
		DefRepo:     filepath.ToSlash(uriOrEmpty(resolvedTarget.ToRepoCloneURL)),
		DefPath:     filepath.ToSlash(pathOrDot(filepath.Join(gr.Def.Path...))),
		DefUnit:     resolvedTarget.ToUnit,
		DefUnitType: resolvedTarget.ToUnitType,
		Def:         gr.IsDef,
		File:        filepath.ToSlash(gr.File),
		Start:       gr.Span[0],
		End:         gr.Span[1],
	}, nil
}

func convertGoDoc(gd *gog.Doc, repoURI string) (*graph.Doc, error) {
	var key graph.DefKey
	if gd.DefKey != nil {
		resolvedTarget, err := ResolveDep(gd.PackageImportPath, repoURI)
		if err != nil {
			return nil, err
		}
		key = graph.DefKey{
			Path:     filepath.ToSlash(pathOrDot(filepath.Join(gd.Path...))),
			Unit:     resolvedTarget.ToUnit,
			UnitType: resolvedTarget.ToUnitType,
		}
	}
	return &graph.Doc{
		DefKey: key,
		Format: gd.Format,
		Data:   gd.Data,
		File:   filepath.ToSlash(gd.File),
		Start:  gd.Span[0],
		End:    gd.Span[1],
	}, nil
}

func uriOrEmpty(cloneURL string) string {
	if cloneURL == "" {
		return ""
	}
	return graph.MakeURI(cloneURL)
}

func pathOrDot(path string) string {
	if path == "" {
		return "."
	}
	return path
}

func treePath(path string) string {
	if path == "" || path == "." {
		return string(".")
	}
	return "./" + path
}

// allowErrorsInGraph is whether the grapher should continue after
// encountering "reasonably common" errors (such as compile errors).
var allowErrorsInGraph = true

func doGraph(pkg *build.Package) (*gog.Output, error) {
	importPath := pkg.ImportPath
	importUnsafe := importPath == "unsafe"

	// Special-case: if this is a Cgo package, treat the CgoFiles as GoFiles or
	// else the character offsets will be junk.
	//
	// See https://codereview.appspot.com/86140043.
	loaderConfig.Build.CgoEnabled = false
	build.Default = *loaderConfig.Build
	if len(pkg.CgoFiles) > 0 {
		var allGoFiles []string
		allGoFiles = append(allGoFiles, pkg.GoFiles...)
		allGoFiles = append(allGoFiles, pkg.CgoFiles...)
		allGoFiles = append(allGoFiles, pkg.TestGoFiles...)
		for i, f := range allGoFiles {
			allGoFiles[i] = filepath.Join(cwd, pkg.Dir, f)
		}
		loaderConfig.CreateFromFilenames(pkg.ImportPath, allGoFiles...)
	} else {
		// Normal import
		loaderConfig.ImportWithTests(importPath)
	}

	if importUnsafe {
		// Special-case "unsafe" because go/loader does not let you load it
		// directly.
		if loaderConfig.ImportPkgs == nil {
			loaderConfig.ImportPkgs = make(map[string]bool)
		}
		loaderConfig.ImportPkgs["unsafe"] = true
	}

	prog, err := loaderConfig.Load()
	if err != nil {
		log.Println("XXX", err)
		return nil, err
	}

	g := gog.New(prog)

	var pkgs []*loader.PackageInfo
	for _, pkg := range prog.Created {
		if strings.HasSuffix(pkg.Pkg.Name(), "_test") {
			// ignore xtest packages
			continue
		}
		pkgs = append(pkgs, pkg)
	}
	for _, pkg := range prog.Imported {
		pkgs = append(pkgs, pkg)
	}

	for _, pkg := range pkgs {
		if err := g.Graph(pkg); err != nil {
			return nil, err
		}
	}

	return &g.Output, nil
}
