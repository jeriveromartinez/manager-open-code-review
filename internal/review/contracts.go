package review

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

const (
	contractAdapterOpenAPI    = "openapi-json/v1"
	contractAdapterJSONSchema = "json-schema/v1"
	contractAdapterProto      = "proto-text/v1"
	contractAdapterGoTypes    = "go-ast/v1"
	contractAdapterNone       = "none"

	// maxContractFiles and maxContractBytes bound the local comparison so a
	// repository with generated contract trees cannot stall the run.
	maxContractFiles = 64
	maxContractBytes = 1 << 20
)

var contractHTTPMethods = map[string]bool{
	"get": true, "put": true, "post": true, "delete": true,
	"options": true, "head": true, "patch": true, "trace": true,
}

// contractModel is the normalized view of one contract file. Every adapter
// produces the same shape, so the compatibility rules are shared and the
// evidence is comparable across formats.
type contractModel struct {
	format     string
	adapter    string
	operations map[string]contractOperation
	schemas    map[string]contractSchema
}

type contractOperation struct {
	requiredParams map[string]bool
}

// contractSchema is recursive: a nested object, array element or combinator
// branch is a child schema, so a removal or a type change inside it is not
// silently reported as compatible.
type contractSchema struct {
	schemaType string
	properties map[string]contractSchema
	required   map[string]bool
}

type contractFinding struct {
	store.ArchitectureContractEvidence
	breaking bool
}

func newContractModel(format, adapter string) contractModel {
	return contractModel{
		format:     format,
		adapter:    adapter,
		operations: make(map[string]contractOperation),
		schemas:    make(map[string]contractSchema),
	}
}

func newContractSchema() contractSchema {
	return contractSchema{
		properties: make(map[string]contractSchema),
		required:   make(map[string]bool),
	}
}

// compareVersionedContracts reads the contract files declared in both trees
// and classifies every change. It never returns an error: a tree it cannot read
// is recorded as unknown evidence, so the review keeps its outcome.
func compareVersionedContracts(ctx context.Context, dir, baseRef, headRef string) store.ArchitectureContracts {
	result := store.ArchitectureContracts{Status: store.ContractsNotDeclared}

	basePaths, baseErr := contractTreePaths(ctx, dir, baseRef)
	headPaths, headErr := contractTreePaths(ctx, dir, headRef)
	if baseErr != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("no se pudo leer la base: %v", baseErr))
	}
	if headErr != nil {
		result.Errors = append(result.Errors, fmt.Sprintf("no se pudo leer la cabeza: %v", headErr))
	}
	if baseErr != nil && headErr != nil {
		result.Status = store.ContractUnknown
		return result
	}

	baseSet := make(map[string]bool, len(basePaths))
	for _, p := range basePaths {
		baseSet[p] = true
	}
	headSet := make(map[string]bool, len(headPaths))
	for _, p := range headPaths {
		headSet[p] = true
	}
	changed := map[string]bool{}
	changedKnown := true
	if strings.TrimSpace(baseRef) != "" && strings.TrimSpace(headRef) != "" {
		raw, err := git(ctx, dir, "diff --name-only", "diff", "--name-only", "-z", baseRef, headRef)
		if err != nil {
			changedKnown = false
			result.Errors = append(result.Errors, fmt.Sprintf("no se pudo listar los cambios: %v", err))
		} else {
			for _, p := range strings.Split(raw, "\x00") {
				if p = normalizeCoveragePath(p); p != "" {
					changed[p] = true
				}
			}
		}
	}
	union := make(map[string]bool, len(baseSet)+len(headSet))
	for p := range baseSet {
		union[p] = true
	}
	for p := range headSet {
		union[p] = true
	}
	paths := make([]string, 0, len(union))
	for p := range union {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	result.Declared = len(paths)

	seen := 0
	for _, path := range paths {
		// A contract declared in both trees and absent from the diff did not
		// change; skipping it avoids reading every contract in the repository.
		if changedKnown && baseSet[path] && headSet[path] && !changed[path] {
			continue
		}
		if seen >= maxContractFiles {
			result.Errors = append(result.Errors,
				fmt.Sprintf("comparación limitada a %d contratos; el resto queda sin evaluar", maxContractFiles))
			result.Contracts = append(result.Contracts, store.ArchitectureContract{
				Path: path, Format: store.ContractFormatUnknown, Adapter: contractAdapterNone,
				Status: store.ContractUnknown, Change: contractChange(baseSet[path], headSet[path]),
				Errors: []string{"límite de contratos alcanzado"},
			})
			break
		}
		baseContent, basePresent, err := readContractFile(ctx, dir, baseRef, path, baseSet[path])
		if err != nil {
			result.Errors = append(result.Errors, err.Error())
			result.Contracts = append(result.Contracts, unknownContract(path, baseSet[path], headSet[path], err))
			continue
		}
		headContent, headPresent, err := readContractFile(ctx, dir, headRef, path, headSet[path])
		if err != nil {
			result.Errors = append(result.Errors, err.Error())
			result.Contracts = append(result.Contracts, unknownContract(path, baseSet[path], headSet[path], err))
			continue
		}
		if basePresent && headPresent && bytes.Equal(baseContent, headContent) {
			continue
		}
		contract := compareContractContents(path, baseContent, headContent, basePresent, headPresent)
		// Only a valid JSON document that detection proved is not a contract is
		// dropped. Every other unclassified candidate stays unknown: dropping a
		// YAML or truncated file would let the aggregate claim compatibility
		// without having inspected it.
		if contract.Format == store.ContractFormatUnknown && !contractPathDeclared(path) &&
			strings.EqualFold(fileExtension(path), ".json") &&
			json.Valid(contractProbe(baseContent, headContent, headPresent)) {
			continue
		}
		seen++
		result.Contracts = append(result.Contracts, contract)
	}

	for _, contract := range result.Contracts {
		if contract.Status != store.ContractUnknown {
			result.Compared++
		}
	}
	result.Status = aggregateContractStatus(result.Declared, result.Contracts, result.Errors)
	return result
}

