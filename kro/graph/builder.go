// Copyright 2025 The Kubernetes Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package graph

import (
	"fmt"
	"slices"
	"strings"

	"github.com/google/cel-go/cel"
	"golang.org/x/exp/maps"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/apiserver/pkg/cel/openapi"
	"k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/kube-openapi/pkg/validation/spec"

	"github.com/upbound/function-kro/input/v1beta1"
	krocel "github.com/upbound/function-kro/kro/cel"
	"github.com/upbound/function-kro/kro/cel/ast"
	"github.com/upbound/function-kro/kro/graph/dag"
	"github.com/upbound/function-kro/kro/graph/fieldpath"
	"github.com/upbound/function-kro/kro/graph/parser"
	kroschema "github.com/upbound/function-kro/kro/graph/schema"
	schemaresolver "github.com/upbound/function-kro/kro/graph/schema/resolver"
	"github.com/upbound/function-kro/kro/graph/variable"
	"github.com/upbound/function-kro/kro/metadata"
)

// NewBuilder creates a new GraphBuilder instance that uses CRDs directly
// instead of API server discovery. This is adapted for Crossplane functions
// which receive CRDs as extra resources.
func NewBuilder(crds ...*extv1.CustomResourceDefinition) (*Builder, error) {
	schemaResolver, err := schemaresolver.NewCombinedResolver(crds...)
	if err != nil {
		return nil, fmt.Errorf("failed to create schema resolver: %w", err)
	}

	// Build a map of GVK to GVR from the CRDs for resource mapping
	gvkToGVR := make(map[schema.GroupVersionKind]schema.GroupVersionResource)
	for _, crd := range crds {
		for _, v := range crd.Spec.Versions {
			gvk := schema.GroupVersionKind{
				Group:   crd.Spec.Group,
				Version: v.Name,
				Kind:    crd.Spec.Names.Kind,
			}
			gvr := schema.GroupVersionResource{
				Group:    crd.Spec.Group,
				Version:  v.Name,
				Resource: crd.Spec.Names.Plural,
			}
			gvkToGVR[gvk] = gvr
		}
	}

	// Build a map of GVK to scope (namespaced vs cluster-scoped)
	gvkNamespaced := make(map[schema.GroupVersionKind]bool)
	for _, crd := range crds {
		namespaced := crd.Spec.Scope == extv1.NamespaceScoped
		for _, v := range crd.Spec.Versions {
			gvk := schema.GroupVersionKind{
				Group:   crd.Spec.Group,
				Version: v.Name,
				Kind:    crd.Spec.Names.Kind,
			}
			gvkNamespaced[gvk] = namespaced
		}
	}

	rgBuilder := &Builder{
		schemaResolver: schemaResolver,
		gvkToGVR:       gvkToGVR,
		gvkNamespaced:  gvkNamespaced,
	}
	return rgBuilder, nil
}

// Builder is an object that is responsible for constructing and managing
// resourceGraphs. It is responsible for transforming the ResourceGraph input
// into a runtime representation that can be used to create the resources.
//
// The GraphBuilder performs several key functions:
//
//  1. It validates the resource definitions and their naming conventions.
//  2. It uses CRD schemas to retrieve the OpenAPI schema for the resources,
//     and validates the resources against the schema.
//  3. Extracts and processes the CEL expressions from the resources definitions.
//  4. Builds the dependency graph between the resources, by inspecting the CEL
//     expressions.
//
// If any of the above steps fail, the Builder will return an error.
type Builder struct {
	// schemaResolver is used to resolve the OpenAPI schema for the resources.
	schemaResolver resolver.SchemaResolver
	// gvkToGVR maps GroupVersionKind to GroupVersionResource
	gvkToGVR map[schema.GroupVersionKind]schema.GroupVersionResource
	// gvkNamespaced tracks whether each GVK is namespaced
	gvkNamespaced map[schema.GroupVersionKind]bool
}

