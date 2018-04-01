// Copyright 2016 The protoc-gen-cobra authors. All rights reserved.
//
// Based on protoc-gen-go from https://github.com/golang/protobuf.
// Copyright 2015 The Go Authors.  All rights reserved.

/*
	The code generator for the plugin for the Google protocol buffer compiler.
	It generates Go code from the protocol buffer description files read by the
	main routine.

	Adapted to generate cobra command line applications.
*/
package generator

import (
	"bufio"
	"bytes"
	"fmt"
	"go/parser"
	"go/printer"
	"go/token"
	"log"
	"os"
	"path"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/golang/protobuf/proto"

	"github.com/golang/protobuf/protoc-gen-go/descriptor"
	plugin "github.com/golang/protobuf/protoc-gen-go/plugin"
)

// generatedCodeVersion indicates a version of the generated code.
// It is incremented whenever an incompatibility between the generated code and
// proto package is introduced; the generated code references
// a constant, proto.ProtoPackageIsVersionN (where N is generatedCodeVersion).
const generatedCodeVersion = 2

// A Plugin provides functionality to add to the output during Go code generation,
// such as to produce RPC stubs.
type Plugin interface {
	// Name identifies the plugin.
	Name() string
	// Init is called once after data structures are built but before
	// code generation begins.
	Init(g *Generator)
	// Generate produces the code generated by the plugin for this file,
	// except for the imports, by calling the generator's methods P, In, and Out.
	Generate(file *FileDescriptor)
	// GenerateImports produces the import declarations for this file.
	// It is called after Generate.
	GenerateImports(file *FileDescriptor, imports []*FileDescriptor)
}

var plugins []Plugin

// RegisterPlugin installs a (second-order) plugin to be run when the Go output is generated.
// It is typically called during initialization.
func RegisterPlugin(p Plugin) {
	plugins = append(plugins, p)
}

// Each type we import as a protocol buffer (other than FileDescriptorProto) needs
// a pointer to the FileDescriptorProto that represents it.  These types achieve that
// wrapping by placing each Proto inside a struct with the pointer to its File. The
// structs have the same names as their contents, with "Proto" removed.
// FileDescriptor is used to store the things that it points to.

// The file and package name method are common to messages and enums.
type common struct {
	file *descriptor.FileDescriptorProto // File this object comes from.
}

// PackageName is name in the package clause in the generated file.
func (c *common) PackageName() string { return uniquePackageOf(c.file) }

func (c *common) File() *descriptor.FileDescriptorProto { return c.file }

func fileIsProto3(file *descriptor.FileDescriptorProto) bool {
	return file.GetSyntax() == "proto3"
}

func (c *common) proto3() bool { return fileIsProto3(c.file) }

// Descriptor represents a protocol buffer message.
type Descriptor struct {
	common
	*descriptor.DescriptorProto
	parent   *Descriptor            // The containing message, if any.
	nested   []*Descriptor          // Inner messages, if any.
	ext      []*ExtensionDescriptor // Extensions, if any.
	typename []string               // Cached typename vector.
	index    int                    // The index into the container, whether the file or another message.
	path     string                 // The SourceCodeInfo path as comma-separated integers.
	group    bool
}

// TypeName returns the elements of the dotted type name.
// The package name is not part of this name.
func (d *Descriptor) TypeName() []string {
	if d.typename != nil {
		return d.typename
	}
	n := 0
	for parent := d; parent != nil; parent = parent.parent {
		n++
	}
	s := make([]string, n, n)
	for parent := d; parent != nil; parent = parent.parent {
		n--
		s[n] = parent.GetName()
	}
	d.typename = s
	return s
}

// ExtensionDescriptor describes an extension. If it's at top level, its parent will be nil.
// Otherwise it will be the descriptor of the message in which it is defined.
type ExtensionDescriptor struct {
	common
	*descriptor.FieldDescriptorProto
	parent *Descriptor // The containing message, if any.
}

// TypeName returns the elements of the dotted type name.
// The package name is not part of this name.
func (e *ExtensionDescriptor) TypeName() (s []string) {
	name := e.GetName()
	if e.parent == nil {
		// top-level extension
		s = make([]string, 1)
	} else {
		pname := e.parent.TypeName()
		s = make([]string, len(pname)+1)
		copy(s, pname)
	}
	s[len(s)-1] = name
	return s
}

// DescName returns the variable name used for the generated descriptor.
func (e *ExtensionDescriptor) DescName() string {
	// The full type name.
	typeName := e.TypeName()
	// Each scope of the extension is individually CamelCased, and all are joined with "_" with an "E_" prefix.
	for i, s := range typeName {
		typeName[i] = CamelCase(s)
	}
	return "E_" + strings.Join(typeName, "_")
}