func contractProbe(base, head []byte, headPresent bool) []byte {
	if headPresent {
		return head
	}
	return base
}

// compareContractContents is the pure core: identical input always produces an
// identical verdict, including the digests and the evidence order.
func compareContractContents(path string, base, head []byte, basePresent, headPresent bool) store.ArchitectureContract {
	contract := store.ArchitectureContract{
		Path:   path,
		Change: contractChange(basePresent, headPresent),
	}
	probe := head
	if !headPresent {
		probe = base
	}
	format, adapter := detectContractFormat(path, probe)
	contract.Format = format
	contract.Adapter = adapter
	if basePresent {
		contract.BaseDigest = contractDigest(base)
	}
	if headPresent {
		contract.HeadDigest = contractDigest(head)
	}
	if format == store.ContractFormatUnknown {
		contract.Status = store.ContractUnknown
		contract.Evidence = append(contract.Evidence, store.ArchitectureContractEvidence{
			Kind:   "unsupported_format",
			Detail: fmt.Sprintf("no hay adaptador para %q; el contrato no se clasifica como compatible", baseName(path)),
		})
		return contract
	}

	baseModel := newContractModel(format, adapter)
	if basePresent {
		var err error
		baseModel, err = parseContract(format, adapter, base)
		if err != nil {
			contract.Status = store.ContractUnknown
			contract.Errors = append(contract.Errors, "base: "+err.Error())
			return contract
		}
	}
	headModel := newContractModel(format, adapter)
	if headPresent {
		var err error
		headModel, err = parseContract(format, adapter, head)
		if err != nil {
			contract.Status = store.ContractUnknown
			contract.Errors = append(contract.Errors, "head: "+err.Error())
			return contract
		}
	}

	findings := compareContractModels(baseModel, headModel)
	status := store.ContractCompatible
	for _, finding := range findings {
		contract.Evidence = append(contract.Evidence, finding.ArchitectureContractEvidence)
		if finding.breaking {
			status = store.ContractIncompatible
		}
	}
	contract.Status = status
	return contract
}

func unknownContract(path string, basePresent, headPresent bool, err error) store.ArchitectureContract {
	return store.ArchitectureContract{
		Path: path, Format: store.ContractFormatUnknown, Adapter: contractAdapterNone,
		Status: store.ContractUnknown, Change: contractChange(basePresent, headPresent),
		Errors: []string{err.Error()},
	}
}

func contractChange(basePresent, headPresent bool) string {
	switch {
	case !basePresent:
		return store.ContractChangeAdded
	case !headPresent:
		return store.ContractChangeRemoved
	default:
		return store.ContractChangeModified
	}
}

