package terraform

import (
	"encoding/json"
	"flag"
	"fmt"
	"regexp"
	"strings"

	"github.com/zclconf/go-cty/cty"
	ctyJson "github.com/zclconf/go-cty/cty/json"

	"github.com/infracost/infracost/internal/config"
	"github.com/infracost/infracost/internal/hcl"
	"github.com/infracost/infracost/internal/schema"
)

type HCLProvider struct {
	Parser   *hcl.Parser
	Provider *PlanJSONProvider

	schema      *PlanSchema
	providerKey string
}

type flagStringSlice []string

func (v *flagStringSlice) String() string { return "" }
func (v *flagStringSlice) Set(raw string) error {
	*v = append(*v, raw)
	return nil
}

type vars struct {
	files []string
	vars  []string
}

var spaceReg = regexp.MustCompile(`\s+`)

func varsFromPlanFlags(planFlags string) (vars, error) {
	f := flag.NewFlagSet("", flag.ContinueOnError)

	var fs flagStringSlice
	var vs flagStringSlice

	f.Var(&vs, "var", "")
	f.Var(&fs, "var-file", "")
	err := f.Parse(spaceReg.Split(planFlags, -1))
	if err != nil {
		return vars{}, err
	}

	return vars{
		files: fs,
		vars:  vs,
	}, nil
}

// NewHCLProvider returns a HCLProvider with a hcl.Parser initialised using the config.ProjectContext.
// It will use input flags from either the terraform-plan-flags or top level var and var-file flags to
// set input vars and files on the underlying hcl.Parser.
func NewHCLProvider(ctx *config.ProjectContext, provider *PlanJSONProvider) (*HCLProvider, error) {
	v, err := varsFromPlanFlags(ctx.ProjectConfig.TerraformPlanFlags)
	if err != nil {
		return nil, fmt.Errorf("could not parse vars from plan flags %w", err)
	}

	var options []hcl.Option
	v.files = append(v.files, ctx.ProjectConfig.TerraformVarFiles...)
	if len(v.files) > 0 {
		withFiles := hcl.OptionWithTFVarsPaths(v.files)
		options = append(options, withFiles)
	}

	v.vars = append(v.vars, ctx.ProjectConfig.TerraformVars...)
	if len(v.vars) > 0 {
		withVars := hcl.OptionWithInputVars(v.vars)
		options = append(options, withVars)
	}

	p := hcl.New(ctx.ProjectConfig.Path, options...)

	return &HCLProvider{
		Parser:   p,
		Provider: provider,
	}, err
}

func (p *HCLProvider) Type() string                                 { return "terraform_hcl" }
func (p *HCLProvider) DisplayType() string                          { return "Terraform directory (HCL)" }
func (p *HCLProvider) AddMetadata(metadata *schema.ProjectMetadata) {}

// LoadResources calls a hcl.Parser to parse the directory config files into hcl.Blocks. It then builds a shallow
// representation of the terraform plan JSON files from these Blocks, this is passed to the PlanJSONProvider.
// The PlanJSONProvider uses this shallow representation to actually load Infracost resources.
func (p *HCLProvider) LoadResources(usage map[string]*schema.UsageData) ([]*schema.Project, error) {
	b, err := p.LoadPlanJSON()
	if err != nil {
		return nil, err
	}

	return p.Provider.LoadResourcesFromSrc(usage, b, nil)
}

// LoadPlanJSON parses the provided directory and returns it as a Terraform Plan JSON.
func (p *HCLProvider) LoadPlanJSON() ([]byte, error) {
	rootModule, err := p.Parser.ParseDirectory()
	if err != nil {
		return nil, err
	}

	return p.modulesToPlanJSON(rootModule)
}

func (p *HCLProvider) newPlanSchema() {
	p.schema = &PlanSchema{
		FormatVersion:    "1.0",
		TerraformVersion: "1.1.0",
		Variables:        nil,
		PlannedValues: struct {
			RootModule PlanModule `json:"root_module"`
		}{
			RootModule: PlanModule{
				Resources:    []ResourceJSON{},
				ChildModules: []PlanModule{},
			},
		},
		ResourceChanges: []ResourceChangesJSON{},
		Configuration: Configuration{
			ProviderConfig: make(map[string]ProviderConfig),
			RootModule: ModuleConfig{
				Resources:   []ResourceData{},
				ModuleCalls: map[string]ModuleCall{},
			},
		},
	}

	p.providerKey = ""
}

func (p *HCLProvider) modulesToPlanJSON(rootModule *hcl.Module) ([]byte, error) {
	p.newPlanSchema()

	mo := p.marshalModule(rootModule)
	p.schema.Configuration.RootModule = mo.ModuleConfig
	p.schema.PlannedValues.RootModule = mo.PlanModule

	b, err := json.MarshalIndent(p.schema, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("error handling built plan json from hcl %w", err)
	}
	return b, nil
}

