package generator

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-swagger/go-swagger/spec"
	"github.com/go-swagger/go-swagger/swag"
)

// GenerateModel generates a model file for a schema defintion
func GenerateModel(modelNames []string, includeModel, includeValidator bool, opts GenOpts) error {
	// Load the spec
	specPath, specDoc, err := loadSpec(opts.Spec)
	if err != nil {
		return err
	}

	if len(modelNames) == 0 {
		for k := range specDoc.Spec().Definitions {
			modelNames = append(modelNames, k)
		}
	}

	for _, modelName := range modelNames {
		// lookup schema
		model, ok := specDoc.Spec().Definitions[modelName]
		if !ok {
			return fmt.Errorf("model %q not found in definitions in %s", modelName, specPath)
		}

		// generate files
		generator := modelGenerator{
			Name:             modelName,
			Model:            model,
			SpecDoc:          specDoc,
			Target:           filepath.Join(opts.Target, opts.ModelPackage),
			IncludeModel:     includeModel,
			IncludeValidator: includeValidator,
			DumpData:         opts.DumpData,
		}

		if err := generator.Generate(); err != nil {
			return err
		}
	}

	return nil
}

type modelGenerator struct {
	Name             string
	Model            spec.Schema
	SpecDoc          *spec.Document
	Target           string
	IncludeModel     bool
	IncludeValidator bool
	Data             interface{}
	DumpData         bool
}

func (m *modelGenerator) Generate() error {
	mod, err := makeCodegenModel(m.Name, m.Target, m.Model, m.SpecDoc)
	if err != nil {
		return err
	}
	if m.DumpData {
		bb, _ := json.MarshalIndent(swag.ToDynamicJSON(mod), "", " ")
		fmt.Fprintln(os.Stdout, string(bb))
		return nil
	}

	m.Data = mod

	if m.IncludeModel {
		if err := m.generateModel(); err != nil {
			return fmt.Errorf("model: %s", err)
		}
	}
	log.Println("generated model", m.Name)

	if m.IncludeValidator {
		if err := m.generateValidator(); err != nil {
			return fmt.Errorf("validator: %s", err)
		}
	}
	log.Println("generated validator", m.Name)
	return nil
}

func (m *modelGenerator) generateValidator() error {
	buf := bytes.NewBuffer(nil)
	if err := modelValidatorTemplate.Execute(buf, m.Data); err != nil {
		return err
	}
	log.Println("rendered validator template:", m.Name)
	return writeToFile(m.Target, m.Name+"Validator", buf.Bytes())
}

func (m *modelGenerator) generateModel() error {
	buf := bytes.NewBuffer(nil)

	if err := modelTemplate.Execute(buf, m.Data); err != nil {
		return err
	}
	log.Println("rendered model template:", m.Name)

	return writeToFile(m.Target, m.Name, buf.Bytes())
}

func makeCodegenModel(name, pkg string, schema spec.Schema, specDoc *spec.Document) (*genModel, error) {
	receiver := "m"
	props := make(map[string]genModelProperty)
	for pn, p := range schema.Properties {
		var required bool
		for _, v := range schema.Required {
			if v == pn {
				required = true
				break
			}
		}

		gmp, err := makeGenModelProperty(propGenBuildParams{
			Path:      "\"" + pn + "\"",
			ParamName: swag.ToJSONName(pn),
			Name:      pn,
			Accessor:  swag.ToGoName(pn),
			Receiver:  receiver,
			IndexVar:  "i",
			ValueExpr: receiver + "." + swag.ToGoName(pn),
			Schema:    p,
			Required:  required,
			TypeResolver: &typeResolver{
				ModelsPackage: filepath.Base(pkg),
				Doc:           specDoc,
			},
		})
		if err != nil {
			return nil, err
		}
		props[swag.ToJSONName(pn)] = gmp
	}

	for _, p := range schema.AllOf {
		if p.Ref.GetURL() != nil {
			tn := filepath.Base(p.Ref.GetURL().Fragment)
			p = specDoc.Spec().Definitions[tn]
		}
		mod, err := makeCodegenModel(name, pkg, p, specDoc)
		if err != nil {
			return nil, err
		}
		if mod != nil {
			for _, prop := range mod.Properties {
				props[prop.ParamName] = prop
			}
		}
	}

	// TODO: add support for oneOf?
	// this would require a struct with unexported fields, custom json marshaller etc

	var properties []genModelProperty
	var hasValidations bool
	for _, v := range props {
		if v.HasValidations {
			hasValidations = v.HasValidations
		}
		properties = append(properties, v)
	}

	return &genModel{
		Package:        filepath.Base(pkg),
		ClassName:      swag.ToGoName(name),
		Name:           name,
		ReceiverName:   receiver,
		Properties:     properties,
		Description:    schema.Description,
		Title:          schema.Title,
		DocString:      modelDocString(swag.ToGoName(name), schema.Description),
		HumanClassName: swag.ToHumanNameLower(swag.ToGoName(name)),
		HasValidations: hasValidations,
	}, nil
}