func contractDigest(content []byte) string {
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func aggregateContractStatus(declared int, contracts []store.ArchitectureContract, errs []string) string {
	if len(contracts) == 0 {
		if len(errs) > 0 {
			return store.ContractUnknown
		}
		if declared > 0 {
			return store.ContractCompatible
		}
		return store.ContractsNotDeclared
	}
	status := store.ContractCompatible
	if len(errs) > 0 {
		status = store.ContractUnknown
	}
	for _, contract := range contracts {
		if contract.Status == store.ContractIncompatible {
			return store.ContractIncompatible
		}
		if contract.Status == store.ContractUnknown {
			status = store.ContractUnknown
		}
	}
	return status
}

func contractTreePaths(ctx context.Context, dir, ref string) ([]string, error) {
	if strings.TrimSpace(ref) == "" {
		return nil, nil
	}
	raw, err := git(ctx, dir, "ls-tree", "ls-tree", "-r", "--name-only", ref)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0)
	for _, line := range strings.Split(raw, "\n") {
		path := normalizeCoveragePath(line)
		if path == "" || !contractPathCandidate(path) {
			continue
		}
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths, nil
}

// readContractFile checks the blob size before buffering it, so a huge
// generated contract is rejected by metadata instead of exhausting memory.
func readContractFile(ctx context.Context, dir, ref, path string, present bool) ([]byte, bool, error) {
	if !present || strings.TrimSpace(ref) == "" {
		return nil, false, nil
	}
	sizeRaw, err := git(ctx, dir, "cat-file -s", "cat-file", "-s", ref+":"+path)
	if err != nil {
		return nil, true, fmt.Errorf("no se pudo medir el contrato %s en %s: %w", path, ref, err)
	}
	size, err := strconv.ParseInt(strings.TrimSpace(sizeRaw), 10, 64)
	if err != nil {
		return nil, true, fmt.Errorf("tamaño inválido para el contrato %s en %s: %w", path, ref, err)
	}
	if size > maxContractBytes {
		return nil, true, fmt.Errorf("el contrato %s supera el tamaño máximo de %d bytes", path, maxContractBytes)
	}
	raw, err := git(ctx, dir, "show", "show", ref+":"+path)
	if err != nil {
		return nil, true, fmt.Errorf("no se pudo leer el contrato %s en %s: %w", path, ref, err)
	}
	return []byte(raw), true, nil
}

// contractPathCandidate is a cheap filter over tree paths; the content check in
// detectContractFormat decides whether a weak candidate is really a contract.
func contractPathCandidate(path string) bool {
	lower := strings.ToLower(path)
	parts := strings.Split(lower, "/")
	file := parts[len(parts)-1]
	switch fileExtension(file) {
	case ".proto", ".graphql", ".gql":
		return true
	case ".json", ".yaml", ".yml":
		if strings.Contains(file, "openapi") || strings.Contains(file, "swagger") || strings.Contains(file, "schema") {
			return true
		}
		for _, part := range parts[:len(parts)-1] {
			switch part {
			case "contract", "contracts", "openapi", "swagger", "schema", "schemas",
				"api", "apis", "sdk", "public", "interfaces", "dto", "dtos":
				return true
			}
		}
	case ".go":
		// Exported types are a contract only in an explicit public root; an
		// internal package, pkg helper or testdata fixture is not a published
		// contract.
		if len(parts) < 2 || parts[0] == "internal" || parts[0] == "vendor" || parts[0] == "testdata" {
			return false
		}
		switch parts[0] {
		case "api", "apis", "sdk", "public", "contract", "contracts", "interfaces", "dto", "dtos":
			return true
		}
	}
	return false
}

// contractPathDeclared distinguishes a path that names a contract from a path
// that merely lives in a contract-like directory.
func contractPathDeclared(path string) bool {
	lower := strings.ToLower(path)
	parts := strings.Split(lower, "/")
	file := parts[len(parts)-1]
	switch fileExtension(file) {
	case ".proto", ".graphql", ".gql", ".go":
		return true
	}
	if strings.Contains(file, "openapi") || strings.Contains(file, "swagger") || strings.Contains(file, "schema") {
		return true
	}
	for _, part := range parts[:len(parts)-1] {
		switch part {
		case "contract", "contracts", "openapi", "swagger", "schema", "schemas":
			return true
		}
	}
	return false
}

func detectContractFormat(path string, content []byte) (string, string) {
	switch fileExtension(strings.ToLower(path)) {
	case ".proto":
		return store.ContractFormatProto, contractAdapterProto
	case ".graphql", ".gql":
		return store.ContractFormatUnknown, contractAdapterNone
	case ".go":
		return store.ContractFormatGoTypes, contractAdapterGoTypes
	case ".yaml", ".yml":
		// A YAML OpenAPI is a declared contract, but this build has no YAML
		// adapter. Guessing compatibility from the extension would be wrong.
		return store.ContractFormatUnknown, contractAdapterNone
	case ".json":
		var doc map[string]any
		if err := json.Unmarshal(content, &doc); err != nil {
			return store.ContractFormatUnknown, contractAdapterNone
		}
		if _, ok := doc["openapi"]; ok {
			return store.ContractFormatOpenAPI, contractAdapterOpenAPI
		}
		if _, ok := doc["swagger"]; ok {
			return store.ContractFormatOpenAPI, contractAdapterOpenAPI
		}
		if _, ok := doc["$schema"]; ok {
			return store.ContractFormatJSONSchema, contractAdapterJSONSchema
		}
		for _, key := range []string{"properties", "definitions", "$defs"} {
			if _, ok := doc[key]; ok {
				return store.ContractFormatJSONSchema, contractAdapterJSONSchema
			}
		}
		return store.ContractFormatUnknown, contractAdapterNone
	default:
		return store.ContractFormatUnknown, contractAdapterNone
	}
}

func parseContract(format, adapter string, content []byte) (contractModel, error) {
	switch format {
	case store.ContractFormatOpenAPI:
		return parseOpenAPIContract(content)
	case store.ContractFormatJSONSchema:
		return parseJSONSchemaContract(content)
	case store.ContractFormatProto:
		return parseProtoContract(content)
	case store.ContractFormatGoTypes:
		return parseGoTypesContract(content)
	default:
		return newContractModel(store.ContractFormatUnknown, adapter), fmt.Errorf("formato no soportado %q", format)
	}
}

func compareContractModels(base, head contractModel) []contractFinding {
	var findings []contractFinding
	add := func(breaking bool, kind, pointer, detail string) {
		findings = append(findings, contractFinding{
			ArchitectureContractEvidence: store.ArchitectureContractEvidence{
				Kind: kind, Pointer: pointer, Detail: detail,
			},
			breaking: breaking,
		})
	}

	for _, key := range sortedOperationKeys(base.operations) {
		if _, ok := head.operations[key]; !ok {
			add(true, "operation_removed", contractOperationPointer(base.format, key),
				fmt.Sprintf("operación %s eliminada", key))
		}
	}
	for _, key := range sortedOperationKeys(head.operations) {
		if _, ok := base.operations[key]; !ok {
			add(false, "operation_added", contractOperationPointer(head.format, key),
				fmt.Sprintf("operación %s añadida", key))
		}
	}
	for _, key := range sortedOperationKeys(base.operations) {
		headOp, ok := head.operations[key]
		if !ok {
			continue
		}
		baseOp := base.operations[key]
		for _, param := range sortedBoolKeys(headOp.requiredParams) {
			if !baseOp.requiredParams[param] {
				add(true, "required_parameter_added",
					contractOperationPointer(base.format, key)+".parameters."+param,
					fmt.Sprintf("la operación %s exige el parámetro %s", key, param))
			}
		}
	}

	for _, name := range sortedSchemaKeys(base.schemas) {
		baseSchema := base.schemas[name]
		headSchema, ok := head.schemas[name]
		if !ok {
			add(true, "schema_removed", contractSchemaPointer(base.format, name),
				fmt.Sprintf("esquema %s eliminado", name))
			continue
		}
		compareContractSchema(base.format, name, baseSchema, headSchema, nil, add)
	}
	for _, name := range sortedSchemaKeys(head.schemas) {
		if _, ok := base.schemas[name]; !ok {
			add(false, "schema_added", contractSchemaPointer(head.format, name),
				fmt.Sprintf("esquema %s añadido", name))
		}
	}

	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].Pointer != findings[j].Pointer {
			return findings[i].Pointer < findings[j].Pointer
		}
		if findings[i].Kind != findings[j].Kind {
			return findings[i].Kind < findings[j].Kind
		}
		return findings[i].Detail < findings[j].Detail
	})
	return findings
}

