package scan

import (
	"fmt"
	"go/ast"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/tools/go/loader"

	"github.com/casualjim/go-swagger/spec"
)

type schemaTypable struct {
	schema *spec.Schema
}

func (st schemaTypable) Typed(tpe, format string) {
	st.schema.Typed(tpe, format)
}

func (st schemaTypable) SetRef(ref spec.Ref) {
	st.schema.Ref = ref
}

func (st schemaTypable) Schema() *spec.Schema {
	return st.schema
}

func (st schemaTypable) Items() swaggerTypable {
	if st.schema.Items == nil {
		st.schema.Items = new(spec.SchemaOrArray)
	}
	if st.schema.Items.Schema == nil {
		st.schema.Items.Schema = new(spec.Schema)
	}

	st.schema.Typed("array", "")
	return schemaTypable{st.schema.Items.Schema}
}

type schemaValidations struct {
	current *spec.Schema
}

func (sv schemaValidations) SetMaximum(val float64, exclusive bool) {
	sv.current.Maximum = &val
	sv.current.ExclusiveMaximum = exclusive
}
func (sv schemaValidations) SetMinimum(val float64, exclusive bool) {
	sv.current.Minimum = &val
	sv.current.ExclusiveMinimum = exclusive
}
func (sv schemaValidations) SetMultipleOf(val float64) { sv.current.MultipleOf = &val }
func (sv schemaValidations) SetMinItems(val int64)     { sv.current.MinItems = &val }
func (sv schemaValidations) SetMaxItems(val int64)     { sv.current.MaxItems = &val }
func (sv schemaValidations) SetMinLength(val int64)    { sv.current.MinLength = &val }
func (sv schemaValidations) SetMaxLength(val int64)    { sv.current.MaxLength = &val }
func (sv schemaValidations) SetPattern(val string)     { sv.current.Pattern = val }
func (sv schemaValidations) SetUnique(val bool)        { sv.current.UniqueItems = val }

func newSchemaAnnotationParser(goName string) *schemaAnnotationParser {
	return &schemaAnnotationParser{GoName: goName, rx: rxModelOverride}
}

type schemaAnnotationParser struct {
	GoName string
	Name   string
	rx     *regexp.Regexp
}

func (sap *schemaAnnotationParser) Matches(line string) bool {
	return sap.rx.MatchString(line)
}

func (sap *schemaAnnotationParser) Parse(lines []string) error {
	if sap.Name != "" {
		return nil
	}

	if len(lines) > 0 {
		for _, line := range lines {
			matches := sap.rx.FindStringSubmatch(line)
			if len(matches) > 1 && len(matches[1]) > 0 {
				sap.Name = matches[1]
				return nil
			}
		}
	}
	return nil
}

type schemaDecl struct {
	File     *ast.File
	Decl     *ast.GenDecl
	TypeSpec *ast.TypeSpec
	GoName   string
	Name     string
}

func (sd *schemaDecl) inferNames() (goName string, name string) {
	if sd.GoName != "" {
		goName, name = sd.GoName, sd.Name
		return
	}
	goName = sd.TypeSpec.Name.Name
	name = goName
	if sd.Decl.Doc != nil {
	DECLS:
		for _, cmt := range sd.Decl.Doc.List {
			for _, ln := range strings.Split(cmt.Text, "\n") {
				matches := rxModelOverride.FindStringSubmatch(ln)
				if len(matches) > 1 && len(matches[1]) > 0 {
					name = matches[1]
					break DECLS
				}
			}
		}
	}
	sd.GoName = goName
	sd.Name = name
	return
}

type schemaParser struct {
	program   *loader.Program
	postDecls []schemaDecl
}

func newSchemaParser(prog *loader.Program) *schemaParser {
	scp := new(schemaParser)
	scp.program = prog
	return scp
}