// ImportedDescriptor describes a type that has been publicly imported from another file.
type ImportedDescriptor struct {
	common
	o Object
}

func (id *ImportedDescriptor) TypeName() []string { return id.o.TypeName() }

// FileDescriptor describes an protocol buffer descriptor file (.proto).
// It includes slices of all the messages and enums defined within it.
// Those slices are constructed by WrapTypes.
type FileDescriptor struct {
	*descriptor.FileDescriptorProto
	desc []*Descriptor          // All the messages defined in this file.
	ext  []*ExtensionDescriptor // All the top-level extensions defined in this file.
	imp  []*ImportedDescriptor  // All types defined in files publicly imported by this file.

	// Comments, stored as a map of path (comma-separated integers) to the comment.
	comments map[string]*descriptor.SourceCodeInfo_Location

	// The full list of symbols that are exported,
	// as a map from the exported object to its symbols.
	// This is used for supporting public imports.
	exported map[Object][]symbol

	index int // The index of this file in the list of files to generate code for

	proto3 bool // whether to generate proto3 code for this file
}

// PackageName is the package name we'll use in the generated code to refer to this file.
func (d *FileDescriptor) PackageName() string { return uniquePackageOf(d.FileDescriptorProto) }

// VarName is the variable name we'll use in the generated code to refer
// to the compressed bytes of this descriptor. It is not exported, so
// it is only valid inside the generated package.
func (d *FileDescriptor) VarName() string { return fmt.Sprintf("fileDescriptor%d", d.index) }

// goPackageOption interprets the file's go_package option.
// If there is no go_package, it returns ("", "", false).
// If there's a simple name, it returns ("", pkg, true).
// If the option implies an import path, it returns (impPath, pkg, true).
func (d *FileDescriptor) goPackageOption() (impPath, pkg string, ok bool) {
	pkg = d.GetOptions().GetGoPackage()
	if pkg == "" {
		return
	}
	ok = true
	// The presence of a slash implies there's an import path.
	slash := strings.LastIndex(pkg, "/")
	if slash < 0 {
		return
	}
	impPath, pkg = pkg, pkg[slash+1:]
	// A semicolon-delimited suffix overrides the package name.
	sc := strings.IndexByte(impPath, ';')
	if sc < 0 {
		return
	}
	impPath, pkg = impPath[:sc], impPath[sc+1:]
	return
}

// goPackageName returns the Go package name to use in the
// generated Go file.  The result explicit reports whether the name
// came from an option go_package statement.  If explicit is false,
// the name was derived from the protocol buffer's package statement
// or the input file name.
func (d *FileDescriptor) goPackageName() (name string, explicit bool) {
	// Does the file have a "go_package" option?
	if _, pkg, ok := d.goPackageOption(); ok {
		return pkg, true
	}

	// Does the file have a package clause?
	if pkg := d.GetPackage(); pkg != "" {
		return pkg, false
	}
	// Use the file base name.
	return baseName(d.GetName()), false
}

// goFileName returns the output name for the generated Go file.
func (d *FileDescriptor) goFileName() string {
	name := *d.Name
	if ext := path.Ext(name); ext == ".proto" || ext == ".protodevel" {
		name = name[:len(name)-len(ext)]
	}
	name += ".cobra.pb.go"

	// Does the file have a "go_package" option?
	// If it does, it may override the filename.
	if impPath, _, ok := d.goPackageOption(); ok && impPath != "" {
		// Replace the existing dirname with the declared import path.
		_, name = path.Split(name)
		name = path.Join(impPath, name)
		return name
	}

	return name
}

func (d *FileDescriptor) addExport(obj Object, sym symbol) {
	d.exported[obj] = append(d.exported[obj], sym)
}

// symbol is an interface representing an exported Go symbol.
type symbol interface {
	// GenerateAlias should generate an appropriate alias
	// for the symbol from the named package.
	GenerateAlias(g *Generator, pkg string)
}

// Object is an interface abstracting the abilities shared by enums, messages, extensions and imported objects.
type Object interface {
	PackageName() string // The name we use in our output (a_b_c), possibly renamed for uniqueness.
	TypeName() []string
	File() *descriptor.FileDescriptorProto
}

// Each package name we generate must be unique. The package we're generating
// gets its own name but every other package must have a unique name that does
// not conflict in the code we generate.  These names are chosen globally (although
// they don't have to be, it simplifies things to do them globally).
func uniquePackageOf(fd *descriptor.FileDescriptorProto) string {
	s, ok := uniquePackageName[fd]
	if !ok {
		log.Fatal("internal error: no package name defined for " + fd.GetName())
	}
	return s
}