// compareContractSchema walks one schema subtree. The pointer keeps the exact
// nested location of every removal, required addition and type change.
func compareContractSchema(format, name string, base, head contractSchema, path []string, add func(bool, string, string, string)) {
	if base.schemaType != head.schemaType {
		add(true, "type_changed", contractSchemaPointer(format, name, path...),
			fmt.Sprintf("%s pasó de %s a %s", contractLocation(path), base.schemaType, head.schemaType))
	}
	for _, prop := range sortedSchemaKeys(base.properties) {
		baseProp := base.properties[prop]
		headProp, ok := head.properties[prop]
		if !ok {
			// Removing an allOf branch relaxes the schema; removing a property
			// or a oneOf/anyOf branch narrows what consumers can rely on.
			if strings.HasPrefix(prop, "$allOf[") {
				add(false, "constraint_removed", contractSchemaPointer(format, name, appendPath(path, prop)...),
					fmt.Sprintf("restricción %s eliminada (%s)", prop, contractLocation(path)))
			} else {
				add(true, "property_removed", contractSchemaPointer(format, name, appendPath(path, prop)...),
					fmt.Sprintf("campo %s eliminado (%s)", prop, contractLocation(path)))
			}
			continue
		}
		compareContractSchema(format, name, baseProp, headProp, appendPath(path, prop), add)
	}
	for _, prop := range sortedSchemaKeys(head.properties) {
		if _, ok := base.properties[prop]; ok {
			continue
		}
		switch {
		case strings.HasPrefix(prop, "$allOf["):
			add(true, "constraint_added", contractSchemaPointer(format, name, appendPath(path, prop)...),
				fmt.Sprintf("restricción %s añadida (%s)", prop, contractLocation(path)))
		case head.required[prop]:
			add(true, "required_field_added", contractSchemaPointer(format, name, appendPath(path, prop)...),
				fmt.Sprintf("campo obligatorio %s añadido (%s)", prop, contractLocation(path)))
		default:
			add(false, "field_added", contractSchemaPointer(format, name, appendPath(path, prop)...),
				fmt.Sprintf("campo opcional %s añadido (%s)", prop, contractLocation(path)))
		}
	}
	for _, prop := range sortedBoolKeys(head.required) {
		if base.required[prop] {
			continue
		}
		if _, existed := base.properties[prop]; existed {
			add(true, "required_field_added", contractSchemaPointer(format, name, appendPath(path, prop)...),
				fmt.Sprintf("campo %s ahora es obligatorio (%s)", prop, contractLocation(path)))
		}
	}
	for _, prop := range sortedBoolKeys(base.required) {
		if head.required[prop] {
			continue
		}
		if _, stillPresent := head.properties[prop]; !stillPresent {
			continue
		}
		add(false, "required_field_relaxed", contractSchemaPointer(format, name, appendPath(path, prop)...),
			fmt.Sprintf("campo %s ya no es obligatorio (%s)", prop, contractLocation(path)))
	}
}