func (p *HCLProvider) marshalModule(module *hcl.Module) ModuleOut {
	moduleConfig := ModuleConfig{
		ModuleCalls: map[string]ModuleCall{},
	}

	planModule := PlanModule{
		Address: newString(module.Name),
	}

	for _, block := range module.Blocks {
		if block.Type() == "provider" {
			p.marshalProviderBlock(block)
		}
	}

	configResources := map[string]struct{}{}
	for _, block := range module.Blocks {
		if block.Type() == "resource" {
			out := p.getResourceOutput(block)

			if _, ok := configResources[out.Configuration.Address]; !ok {
				moduleConfig.Resources = append(moduleConfig.Resources, out.Configuration)

				configResources[out.Configuration.Address] = struct{}{}
			}

			planModule.Resources = append(planModule.Resources, out.Planned)

			p.schema.ResourceChanges = append(p.schema.ResourceChanges, out.Changes)
		}
	}

	for _, m := range module.Modules {
		pieces := strings.Split(m.Name, ".")
		modKey := pieces[len(pieces)-1]

		mo := p.marshalModule(m)

		moduleConfig.ModuleCalls[modKey] = ModuleCall{
			Source:       m.Source,
			ModuleConfig: mo.ModuleConfig,
		}

		planModule.ChildModules = append(planModule.ChildModules, mo.PlanModule)
	}

	return ModuleOut{
		PlanModule:   planModule,
		ModuleConfig: moduleConfig,
	}
}

func (p *HCLProvider) getResourceOutput(block *hcl.Block) ResourceOutput {
	planned := ResourceJSON{
		Address:       block.FullName(),
		Mode:          "managed",
		Type:          block.TypeLabel(),
		Name:          stripCount(block.NameLabel()),
		Index:         block.Index(),
		SchemaVersion: 0,
	}

	changes := ResourceChangesJSON{
		Address:       block.FullName(),
		ModuleAddress: newString(block.ModuleAddress()),
		Mode:          "managed",
		Type:          block.TypeLabel(),
		Name:          stripCount(block.NameLabel()),
		Index:         block.Index(),
		Change: ResourceChange{
			Actions: []string{"create"},
		},
	}

	jsonValues := marshalAttributeValues(block.Type(), block.Values())
	marshalBlock(block, jsonValues)

	changes.Change.After = jsonValues
	planned.Values = jsonValues

	providerConfigKey := p.providerKey
	providerAttr := block.GetAttribute("provider")
	if providerAttr != nil {
		value := providerAttr.Value()
		r, err := providerAttr.Reference()
		if err == nil {
			providerConfigKey = r.String()
		}

		if err != nil && value.Type() == cty.String {
			providerConfigKey = value.AsString()
		}
	}

	var configuration ResourceData
	if block.HasModuleBlock() {
		configuration = ResourceData{
			Address:           stripCount(block.LocalName()),
			Mode:              "managed",
			Type:              block.TypeLabel(),
			Name:              stripCount(block.NameLabel()),
			ProviderConfigKey: block.ModuleName() + ":" + block.Provider(),
			Expressions:       blockToReferences(block),
			CountExpression:   countReferences(block),
		}
	} else {
		configuration = ResourceData{
			Address:           stripCount(block.FullName()),
			Mode:              "managed",
			Type:              block.TypeLabel(),
			Name:              stripCount(block.NameLabel()),
			ProviderConfigKey: providerConfigKey,
			Expressions:       blockToReferences(block),
			CountExpression:   countReferences(block),
		}
	}

	return ResourceOutput{
		Planned:       planned,
		Changes:       changes,
		Configuration: configuration,
	}
}

func (p *HCLProvider) marshalProviderBlock(block *hcl.Block) string {
	name := block.TypeLabel()
	if a := block.GetAttribute("alias"); a != nil {
		name = name + "." + a.Value().AsString()
	}

	region := ""
	value := block.GetAttribute("region").Value()
	if value != cty.NilVal {
		region = value.AsString()
	}

	p.schema.Configuration.ProviderConfig[name] = ProviderConfig{
		Name: name,
		Expressions: map[string]interface{}{
			"region": map[string]interface{}{
				"constant_value": region,
			},
		},
	}

	if p.providerKey == "" {
		p.providerKey = name
	}

	return name
}

func countReferences(block *hcl.Block) *countExpression {
	for _, attribute := range block.GetAttributes() {
		name := attribute.Name()
		if name != "count" {
			continue
		}

		exp := countExpression{}

		references := attribute.AllReferences()
		if len(references) > 0 {
			for _, ref := range references {
				exp.References = append(
					exp.References,
					strings.ReplaceAll(ref.String(), "variable", "var"),
				)
			}

			return &exp
		}

		v := attribute.Value()
		i, _ := v.AsBigFloat().Int64()
		exp.ConstantValue = &i

		return &exp
	}

	return nil
}