// Generator is the type whose methods generate the output, stored in the associated response structure.
type Generator struct {
	*bytes.Buffer

	Request  *plugin.CodeGeneratorRequest  // The input.
	Response *plugin.CodeGeneratorResponse // The output.

	Param             map[string]string // Command-line parameters.
	PackageImportPath string            // Go import path of the package we're generating code for
	ImportPrefix      string            // String to prefix to imported package file names.
	ImportMap         map[string]string // Mapping from .proto file name to import path

	Pkg map[string]string // The names under which we import support packages

	packageName      string                     // What we're calling ourselves.
	allFiles         []*FileDescriptor          // All files in the tree
	allFilesByName   map[string]*FileDescriptor // All files by filename.
	genFiles         []*FileDescriptor          // Those files we will generate output for.
	file             *FileDescriptor            // The file we are compiling now.
	usedPackages     map[string]bool            // Names of packages used in current file.
	typeNameToObject map[string]Object          // Key is a fully-qualified name in input syntax.
	init             []string                   // Lines to emit in the init function.
	indent           string
	writeOutput      bool
}

// New creates a new generator and allocates the request and response protobufs.
func New() *Generator {
	g := new(Generator)
	g.Buffer = new(bytes.Buffer)
	g.Request = new(plugin.CodeGeneratorRequest)
	g.Response = new(plugin.CodeGeneratorResponse)
	return g
}

// Error reports a problem, including an error, and exits the program.
func (g *Generator) Error(err error, msgs ...string) {
	s := strings.Join(msgs, " ") + ":" + err.Error()
	log.Print("protoc-gen-cobra: error:", s)
	os.Exit(1)
}

// Fail reports a problem and exits the program.
func (g *Generator) Fail(msgs ...string) {
	s := strings.Join(msgs, " ")
	log.Print("protoc-gen-cobra: error:", s)
	os.Exit(1)
}

// CommandLineParameters breaks the comma-separated list of key=value pairs
// in the parameter (a member of the request protobuf) into a key/value map.
// It then sets file name mappings defined by those entries.
func (g *Generator) CommandLineParameters(parameter string) {
	g.Param = make(map[string]string)
	for _, p := range strings.Split(parameter, ",") {
		if i := strings.Index(p, "="); i < 0 {
			g.Param[p] = ""
		} else {
			g.Param[p[0:i]] = p[i+1:]
		}
	}

	g.ImportMap = make(map[string]string)
	pluginList := "none" // Default list of plugin names to enable (empty means all).
	// TODO: support for dynamic templates for the cobra generated code?
	for k, v := range g.Param {
		switch k {
		case "import_prefix":
			g.ImportPrefix = v
		case "import_path":
			g.PackageImportPath = v
		case "plugins":
			pluginList = v
		default:
			if len(k) > 0 && k[0] == 'M' {
				g.ImportMap[k[1:]] = v
			}
		}
	}

	if pluginList != "" {
		// Amend the set of plugins.
		enabled := make(map[string]bool)
		for _, name := range strings.Split(pluginList, "+") {
			enabled[name] = true
		}
		var nplugins []Plugin
		for _, p := range plugins {
			if enabled[p.Name()] {
				nplugins = append(nplugins, p)
			}
		}
		plugins = nplugins
	}
}

// DefaultPackageName returns the package name printed for the object.
// If its file is in a different package, it returns the package name we're using for this file, plus ".".
// Otherwise it returns the empty string.
func (g *Generator) DefaultPackageName(obj Object) string {
	pkg := obj.PackageName()
	if pkg == g.packageName {
		return ""
	}
	return pkg + "."
}

// For each input file, the unique package name to use, underscored.
var uniquePackageName = make(map[*descriptor.FileDescriptorProto]string)

// Package names already registered.  Key is the name from the .proto file;
// value is the name that appears in the generated code.
var pkgNamesInUse = make(map[string]bool)

// Create and remember a guaranteed unique package name for this file descriptor.
// Pkg is the candidate name.  If f is nil, it's a builtin package like "proto" and
// has no file descriptor.
func RegisterUniquePackageName(pkg string, f *FileDescriptor) string {
	// Convert dots to underscores before finding a unique alias.
	pkg = strings.Map(badToUnderscore, pkg)

	for i, orig := 1, pkg; pkgNamesInUse[pkg]; i++ {
		// It's a duplicate; must rename.
		pkg = orig + strconv.Itoa(i)
	}
	// Install it.
	pkgNamesInUse[pkg] = true
	if f != nil {
		uniquePackageName[f.FileDescriptorProto] = pkg
	}
	return pkg
}

var isGoKeyword = map[string]bool{
	"break":       true,
	"case":        true,
	"chan":        true,
	"const":       true,
	"continue":    true,
	"default":     true,
	"else":        true,
	"defer":       true,
	"fallthrough": true,
	"for":         true,
	"func":        true,
	"go":          true,
	"goto":        true,
	"if":          true,
	"import":      true,
	"interface":   true,
	"map":         true,
	"package":     true,
	"range":       true,
	"return":      true,
	"select":      true,
	"struct":      true,
	"switch":      true,
	"type":        true,
	"var":         true,
}