func appendPath(path []string, element string) []string {
	if len(path) == 0 {
		return []string{element}
	}
	next := make([]string, 0, len(path)+1)
	next = append(next, path...)
	return append(next, element)
}

func contractLocation(path []string) string {
	if len(path) == 0 {
		return "el esquema"
	}
	return "el campo " + strings.Join(path, ".")
}

func contractOperationPointer(format, key string) string {
	switch format {
	case store.ContractFormatProto:
		return "rpcs." + key
	default:
		return "paths." + key
	}
}

func contractSchemaPointer(format, name string, path ...string) string {
	prefix := "schemas"
	switch format {
	case store.ContractFormatOpenAPI:
		prefix = "components.schemas"
	case store.ContractFormatProto:
		prefix = "messages"
	case store.ContractFormatGoTypes:
		prefix = "types"
	}
	pointer := prefix + "." + name
	for _, element := range path {
		pointer += ".properties." + element
	}
	return pointer
}

func parseOpenAPIContract(content []byte) (contractModel, error) {
	var doc map[string]any
	if err := json.Unmarshal(content, &doc); err != nil {
		return contractModel{}, err
	}
	model := newContractModel(store.ContractFormatOpenAPI, contractAdapterOpenAPI)
	if paths, ok := doc["paths"].(map[string]any); ok {
		for path, itemAny := range paths {
			item, ok := itemAny.(map[string]any)
			if !ok {
				continue
			}
			// Path-item parameters are shared by every operation in the item.
			shared := openAPIParameters(item["parameters"])
			for method, opAny := range item {
				method = strings.ToLower(method)
				if !contractHTTPMethods[method] {
					continue
				}
				operation := contractOperation{requiredParams: make(map[string]bool)}
				for _, param := range shared {
					operation.requiredParams[param] = true
				}
				if op, ok := opAny.(map[string]any); ok {
					for _, param := range openAPIParameters(op["parameters"]) {
						operation.requiredParams[param] = true
					}
				}
				model.operations[strings.ToUpper(method)+" "+path] = operation
			}
		}
	}
	if components, ok := doc["components"].(map[string]any); ok {
		if schemas, ok := components["schemas"].(map[string]any); ok {
			for name, schemaAny := range schemas {
				if schema, ok := schemaAny.(map[string]any); ok {
					model.schemas[name] = parseSchemaObject(schema)
				}
			}
		}
	}
	// Swagger 2.0 declares its schemas at the top level instead of components.
	if definitions, ok := doc["definitions"].(map[string]any); ok {
		for name, schemaAny := range definitions {
			if schema, ok := schemaAny.(map[string]any); ok {
				model.schemas[name] = parseSchemaObject(schema)
			}
		}
	}
	return model, nil
}