// NewResourceGraphDefinition creates a new Graph object from the given ResourceGraph
// and the CRD for the composite resource. The Graph object is a fully processed
// and validated representation of the resource graph, its underlying resources,
// and the relationships between the resources.
func (b *Builder) NewResourceGraphDefinition(rg *v1beta1.ResourceGraph, xrCRD *extv1.CustomResourceDefinition) (*Graph, error) {
	// Before anything else, let's copy the resource graph to avoid modifying the
	// original object.
	rgd := rg.DeepCopy()

	// Validate the naming convention of the resources.
	// kro leverages CEL expressions to allow users to define new types and
	// express relationships between resources. This means that we need to ensure
	// that the names of the resources are valid to be used in CEL expressions.
	err := validateResourceIDs(rgd)
	if err != nil {
		return nil, fmt.Errorf("failed to validate resource graph: %w", err)
	}

	// For each resource in the resource graph, we need to:
	// 1. Check if it looks like a valid Kubernetes resource.
	// 2. Based the GVK, we need to load the OpenAPI schema for the resource.
	// 3. Extract the CEL expressions from the resource + validate them.

	resources := make(map[string]*Resource)
	for i, rgResource := range rgd.Resources {
		id := rgResource.ID
		order := i
		r, err := b.buildRGResource(rgResource, order)
		if err != nil {
			return nil, fmt.Errorf("failed to build resource %q: %w", id, err)
		}
		if resources[id] != nil {
			return nil, fmt.Errorf("found resources with duplicate id %q", id)
		}
		resources[id] = r
	}

	// Build the instance resource from the XR CRD
	instance, err := b.buildInstanceResource(xrCRD, rgd, resources)
	if err != nil {
		return nil, fmt.Errorf("failed to build instance resource: %w", err)
	}

	// Collect all OpenAPI schemas for CEL type checking
	schemas := make(map[string]*spec.Schema)
	for id, resource := range resources {
		if resource.schema != nil {
			schemas[id] = resource.schema
		}
	}

	// Include the instance spec schema as "schema"
	schemaWithoutStatus, err := getSchemaWithoutStatus(instance.crd)
	if err != nil {
		return nil, fmt.Errorf("failed to get schema without status: %w", err)
	}
	schemas["schema"] = schemaWithoutStatus

	// Create a DeclTypeProvider for type introspection
	typeProvider := krocel.CreateDeclTypeProvider(schemas)

	// Build the dependency graph by inspecting CEL expressions
	dag, err := b.buildDependencyGraph(resources)
	if err != nil {
		return nil, fmt.Errorf("failed to build dependency graph: %w", err)
	}

	// Get topological order
	topologicalOrder, err := dag.TopologicalSort()
	if err != nil {
		return nil, fmt.Errorf("failed to get topological order: %w", err)
	}

	// Create typed CEL environment for template expressions
	templatesEnv, err := krocel.TypedEnvironment(schemas)
	if err != nil {
		return nil, fmt.Errorf("failed to create typed CEL environment: %w", err)
	}

	// Create CEL environment for includeWhen expressions (schema only)
	var schemaEnv *cel.Env
	if schemas["schema"] != nil {
		schemaEnv, err = krocel.TypedEnvironment(map[string]*spec.Schema{"schema": schemas["schema"]})
		if err != nil {
			return nil, fmt.Errorf("failed to create CEL environment for includeWhen validation: %w", err)
		}
	}

	// Validate all CEL expressions for each resource
	for _, resource := range resources {
		if err := validateNode(resource, templatesEnv, schemaEnv, schemas[resource.id], typeProvider); err != nil {
			return nil, fmt.Errorf("failed to validate node %q: %w", resource.id, err)
		}
	}

	resourceGraph := &Graph{
		DAG:              dag,
		Instance:         instance,
		Resources:        resources,
		TopologicalOrder: topologicalOrder,
	}
	return resourceGraph, nil
}

// buildExternalRefResource builds an empty resource with metadata from the given externalRef definition.
func (b *Builder) buildExternalRefResource(externalRef *v1beta1.ExternalRef) map[string]interface{} {
	resourceObject := map[string]interface{}{}
	resourceObject["apiVersion"] = externalRef.APIVersion
	resourceObject["kind"] = externalRef.Kind
	metadata := map[string]interface{}{
		"name": externalRef.Metadata.Name,
	}
	if externalRef.Metadata.Namespace != "" {
		metadata["namespace"] = externalRef.Metadata.Namespace
	}
	resourceObject["metadata"] = metadata
	return resourceObject
}