func (scp *schemaParser) Parse(gofile *ast.File, target interface{}) error {
	tgt := target.(map[string]spec.Schema)
	for _, decl := range gofile.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spc := range gd.Specs {
			if ts, ok := spc.(*ast.TypeSpec); ok {
				sd := schemaDecl{gofile, gd, ts, "", ""}
				sd.inferNames()
				if err := scp.parseDecl(tgt, sd); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (scp *schemaParser) parseDecl(definitions map[string]spec.Schema, decl schemaDecl) error {
	// check if there is a +swagger:model tag that is followed by a word,
	// this word is the type name for swagger
	// the package and type are recorded in the extensions
	// once type name is found convert it to a schema, by looking up the schema in the
	// definitions dictionary that got passed into this parse method
	decl.inferNames()
	schema := definitions[decl.Name]
	schPtr := &schema

	// analyze doc comment for the model
	sp := new(sectionedParser)
	sp.setTitle = func(lines []string) { schema.Title = joinDropLast(lines) }
	sp.setDescription = func(lines []string) { schema.Description = joinDropLast(lines) }
	if err := sp.Parse(decl.Decl.Doc); err != nil {
		return err
	}

	// analyze struct body for fields etc
	// each exported struct field:
	// * gets a type mapped to a go primitive
	// * perhaps gets a format
	// * has to document the validations that apply for the type and the field
	// * when the struct field points to a model it becomes a ref: #/definitions/ModelName
	// * the first line of the comment is the title
	// * the following lines are the description
	if tpe, ok := decl.TypeSpec.Type.(*ast.StructType); ok {
		if err := scp.parseStructType(decl.File, schPtr, tpe, make(map[string]struct{})); err != nil {
			return err
		}
	}
	if decl.Name != decl.GoName {
		schPtr.AddExtension("x-go-name", decl.GoName)
	}
	for _, pkgInfo := range scp.program.AllPackages {
		if pkgInfo.Importable {
			for _, fil := range pkgInfo.Files {
				if fil.Pos() == decl.File.Pos() {
					schPtr.AddExtension("x-go-package", pkgInfo.Pkg.Path())
				}
			}
		}
	}
	definitions[decl.Name] = schema
	return nil
}

func (scp *schemaParser) parseEmbeddedStruct(gofile *ast.File, schema *spec.Schema, expr ast.Expr, seenPreviously map[string]struct{}) error {
	switch tpe := expr.(type) {
	case *ast.Ident:
		// do lookup of type
		// take primitives into account, they should result in an error for swagger
		pkg, err := scp.packageForFile(gofile)
		if err != nil {
			return err
		}
		file, _, ts, err := findSourceFile(pkg, tpe.Name)
		if err != nil {
			return err
		}
		if st, ok := ts.Type.(*ast.StructType); ok {
			return scp.parseStructType(file, schema, st, seenPreviously)
		}

	case *ast.SelectorExpr:
		// look up package, file and then type
		pkg, err := scp.packageForSelector(gofile, tpe.X)
		if err != nil {
			return fmt.Errorf("embedded struct: %v", err)
		}
		file, _, ts, err := findSourceFile(pkg, tpe.Sel.Name)
		if err != nil {
			return fmt.Errorf("embedded struct: %v", err)
		}
		if st, ok := ts.Type.(*ast.StructType); ok {
			return scp.parseStructType(file, schema, st, seenPreviously)
		}
	}
	return fmt.Errorf("unable to resolve embedded struct for: %v\n", expr)
}

func (scp *schemaParser) parseStructType(gofile *ast.File, schema *spec.Schema, tpe *ast.StructType, seenPreviously map[string]struct{}) error {
	schema.Typed("object", "")
	if tpe.Fields != nil {
		if schema.Properties == nil {
			schema.Properties = make(map[string]spec.Schema)
		}
		seenProperties := seenPreviously
		// first process embedded structs in order of embedding
		for _, fld := range tpe.Fields.List {
			if len(fld.Names) == 0 {
				// when the embedded struct is annotated with swagger:allOf it will be used as allOf property
				// otherwise the fields will just be included as normal properties
				if err := scp.parseEmbeddedStruct(gofile, schema, fld.Type, seenProperties); err != nil {
					return err
				}
				for k := range schema.Properties {
					seenProperties[k] = struct{}{}
				}
			}
		}
		// then add and possibly override values
		for _, fld := range tpe.Fields.List {
			if len(fld.Names) > 0 && fld.Names[0] != nil && fld.Names[0].IsExported() {
				var nm, gnm string
				nm = fld.Names[0].Name
				gnm = nm
				if fld.Tag != nil && len(strings.TrimSpace(fld.Tag.Value)) > 0 {
					tv, err := strconv.Unquote(fld.Tag.Value)
					if err != nil {
						return err
					}

					if strings.TrimSpace(tv) != "" {
						st := reflect.StructTag(tv)
						if st.Get("json") != "" {
							nm = strings.Split(st.Get("json"), ",")[0]
						}
					}
				}

				ps := schema.Properties[nm]
				if err := parseProperty(scp, gofile, fld.Type, schemaTypable{&ps}); err != nil {
					return err
				}

				sp := new(sectionedParser)
				sp.setDescription = func(lines []string) { ps.Description = joinDropLast(lines) }
				if ps.Ref.GetURL() == nil {
					sp.taggers = []tagParser{
						newSingleLineTagParser("maximum", &setMaximum{schemaValidations{&ps}, rxf(rxMaximumFmt, "")}),
						newSingleLineTagParser("minimum", &setMinimum{schemaValidations{&ps}, rxf(rxMinimumFmt, "")}),
						newSingleLineTagParser("multipleOf", &setMultipleOf{schemaValidations{&ps}, rxf(rxMultipleOfFmt, "")}),
						newSingleLineTagParser("minLength", &setMinLength{schemaValidations{&ps}, rxf(rxMinLengthFmt, "")}),
						newSingleLineTagParser("maxLength", &setMaxLength{schemaValidations{&ps}, rxf(rxMaxLengthFmt, "")}),
						newSingleLineTagParser("pattern", &setPattern{schemaValidations{&ps}, rxf(rxPatternFmt, "")}),
						newSingleLineTagParser("minItems", &setMinItems{schemaValidations{&ps}, rxf(rxMinItemsFmt, "")}),
						newSingleLineTagParser("maxItems", &setMaxItems{schemaValidations{&ps}, rxf(rxMaxItemsFmt, "")}),
						newSingleLineTagParser("unique", &setUnique{schemaValidations{&ps}, rxf(rxUniqueFmt, "")}),
						newSingleLineTagParser("required", &setRequiredSchema{schema, nm}),
						newSingleLineTagParser("readOnly", &setReadOnlySchema{&ps}),
					}

					// check if this is a primitive, if so parse the validations from the
					// doc comments of the slice declaration.
					if ftpe, ok := fld.Type.(*ast.ArrayType); ok {
						if iftpe, ok := ftpe.Elt.(*ast.Ident); ok && iftpe.Obj == nil {
							if ps.Items != nil && ps.Items.Schema != nil {
								itemsTaggers := []tagParser{
									newSingleLineTagParser("itemsMaximum", &setMaximum{schemaValidations{ps.Items.Schema}, rxf(rxMaximumFmt, rxItemsPrefix)}),
									newSingleLineTagParser("itemsMinimum", &setMinimum{schemaValidations{ps.Items.Schema}, rxf(rxMinimumFmt, rxItemsPrefix)}),
									newSingleLineTagParser("itemsMultipleOf", &setMultipleOf{schemaValidations{ps.Items.Schema}, rxf(rxMultipleOfFmt, rxItemsPrefix)}),
									newSingleLineTagParser("itemsMinLength", &setMinLength{schemaValidations{ps.Items.Schema}, rxf(rxMinLengthFmt, rxItemsPrefix)}),
									newSingleLineTagParser("itemsMaxLength", &setMaxLength{schemaValidations{ps.Items.Schema}, rxf(rxMaxLengthFmt, rxItemsPrefix)}),
									newSingleLineTagParser("itemsPattern", &setPattern{schemaValidations{ps.Items.Schema}, rxf(rxPatternFmt, rxItemsPrefix)}),
									newSingleLineTagParser("itemsMinItems", &setMinItems{schemaValidations{ps.Items.Schema}, rxf(rxMinItemsFmt, rxItemsPrefix)}),
									newSingleLineTagParser("itemsMaxItems", &setMaxItems{schemaValidations{ps.Items.Schema}, rxf(rxMaxItemsFmt, rxItemsPrefix)}),
									newSingleLineTagParser("itemsUnique", &setUnique{schemaValidations{ps.Items.Schema}, rxf(rxUniqueFmt, rxItemsPrefix)}),
								}

								// items matchers should go before the default matchers so they match first
								sp.taggers = append(itemsTaggers, sp.taggers...)
							}
						}
					}
				} else {
					sp.taggers = []tagParser{
						newSingleLineTagParser("required", &setRequiredSchema{schema, nm}),
					}
				}
				if err := sp.Parse(fld.Doc); err != nil {
					return err
				}

				if nm != gnm {
					ps.AddExtension("x-go-name", gnm)
				}
				seenProperties[nm] = struct{}{}
				schema.Properties[nm] = ps
			}
		}
		for k := range schema.Properties {
			if _, ok := seenProperties[k]; !ok {
				delete(schema.Properties, k)
			}
		}
	}

	return nil
}

func (scp *schemaParser) packageForFile(gofile *ast.File) (*loader.PackageInfo, error) {
	for pkg, pkgInfo := range scp.program.AllPackages {
		if pkg.Name() == gofile.Name.Name {
			return pkgInfo, nil
		}
	}
	fn := scp.program.Fset.File(gofile.Pos()).Name()
	return nil, fmt.Errorf("unable to determine package for %s", fn)
}

func (scp *schemaParser) packageForSelector(gofile *ast.File, expr ast.Expr) (*loader.PackageInfo, error) {

	if pth, ok := expr.(*ast.Ident); ok {
		// lookup import
		var selPath string
		for _, imp := range gofile.Imports {
			pv, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				pv = imp.Path.Value
			}
			if imp.Name != nil {
				if imp.Name.Name == pth.Name {
					selPath = pv
					break
				}
			} else {
				parts := strings.Split(pv, "/")
				if len(parts) > 0 && parts[len(parts)-1] == pth.Name {
					selPath = pv
					break
				}
			}
		}
		// find actual struct
		if selPath == "" {
			return nil, fmt.Errorf("no import found for %s", pth.Name)
		}

		pkg := scp.program.Package(selPath)
		if pkg == nil {
			return nil, fmt.Errorf("no package found for %s", selPath)
		}
		return pkg, nil
	}
	return nil, fmt.Errorf("can't determine selector path from %v", expr)
}

func (scp *schemaParser) parseSelectorProperty(pkg *loader.PackageInfo, expr *ast.Ident, prop swaggerTypable) error {
	// find the file this selector points to
	file, gd, ts, err := findSourceFile(pkg, expr.Name)
	if err != nil {
		return swaggerSchemaForType(expr.Name, prop)
	}
	if at, ok := ts.Type.(*ast.ArrayType); ok {
		// the swagger spec defines strfmt base64 as []byte.
		// in that case we don't actually want to turn it into an array
		// but we want to turn it into a string
		if _, ok := at.Elt.(*ast.Ident); ok {
			if strfmtName, ok := strfmtName(gd.Doc); ok {
				prop.Typed("string", strfmtName)
				return nil
			}
		}
		// this is a selector, so most likely not base64
		if strfmtName, ok := strfmtName(gd.Doc); ok {
			prop.Items().Typed("string", strfmtName)
			return nil
		}
	}

	// look at doc comments for +swagger:strfmt [name]
	// when found this is the format name, create a schema with that name
	if strfmtName, ok := strfmtName(gd.Doc); ok {
		prop.Typed("string", strfmtName)
		return nil
	}
	switch tpe := ts.Type.(type) {
	case *ast.ArrayType:
		switch atpe := tpe.Elt.(type) {
		case *ast.Ident:
			return scp.parseSelectorProperty(pkg, atpe, prop.Items())
		case *ast.SelectorExpr:
			return scp.typeForSelector(file, atpe, prop.Items())
		default:
			return fmt.Errorf("unknown selector type: %#v", tpe)
		}
	case *ast.StructType:
		sd := schemaDecl{file, gd, ts, "", ""}
		sd.inferNames()
		ref, err := spec.NewRef("#/definitions/" + sd.Name)
		if err != nil {
			return err
		}
		prop.SetRef(ref)
		scp.postDecls = append(scp.postDecls, sd)
		return nil

	case *ast.Ident:
		return scp.parseSelectorProperty(pkg, tpe, prop)

	case *ast.SelectorExpr:
		return scp.typeForSelector(file, tpe, prop)

	default:
		return swaggerSchemaForType(expr.Name, prop)
	}

}

func (scp *schemaParser) typeForSelector(gofile *ast.File, expr *ast.SelectorExpr, prop swaggerTypable) error {
	pkg, err := scp.packageForSelector(gofile, expr.X)
	if err != nil {
		return err
	}

	return scp.parseSelectorProperty(pkg, expr.Sel, prop)
}

func findSourceFile(pkg *loader.PackageInfo, typeName string) (*ast.File, *ast.GenDecl, *ast.TypeSpec, error) {
	for _, file := range pkg.Files {
		for _, decl := range file.Decls {
			if gd, ok := decl.(*ast.GenDecl); ok {
				for _, gs := range gd.Specs {
					if ts, ok := gs.(*ast.TypeSpec); ok {
						strfmtNme, isStrfmt := strfmtName(gd.Doc)
						if (isStrfmt && strfmtNme == typeName) || ts.Name != nil && ts.Name.Name == typeName {
							return file, gd, ts, nil
						}
					}
				}
			}
		}
	}
	return nil, nil, nil, fmt.Errorf("unable to find %s in %s", typeName, pkg.String())
}

func allOfMember(comments *ast.CommentGroup) bool {
	if comments != nil {
		for _, cmt := range comments.List {
			for _, ln := range strings.Split(cmt.Text, "\n") {
				if rxAllOf.MatchString(ln) {
					return true
				}
			}
		}
	}
	return false
}

func strfmtName(comments *ast.CommentGroup) (string, bool) {
	if comments != nil {
		for _, cmt := range comments.List {
			for _, ln := range strings.Split(cmt.Text, "\n") {
				matches := rxStrFmt.FindStringSubmatch(ln)
				if len(matches) > 1 && len(strings.TrimSpace(matches[1])) > 0 {
					return strings.TrimSpace(matches[1]), true
				}
			}
		}
	}
	return "", false
}

func parseProperty(scp *schemaParser, gofile *ast.File, fld ast.Expr, prop swaggerTypable) error {
	switch ftpe := fld.(type) {
	case *ast.Ident: // simple value
		pkg, err := scp.packageForFile(gofile)
		if err != nil {
			return swaggerSchemaForType(ftpe.Name, prop)
		}
		file, gd, tsp, err := findSourceFile(pkg, ftpe.Name)
		if err != nil {
			return swaggerSchemaForType(ftpe.Name, prop)
		}

		if _, ok := tsp.Type.(*ast.ArrayType); ok {
			if sfn, ok := strfmtName(gd.Doc); ok {
				prop.Items().Typed("string", sfn)
				return nil
			}
			return parseProperty(scp, gofile, tsp.Type, prop)
		}

		// Check if this might be a type decorated with strfmt
		if sfn, ok := strfmtName(gd.Doc); ok {
			prop.Typed("string", sfn)
			return nil
		}

		if _, ok := tsp.Type.(*ast.StructType); ok {
			// At this stage we're no longer interested in actually
			// parsing a struct like this, we're going to reference it instead
			// In addition to referencing, it is added to a bag of discovered schemas
			sd := schemaDecl{gofile, gd, tsp, "", ""}
			sd.inferNames()
			ref, err := spec.NewRef("#/definitions/" + sd.Name)
			if err != nil {
				return err
			}
			prop.SetRef(ref)
			scp.postDecls = append(scp.postDecls, sd)
			return nil
		}

		return parseProperty(scp, file, tsp.Type, prop)

	case *ast.StarExpr: // pointer to something, optional by default
		parseProperty(scp, gofile, ftpe.X, prop)

	case *ast.ArrayType: // slice type
		if err := parseProperty(scp, gofile, ftpe.Elt, prop.Items()); err != nil {
			return err
		}

	case *ast.StructType:
		schema := prop.Schema()
		if schema == nil {
			return fmt.Errorf("items doesn't support embedded structs")
		}
		return scp.parseStructType(gofile, prop.Schema(), ftpe, make(map[string]struct{}))

	case *ast.SelectorExpr:
		err := scp.typeForSelector(gofile, ftpe, prop)
		return err

	case *ast.MapType:
		// check if key is a string type, if not print a message
		// and skip the map property. Only maps with string keys can go into additional properties
		sch := prop.Schema()
		if keyIdent, ok := ftpe.Key.(*ast.Ident); sch != nil && ok {
			if keyIdent.Name == "string" {
				if sch.AdditionalProperties == nil {
					sch.AdditionalProperties = new(spec.SchemaOrBool)
				}
				sch.AdditionalProperties.Allows = false
				if sch.AdditionalProperties.Schema == nil {
					sch.AdditionalProperties.Schema = new(spec.Schema)
				}
				parseProperty(scp, gofile, ftpe.Value, schemaTypable{sch.AdditionalProperties.Schema})
				sch.Typed("object", "")
			}
		}

	case *ast.InterfaceType:
		// NOTE:
		// what to do with an interface? support it?
		// ignoring it for now
		// I guess something can be done with a discriminator field
		// but is it worth the trouble?
	default:
		return fmt.Errorf("%s is unsupported for a schema", ftpe)
	}
	return nil
}