func openAPIParameters(raw any) []string {
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	var required []string
	for _, itemAny := range items {
		item, ok := itemAny.(map[string]any)
		if !ok {
			continue
		}
		isRequired, _ := item["required"].(bool)
		if !isRequired {
			continue
		}
		name, _ := item["name"].(string)
		in, _ := item["in"].(string)
		if name == "" {
			continue
		}
		if in == "" {
			in = "query"
		}
		required = append(required, in+"."+name)
	}
	sort.Strings(required)
	return required
}

func parseJSONSchemaContract(content []byte) (contractModel, error) {
	var doc map[string]any
	if err := json.Unmarshal(content, &doc); err != nil {
		return contractModel{}, err
	}
	model := newContractModel(store.ContractFormatJSONSchema, contractAdapterJSONSchema)
	model.schemas["$root"] = parseSchemaObject(doc)
	for _, key := range []string{"definitions", "$defs"} {
		defs, ok := doc[key].(map[string]any)
		if !ok {
			continue
		}
		for name, schemaAny := range defs {
			if schema, ok := schemaAny.(map[string]any); ok {
				model.schemas[name] = parseSchemaObject(schema)
			}
		}
	}
	return model, nil
}

func parseSchemaObject(node map[string]any) contractSchema {
	schema := newContractSchema()
	schema.schemaType = schemaNodeType(node)
	if props, ok := node["properties"].(map[string]any); ok {
		for name, propAny := range props {
			if prop, ok := propAny.(map[string]any); ok {
				schema.properties[name] = parseSchemaObject(prop)
			} else {
				schema.properties[name] = contractSchema{schemaType: "any", properties: map[string]contractSchema{}}
			}
		}
	}
	if required, ok := node["required"].([]any); ok {
		for _, item := range required {
			if name, ok := item.(string); ok {
				schema.required[name] = true
			}
		}
	}
	if items, ok := node["items"].(map[string]any); ok {
		schema.properties["$items"] = parseSchemaObject(items)
	}
	for _, combinator := range []string{"allOf", "oneOf", "anyOf"} {
		branches, ok := node[combinator].([]any)
		if !ok {
			continue
		}
		for i, branchAny := range branches {
			if branch, ok := branchAny.(map[string]any); ok {
				schema.properties[fmt.Sprintf("$%s[%d]", combinator, i)] = parseSchemaObject(branch)
			}
		}
	}
	return schema
}

func schemaNodeType(node map[string]any) string {
	if ref, ok := node["$ref"].(string); ok {
		return "ref:" + ref
	}
	if kind, ok := node["type"].(string); ok {
		return kind
	}
	if _, ok := node["properties"]; ok {
		return "object"
	}
	if _, ok := node["items"]; ok {
		return "array"
	}
	for _, combinator := range []string{"oneOf", "anyOf", "allOf"} {
		if _, ok := node[combinator]; ok {
			return combinator
		}
	}
	return "any"
}