// defaultGoPackage returns the package name to use,
// derived from the import path of the package we're building code for.
func (g *Generator) defaultGoPackage() string {
	p := g.PackageImportPath
	if i := strings.LastIndex(p, "/"); i >= 0 {
		p = p[i+1:]
	}
	if p == "" {
		return ""
	}

	p = strings.Map(badToUnderscore, p)
	// Identifier must not be keyword: insert _.
	if isGoKeyword[p] {
		p = "_" + p
	}
	// Identifier must not begin with digit: insert _.
	if r, _ := utf8.DecodeRuneInString(p); unicode.IsDigit(r) {
		p = "_" + p
	}
	return p
}

// SetPackageNames sets the package name for this run.
// The package name must agree across all files being generated.
// It also defines unique package names for all imported files.
func (g *Generator) SetPackageNames() {
	// Register the name for this package.  It will be the first name
	// registered so is guaranteed to be unmodified.
	pkg, explicit := g.genFiles[0].goPackageName()

	// Check all files for an explicit go_package option.
	for _, f := range g.genFiles {
		thisPkg, thisExplicit := f.goPackageName()
		if thisExplicit {
			if !explicit {
				// Let this file's go_package option serve for all input files.
				pkg, explicit = thisPkg, true
			} else if thisPkg != pkg {
				g.Fail("inconsistent package names:", thisPkg, pkg)
			}
		}
	}

	// If we don't have an explicit go_package option but we have an
	// import path, use that.
	if !explicit {
		p := g.defaultGoPackage()
		if p != "" {
			pkg, explicit = p, true
		}
	}

	// If there was no go_package and no import path to use,
	// double-check that all the inputs have the same implicit
	// Go package name.
	if !explicit {
		for _, f := range g.genFiles {
			thisPkg, _ := f.goPackageName()
			if thisPkg != pkg {
				g.Fail("inconsistent package names:", thisPkg, pkg)
			}
		}
	}

	g.packageName = RegisterUniquePackageName(pkg, g.genFiles[0])

	// Register the support package names. They might collide with the
	// name of a package we import.
	g.Pkg = map[string]string{
		"fmt":   RegisterUniquePackageName("fmt", nil),
		"math":  RegisterUniquePackageName("math", nil),
		"proto": RegisterUniquePackageName("proto", nil),
	}

AllFiles:
	for _, f := range g.allFiles {
		for _, genf := range g.genFiles {
			if f == genf {
				// In this package already.
				uniquePackageName[f.FileDescriptorProto] = g.packageName
				continue AllFiles
			}
		}
		// The file is a dependency, so we want to ignore its go_package option
		// because that is only relevant for its specific generated output.
		pkg := f.GetPackage()
		if pkg == "" {
			pkg = baseName(*f.Name)
		}
		RegisterUniquePackageName(pkg, f)
	}
}

// WrapTypes walks the incoming data, wrapping DescriptorProtos, EnumDescriptorProtos
// and FileDescriptorProtos into file-referenced objects within the Generator.
// It also creates the list of files to generate and so should be called before GenerateAllFiles.
func (g *Generator) WrapTypes() {
	g.allFiles = make([]*FileDescriptor, 0, len(g.Request.ProtoFile))
	g.allFilesByName = make(map[string]*FileDescriptor, len(g.allFiles))
	for _, f := range g.Request.ProtoFile {
		// We must wrap the descriptors before we wrap the enums
		descs := wrapDescriptors(f)
		g.buildNestedDescriptors(descs)
		exts := wrapExtensions(f)
		fd := &FileDescriptor{
			FileDescriptorProto: f,
			desc:                descs,
			ext:                 exts,
			exported:            make(map[Object][]symbol),
			proto3:              fileIsProto3(f),
		}
		extractComments(fd)
		g.allFiles = append(g.allFiles, fd)
		g.allFilesByName[f.GetName()] = fd
	}
	for _, fd := range g.allFiles {
		fd.imp = wrapImported(fd.FileDescriptorProto, g)
	}

	g.genFiles = make([]*FileDescriptor, 0, len(g.Request.FileToGenerate))
	for _, fileName := range g.Request.FileToGenerate {
		fd := g.allFilesByName[fileName]
		if fd == nil {
			g.Fail("could not find file named", fileName)
		}
		fd.index = len(g.genFiles)
		g.genFiles = append(g.genFiles, fd)
	}
}

