package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/types"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/tools/go/packages"
)

var (
	typeNames = flag.String("type", "", "comma-separated list of type names; must be set")
	output    = flag.String("output", "", "output file name; default srcdir/<type>_string.go")
	buildTags = flag.String("tags", "", "comma-separated list of build tags to apply")
)

// Usage is a replacement usage function for the flags package.
func Usage() {
	fmt.Fprintf(os.Stderr, "Usage of enum2kaitai:\n")
	fmt.Fprintf(os.Stderr, "\tenum2kaitai [flags] -type T [directory]\n")
	fmt.Fprintf(os.Stderr, "\tenum2kaitai [flags] -type T files... # Must be a single package\n")
	fmt.Fprintf(os.Stderr, "Flags:\n")
	flag.PrintDefaults()
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("enum2kaitai: ")
	flag.Usage = Usage
	flag.Parse()
	if len(*typeNames) == 0 {
		flag.Usage()
		os.Exit(2)
	}
	types := strings.Split(*typeNames, ",")
	var tags []string
	if len(*buildTags) > 0 {
		tags = strings.Split(*buildTags, ",")
	}

	// We accept either one directory or a list of files. Which do we have?
	args := flag.Args()
	if len(args) == 0 {
		// Default: process whole package in current directory.
		args = []string{"."}
	}

	// Parse the package once.
	var dir string
	g := Generator{namedTypeDeps: make(map[string]bool)}
	// TODO(suzmue): accept other patterns for packages (directories, list of files, import paths, etc).
	if len(args) == 1 && isDirectory(args[0]) {
		dir = args[0]
	} else {
		if len(tags) != 0 {
			log.Fatal("-tags option applies only to directories, not when files are specified")
		}
		dir = filepath.Dir(args[0])
	}

	g.parsePackage(args, tags)

	// Print the header and package clause.
	g.Printf("# Code generated by \"enum2kaitai %s\"; DO NOT EDIT.\n", strings.Join(os.Args[1:], " "))
	g.Printf("\n")

	// Run generate for each type.
	g.Printf("types:\n")
	for _, typeName := range types {
		g.generate(typeName)
	}

	// Display named type dependencies.
	var namedTypeDeps []string
	for namedTypeDep := range g.namedTypeDeps {
		namedTypeDeps = append(namedTypeDeps, namedTypeDep)
	}
	sort.Strings(namedTypeDeps)
	for _, namedTypeDep := range namedTypeDeps {
		log.Println("depends on named Go type:", namedTypeDep)
	}

	// Get output.
	src := g.buf.Bytes()

	// Write to file.
	outputName := *output
	if outputName == "" {
		baseName := fmt.Sprintf("%s_type.ksy", types[0])
		outputName = filepath.Join(dir, strings.ToLower(baseName))
	}
	err := ioutil.WriteFile(outputName, src, 0644)
	if err != nil {
		log.Fatalf("writing output: %s", err)
	}
}

// isDirectory reports whether the named file is a directory.
func isDirectory(name string) bool {
	info, err := os.Stat(name)
	if err != nil {
		log.Fatal(err)
	}
	return info.IsDir()
}

// Generator holds the state of the analysis. Primarily used to buffer
// the output for format.Source.
type Generator struct {
	buf           bytes.Buffer // Accumulated output.
	pkg           *Package     // Package we are scanning.
	namedTypeDeps map[string]bool
}

func (g *Generator) Printf(format string, args ...interface{}) {
	fmt.Fprintf(&g.buf, format, args...)
}

// File holds a single parsed file and associated data.
type File struct {
	pkg  *Package  // Package to which this file belongs.
	file *ast.File // Parsed AST.
	// These fields are reset for each type being generated.
	typeName string // Name of the constant type.
}

type Package struct {
	name  string
	defs  map[*ast.Ident]types.Object
	files []*File
}

// parsePackage analyzes the single package constructed from the patterns and tags.
// parsePackage exits if there is an error.
func (g *Generator) parsePackage(patterns []string, tags []string) {
	cfg := &packages.Config{
		Mode:       packages.LoadSyntax,
		BuildFlags: []string{fmt.Sprintf("-tags=%s", strings.Join(tags, " "))},
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		log.Fatal(err)
	}
	if len(pkgs) != 1 {
		log.Fatalf("error: %d packages found", len(pkgs))
	}
	g.addPackage(pkgs[0])
}

// addPackage adds a type checked Package and its syntax files to the generator.
func (g *Generator) addPackage(pkg *packages.Package) {
	g.pkg = &Package{
		name: pkg.Name,
		//defs:  pkg.TypesInfo.Defs,
		files: make([]*File, len(pkg.Syntax)),
	}

	for i, file := range pkg.Syntax {
		g.pkg.files[i] = &File{
			file: file,
			pkg:  g.pkg,
		}
	}
	topLevelDefs := make(map[*ast.Ident]types.Object)
	for _, file := range g.pkg.files {
		for _, decl := range file.file.Decls {
			switch decl := decl.(type) {
			case *ast.GenDecl:
				for _, spec := range decl.Specs {
					switch spec := spec.(type) {
					case *ast.TypeSpec:
						def, ok := pkg.TypesInfo.Defs[spec.Name]
						if !ok {
							log.Fatalf("unable to locate top-level definition of type %q", spec.Name)
						}
						topLevelDefs[spec.Name] = def
					}
				}
			}
		}
	}
	g.pkg.defs = topLevelDefs
}