var (
	protoMessagePattern = regexp.MustCompile(`(?m)^\s*message\s+([A-Za-z_]\w*)\s*\{`)
	protoServicePattern = regexp.MustCompile(`(?m)^\s*service\s+([A-Za-z_]\w*)\s*\{`)
	protoRPCPattern     = regexp.MustCompile(`(?m)\brpc\s+([A-Za-z_]\w*)\s*\(`)
	protoFieldPattern   = regexp.MustCompile(`(?m)^\s*(?:(repeated|optional|required)\s+)?(map\s*<[^>]+>|[.\w]+)\s+([A-Za-z_]\w*)\s*=\s*\d+`)
)

func parseProtoContract(content []byte) (contractModel, error) {
	source := string(content)
	model := newContractModel(store.ContractFormatProto, contractAdapterProto)
	for _, match := range protoMessagePattern.FindAllStringSubmatchIndex(source, -1) {
		name := source[match[2]:match[3]]
		body := braceBody(source, match[1]-1)
		if body == "" {
			continue
		}
		schema := newContractSchema()
		schema.schemaType = "message"
		for _, field := range protoFieldPattern.FindAllStringSubmatch(stripProtoNested(body), -1) {
			label, kind, fieldName := field[1], field[2], field[3]
			schema.properties[fieldName] = contractSchema{schemaType: kind, properties: map[string]contractSchema{}}
			if label == "required" {
				schema.required[fieldName] = true
			}
		}
		model.schemas[name] = schema
	}
	for _, match := range protoServicePattern.FindAllStringSubmatchIndex(source, -1) {
		service := source[match[2]:match[3]]
		body := braceBody(source, match[1]-1)
		for _, rpc := range protoRPCPattern.FindAllStringSubmatch(body, -1) {
			model.operations["rpc "+service+"."+rpc[1]] = contractOperation{requiredParams: make(map[string]bool)}
		}
	}
	return model, nil
}

func stripProtoNested(body string) string {
	for _, pattern := range []*regexp.Regexp{protoMessagePattern, regexp.MustCompile(`(?m)^\s*enum\s+([A-Za-z_]\w*)\s*\{`)} {
		for _, match := range pattern.FindAllStringSubmatchIndex(body, -1) {
			start := match[1] - 1
			end := braceEnd(body, start)
			if end <= start {
				continue
			}
			body = body[:start] + strings.Repeat(" ", end-start) + body[end:]
		}
	}
	return body
}

func braceBody(source string, open int) string {
	end := braceEnd(source, open)
	if end <= open {
		return ""
	}
	return source[open+1 : end]
}

func braceEnd(source string, open int) int {
	if open < 0 || open >= len(source) || source[open] != '{' {
		return -1
	}
	depth := 0
	for i := open; i < len(source); i++ {
		switch source[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func parseGoTypesContract(content []byte) (contractModel, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "contract.go", content, parser.SkipObjectResolution)
	if err != nil {
		return contractModel{}, err
	}
	model := newContractModel(store.ContractFormatGoTypes, contractAdapterGoTypes)
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			typeSpec, ok := spec.(*ast.TypeSpec)
			if !ok || !ast.IsExported(typeSpec.Name.Name) {
				continue
			}
			structType, ok := typeSpec.Type.(*ast.StructType)
			if !ok {
				continue
			}
			model.schemas[typeSpec.Name.Name] = parseGoStruct(fset, structType)
		}
	}
	return model, nil
}

func parseGoStruct(fset *token.FileSet, structType *ast.StructType) contractSchema {
	schema := newContractSchema()
	schema.schemaType = "struct"
	for _, field := range structType.Fields.List {
		fieldSchema := parseGoExpr(fset, field.Type)
		if len(field.Names) == 0 {
			if embedded := lastTypeSegment(goTypeName(fset, field.Type)); ast.IsExported(embedded) {
				schema.properties[embedded] = fieldSchema
			}
			continue
		}
		for _, name := range field.Names {
			if ast.IsExported(name.Name) {
				schema.properties[name.Name] = fieldSchema
			}
		}
	}
	return schema
}

func parseGoExpr(fset *token.FileSet, expr ast.Expr) contractSchema {
	schema := contractSchema{schemaType: goTypeName(fset, expr), properties: map[string]contractSchema{}}
	switch typed := expr.(type) {
	case *ast.StructType:
		return parseGoStruct(fset, typed)
	case *ast.ArrayType:
		element := parseGoExpr(fset, typed.Elt)
		element.schemaType = "[]" + element.schemaType
		return element
	case *ast.StarExpr:
		element := parseGoExpr(fset, typed.X)
		element.schemaType = "*" + element.schemaType
		return element
	}
	return schema
}