// Scan the descriptors in this file.  For each one, build the slice of nested descriptors
func (g *Generator) buildNestedDescriptors(descs []*Descriptor) {
	for _, desc := range descs {
		if len(desc.NestedType) != 0 {
			for _, nest := range descs {
				if nest.parent == desc {
					desc.nested = append(desc.nested, nest)
				}
			}
			if len(desc.nested) != len(desc.NestedType) {
				g.Fail("internal error: nesting failure for", desc.GetName())
			}
		}
	}
}

// Construct the Descriptor
func newDescriptor(desc *descriptor.DescriptorProto, parent *Descriptor, file *descriptor.FileDescriptorProto, index int) *Descriptor {
	d := &Descriptor{
		common:          common{file},
		DescriptorProto: desc,
		parent:          parent,
		index:           index,
	}
	if parent == nil {
		d.path = fmt.Sprintf("%d,%d", messagePath, index)
	} else {
		d.path = fmt.Sprintf("%s,%d,%d", parent.path, messageMessagePath, index)
	}

	// The only way to distinguish a group from a message is whether
	// the containing message has a TYPE_GROUP field that matches.
	if parent != nil {
		parts := d.TypeName()
		if file.Package != nil {
			parts = append([]string{*file.Package}, parts...)
		}
		exp := "." + strings.Join(parts, ".")
		for _, field := range parent.Field {
			if field.GetType() == descriptor.FieldDescriptorProto_TYPE_GROUP && field.GetTypeName() == exp {
				d.group = true
				break
			}
		}
	}

	for _, field := range desc.Extension {
		d.ext = append(d.ext, &ExtensionDescriptor{common{file}, field, d})
	}

	return d
}

// Return a slice of all the Descriptors defined within this file
func wrapDescriptors(file *descriptor.FileDescriptorProto) []*Descriptor {
	sl := make([]*Descriptor, 0, len(file.MessageType)+10)
	for i, desc := range file.MessageType {
		sl = wrapThisDescriptor(sl, desc, nil, file, i)
	}
	return sl
}

// Wrap this Descriptor, recursively
func wrapThisDescriptor(sl []*Descriptor, desc *descriptor.DescriptorProto, parent *Descriptor, file *descriptor.FileDescriptorProto, index int) []*Descriptor {
	sl = append(sl, newDescriptor(desc, parent, file, index))
	me := sl[len(sl)-1]
	for i, nested := range desc.NestedType {
		sl = wrapThisDescriptor(sl, nested, me, file, i)
	}
	return sl
}

// Return a slice of all the top-level ExtensionDescriptors defined within this file.
func wrapExtensions(file *descriptor.FileDescriptorProto) []*ExtensionDescriptor {
	var sl []*ExtensionDescriptor
	for _, field := range file.Extension {
		sl = append(sl, &ExtensionDescriptor{common{file}, field, nil})
	}
	return sl
}

// Return a slice of all the types that are publicly imported into this file.
func wrapImported(file *descriptor.FileDescriptorProto, g *Generator) (sl []*ImportedDescriptor) {
	for _, index := range file.PublicDependency {
		df := g.fileByName(file.Dependency[index])
		for _, d := range df.desc {
			if d.GetOptions().GetMapEntry() {
				continue
			}
			sl = append(sl, &ImportedDescriptor{common{file}, d})
		}
		for _, ext := range df.ext {
			sl = append(sl, &ImportedDescriptor{common{file}, ext})
		}
	}
	return
}

func extractComments(file *FileDescriptor) {
	file.comments = make(map[string]*descriptor.SourceCodeInfo_Location)
	for _, loc := range file.GetSourceCodeInfo().GetLocation() {
		if loc.LeadingComments == nil {
			continue
		}
		var p []string
		for _, n := range loc.Path {
			p = append(p, strconv.Itoa(int(n)))
		}
		file.comments[strings.Join(p, ",")] = loc
	}
}

// P prints the arguments to the generated output.  It handles strings and int32s, plus
// handling indirections because they may be *string, etc.
func (g *Generator) P(str ...interface{}) {
	if !g.writeOutput {
		return
	}
	g.WriteString(g.indent)
	for _, v := range str {
		switch s := v.(type) {
		case string:
			g.WriteString(s)
		case *string:
			g.WriteString(*s)
		case bool:
			fmt.Fprintf(g, "%t", s)
		case *bool:
			fmt.Fprintf(g, "%t", *s)
		case int:
			fmt.Fprintf(g, "%d", s)
		case *int32:
			fmt.Fprintf(g, "%d", *s)
		case *int64:
			fmt.Fprintf(g, "%d", *s)
		case float64:
			fmt.Fprintf(g, "%g", s)
		case *float64:
			fmt.Fprintf(g, "%g", *s)
		default:
			g.Fail(fmt.Sprintf("unknown type in printer: %T", v))
		}
	}
	g.WriteByte('\n')
}