// generate produces the String method for the named type.
func (g *Generator) generate(typeName string) {
	var underlying types.Type
	for ident, def := range g.pkg.defs {
		if ident.Name == typeName {
			underlying = def.Type().Underlying()
			break
		}
	}
	if underlying == nil {
		log.Fatalf("unable to locate type definition of type name %q", typeName)
	}
	log.Printf("generating type: %q", snakeCase(typeName))
	g.Printf("  %s:\n", snakeCase(typeName))
	g.Printf("    seq:\n")
	g.generateType(underlying)
}

func (g *Generator) generateType(t types.Type) {
	switch t := t.(type) {
	case *types.Struct:
		for i := 0; i < t.NumFields(); i++ {
			field := t.Field(i)
			g.Printf("      - id: %s\n", snakeCase(field.Name()))
			for _, s := range strings.Split(g.kaiType(field.Type()), "\n") {
				g.Printf("        %s\n", s)
			}
		}
	default:
		panic(fmt.Errorf("support for type %T not yet implemented", t))
	}
}

func (g *Generator) kaiType(t types.Type) string {
	buf := &strings.Builder{}
	switch t := t.(type) {
	case *types.Basic:
		return fmt.Sprintf("type: %s # %s", basicKindToKai(t.Kind()), t.Name())
	case *types.Named:
		name := t.Obj().Name()
		g.namedTypeDeps[name] = true
		if underlying, ok := t.Underlying().(*types.Basic); ok {
			// enum?
			buf := &strings.Builder{}
			fmt.Fprintf(buf, "type: %s\n", basicKindToKai(underlying.Kind()))
			fmt.Fprintf(buf, "enum: %s", snakeCase(t.Obj().Name()))
			return buf.String()
		}
		return fmt.Sprintf("type: %s # %s", snakeCase(name), name)
	case *types.Array:
		// TODO: figure out a better way to handle arrays of arrays and slices of
		// slices.
		fmt.Fprintf(buf, "%s\n", g.kaiType(t.Elem()))
		fmt.Fprintf(buf, "repeat: expr\n")
		fmt.Fprintf(buf, "repeat-expr: %d # %s", t.Len(), types.TypeString(t, skipQualifier))
	case *types.Slice:
		fmt.Fprintf(buf, "%s\n", g.kaiType(t.Elem()))
		fmt.Fprintf(buf, "repeat: expr\n")
		fmt.Fprintf(buf, "repeat-expr: todo_add_slice_len # %s", types.TypeString(t, skipQualifier))
	case *types.Pointer:
		fmt.Fprintf(buf, "type: pointer # %s", types.TypeString(t, skipQualifier))
		// TODO: add skip bytes?
	case *types.Signature:
		fmt.Fprintf(buf, "type: func_signature # %s", types.TypeString(t, skipQualifier))
		// TODO: add skip bytes?
	default:
		panic(fmt.Errorf("support for type %T not yet implemented", t))
	}
	return buf.String()
}

func skipQualifier(pkg *types.Package) string {
	return ""
}

func basicKindToKai(kind types.BasicKind) string {
	switch kind {
	// predeclared types
	case types.Bool:
		return "b8" // bool 8-bit
	case types.Int:
		return "s8" // signed int 64-bit
	case types.Int8:
		return "s1" // signed int 8-bit
	case types.Int16:
		return "s2" // signed int 16-bit
	case types.Int32:
		return "s4" // signed int 32-bit
	case types.Int64:
		return "s8" // signed int 64-bit
	case types.Uint:
		return "u8" // unsigned int 64-bit
	case types.Uint8:
		return "u1" // unsigned int 8-bit
	case types.Uint16:
		return "u2" // unsigned int 16-bit
	case types.Uint32:
		return "u4" // unsigned int 32-bit
	case types.Uint64:
		return "u8" // unsigned int 64-bit
	case types.Uintptr:
		return "u8" // unsigned int 64-bit
	case types.Float32:
		return "f2" // single-precision float
	case types.Float64:
		return "f4" // double-precision float
	case types.Complex64:
		return "go_complex64" // single-precision complex
	case types.Complex128:
		return "go_complex128" // double-precision complex
	case types.String:
		return "go_string"
	case types.UnsafePointer:
		return "go_unsafe_ptr"
	// types for untyped values
	//case types.UntypedBool:
	//case types.UntypedInt:
	//case types.UntypedRune:
	//case types.UntypedFloat:
	//case types.UntypedComplex:
	//case types.UntypedString:
	//case types.UntypedNil:
	default:
		panic(fmt.Errorf("support for basic kind %v not yet implemented", kind))
	}
}

// snakeCase returns the snake_case version of the given string.
func snakeCase(s string) string {
	out := &strings.Builder{}
	prevUpper := true
	for _, r := range s {
		if unicode.IsUpper(r) {
			if !prevUpper {
				out.WriteRune('_')
			}
			out.WriteRune(unicode.ToLower(r))
			prevUpper = true
		} else {
			out.WriteRune(r)
			prevUpper = false
		}
	}
	return out.String()
}