func blockToReferences(block *hcl.Block) map[string]interface{} {
	expressionValues := make(map[string]interface{})

	for _, attribute := range block.GetAttributes() {
		references := attribute.AllReferences()
		if len(references) > 0 {
			r := refs{}
			for _, ref := range references {
				r.References = append(r.References, ref.JSONString())
			}

			// counts are special expressions that have their own json key.
			// So we ignore them here.
			name := attribute.Name()
			if name == "count" {
				continue
			}

			expressionValues[name] = r
		}

		childExpressions := make(map[string][]interface{})
		for _, child := range block.Children() {
			vals := childExpressions[child.Type()]
			childReferences := blockToReferences(child)

			if len(childReferences) > 0 {
				childExpressions[child.Type()] = append(vals, childReferences)
			}
		}

		if len(childExpressions) > 0 {
			for name, v := range childExpressions {
				expressionValues[name] = v
			}
		}
	}

	return expressionValues
}

func marshalBlock(block *hcl.Block, jsonValues map[string]interface{}) {
	for _, b := range block.Children() {
		key := b.Type()
		if key == "dynamic" || key == "depends_on" {
			continue
		}

		childValues := marshalAttributeValues(key, b.Values())
		if len(b.Children()) > 0 {
			marshalBlock(b, childValues)
		}

		if v, ok := jsonValues[key]; ok {
			jsonValues[key] = append(v.([]interface{}), childValues)
			continue
		}

		jsonValues[key] = []interface{}{childValues}
	}
}

func marshalAttributeValues(blockType string, value cty.Value) map[string]interface{} {
	if value == cty.NilVal || value.IsNull() {
		return nil
	}
	ret := make(map[string]interface{})

	it := value.ElementIterator()
	for it.Next() {
		k, v := it.Element()
		vJSON, _ := ctyJson.Marshal(v, v.Type())
		key := k.AsString()

		if (blockType == "resource" || blockType == "module") && key == "count" {
			continue
		}

		ret[key] = json.RawMessage(vJSON)
	}
	return ret
}

type ResourceOutput struct {
	Planned       ResourceJSON
	Changes       ResourceChangesJSON
	Configuration ResourceData
}

type ResourceJSON struct {
	Address       string                 `json:"address"`
	Mode          string                 `json:"mode"`
	Type          string                 `json:"type"`
	Name          string                 `json:"name"`
	Index         *int64                 `json:"index,omitempty"`
	SchemaVersion int                    `json:"schema_version"`
	Values        map[string]interface{} `json:"values"`
}

type ResourceChangesJSON struct {
	Address       string         `json:"address"`
	ModuleAddress *string        `json:"module_address,omitempty"`
	Mode          string         `json:"mode"`
	Type          string         `json:"type"`
	Name          string         `json:"name"`
	Index         *int64         `json:"index,omitempty"`
	Change        ResourceChange `json:"change"`
}

type ResourceChange struct {
	Actions []string               `json:"actions"`
	Before  interface{}            `json:"before"`
	After   map[string]interface{} `json:"after"`
}

type PlanSchema struct {
	FormatVersion    string      `json:"format_version"`
	TerraformVersion string      `json:"terraform_version"`
	Variables        interface{} `json:"variables,omitempty"`
	PlannedValues    struct {
		RootModule PlanModule `json:"root_module"`
	} `json:"planned_values"`
	ResourceChanges []ResourceChangesJSON `json:"resource_changes"`
	Configuration   Configuration         `json:"configuration"`
}

type PlanModule struct {
	Resources    []ResourceJSON `json:"resources,omitempty"`
	Address      *string        `json:"address,omitempty"`
	ChildModules []PlanModule   `json:"child_modules,omitempty"`
}

type Configuration struct {
	ProviderConfig map[string]ProviderConfig `json:"provider_config"`
	RootModule     ModuleConfig              `json:"root_module"`
}

type ModuleConfig struct {
	Resources   []ResourceData        `json:"resources,omitempty"`
	ModuleCalls map[string]ModuleCall `json:"module_calls,omitempty"`
}

type ModuleOut struct {
	PlanModule   PlanModule
	ModuleConfig ModuleConfig
}

type ProviderConfig struct {
	Name        string                 `json:"name"`
	Expressions map[string]interface{} `json:"expressions,omitempty"`
}

type ResourceData struct {
	Address           string                 `json:"address"`
	Mode              string                 `json:"mode"`
	Type              string                 `json:"type"`
	Name              string                 `json:"name"`
	ProviderConfigKey string                 `json:"provider_config_key"`
	Expressions       map[string]interface{} `json:"expressions,omitempty"`
	SchemaVersion     int                    `json:"schema_version"`
	CountExpression   *countExpression       `json:"count_expression,omitempty"`
}

type ModuleCall struct {
	Source       string       `json:"source"`
	ModuleConfig ModuleConfig `json:"module"`
}

type countExpression struct {
	References    []string `json:"references,omitempty"`
	ConstantValue *int64   `json:"constant_value,omitempty"`
}

type refs struct {
	References []string `json:"references"`
}

func newString(s string) *string {
	if s == "" {
		return nil
	}

	return &s
}

var countRegex = regexp.MustCompile(`\[\d+\]$`)

func stripCount(s string) string {
	return countRegex.ReplaceAllString(s, "")
}
