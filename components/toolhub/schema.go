package toolhub

// Schema helpers. They build the same JSONSchema values a hand-written literal
// does, so a definition built with them encodes to the same bytes:
//
//	Object(
//		Prop("path", String("File path")),
//		Prop("limit", Number("Maximum entries")),
//	).Require("path")
//
// is
//
//	JSONSchema{
//		Type: "object",
//		Properties: NewProperties(
//			Property{Name: "path", Schema: JSONSchema{Type: "string", Description: "File path"}},
//			Property{Name: "limit", Schema: JSONSchema{Type: "number", Description: "Maximum entries"}},
//		),
//		Required: []string{"path"},
//	}
//
// An empty description is left out of the encoded schema, as in a literal.

// Object returns an object schema with the given properties in order. With no
// properties it still carries an empty, non-nil property set, which encodes as
// "properties":{} (the schema of a tool without arguments).
func Object(props ...Property) JSONSchema {
	return JSONSchema{Type: "object", Properties: NewProperties(props...)}
}

// Prop names a property schema for Object.
func Prop(name string, s JSONSchema) Property {
	return Property{Name: name, Schema: s}
}

// String returns a string schema.
func String(desc string) JSONSchema {
	return JSONSchema{Type: "string", Description: desc}
}

// Enum returns a string schema that allows only the given values. Called with no
// values it writes "enum":[], which denies every string (see JSONSchema).
func Enum(desc string, values ...string) JSONSchema {
	enum := make([]string, len(values))
	copy(enum, values)
	return JSONSchema{Type: "string", Enum: enum, Description: desc}
}

// Number returns a number schema.
func Number(desc string) JSONSchema {
	return JSONSchema{Type: "number", Description: desc}
}

// Integer returns an integer schema. The default policy accepts a JSON number
// with no fractional part for it.
func Integer(desc string) JSONSchema {
	return JSONSchema{Type: "integer", Description: desc}
}

// Boolean returns a boolean schema.
func Boolean(desc string) JSONSchema {
	return JSONSchema{Type: "boolean", Description: desc}
}

// Array returns an array schema whose items follow items. items is copied.
func Array(items JSONSchema, desc string) JSONSchema {
	it := items.Clone()
	return JSONSchema{Type: "array", Items: &it, Description: desc}
}

// Require returns s with Required set to a copy of names. Called with no names it
// clears Required, so nothing is written.
func (s JSONSchema) Require(names ...string) JSONSchema {
	if len(names) == 0 {
		s.Required = nil
		return s
	}
	s.Required = make([]string, len(names))
	copy(s.Required, names)
	return s
}