// buildRGResource builds a resource from the given resource definition.
func (b *Builder) buildRGResource(
	rgResource *v1beta1.Resource,
	order int,
) (*Resource, error) {
	// Unmarshal the resource into a map[string]interface{}
	resourceObject := map[string]interface{}{}
	if len(rgResource.Template.Raw) > 0 {
		err := yaml.UnmarshalStrict(rgResource.Template.Raw, &resourceObject)
		if err != nil {
			return nil, fmt.Errorf("failed to unmarshal resource %s: %w", rgResource.ID, err)
		}
	} else if rgResource.ExternalRef != nil {
		resourceObject = b.buildExternalRefResource(rgResource.ExternalRef)
	} else {
		return nil, fmt.Errorf("exactly one of template or externalRef must be provided")
	}

	// Validate it looks like a Kubernetes resource
	err := validateKubernetesObjectStructure(resourceObject)
	if err != nil {
		return nil, fmt.Errorf("resource %s is not a valid Kubernetes object: %v", rgResource.ID, err)
	}

	// Extract GVK
	gvk, err := metadata.ExtractGVKFromUnstructured(resourceObject)
	if err != nil {
		return nil, fmt.Errorf("failed to extract GVK from resource %s: %w", rgResource.ID, err)
	}

	// Load OpenAPI schema
	resourceSchema, err := b.schemaResolver.ResolveSchema(gvk)
	if err != nil {
		return nil, fmt.Errorf("failed to get schema for resource %s: %w", rgResource.ID, err)
	}

	// Extract CEL fieldDescriptors from the resource
	var fieldDescriptors []variable.FieldDescriptor
	if gvk.Group == "apiextensions.k8s.io" && gvk.Version == "v1" && gvk.Kind == "CustomResourceDefinition" {
		fieldDescriptors, err = parser.ParseSchemalessResource(resourceObject)
		if err != nil {
			return nil, fmt.Errorf("failed to parse schemaless resource %s: %w", rgResource.ID, err)
		}

		for _, expr := range fieldDescriptors {
			if !strings.HasPrefix(expr.Path, "metadata.") {
				return nil, fmt.Errorf("CEL expressions in CRDs are only supported for metadata fields, found in path %q, resource %s", expr.Path, rgResource.ID)
			}
		}
	} else {
		fieldDescriptors, err = parser.ParseResource(resourceObject, resourceSchema)
		if err != nil {
			return nil, fmt.Errorf("failed to extract CEL expressions from schema for resource %s: %w", rgResource.ID, err)
		}

		// Set ExpectedType on each descriptor
		for i := range fieldDescriptors {
			setExpectedTypeOnDescriptor(&fieldDescriptors[i], resourceSchema, rgResource.ID)
		}
	}

	templateVariables := make([]*variable.ResourceField, 0, len(fieldDescriptors))
	for _, fieldDescriptor := range fieldDescriptors {
		templateVariables = append(templateVariables, &variable.ResourceField{
			Kind:            variable.ResourceVariableKindStatic,
			FieldDescriptor: fieldDescriptor,
		})
	}

	// Parse ReadyWhen expressions
	readyWhen, err := parser.ParseConditionExpressions(rgResource.ReadyWhen)
	if err != nil {
		return nil, fmt.Errorf("failed to parse readyWhen expressions: %v", err)
	}

	// Parse IncludeWhen expressions
	includeWhen, err := parser.ParseConditionExpressions(rgResource.IncludeWhen)
	if err != nil {
		return nil, fmt.Errorf("failed to parse includeWhen expressions: %v", err)
	}

	// Get GVR and namespaced scope from our maps
	gvr, ok := b.gvkToGVR[gvk]
	if !ok {
		// Fallback: compute GVR from GVK (pluralize kind)
		gvr = schema.GroupVersionResource{
			Group:    gvk.Group,
			Version:  gvk.Version,
			Resource: strings.ToLower(gvk.Kind) + "s",
		}
	}
	namespaced := b.gvkNamespaced[gvk]

	return &Resource{
		id:                     rgResource.ID,
		gvr:                    gvr,
		schema:                 resourceSchema,
		originalObject:         &unstructured.Unstructured{Object: resourceObject},
		variables:              templateVariables,
		readyWhenExpressions:   readyWhen,
		includeWhenExpressions: includeWhen,
		namespaced:             namespaced,
		order:                  order,
		isExternalRef:          rgResource.ExternalRef != nil,
	}, nil
}