// addInitf stores the given statement to be printed inside the file's init function.
// The statement is given as a format specifier and arguments.
func (g *Generator) addInitf(stmt string, a ...interface{}) {
	g.init = append(g.init, fmt.Sprintf(stmt, a...))
}

// In Indents the output one tab stop.
func (g *Generator) In() { g.indent += "\t" }

// Out unindents the output one tab stop.
func (g *Generator) Out() {
	if len(g.indent) > 0 {
		g.indent = g.indent[1:]
	}
}

// GenerateAllFiles generates the output for all the files we're outputting.
func (g *Generator) GenerateAllFiles() {
	// Initialize the plugins
	for _, p := range plugins {
		p.Init(g)
	}
	// Generate the output. The generator runs for every file, even the files
	// that we don't generate output for, so that we can collate the full list
	// of exported symbols to support public imports.
	genFileMap := make(map[*FileDescriptor]bool, len(g.genFiles))
	for _, file := range g.genFiles {
		genFileMap[file] = len(file.FileDescriptorProto.Service) > 0
	}
	for _, file := range g.allFiles {
		g.Reset()
		g.writeOutput = genFileMap[file]
		g.generate(file)
		if !g.writeOutput {
			continue
		}
		g.Response.File = append(g.Response.File, &plugin.CodeGeneratorResponse_File{
			Name:    proto.String(file.goFileName()),
			Content: proto.String(g.String()),
		})
	}
}

// Run all the plugins associated with the file.
func (g *Generator) runPlugins(file *FileDescriptor) {
	for _, p := range plugins {
		p.Generate(file)
	}
}

// FileOf return the FileDescriptor for this FileDescriptorProto.
func (g *Generator) FileOf(fd *descriptor.FileDescriptorProto) *FileDescriptor {
	for _, file := range g.allFiles {
		if file.FileDescriptorProto == fd {
			return file
		}
	}
	g.Fail("could not find file in table:", fd.GetName())
	return nil
}

// Fill the response protocol buffer with the generated output for all the files we're
// supposed to generate.
func (g *Generator) generate(file *FileDescriptor) {
	g.file = g.FileOf(file.FileDescriptorProto)
	g.usedPackages = make(map[string]bool)

	if g.file.index == 0 {
		// For one file in the package, assert version compatibility.
		g.P("// This is a compile-time assertion to ensure that this generated file")
		g.P("// is compatible with the proto package it is being compiled against.")
		g.P("// A compilation error at this line likely means your copy of the")
		g.P("// proto package needs to be updated.")
		g.P("const _ = ", g.Pkg["proto"], ".ProtoPackageIsVersion", generatedCodeVersion, " // please upgrade the proto package")
		g.P()
	}

	for _, td := range g.file.imp {
		g.generateImported(td)
	}
	for _, desc := range g.file.desc {
		// Don't generate virtual messages for maps.
		if desc.GetOptions().GetMapEntry() {
			continue
		}
	}
	g.generateInitFunction()

	// Run the plugins before the imports so we know which imports are necessary.
	g.runPlugins(file)

	// Generate header and imports last, though they appear first in the output.
	rem := g.Buffer
	g.Buffer = new(bytes.Buffer)
	g.generateHeader()
	g.generateImports()
	if !g.writeOutput {
		return
	}
	g.Write(rem.Bytes())

	// Reformat generated code.
	fset := token.NewFileSet()
	raw := g.Bytes()
	ast, err := parser.ParseFile(fset, "", g, parser.ParseComments)
	if err != nil {
		// Print out the bad code with line numbers.
		// This should never happen in practice, but it can while changing generated code,
		// so consider this a debugging aid.
		var src bytes.Buffer
		s := bufio.NewScanner(bytes.NewReader(raw))
		for line := 1; s.Scan(); line++ {
			fmt.Fprintf(&src, "%5d\t%s\n", line, s.Bytes())
		}
		g.Fail("bad Go source code was generated:", err.Error(), "\n"+src.String())
	}
	g.Reset()
	err = (&printer.Config{Mode: printer.TabIndent | printer.UseSpaces, Tabwidth: 8}).Fprint(g, fset, ast)
	if err != nil {
		g.Fail("generated Go source code could not be reformatted:", err.Error())
	}
}