type genModel struct {
	Package        string //`json:"package,omitempty"`
	ReceiverName   string //`json:"receiverName,omitempty"`
	ClassName      string //`json:"classname,omitempty"`
	Name           string //`json:"name,omitempty"`
	Title          string
	Description    string             //`json:"description,omitempty"`
	Properties     []genModelProperty //`json:"properties,omitempty"`
	DocString      string             //`json:"docString,omitempty"`
	HumanClassName string             //`json:"humanClassname,omitempty"`
	Imports        map[string]string  //`json:"imports,omitempty"`
	DefaultImports []string           //`json:"defaultImports,omitempty"`
	HasValidations bool               //`json:"hasValidatins,omitempty"`
}

func modelDocString(className, desc string) string {
	return commentedLines(fmt.Sprintf("%s %s", className, desc))
}

type propGenBuildParams struct {
	Path         string
	Name         string
	ParamName    string
	Accessor     string
	Receiver     string
	IndexVar     string
	ValueExpr    string
	Schema       spec.Schema
	Required     bool
	TypeResolver *typeResolver
}

func (pg propGenBuildParams) NewSliceBranch(schema *spec.Schema) propGenBuildParams {
	indexVar := pg.IndexVar
	pg.Path = "fmt.Sprintf(\"%s.%v\", " + pg.Path + ", " + indexVar + ")"
	pg.IndexVar = indexVar + "i"
	pg.ValueExpr = pg.ValueExpr + "[" + indexVar + "]"
	pg.Schema = *schema
	pg.Required = false
	return pg
}

func makeGenModelProperty(params propGenBuildParams) (genModelProperty, error) {
	// log.Printf("property: (path %s) (param %s) (accessor %s) (receiver %s) (indexVar %s) (expr %s) required %t", path, paramName, accessor, receiver, indexVar, valueExpression, required)
	ex := ""
	if params.Schema.Example != nil {
		ex = fmt.Sprintf("%#v", params.Schema.Example)
	}
	validations, err := modelValidations(params, false)
	if err != nil {
		return genModelProperty{}, err
	}

	ctx := makeGenValidations(validations)

	singleSchemaSlice := params.Schema.Items != nil && params.Schema.Items.Schema != nil
	var items []genModelProperty
	if singleSchemaSlice {
		ctx.HasSliceValidations = true

		elProp, err := makeGenModelProperty(params.NewSliceBranch(params.Schema.Items.Schema))
		if err != nil {
			return genModelProperty{}, err
		}
		items = []genModelProperty{
			elProp,
		}
	} else if params.Schema.Items != nil {
		for _, s := range params.Schema.Items.Schemas {
			elProp, err := makeGenModelProperty(params.NewSliceBranch(&s))
			if err != nil {
				return genModelProperty{}, err
			}
			items = append(items, elProp)
		}
	}

	allowsAdditionalItems :=
		params.Schema.AdditionalItems != nil &&
			(params.Schema.AdditionalItems.Allows || params.Schema.AdditionalItems.Schema != nil)
	hasAdditionalItems := allowsAdditionalItems && !singleSchemaSlice
	var additionalItems *genModelProperty
	if params.Schema.AdditionalItems != nil && params.Schema.AdditionalItems.Schema != nil {
		it, err := makeGenModelProperty(params.NewSliceBranch(params.Schema.AdditionalItems.Schema))
		if err != nil {
			return genModelProperty{}, err
		}
		additionalItems = &it
	}

	ctx.HasSliceValidations = len(items) > 0 || hasAdditionalItems
	ctx.HasValidations = ctx.HasValidations || ctx.HasSliceValidations

	var xmlName string
	if params.Schema.XML != nil {
		xmlName = params.ParamName
		if params.Schema.XML.Name != "" {
			xmlName = params.Schema.XML.Name
			if params.Schema.XML.Attribute {
				xmlName += ",attr"
			}
		}
	}

	return genModelProperty{
		sharedParam:     ctx,
		DataType:        ctx.Type,
		Example:         ex,
		Name:            params.Name,
		DocString:       propertyDocString(params.Accessor, params.Schema.Description, ex),
		Title:           params.Schema.Title,
		Description:     params.Schema.Description,
		ReceiverName:    params.Receiver,
		IsComplexObject: !ctx.IsPrimitive && !ctx.IsCustomFormatter && !ctx.IsContainer,

		HasAdditionalItems:    hasAdditionalItems,
		AllowsAdditionalItems: allowsAdditionalItems,
		AdditionalItems:       additionalItems,

		Items:             items,
		ItemsLen:          len(items),
		SingleSchemaSlice: singleSchemaSlice,

		XMLName: xmlName,
	}, nil
}