// buildDependencyGraph builds the dependency graph between the resources.
func (b *Builder) buildDependencyGraph(
	resources map[string]*Resource,
) (*dag.DirectedAcyclicGraph[string], error) {
	resourceNames := maps.Keys(resources)
	resourceNames = append(resourceNames, "schema")

	env, err := krocel.DefaultEnvironment(krocel.WithResourceIDs(resourceNames))
	if err != nil {
		return nil, fmt.Errorf("failed to create CEL environment: %w", err)
	}

	directedAcyclicGraph := dag.NewDirectedAcyclicGraph[string]()
	for _, resource := range resources {
		if err := directedAcyclicGraph.AddVertex(resource.id, resource.order); err != nil {
			return nil, fmt.Errorf("failed to add vertex to graph: %w", err)
		}
	}

	for _, resource := range resources {
		for _, templateVariable := range resource.variables {
			for _, expression := range templateVariable.Expressions {
				resourceDependencies, isStatic, err := extractDependencies(env, expression, resourceNames)
				if err != nil {
					return nil, fmt.Errorf("failed to extract dependencies: %w", err)
				}

				if !isStatic && templateVariable.Kind == variable.ResourceVariableKindStatic {
					templateVariable.Kind = variable.ResourceVariableKindDynamic
				}

				resource.addDependencies(resourceDependencies...)
				templateVariable.AddDependencies(resourceDependencies...)
				if err := directedAcyclicGraph.AddDependencies(resource.id, resourceDependencies); err != nil {
					return nil, err
				}
			}
		}
	}

	return directedAcyclicGraph, nil
}

// buildInstanceResource builds the instance resource from the XR CRD.
func (b *Builder) buildInstanceResource(
	xrCRD *extv1.CustomResourceDefinition,
	rgd *v1beta1.ResourceGraph,
	resources map[string]*Resource,
) (*Resource, error) {
	// Get GVR from the XR CRD
	gvr := schema.GroupVersionResource{
		Group:    xrCRD.Spec.Group,
		Version:  xrCRD.Spec.Versions[0].Name,
		Resource: xrCRD.Spec.Names.Plural,
	}

	// Get the schema from the CRD
	instanceSchemaExt := xrCRD.Spec.Versions[0].Schema.OpenAPIV3Schema
	instanceSchema, err := kroschema.ConvertJSONSchemaPropsToSpecSchema(instanceSchemaExt)
	if err != nil {
		return nil, fmt.Errorf("failed to convert JSON schema to spec schema: %w", err)
	}

	// Parse status CEL expressions if provided
	statusVariables := []*variable.ResourceField{}
	if len(rgd.Status.Raw) > 0 {
		unstructuredStatus := map[string]interface{}{}
		err := yaml.UnmarshalStrict(rgd.Status.Raw, &unstructuredStatus)
		if err != nil {
			return nil, fmt.Errorf("failed to unmarshal status schema: %w", err)
		}

		fieldDescriptors, err := parser.ParseSchemalessResource(unstructuredStatus)
		if err != nil {
			return nil, fmt.Errorf("failed to extract CEL expressions from status: %w", err)
		}

		resourceNames := maps.Keys(resources)
		env, err := krocel.DefaultEnvironment(krocel.WithResourceIDs(resourceNames))
		if err != nil {
			return nil, fmt.Errorf("failed to create CEL environment: %w", err)
		}

		for _, fd := range fieldDescriptors {
			path := "status." + fd.Path
			fd.Path = path

			instanceDependencies, isStatic, err := extractDependencies(env, fd.Expressions[0], resourceNames)
			if err != nil {
				return nil, fmt.Errorf("failed to extract dependencies: %w", err)
			}
			if isStatic {
				return nil, fmt.Errorf("instance status field must refer to a resource: %s", fd.Path)
			}

			statusVariables = append(statusVariables, &variable.ResourceField{
				FieldDescriptor: fd,
				Kind:            variable.ResourceVariableKindDynamic,
				Dependencies:    instanceDependencies,
			})
		}
	}

	instance := &Resource{
		id:        "instance",
		gvr:       gvr,
		schema:    instanceSchema,
		crd:       xrCRD,
		variables: statusVariables,
	}

	return instance, nil
}