// Generate the header, including package definition
func (g *Generator) generateHeader() {
	g.P("// Code generated by protoc-gen-cobra.")
	g.P("// source: ", g.file.Name)
	g.P("// DO NOT EDIT!")
	g.P()

	name := g.file.PackageName()

	if g.file.index == 0 {
		// Generate package docs for the first file in the package.
		g.P("/*")
		g.P("Package ", name, " is a generated protocol buffer package.")
		g.P()
		if loc, ok := g.file.comments[strconv.Itoa(packagePath)]; ok {
			// not using g.PrintComments because this is a /* */ comment block.
			text := strings.TrimSuffix(loc.GetLeadingComments(), "\n")
			for _, line := range strings.Split(text, "\n") {
				line = strings.TrimPrefix(line, " ")
				// ensure we don't escape from the block comment
				line = strings.Replace(line, "*/", "* /", -1)
				g.P(line)
			}
			g.P()
		}
		var cmds []string
		g.P("It is generated from these files:")
		for _, f := range g.genFiles {
			g.P("\t", f.Name)
			for _, msg := range f.desc {
				if msg.parent != nil {
					continue
				}
			}
			for _, service := range f.Service {
				origServName := service.GetName()
				servName := CamelCase(origServName)
				cmds = append(cmds, servName+"ClientCommand")
			}
		}
		g.P()
		g.P("It has these top-level commands:")
		for _, cmd := range cmds {
			g.P("\t", cmd)
		}
		g.P("*/")
		g.P() // This is to not conflict with grpc pkg doc.
	}

	g.P("package ", name)
	g.P()
}

// PrintComments prints any comments from the source .proto file.
// The path is a comma-separated list of integers.
// It returns an indication of whether any comments were printed.
// See descriptor.proto for its format.
func (g *Generator) PrintComments(path string) bool {
	if !g.writeOutput {
		return false
	}
	if loc, ok := g.file.comments[path]; ok {
		text := strings.TrimSuffix(loc.GetLeadingComments(), "\n")
		for _, line := range strings.Split(text, "\n") {
			g.P("// ", strings.TrimPrefix(line, " "))
		}
		return true
	}
	return false
}

func (g *Generator) fileByName(filename string) *FileDescriptor {
	return g.allFilesByName[filename]
}

// weak returns whether the ith import of the current file is a weak import.
func (g *Generator) weak(i int32) bool {
	for _, j := range g.file.WeakDependency {
		if j == i {
			return true
		}
	}
	return false
}

// Generate the imports
func (g *Generator) generateImports() {
	// We almost always need a proto import.  Rather than computing when we
	// do, which is tricky when there's a plugin, just import it and
	// reference it later. The same argument applies to the fmt and math packages.
	g.P("import " + g.Pkg["proto"] + " " + strconv.Quote(g.ImportPrefix+"github.com/golang/protobuf/proto"))
	g.P("import " + g.Pkg["fmt"] + ` "fmt"`)
	g.P("import " + g.Pkg["math"] + ` "math"`)
	g.P()
	// TODO: may need to worry about uniqueness across plugins
	for _, p := range plugins {
		p.GenerateImports(g.file, g.allFiles)
		g.P()
	}
	g.P("// Reference imports to suppress errors if they are not otherwise used.")
	g.P("var _ = ", g.Pkg["proto"], ".Marshal")
	g.P("var _ = ", g.Pkg["fmt"], ".Errorf")
	g.P("var _ = ", g.Pkg["math"], ".Inf")
	g.P()
}

func (g *Generator) generateImported(id *ImportedDescriptor) {
	// Don't generate public import symbols for files that we are generating
	// code for, since those symbols will already be in this package.
	// We can't simply avoid creating the ImportedDescriptor objects,
	// because g.genFiles isn't populated at that stage.
	tn := id.TypeName()
	sn := tn[len(tn)-1]
	df := g.FileOf(id.o.File())
	filename := *df.Name
	for _, fd := range g.genFiles {
		if *fd.Name == filename {
			g.P("// Ignoring public import of ", sn, " from ", filename)
			g.P()
			return
		}
	}
	g.P("// ", sn, " from public import ", filename)
	g.usedPackages[df.PackageName()] = true

	for _, sym := range df.exported[id.o] {
		sym.GenerateAlias(g, df.PackageName())
	}

	g.P()
}

func needsStar(typ descriptor.FieldDescriptorProto_Type) bool {
	switch typ {
	case descriptor.FieldDescriptorProto_TYPE_GROUP:
		return false
	case descriptor.FieldDescriptorProto_TYPE_MESSAGE:
		return false
	case descriptor.FieldDescriptorProto_TYPE_BYTES:
		return false
	}
	return true
}

// TypeName is the printed name appropriate for an item. If the object is in the current file,
// TypeName drops the package name and underscores the rest.
// Otherwise the object is from another package; and the result is the underscored
// package name followed by the item name.
// The result always has an initial capital.
func (g *Generator) TypeName(obj Object) string {
	return g.DefaultPackageName(obj) + CamelCaseSlice(obj.TypeName())
}

// Method names that may be generated.  Fields with these names get an
// underscore appended. Any change to this set is a potential incompatible
// API change because it changes generated field names.
var methodNames = [...]string{
	"Reset",
	"String",
	"ProtoMessage",
	"Marshal",
	"Unmarshal",
	"ExtensionRangeArray",
	"ExtensionMap",
	"Descriptor",
}

