// Copyright 2017 Google Inc. All rights reserved.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to writing, software distributed
// under the License is distributed on a "AS IS" BASIS, WITHOUT WARRANTIES OR
// CONDITIONS OF ANY KIND, either express or implied.
//
// See the License for the specific language governing permissions and
// limitations under the License.

// Package parser parses Go code and keeps track of all the types defined
// and provides access to all the constants defined for an int type.
package parser

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/packages"
)

// A Package contains all the information related to a parsed package.
type Package struct {
	Name string
	buf  bytes.Buffer // Accumulated output.

	defs  map[*ast.Ident]types.Object
	files []*goFile
}

// ParsePackage parses the package in the given directory and returns it.
func ParsePackage(directory string) (*Package, error) {
	p := &Package{}

	cfg := &packages.Config{
		Mode: packages.LoadSyntax,
		// TODO: Need to think about constants in test files. Maybe write type_string_test.go
		// in a separate pass? For later.
		Tests: false,
	}

	pkgs, err := packages.Load(cfg, directory)
	if err != nil {
		return nil, err
	}
	if len(pkgs) != 1 {
		return nil, fmt.Errorf("%d packages found", len(pkgs))
	}

	pkg := pkgs[0]
	p.Name = pkg.Name
	p.defs = pkg.TypesInfo.Defs
	p.files = make([]*goFile, len(pkg.Syntax))

	for i, file := range pkg.Syntax {
		p.files[i] = &goFile{
			file: file,
			pkg:  p,
		}
	}

	return p, nil
}

// generate produces the String method for the named type.
func (pkg *Package) ValuesOfType(typeName string) (_ []string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = r.(error)
		}
	}()
	var values []string
	for _, file := range pkg.files {
		// Set the state for this run of the walker.
		file.typeName = typeName
		file.values = nil
		if file.file != nil {
			ast.Inspect(file.file, file.genDecl)
			for _, v := range file.values {
				values = append(values, v.originalName)
			}
		}
	}

	if len(values) == 0 {
		return nil, fmt.Errorf("no values defined for type %s", typeName)
	}

	return values, nil
}

// This parser is based on https://raw.githubusercontent.com/golang/tools/63e6ed9258fa6cbc90aab9b1eef3e0866e89b874/cmd/stringer/stringer.go

// constantValue represents a declared constant.
type constantValue struct {
	originalName string // The name of the constant.
	// The value is stored as a bit pattern alone. The boolean tells us
	// whether to interpret it as an int64 or a uint64; the only place
	// this matters is when sorting.
	// Much of the time the str field is all we need; it is printed
	// by constantValue.String.
	value  uint64 // Will be converted to int64 when needed.
	signed bool   // Whether the constant is a signed type.
	str    string // The string representation given by the "go/constant" package.
}

// goFile holds a single parsed file and associated data.
type goFile struct {
	pkg  *Package  // Package to which this file belongs.
	file *ast.File // Parsed AST.
	// These fields are reset for each type being generated.
	typeName string          // Name of the constant type.
	values   []constantValue // Accumulator for constant values of that type.
}

// genDecl processes one declaration clause.
func (f *goFile) genDecl(node ast.Node) bool {
	decl, ok := node.(*ast.GenDecl)
	if !ok || decl.Tok != token.CONST {
		// We only care about const declarations.
		return true
	}
	// The name of the type of the constants we are declaring.
	// Can change if this is a multi-element declaration.
	typ := ""
	// Loop over the elements of the declaration. Each element is a ValueSpec:
	// a list of names possibly followed by a type, possibly followed by values.
	// If the type and value are both missing, we carry down the type (and value,
	// but the "go/types" package takes care of that).
	for _, spec := range decl.Specs {
		vspec := spec.(*ast.ValueSpec) // Guaranteed to succeed as this is CONST.
		if vspec.Type == nil && len(vspec.Values) > 0 {
			// "X = 1". With no type but a value. If the constant is untyped,
			// skip this vspec and reset the remembered type.
			typ = ""

			// If this is a simple type conversion, remember the type.
			// We don't mind if this is actually a call; a qualified call won't
			// be matched (that will be SelectorExpr, not Ident), and only unusual
			// situations will result in a function call that appears to be
			// a type conversion.
			ce, ok := vspec.Values[0].(*ast.CallExpr)
			if !ok {
				continue
			}
			id, ok := ce.Fun.(*ast.Ident)
			if !ok {
				continue
			}
			typ = id.Name
		}
		if vspec.Type != nil {
			// "X T". We have a type. Remember it.
			ident, ok := vspec.Type.(*ast.Ident)
			if !ok {
				continue
			}
			typ = ident.Name
		}
		if typ != f.typeName {
			// This is not the type we're looking for.
			continue
		}
		// We now have a list of names (from one line of source code) all being
		// declared with the desired type.
		// Grab their names and actual values and store them in f.values.
		for _, name := range vspec.Names {
			if name.Name == "_" {
				continue
			}
			// This dance lets the type checker find the values for us. It's a
			// bit tricky: look up the object declared by the name, find its
			// types.Const, and extract its value.
			obj, ok := f.pkg.defs[name]
			if !ok {
				panic(fmt.Errorf("no value for constant %s", name))
			}
			info := obj.Type().Underlying().(*types.Basic).Info()
			if info&types.IsInteger == 0 {
				panic(fmt.Errorf("can't handle non-integer constant type %s", typ))
			}
			value := obj.(*types.Const).Val() // Guaranteed to succeed as this is CONST.
			if value.Kind() != constant.Int {
				panic(fmt.Errorf("can't happen: constant is not an integer %s", name))
			}
			i64, isInt := constant.Int64Val(value)
			u64, isUint := constant.Uint64Val(value)
			if !isInt && !isUint {
				panic(fmt.Errorf("internal error: value of %s is not an integer: %s", name, value.String()))
			}
			if !isInt {
				u64 = uint64(i64)
			}
			v := constantValue{
				originalName: name.Name,
				value:        u64,
				signed:       info&types.IsUnsigned == 0,
				str:          value.String(),
			}
			f.values = append(f.values, v)
		}
	}
	return false
}