// extractDependencies extracts the dependencies from the given CEL expression.
func extractDependencies(env *cel.Env, expression string, resourceNames []string) ([]string, bool, error) {
	inspector := ast.NewInspectorWithEnv(env, resourceNames)

	inspectionResult, err := inspector.Inspect(expression)
	if err != nil {
		return nil, false, fmt.Errorf("failed to inspect expression: %w", err)
	}

	isStatic := true
	dependencies := make([]string, 0)
	for _, resource := range inspectionResult.ResourceDependencies {
		if resource.ID != "schema" && !slices.Contains(dependencies, resource.ID) {
			isStatic = false
			dependencies = append(dependencies, resource.ID)
		}
	}
	if len(inspectionResult.UnknownResources) > 0 {
		return nil, false, fmt.Errorf("found unknown resources in CEL expression: [%v]", inspectionResult.UnknownResources)
	}
	if len(inspectionResult.UnknownFunctions) > 0 {
		return nil, false, fmt.Errorf("found unknown functions in CEL expression: [%v]", inspectionResult.UnknownFunctions)
	}
	return dependencies, isStatic, nil
}

// resolveSchemaAndTypeName resolves schema and type name for a field path
func resolveSchemaAndTypeName(segments []fieldpath.Segment, rootSchema *spec.Schema, resourceID string) (*spec.Schema, string, error) {
	typeName := krocel.TypeNamePrefix + resourceID
	currentSchema := rootSchema

	for _, seg := range segments {
		if seg.Name != "" {
			typeName = typeName + "." + seg.Name
			currentSchema = lookupSchemaAtPath(currentSchema, seg.Name)
			if currentSchema == nil {
				return nil, "", fmt.Errorf("field %q not found in schema", seg.Name)
			}
		}

		if seg.Index != -1 {
			if currentSchema.Items != nil && currentSchema.Items.Schema != nil {
				currentSchema = currentSchema.Items.Schema
				typeName = typeName + ".@idx"
			} else {
				return nil, "", fmt.Errorf("field is not an array")
			}
		}
	}

	return currentSchema, typeName, nil
}

func setExpectedTypeOnDescriptor(descriptor *variable.FieldDescriptor, rootSchema *spec.Schema, resourceID string) {
	if !descriptor.StandaloneExpression {
		descriptor.ExpectedType = cel.StringType
		return
	}

	segments, err := fieldpath.Parse(descriptor.Path)
	if err != nil {
		descriptor.ExpectedType = cel.DynType
		return
	}

	schema, typeName, err := resolveSchemaAndTypeName(segments, rootSchema, resourceID)
	if err != nil {
		descriptor.ExpectedType = cel.DynType
		return
	}

	descriptor.ExpectedType = getCelTypeFromSchema(schema, typeName)
}

func getCelTypeFromSchema(schema *spec.Schema, typeName string) *cel.Type {
	if schema == nil {
		return cel.DynType
	}

	declType := krocel.SchemaDeclTypeWithMetadata(&openapi.Schema{Schema: schema}, false)
	if declType == nil {
		return cel.DynType
	}

	declType = declType.MaybeAssignTypeName(typeName)
	return declType.CelType()
}

func lookupSchemaAtPath(schema *spec.Schema, path string) *spec.Schema {
	if path == "" {
		return schema
	}

	parts := strings.Split(path, ".")
	current := schema

	for _, part := range parts {
		if current == nil {
			return nil
		}

		if prop, ok := current.Properties[part]; ok {
			current = &prop
			continue
		}

		if current.Items != nil && current.Items.Schema != nil {
			current = current.Items.Schema
			if prop, ok := current.Properties[part]; ok {
				current = &prop
				continue
			}
		}

		return nil
	}

	return current
}

// validateNode validates all CEL expressions for a single resource node
func validateNode(resource *Resource, templatesEnv, schemaEnv *cel.Env, resourceSchema *spec.Schema, typeProvider *krocel.DeclTypeProvider) error {
	if err := validateTemplateExpressions(templatesEnv, resource, typeProvider); err != nil {
		return err
	}

	if len(resource.includeWhenExpressions) > 0 {
		if err := validateIncludeWhenExpressions(schemaEnv, resource); err != nil {
			return err
		}
	}

	if len(resource.readyWhenExpressions) > 0 {
		resourceEnv, err := krocel.TypedEnvironment(map[string]*spec.Schema{resource.id: resourceSchema})
		if err != nil {
			return fmt.Errorf("failed to create CEL environment for readyWhen validation: %w", err)
		}

		if err := validateReadyWhenExpressions(resourceEnv, resource); err != nil {
			return err
		}
	}

	return nil
}