func goTypeName(fset *token.FileSet, expr ast.Expr) string {
	var buffer bytes.Buffer
	if err := printer.Fprint(&buffer, fset, expr); err != nil {
		return "any"
	}
	return buffer.String()
}

func lastTypeSegment(value string) string {
	value = strings.TrimPrefix(value, "*")
	if i := strings.LastIndex(value, "."); i >= 0 {
		return value[i+1:]
	}
	return value
}

func sortedSchemaKeys(values map[string]contractSchema) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedOperationKeys(values map[string]contractOperation) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedBoolKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// contractComparisonChanged reports whether the diff touches a declared
// contract, which is an explicit public-interface signal for the architecture
// analysis.
func contractComparisonChanged(contracts store.ArchitectureContracts) bool {
	return len(contracts.Contracts) > 0
}

func appendContractSignal(signals []string) []string {
	for _, signal := range signals {
		if signal == architectureSignalInterface {
			return signals
		}
	}
	return append(signals, architectureSignalInterface)
}

const contractDimensionName = "Compatibilidad de contratos"

// attachContractComparison integrates the local result into the verdict as a
// dimension, evidence and controls. It is idempotent and never changes the
// verdict status or the review outcome.
func attachContractComparison(verdict store.ArchitectureVerdict, contracts store.ArchitectureContracts) store.ArchitectureVerdict {
	verdict.Contracts = contracts
	if contracts.Declared == 0 && len(contracts.Errors) == 0 {
		return verdict
	}
	verdict.Controls.ContractsReadOnly = true

	dimension := store.ArchitectureDimension{
		Name:    contractDimensionName,
		Content: contractDimensionContent(contracts),
	}
	dimensions := []store.ArchitectureDimension{dimension}
	for _, existing := range verdict.Dimensions {
		if existing.Name != contractDimensionName {
			dimensions = append(dimensions, existing)
		}
	}
	verdict.Dimensions = dimensions

	for _, contract := range contracts.Contracts {
		switch contract.Status {
		case store.ContractIncompatible:
			verdict.Risks = appendUniqueString(verdict.Risks, contractRiskLine(contract))
		case store.ContractUnknown:
			verdict.Uncertainties = appendUniqueString(verdict.Uncertainties,
				fmt.Sprintf("Contrato `%s` sin clasificar (%s): %s", contract.Path, contract.Format,
					strings.Join(contract.Errors, "; ")))
		}
	}
	for _, err := range contracts.Errors {
		verdict.Uncertainties = appendUniqueString(verdict.Uncertainties, "Contratos: "+err)
	}
	return verdict
}

func contractDimensionContent(contracts store.ArchitectureContracts) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Estado: %s · %d declarado(s), %d comparado(s)", contracts.Status,
		contracts.Declared, contracts.Compared)
	for _, contract := range contracts.Contracts {
		fmt.Fprintf(&b, "\n- `%s` — %s (%s, %s, %s)", contract.Path, contract.Status,
			contract.Format, contract.Adapter, contract.Change)
		for _, evidence := range contract.Evidence {
			pointer := evidence.Pointer
			if pointer == "" {
				pointer = "-"
			}
			fmt.Fprintf(&b, "\n  - %s `%s`: %s", evidence.Kind, pointer, evidence.Detail)
		}
		for _, err := range contract.Errors {
			fmt.Fprintf(&b, "\n  - error: %s", err)
		}
	}
	for _, err := range contracts.Errors {
		fmt.Fprintf(&b, "\n- error: %s", err)
	}
	if contracts.Status == store.ContractIncompatible {
		b.WriteString("\n- La rotura es objetiva; una versión nueva o una transición intencional queda como decisión del mantenedor.")
	}
	return b.String()
}

func contractRiskLine(contract store.ArchitectureContract) string {
	var breaks []string
	for _, evidence := range contract.Evidence {
		if evidence.Kind == "unsupported_format" {
			continue
		}
		breaks = append(breaks, evidence.Kind+": "+evidence.Detail)
	}
	detail := strings.Join(breaks, "; ")
	if detail == "" {
		detail = "cambio incompatible"
	}
	return fmt.Sprintf("Contrato `%s` (%s, %s): incompatible — %s",
		contract.Path, contract.Format, contract.Adapter, detail)
}

func appendUniqueString(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
