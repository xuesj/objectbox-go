/*
 * Copyright 2018 ObjectBox Ltd. All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package generator

import (
	"fmt"
	"go/ast"
	"go/types"
	"strings"

	"github.com/objectbox/objectbox-go/internal/generator/modelinfo"
)

type uid = uint64
type id = uint32

type Binding struct {
	Package  string
	Entities []*Entity

	currentEntityName string
	err               error
}

type Entity struct {
	Name           string
	Id             id
	Uid            uid
	Properties     []*Property
	IdProperty     *Property
	LastPropertyId modelinfo.IdUid

	binding *Binding // parent
}

type Property struct {
	Name        string
	ObName      string
	Id          id
	Uid         uid
	Annotations map[string]*Annotation
	ObType      string
	ObFlags     []string
	GoType      string
	FbType      string
	Relation    *Relation
	Index       *Index

	entity *Entity
}

type Relation struct {
	Target string
}

type Index struct {
	Id  id
	Uid uid
}

type Annotation struct {
	Value string
}

func newBinding() (*Binding, error) {
	return &Binding{}, nil
}

func (binding *Binding) createFromAst(f *file) (err error) {
	binding.Package = f.f.Name.Name // this is actually package name, not file name

	// process all structs
	f.walk(func(node ast.Node) bool {
		return binding.entityLoader(node)
	})

	if binding.err != nil {
		return binding.err
	}

	return nil
}

// this function only processes structs and cuts-off on types that can't contain a struct
func (binding *Binding) entityLoader(node ast.Node) bool {
	if binding.err != nil {
		return false
	}

	switch v := node.(type) {
	case *ast.TypeSpec:
		// this might be the name of the next struct
		binding.currentEntityName = v.Name.Name
		return true
	case *ast.StructType:
		if binding.currentEntityName == "" {
			// NOTE this should probably not happen
			binding.err = fmt.Errorf("encountered a struct without a name")
			return false
		} else {
			binding.err = binding.createEntityFromAst(node)
			// reset after it has been "consumed"
			binding.currentEntityName = ""
		}
		return true

	case *ast.GenDecl:
		return true
	case *ast.File:
		return true
	}

	return false
}

func (binding *Binding) createEntityFromAst(node ast.Node) (err error) {
	entity := &Entity{
		binding: binding,
		Name:    binding.currentEntityName,
	}

	propertiesByName := make(map[string]bool)

	var propertyError = func(err error, property *Property) error {
		return fmt.Errorf("%s on property %s, entity %s", err, property.Name, entity.Name)
	}

	switch t := node.(type) {
	case *ast.StructType:
		for _, f := range t.Fields.List {
			if len(f.Names) != 1 {
				return fmt.Errorf("struct %s has a f with an invalid number of names, one expected, got %v",
					entity.Name, len(f.Names))
			}

			property := &Property{
				entity: entity,
				Name:   f.Names[0].Name,
			}

			if f.Tag != nil {
				if err = property.setAnnotations(f.Tag.Value); err != nil {
					return propertyError(err, property)
				}
			}

			// transient properties are not stored, thus no need to use it in the binding
			if property.Annotations["transient"] != nil {
				continue
			}

			if err = property.setType(f.Type); err != nil {
				return propertyError(err, property)
			}

			if err = property.setObFlags(*f); err != nil {
				return propertyError(err, property)
			}

			// if this is an ID, set it as entity.IdProperty
			if property.Annotations["id"] != nil {
				if entity.IdProperty != nil {
					return fmt.Errorf("struct %s has multiple ID properties - %s and %s",
						entity.Name, entity.IdProperty.Name, property.Name)
				}
				entity.IdProperty = property
			}

			if property.Annotations["nameindb"] != nil {
				if len(property.Annotations["nameindb"].Value) == 0 {
					return propertyError(fmt.Errorf("nameInDb annotation value must not be empty"), property)
				} else {
					property.ObName = property.Annotations["nameindb"].Value
				}
			} else {
				property.ObName = property.Name
			}

			// ObjectBox core internally converts to lowercase so we should check it as this as well
			var realObName = strings.ToLower(property.ObName)
			if propertiesByName[realObName] {
				return propertyError(fmt.Errorf(
					"duplicate name (note that property names are case insensitive)"), property)
			} else {
				propertiesByName[realObName] = true
			}

			entity.Properties = append(entity.Properties, property)
		}
	}

	if len(entity.Properties) == 0 {
		return fmt.Errorf("there are no properties in the entity %s", entity.Name)
	}

	if entity.IdProperty == nil {
		// try to find an ID property by name
		for _, property := range entity.Properties {
			if strings.ToLower(property.Name) == "id" && strings.ToLower(property.GoType) == "uint64" {
				if entity.IdProperty == nil {
					entity.IdProperty = property
					property.addObFlag("ID")
				} else {
					// fail in case multiple fields match this condition
					return fmt.Errorf(
						"id field is missing on entity %s - annotate a field with `id` tag", entity.Name)
				}
			}
		}

		if entity.IdProperty == nil {
			return fmt.Errorf("id field is missing on entity %s - either annotate a field with `id` tag "+
				"or use a uint64 field named 'Id/id/ID'", entity.Name)
		}
	}

	binding.Entities = append(binding.Entities, entity)
	return nil
}

// Supported annotations:
// id
// index (value|hash|hash64)
// unique
// nameInDb
// transient
func (property *Property) setAnnotations(tags string) error {
	if len(tags) > 1 && tags[0] == tags[len(tags)-1] && (tags[0] == '`' || tags[0] == '"') {
		tags = tags[1 : len(tags)-1]
	}
	if tags == "" {
		return nil
	}

	property.Annotations = make(map[string]*Annotation)

	// tags are space separated
	for _, tag := range strings.Split(tags, " ") {
		if len(tag) > 0 {
			var name string
			var value = &Annotation{}

			// if it contains a colon, it's a key:"value" pair
			if i := strings.IndexRune(tag, ':'); i >= 0 {
				name = tag[0:i]
				tag = tag[i+1:]

				if len(tag) > 1 && tag[0] == tag[len(tag)-1] && tag[0] == '"' {
					value.Value = strings.TrimSpace(tag[1 : len(tag)-1])
				} else {
					return fmt.Errorf("invalid annotation value %s for %s, expecting `name:\"value\"` format",
						tag, name)
				}
			} else {
				// otherwise there's no value
				name = tag
			}

			name = strings.ToLower(name)

			if property.Annotations[name] != nil {
				return fmt.Errorf("duplicate annotation %s", name)
			} else {
				property.Annotations[name] = value
			}
		}
	}

	return nil
}

func (property *Property) setType(t ast.Expr) error {
	property.GoType = types.ExprString(t)

	ts := property.GoType
	if property.GoType == "string" {
		property.ObType = "String"
		property.FbType = "UOffsetT"
	} else if ts == "int" || ts == "int64" {
		property.ObType = "Long"
		property.FbType = "Int64"
	} else if ts == "uint" || ts == "uint64" {
		property.ObType = "Long"
		property.FbType = "Uint64"
	} else if ts == "int32" || ts == "rune" {
		property.ObType = "Int"
		property.FbType = "Int32"
	} else if ts == "uint32" {
		property.ObType = "Int"
		property.FbType = "Uint32"
	} else if ts == "int16" {
		property.ObType = "Short"
		property.FbType = "Int16"
	} else if ts == "uint16" {
		property.ObType = "Short"
		property.FbType = "Uint16"
	} else if ts == "int8" {
		property.ObType = "Byte"
		property.FbType = "Int8"
	} else if ts == "uint8" {
		property.ObType = "Byte"
		property.FbType = "Uint8"
	} else if ts == "byte" {
		property.ObType = "Byte"
		property.FbType = "Byte"
	} else if ts == "[]byte" {
		property.ObType = "ByteVector"
		property.FbType = "UOffsetT"
	} else if ts == "float64" {
		property.ObType = "Double"
		property.FbType = "Float64"
	} else if ts == "float32" {
		property.ObType = "Float"
		property.FbType = "Float32"
	} else if ts == "bool" {
		property.ObType = "Bool"
		property.FbType = "Bool"
	} else {
		return fmt.Errorf("unknown type %s", ts)
	}

	if property.Annotations["date"] != nil {
		if property.ObType != "Long" {
			return fmt.Errorf("invalid underlying type (%s) for date field", property.ObType)
		} else {
			property.ObType = "Date"
		}
	}

	if property.Annotations["link"] != nil {
		if property.ObType != "Long" {
			return fmt.Errorf("invalid underlying type (%s) for relation field", property.ObType)
		} else {
			property.ObType = "Relation"
		}
		property.Relation = &Relation{
			Target: property.Annotations["link"].Value,
		}
	}

	return nil
}

func (property *Property) addObFlag(flag string) {
	property.ObFlags = append(property.ObFlags, flag)
}

func (property *Property) setIndex() error {
	if property.Index != nil {
		return fmt.Errorf("index is already defined")
	} else {
		property.Index = &Index{}
		return nil
	}
}

func (property *Property) setObFlags(f ast.Field) error {
	if property.Annotations["id"] != nil {
		property.addObFlag("ID")
	}

	if property.Annotations["index"] != nil {
		property.addObFlag("INDEXED")
		switch strings.ToLower(property.Annotations["index"].Value) {
		case "":
			// default
		case "value":
			// TODO this doesn't seem to be implemented by the c-api?
			property.addObFlag("INDEX_VALUE")
		case "hash":
			property.addObFlag("INDEX_HASH")
		case "hash64":
			property.addObFlag("INDEX_HASH64")
		default:
			return fmt.Errorf("unknown index type %s", property.Annotations["index"].Value)
		}

		if err := property.setIndex(); err != nil {
			return err
		}
	}

	if property.Annotations["unique"] != nil {
		property.addObFlag("UNIQUE")

		if err := property.setIndex(); err != nil {
			return err
		}
	}

	if property.Relation != nil {
		if err := property.setIndex(); err != nil {
			return err
		}
	}

	return nil
}

// called from the template
// avoid GO error "imported and not used"
func (binding *Binding) UsesFbUtils() bool {
	for _, entity := range binding.Entities {
		for _, property := range entity.Properties {
			if strings.ToLower(property.ObType) == "string" || strings.ToLower(property.ObType) == "bytevector" {
				return true
			}
		}
	}
	return false
}

// called from the template
// avoid GO error "variable declared and not used"
func (entity *Entity) HasNonIdProperty() bool {
	for _, prop := range entity.Properties {
		if prop != entity.IdProperty {
			return true
		}
	}

	return false
}

// calculates flatbuffers vTableOffset
// called from the template
func (property *Property) FbvTableOffset() uint16 {
	// derived from the FB generated code & https://google.github.io/flatbuffers/md__internals.html
	var result = 4 + 2*uint32(property.FbSlot())

	if uint32(uint16(result)) != result {
		panic(fmt.Errorf("can't calculate FlatBuffers VTableOffset: property %s ID %d is too large",
			property.Name, property.Id))
	}

	return uint16(result)
}

// calculates flatbuffers slot number
// called from the template
func (property *Property) FbSlot() int {
	return int(property.Id - 1)
}