func validateTemplateExpressions(env *cel.Env, resource *Resource, typeProvider *krocel.DeclTypeProvider) error {
	for _, templateVariable := range resource.variables {
		if len(templateVariable.Expressions) == 1 {
			expression := templateVariable.Expressions[0]

			checkedAST, err := parseAndCheckCELExpression(env, expression)
			if err != nil {
				return fmt.Errorf("failed to type-check template expression %q at path %q: %w", expression, templateVariable.Path, err)
			}
			outputType := checkedAST.OutputType()
			if err := validateExpressionType(outputType, templateVariable.ExpectedType, expression, resource.id, templateVariable.Path, typeProvider); err != nil {
				return err
			}
		} else if len(templateVariable.Expressions) > 1 {
			for _, expression := range templateVariable.Expressions {
				checkedAST, err := parseAndCheckCELExpression(env, expression)
				if err != nil {
					return fmt.Errorf("failed to type-check template expression %q at path %q: %w", expression, templateVariable.Path, err)
				}

				outputType := checkedAST.OutputType()
				if err := validateExpressionType(outputType, templateVariable.ExpectedType, expression, resource.id, templateVariable.Path, typeProvider); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validateExpressionType(outputType, expectedType *cel.Type, expression, resourceID, path string, typeProvider *krocel.DeclTypeProvider) error {
	if expectedType.IsAssignableType(outputType) {
		return nil
	}

	compatible, compatErr := krocel.AreTypesStructurallyCompatible(outputType, expectedType, typeProvider)
	if compatible {
		return nil
	}
	if compatErr != nil {
		return fmt.Errorf(
			"type mismatch in resource %q at path %q: expression %q returns type %q but expected %q: %w",
			resourceID, path, expression, outputType.String(), expectedType.String(), compatErr,
		)
	}

	return fmt.Errorf(
		"type mismatch in resource %q at path %q: expression %q returns type %q but expected %q",
		resourceID, path, expression, outputType.String(), expectedType.String(),
	)
}

func parseAndCheckCELExpression(env *cel.Env, expression string) (*cel.Ast, error) {
	parsedAST, issues := env.Parse(expression)
	if issues != nil && issues.Err() != nil {
		return nil, issues.Err()
	}

	checkedAST, issues := env.Check(parsedAST)
	if issues != nil && issues.Err() != nil {
		return nil, issues.Err()
	}

	return checkedAST, nil
}

func validateConditionExpression(env *cel.Env, expression, conditionType, resourceID string) error {
	checkedAST, err := parseAndCheckCELExpression(env, expression)
	if err != nil {
		return fmt.Errorf("failed to type-check %s expression %q in resource %q: %w", conditionType, expression, resourceID, err)
	}

	outputType := checkedAST.OutputType()
	if !krocel.IsBoolOrOptionalBool(outputType) {
		return fmt.Errorf(
			"%s expression %q in resource %q must return bool or optional_type(bool), but returns %q",
			conditionType, expression, resourceID, outputType.String(),
		)
	}

	return nil
}

func validateIncludeWhenExpressions(env *cel.Env, resource *Resource) error {
	for _, expression := range resource.includeWhenExpressions {
		if err := validateConditionExpression(env, expression, "includeWhen", resource.id); err != nil {
			return err
		}
	}
	return nil
}

func validateReadyWhenExpressions(env *cel.Env, resource *Resource) error {
	for _, expression := range resource.readyWhenExpressions {
		if err := validateConditionExpression(env, expression, "readyWhen", resource.id); err != nil {
			return err
		}
	}
	return nil
}

func getSchemaWithoutStatus(crd *extv1.CustomResourceDefinition) (*spec.Schema, error) {
	crdCopy := crd.DeepCopy()

	if len(crdCopy.Spec.Versions) < 1 {
		return nil, fmt.Errorf("expected CRD to have at least one version")
	}
	if crdCopy.Spec.Versions[0].Schema == nil {
		return nil, fmt.Errorf("expected CRD version to have schema defined")
	}

	openAPISchema := crdCopy.Spec.Versions[0].Schema.OpenAPIV3Schema

	if openAPISchema.Properties == nil {
		openAPISchema.Properties = make(map[string]extv1.JSONSchemaProps)
	}

	delete(openAPISchema.Properties, "status")

	specSchema, err := kroschema.ConvertJSONSchemaPropsToSpecSchema(openAPISchema)
	if err != nil {
		return nil, err
	}

	if specSchema.Properties == nil {
		specSchema.Properties = make(map[string]spec.Schema)
	}
	specSchema.Properties["metadata"] = kroschema.ObjectMetaSchema
	return specSchema, nil
}