// Names of messages in the `google.protobuf` package for which
// we will generate XXX_WellKnownType methods.
var wellKnownTypes = map[string]bool{
	"Any":       true,
	"Duration":  true,
	"Empty":     true,
	"Struct":    true,
	"Timestamp": true,

	"Value":       true,
	"ListValue":   true,
	"DoubleValue": true,
	"FloatValue":  true,
	"Int64Value":  true,
	"UInt64Value": true,
	"Int32Value":  true,
	"UInt32Value": true,
	"BoolValue":   true,
	"StringValue": true,
	"BytesValue":  true,
}

func (g *Generator) generateInitFunction() {
	for _, d := range g.file.desc {
		for _, ext := range d.ext {
			g.generateExtensionRegistration(ext)
		}
	}
	for _, ext := range g.file.ext {
		g.generateExtensionRegistration(ext)
	}
	if len(g.init) == 0 {
		return
	}
	g.P("func init() {")
	g.In()
	for _, l := range g.init {
		g.P(l)
	}
	g.Out()
	g.P("}")
	g.init = nil
}

func (g *Generator) generateExtensionRegistration(ext *ExtensionDescriptor) {
	g.addInitf("%s.RegisterExtension(%s)", g.Pkg["proto"], ext.DescName())
}

// And now lots of helper functions.

// Is c an ASCII lower-case letter?
func isASCIILower(c byte) bool {
	return 'a' <= c && c <= 'z'
}

// Is c an ASCII digit?
func isASCIIDigit(c byte) bool {
	return '0' <= c && c <= '9'
}

// CamelCase returns the CamelCased name.
// If there is an interior underscore followed by a lower case letter,
// drop the underscore and convert the letter to upper case.
// There is a remote possibility of this rewrite causing a name collision,
// but it's so remote we're prepared to pretend it's nonexistent - since the
// C++ generator lowercases names, it's extremely unlikely to have two fields
// with different capitalizations.
// In short, _my_field_name_2 becomes XMyFieldName_2.
func CamelCase(s string) string {
	if s == "" {
		return ""
	}
	t := make([]byte, 0, 32)
	i := 0
	if s[0] == '_' {
		// Need a capital letter; drop the '_'.
		t = append(t, 'X')
		i++
	}
	// Invariant: if the next letter is lower case, it must be converted
	// to upper case.
	// That is, we process a word at a time, where words are marked by _ or
	// upper case letter. Digits are treated as words.
	for ; i < len(s); i++ {
		c := s[i]
		if c == '_' && i+1 < len(s) && isASCIILower(s[i+1]) {
			continue // Skip the underscore in s.
		}
		if isASCIIDigit(c) {
			t = append(t, c)
			continue
		}
		// Assume we have a letter now - if not, it's a bogus identifier.
		// The next word is a sequence of characters that must start upper case.
		if isASCIILower(c) {
			c ^= ' ' // Make it a capital letter.
		}
		t = append(t, c) // Guaranteed not lower case.
		// Accept lower case sequence that follows.
		for i+1 < len(s) && isASCIILower(s[i+1]) {
			i++
			t = append(t, s[i])
		}
	}
	return string(t)
}

// CamelCaseSlice is like CamelCase, but the argument is a slice of strings to
// be joined with "_".
func CamelCaseSlice(elem []string) string { return CamelCase(strings.Join(elem, "_")) }

// dottedSlice turns a sliced name into a dotted name.
func dottedSlice(elem []string) string { return strings.Join(elem, ".") }

// badToUnderscore is the mapping function used to generate Go names from package names,
// which can be dotted in the input .proto file.  It replaces non-identifier characters such as
// dot or dash with underscore.
func badToUnderscore(r rune) rune {
	if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
		return r
	}
	return '_'
}

// baseName returns the last path element of the name, with the last dotted suffix removed.
func baseName(name string) string {
	// First, find the last element
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	// Now drop the suffix
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[0:i]
	}
	return name
}

// The SourceCodeInfo message describes the location of elements of a parsed
// .proto file by way of a "path", which is a sequence of integers that
// describe the route from a FileDescriptorProto to the relevant submessage.
// The path alternates between a field number of a repeated field, and an index
// into that repeated field. The constants below define the field numbers that
// are used.
//
// See descriptor.proto for more information about this.
const (
	// tag numbers in FileDescriptorProto
	packagePath = 2 // package
	messagePath = 4 // message_type
	enumPath    = 5 // enum_type
	// tag numbers in DescriptorProto
	messageFieldPath   = 2 // field
	messageMessagePath = 3 // nested_type
	messageEnumPath    = 4 // enum_type
	messageOneofPath   = 8 // oneof_decl
	// tag numbers in EnumDescriptorProto
	enumValuePath = 2 // value
)