// NOTE:
// untyped data requires a cast somehow to the inner type
// I wonder if this is still a problem after adding support for tuples
// and anonymous structs. At that point there is very little that would
// end up being cast to interface, and if it does it truly is the best guess

type genModelProperty struct {
	sharedParam
	Example               string //`json:"example,omitempty"`
	Name                  string
	Title                 string
	Description           string             //`json:"description,omitempty"`
	DataType              string             //`json:"dataType,omitempty"`
	DocString             string             //`json:"docString,omitempty"`
	Location              string             //`json:"location,omitempty"`
	ReceiverName          string             //`json:"receiverName,omitempty"`
	IsComplexObject       bool               //`json:"isComplex,omitempty"` // not slice, custom formatter or primitive
	SingleSchemaSlice     bool               //`json:"singleSchemaSlice,omitempty"`
	Items                 []genModelProperty //`json:"items,omitempty"`
	ItemsLen              int                //`json:"itemsLength,omitempty"`
	AllowsAdditionalItems bool               //`json:"allowsAdditionalItems,omitempty"`
	HasAdditionalItems    bool               //`json:"hasAdditionalItems,omitempty"`
	AdditionalItems       *genModelProperty  //`json:"additionalItems,omitempty"`
	Object                *genModelProperty  //`json:"object,omitempty"`
	XMLName               string             //`json:"xmlName,omitempty"`
}

func modelValidations(params propGenBuildParams, isAnonymous bool) (commonValidations, error) {
	tpe, err := params.TypeResolver.ResolveSchema(&params.Schema, isAnonymous)
	if err != nil {
		return commonValidations{}, err
	}

	_, isPrimitive := primitives[tpe.GoType]
	_, isCustomFormatter := customFormatters[tpe.GoType]
	model := params.Schema

	return commonValidations{
		propertyDescriptor: propertyDescriptor{
			PropertyName:      params.Accessor,
			ParamName:         params.ParamName,
			ValueExpression:   params.ValueExpr,
			IndexVar:          params.IndexVar,
			Path:              params.Path,
			IsContainer:       tpe.IsArray,
			IsPrimitive:       isPrimitive,
			IsCustomFormatter: isCustomFormatter,
			IsMap:             tpe.IsMap,
			T:                 tpe,
		},
		Required:         params.Required,
		Type:             tpe.GoType,
		Format:           model.Format,
		Default:          model.Default,
		Maximum:          model.Maximum,
		ExclusiveMaximum: model.ExclusiveMaximum,
		Minimum:          model.Minimum,
		ExclusiveMinimum: model.ExclusiveMinimum,
		MaxLength:        model.MaxLength,
		MinLength:        model.MinLength,
		Pattern:          model.Pattern,
		MaxItems:         model.MaxItems,
		MinItems:         model.MinItems,
		UniqueItems:      model.UniqueItems,
		MultipleOf:       model.MultipleOf,
		Enum:             model.Enum,
	}, nil
}

func propertyDocString(propertyName, description, example string) string {
	ex := ""
	if strings.TrimSpace(example) != "" {
		ex = " eg.\n\n    " + example
	}
	return commentedLines(fmt.Sprintf("%s %s%s", propertyName, description, ex))
}
