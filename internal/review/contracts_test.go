package review

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jeriveromartinez/qa-control-center/internal/store"
)

func contractFixture(t *testing.T, dir, side, file string) []byte {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("testdata", "contracts", dir, side, file))
	if err != nil {
		t.Fatalf("read fixture %s/%s/%s: %v", dir, side, file, err)
	}
	return content
}

func contractHasEvidence(contract store.ArchitectureContract, kind, pointer string) bool {
	for _, evidence := range contract.Evidence {
		if evidence.Kind == kind && evidence.Pointer == pointer {
			return true
		}
	}
	return false
}

func TestContractCompatibilityFixtures(t *testing.T) {
	cases := []struct {
		name     string
		dir      string
		file     string
		want     string
		evidence []store.ArchitectureContractEvidence
	}{
		{
			name: "breaking removal", dir: "breaking-removal", file: "openapi.json",
			want: store.ContractIncompatible,
			evidence: []store.ArchitectureContractEvidence{
				{Kind: "operation_removed", Pointer: "paths.GET /pets"},
			},
		},
		{
			name: "new required field", dir: "required-field", file: "openapi.json",
			want: store.ContractIncompatible,
			evidence: []store.ArchitectureContractEvidence{
				{Kind: "required_field_added", Pointer: "components.schemas.Pet.properties.status"},
			},
		},
		{
			name: "type change", dir: "type-change", file: "openapi.json",
			want: store.ContractIncompatible,
			evidence: []store.ArchitectureContractEvidence{
				{Kind: "type_changed", Pointer: "components.schemas.Pet.properties.id"},
			},
		},
		{
			name: "compatible addition", dir: "compatible-addition", file: "openapi.json",
			want: store.ContractCompatible,
			evidence: []store.ArchitectureContractEvidence{
				{Kind: "field_added", Pointer: "components.schemas.Pet.properties.tag"},
				{Kind: "operation_added", Pointer: "paths.GET /health"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := contractFixture(t, tc.dir, "base", tc.file)
			head := contractFixture(t, tc.dir, "head", tc.file)
			contract := compareContractContents("api/"+tc.file, base, head, true, true)
			if contract.Format != store.ContractFormatOpenAPI || contract.Adapter != contractAdapterOpenAPI {
				t.Fatalf("format/adapter = %q/%q", contract.Format, contract.Adapter)
			}
			if contract.Status != tc.want {
				t.Fatalf("status = %q, want %q (evidence: %+v)", contract.Status, tc.want, contract.Evidence)
			}
			if contract.BaseDigest == "" || contract.HeadDigest == "" || contract.BaseDigest == contract.HeadDigest {
				t.Errorf("digests = %q/%q, want two different content digests", contract.BaseDigest, contract.HeadDigest)
			}
			for _, want := range tc.evidence {
				if !contractHasEvidence(contract, want.Kind, want.Pointer) {
					t.Errorf("evidence %+v missing %s %s", contract.Evidence, want.Kind, want.Pointer)
				}
			}
		})
	}
}

func TestContractGraphQLFormatIsNeverCompatible(t *testing.T) {
	base := contractFixture(t, "unknown-format", "base", "schema.graphql")
	head := contractFixture(t, "unknown-format", "head", "schema.graphql")
	contract := compareContractContents("api/schema.graphql", base, head, true, true)
	if contract.Status != store.ContractUnknown {
		t.Fatalf("status = %q, want unknown", contract.Status)
	}
	if contract.Status == store.ContractCompatible {
		t.Fatal("an unsupported format was reported as compatible")
	}
	if contract.Format != store.ContractFormatUnknown || contract.Adapter != contractAdapterNone {
		t.Errorf("format/adapter = %q/%q, want unknown/none", contract.Format, contract.Adapter)
	}
	if !contractHasEvidence(contract, "unsupported_format", "") {
		t.Errorf("evidence = %+v, want unsupported_format", contract.Evidence)
	}

	contracts := store.ArchitectureContracts{
		Status: store.ContractUnknown, Declared: 1, Contracts: []store.ArchitectureContract{contract},
	}
	verdict := attachContractComparison(
		store.ArchitectureVerdict{Status: store.ArchitectureComplete}, contracts)
	if len(verdict.Risks) != 0 {
		t.Errorf("risks = %v, an unknown contract is not a proven break", verdict.Risks)
	}
	if !containsSubstring(verdict.Uncertainties, "sin clasificar") {
		t.Errorf("uncertainties = %v, want the unsupported format recorded", verdict.Uncertainties)
	}
	if got := verdict.Dimensions[0].Name; got != contractDimensionName {
		t.Errorf("dimension = %q, want %q", got, contractDimensionName)
	}
	if verdict.Contracts.Status != store.ContractUnknown || len(verdict.Contracts.Contracts) != 1 {
		t.Errorf("verdict contracts = %+v, want the comparison preserved", verdict.Contracts)
	}
	if content := verdict.Dimensions[0].Content; !containsSubstring([]string{content}, "api/schema.graphql") ||
		!containsSubstring([]string{content}, "(unknown, none, modified)") {
		t.Errorf("contract dimension lost the path or adapter reference: %q", content)
	}
}

func TestContractEvidenceIsReproducible(t *testing.T) {
	base := contractFixture(t, "breaking-removal", "base", "openapi.json")
	head := contractFixture(t, "breaking-removal", "head", "openapi.json")

	first := compareContractContents("api/openapi.json", base, head, true, true)
	second := compareContractContents("api/openapi.json", base, head, true, true)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("same input produced different verdicts:\nfirst: %+v\nsecond: %+v", first, second)
	}
	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("verdict is not byte-stable:\n%s\n%s", firstJSON, secondJSON)
	}
	for i := 1; i < len(first.Evidence); i++ {
		if first.Evidence[i-1].Pointer > first.Evidence[i].Pointer {
			t.Errorf("evidence is not sorted: %+v", first.Evidence)
		}
	}
}

func TestContractProtoAndGoAdapters(t *testing.T) {
	cases := []struct {
		name       string
		path       string
		base, head string
		format     string
		want       string
		evidence   store.ArchitectureContractEvidence
	}{
		{
			name: "proto type change and rpc removal",
			path: "api/pet.proto", format: store.ContractFormatProto,
			base: `syntax = "proto3";
message Pet { string id = 1; }
service Pets { rpc Get (Pet) returns (Pet); }
`,
			head: `syntax = "proto3";
message Pet { int32 id = 1; }
`,
			want:     store.ContractIncompatible,
			evidence: store.ArchitectureContractEvidence{Kind: "type_changed", Pointer: "messages.Pet.properties.id"},
		},
		{
			name: "go exported type removal",
			path: "api/pet.go", format: store.ContractFormatGoTypes,
			base:     "package api\n\ntype Pet struct {\n\tID   int\n\tName string\n}\n",
			head:     "package api\n\ntype Pet struct {\n\tID int\n}\n",
			want:     store.ContractIncompatible,
			evidence: store.ArchitectureContractEvidence{Kind: "property_removed", Pointer: "types.Pet.properties.Name"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			contract := compareContractContents(tc.path, []byte(tc.base), []byte(tc.head), true, true)
			if contract.Format != tc.format {
				t.Fatalf("format = %q, want %q", contract.Format, tc.format)
			}
			if contract.Status != tc.want {
				t.Fatalf("status = %q, want %q (evidence: %+v)", contract.Status, tc.want, contract.Evidence)
			}
			if !contractHasEvidence(contract, tc.evidence.Kind, tc.evidence.Pointer) {
				t.Errorf("evidence %+v missing %s %s", contract.Evidence, tc.evidence.Kind, tc.evidence.Pointer)
			}
		})
	}
}

func TestContractYAMLAdapters(t *testing.T) {
	cases := []struct {
		name     string
		dir      string
		file     string
		evidence []store.ArchitectureContractEvidence
	}{
		{
			name: "openapi 3", dir: "yaml-openapi3", file: "openapi.yaml",
			evidence: []store.ArchitectureContractEvidence{
				{Kind: "operation_removed", Pointer: "paths.GET /pets/{petId}"},
				{Kind: "type_changed", Pointer: "components.schemas.Pet.properties.id"},
				{Kind: "required_field_added", Pointer: "components.schemas.Pet.properties.status"},
				{Kind: "field_added", Pointer: "components.schemas.Pet.properties.tag"},
			},
		},
		{
			name: "swagger 2.0", dir: "yaml-swagger2", file: "swagger.yaml",
			evidence: []store.ArchitectureContractEvidence{
				{Kind: "required_parameter_added", Pointer: "paths.GET /orders.parameters.query.status"},
				{Kind: "type_changed", Pointer: "components.schemas.Order.properties.id"},
				{Kind: "required_field_added", Pointer: "components.schemas.Order.properties.currency"},
				{Kind: "schema_removed", Pointer: "components.schemas.Customer"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := contractFixture(t, tc.dir, "base", tc.file)
			head := contractFixture(t, tc.dir, "head", tc.file)
			contract := compareContractContents("api/"+tc.file, base, head, true, true)
			if contract.Format != store.ContractFormatOpenAPI || contract.Adapter != contractAdapterOpenAPIYAML {
				t.Fatalf("format/adapter = %q/%q", contract.Format, contract.Adapter)
			}
			if contract.Status != store.ContractIncompatible {
				t.Fatalf("status = %q, want incompatible (evidence: %+v)", contract.Status, contract.Evidence)
			}
			if contract.BaseDigest == "" || contract.HeadDigest == "" || contract.BaseDigest == contract.HeadDigest {
				t.Errorf("digests = %q/%q, want two different content digests", contract.BaseDigest, contract.HeadDigest)
			}
			for _, want := range tc.evidence {
				if !contractHasEvidence(contract, want.Kind, want.Pointer) {
					t.Errorf("evidence %+v missing %s %s", contract.Evidence, want.Kind, want.Pointer)
				}
			}
		})
	}
}

func TestContractYAMLAnchorsAndAliases(t *testing.T) {
	base := contractFixture(t, "yaml-anchors", "base", "openapi.yaml")
	head := contractFixture(t, "yaml-anchors", "head", "openapi.yaml")
	contract := compareContractContents("api/openapi.yaml", base, head, true, true)
	if contract.Format != store.ContractFormatOpenAPI || contract.Adapter != contractAdapterOpenAPIYAML {
		t.Fatalf("format/adapter = %q/%q", contract.Format, contract.Adapter)
	}
	if contract.Status != store.ContractIncompatible {
		t.Fatalf("status = %q, want incompatible (evidence: %+v)", contract.Status, contract.Evidence)
	}
	// The alias resolves to the anchored mapping, so both schemas carry the change.
	for _, name := range []string{"Item", "ItemAlias"} {
		pointer := "components.schemas." + name + ".properties.id"
		if !contractHasEvidence(contract, "type_changed", pointer) {
			t.Errorf("evidence %+v missing %s", contract.Evidence, pointer)
		}
	}
}

func TestContractYAMLMatchesJSONClassification(t *testing.T) {
	jsonBase := contractFixture(t, "unquoted-status-code", "base", "openapi.json")
	jsonHead := contractFixture(t, "unquoted-status-code", "head", "openapi.json")
	yamlBase := contractFixture(t, "unquoted-status-code", "base", "openapi.yaml")
	yamlHead := contractFixture(t, "unquoted-status-code", "head", "openapi.yaml")

	fromJSON := compareContractContents("api/openapi.json", jsonBase, jsonHead, true, true)
	fromYAML := compareContractContents("api/openapi.yaml", yamlBase, yamlHead, true, true)

	if fromJSON.Status != store.ContractIncompatible || fromYAML.Status != fromJSON.Status {
		t.Fatalf("statuses = json %q, yaml %q; want both incompatible", fromJSON.Status, fromYAML.Status)
	}
	if fromYAML.Format != fromJSON.Format {
		t.Errorf("formats = json %q, yaml %q", fromJSON.Format, fromYAML.Format)
	}
	if fromJSON.Adapter != contractAdapterOpenAPI || fromYAML.Adapter != contractAdapterOpenAPIYAML {
		t.Errorf("adapters = json %q, yaml %q", fromJSON.Adapter, fromYAML.Adapter)
	}
	if !reflect.DeepEqual(fromYAML.Evidence, fromJSON.Evidence) {
		t.Fatalf("evidence differs between JSON and YAML:\njson: %+v\nyaml: %+v", fromJSON.Evidence, fromYAML.Evidence)
	}
	for _, want := range []store.ArchitectureContractEvidence{
		{Kind: "operation_removed", Pointer: "paths.GET /pets/{petId}"},
		{Kind: "type_changed", Pointer: "components.schemas.Pet.properties.id"},
		{Kind: "required_field_added", Pointer: "components.schemas.Pet.properties.status"},
		{Kind: "field_added", Pointer: "components.schemas.Pet.properties.tag"},
	} {
		if !contractHasEvidence(fromYAML, want.Kind, want.Pointer) {
			t.Errorf("yaml evidence %+v missing %s %s", fromYAML.Evidence, want.Kind, want.Pointer)
		}
	}
}

func TestContractYAMLMultiDocumentRules(t *testing.T) {
	base := []byte("openapi: 3.0.3\npaths: {}\n")
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "empty document before a real one",
			content: "---\nnull\n---\nopenapi: 3.0.3\npaths: {}\n",
			want:    store.ContractUnknown,
		},
		{
			name:    "one trailing empty document",
			content: "openapi: 3.0.3\npaths: {}\n---\n",
			want:    store.ContractCompatible,
		},
		{
			name:    "two trailing empty documents",
			content: "openapi: 3.0.3\npaths: {}\n---\n---\n",
			want:    store.ContractUnknown,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			contract := compareContractContents("api/openapi.yaml", base, []byte(tc.content), true, true)
			if contract.Status != tc.want {
				t.Fatalf("status = %q, want %q (evidence: %+v)", contract.Status, tc.want, contract.Evidence)
			}
		})
	}
}

func TestContractYAMLUnsupportedShapesStayUnknown(t *testing.T) {
	cases := []struct {
		name string
		dir  string
	}{
		{name: "malformed", dir: "yaml-malformed"},
		{name: "multi document", dir: "yaml-multi-document"},
		{name: "non object root", dir: "yaml-nonobject"},
		{name: "collection key", dir: "yaml-collection-key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := contractFixture(t, tc.dir, "base", "openapi.yaml")
			head := contractFixture(t, tc.dir, "head", "openapi.yaml")
			contract := compareContractContents("api/openapi.yaml", base, head, true, true)
			if contract.Status != store.ContractUnknown {
				t.Fatalf("status = %q, want unknown (evidence: %+v)", contract.Status, contract.Evidence)
			}
			if contract.Format != store.ContractFormatUnknown || contract.Adapter != contractAdapterNone {
				t.Errorf("format/adapter = %q/%q, want unknown/none", contract.Format, contract.Adapter)
			}
			if !contractHasEvidence(contract, "unsupported_format", "") {
				t.Errorf("evidence = %+v, want unsupported_format", contract.Evidence)
			}
		})
	}
}

func TestContractJSONSchemaAdapter(t *testing.T) {
	base := `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object",
		"required":["id"],"properties":{"id":{"type":"integer"}}}`
	head := `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object",
		"required":["id","name"],"properties":{"id":{"type":"string"},"name":{"type":"string"}}}`

	contract := compareContractContents("contracts/pet.schema.json", []byte(base), []byte(head), true, true)
	if contract.Format != store.ContractFormatJSONSchema || contract.Adapter != contractAdapterJSONSchema {
		t.Fatalf("format/adapter = %q/%q", contract.Format, contract.Adapter)
	}
	if contract.Status != store.ContractIncompatible {
		t.Fatalf("status = %q, want incompatible (evidence: %+v)", contract.Status, contract.Evidence)
	}
	for _, want := range []store.ArchitectureContractEvidence{
		{Kind: "type_changed", Pointer: "schemas.$root.properties.id"},
		{Kind: "required_field_added", Pointer: "schemas.$root.properties.name"},
	} {
		if !contractHasEvidence(contract, want.Kind, want.Pointer) {
			t.Errorf("evidence %+v missing %s %s", contract.Evidence, want.Kind, want.Pointer)
		}
	}
}

func TestContractYAMLJSONSchemaAdapter(t *testing.T) {
	base := `$schema: https://json-schema.org/draft/2020-12/schema
type: object
required:
  - id
properties:
  id:
    type: integer
`
	head := `$schema: https://json-schema.org/draft/2020-12/schema
type: object
required:
  - id
  - name
properties:
  id:
    type: string
  name:
    type: string
`
	contract := compareContractContents("contracts/pet.schema.yaml", []byte(base), []byte(head), true, true)
	if contract.Format != store.ContractFormatJSONSchema || contract.Adapter != contractAdapterJSONSchemaYAML {
		t.Fatalf("format/adapter = %q/%q", contract.Format, contract.Adapter)
	}
	if contract.Status != store.ContractIncompatible {
		t.Fatalf("status = %q, want incompatible (evidence: %+v)", contract.Status, contract.Evidence)
	}
	for _, want := range []store.ArchitectureContractEvidence{
		{Kind: "type_changed", Pointer: "schemas.$root.properties.id"},
		{Kind: "required_field_added", Pointer: "schemas.$root.properties.name"},
	} {
		if !contractHasEvidence(contract, want.Kind, want.Pointer) {
			t.Errorf("evidence %+v missing %s %s", contract.Evidence, want.Kind, want.Pointer)
		}
	}
}

func TestContractComparisonDoesNotChangeReviewOutcome(t *testing.T) {
	base := contractFixture(t, "breaking-removal", "base", "openapi.json")
	head := contractFixture(t, "breaking-removal", "head", "openapi.json")
	contract := compareContractContents("api/openapi.json", base, head, true, true)
	contracts := store.ArchitectureContracts{
		Status: store.ContractIncompatible, Declared: 1, Compared: 1,
		Contracts: []store.ArchitectureContract{contract},
	}

	verdict := newArchitectureVerdict(store.ArchitectureAnalysisObservation, true,
		[]string{architectureSignalInterface}, store.ArchitectureScope{},
		store.ArchitectureComparison{}, store.ArchitectureBudget{})
	verdict.Status = store.ArchitectureComplete
	got := attachContractComparison(verdict, contracts)

	if got.Status != store.ArchitectureComplete {
		t.Errorf("verdict status = %q, the contract result must not change it", got.Status)
	}
	if !got.Controls.Observation || got.Controls.WritesDecision {
		t.Errorf("controls = %+v, want observation without a write decision", got.Controls)
	}
	if !got.Controls.ContractsReadOnly {
		t.Error("contracts_read_only control was not recorded")
	}
	if !containsSubstring(got.Risks, "Contrato `api/openapi.json`") {
		t.Errorf("risks = %v, want the incompatible contract recorded", got.Risks)
	}
	if len(got.Dimensions) == 0 || got.Dimensions[0].Name != contractDimensionName {
		t.Errorf("dimensions = %+v, want the contract dimension first", got.Dimensions)
	}
	if !containsSubstring([]string{got.Dimensions[0].Content}, "operation_removed") {
		t.Errorf("contract dimension has no evidence: %q", got.Dimensions[0].Content)
	}
	if content := got.Dimensions[0].Content; !containsSubstring([]string{content}, "objetiva") ||
		!containsSubstring([]string{content}, "mantenedor") {
		t.Errorf("contract dimension does not separate the objective break from the maintainer decision: %q", content)
	}

	status, code := nativeOutcome(nativeReview{Status: "complete"})
	if status != store.RunSucceeded || code != "" {
		t.Errorf("review outcome = %s/%q, want succeeded — contracts are observation only", status, code)
	}
}

func containsSubstring(values []string, want string) bool {
	for _, value := range values {
		if strings.Contains(value, want) {
			return true
		}
	}
	return false
}

func TestContractNestedSwaggerAndPathParameters(t *testing.T) {
	nestedBase := `{
	  "openapi": "3.0.3",
	  "paths": {},
	  "components": {"schemas": {"Pet": {"type": "object", "properties": {
	    "owner": {"type": "object", "properties": {
	      "name": {"type": "string"}, "id": {"type": "integer"}}},
	    "tags": {"type": "array", "items": {"type": "string"}}
	  }}}}
	}`
	nestedHead := `{
	  "openapi": "3.0.3",
	  "paths": {},
	  "components": {"schemas": {"Pet": {"type": "object", "properties": {
	    "owner": {"type": "object", "properties": {
	      "id": {"type": "integer"}}},
	    "tags": {"type": "array", "items": {"type": "integer"}}
	  }}}}
	}`
	contract := compareContractContents("api/openapi.json", []byte(nestedBase), []byte(nestedHead), true, true)
	if contract.Status != store.ContractIncompatible {
		t.Fatalf("nested status = %q, want incompatible", contract.Status)
	}
	for _, want := range []store.ArchitectureContractEvidence{
		{Kind: "property_removed", Pointer: "components.schemas.Pet.properties.owner.properties.name"},
		{Kind: "type_changed", Pointer: "components.schemas.Pet.properties.tags.properties.$items"},
	} {
		if !contractHasEvidence(contract, want.Kind, want.Pointer) {
			t.Errorf("nested evidence %+v missing %s %s", contract.Evidence, want.Kind, want.Pointer)
		}
	}

	swaggerBase := `{"swagger":"2.0","paths":{},"definitions":{"Pet":{"type":"object","properties":{"id":{"type":"integer"}}}}}`
	swaggerHead := `{"swagger":"2.0","paths":{},"definitions":{"Pet":{"type":"object","properties":{"id":{"type":"string"}}}}}`
	swagger := compareContractContents("api/swagger.json", []byte(swaggerBase), []byte(swaggerHead), true, true)
	if swagger.Status != store.ContractIncompatible {
		t.Fatalf("swagger status = %q, want incompatible (evidence: %+v)", swagger.Status, swagger.Evidence)
	}
	if !contractHasEvidence(swagger, "type_changed", "components.schemas.Pet.properties.id") {
		t.Errorf("swagger evidence %+v missing the definitions type change", swagger.Evidence)
	}

	paramsBase := `{
	  "openapi": "3.0.3",
	  "paths": {"/pets": {
	    "parameters": [{"name": "tenant", "in": "header", "required": false}],
	    "get": {"responses": {"200": {"description": "ok"}}}
	  }}
	}`
	paramsHead := `{
	  "openapi": "3.0.3",
	  "paths": {"/pets": {
	    "parameters": [{"name": "tenant", "in": "header", "required": true}],
	    "get": {"responses": {"200": {"description": "ok"}}}
	  }}
	}`
	params := compareContractContents("api/openapi.json", []byte(paramsBase), []byte(paramsHead), true, true)
	if params.Status != store.ContractIncompatible {
		t.Fatalf("path parameter status = %q, want incompatible", params.Status)
	}
	if !contractHasEvidence(params, "required_parameter_added", "paths.GET /pets.parameters.header.tenant") {
		t.Errorf("path-level parameter was not merged into the operation: %+v", params.Evidence)
	}
}

func runTestGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func writeContractRepoFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commitContractRepo(t *testing.T, dir, message string) string {
	t.Helper()
	runTestGit(t, dir, "add", "-A")
	runTestGit(t, dir, "-c", "user.name=test", "-c", "user.email=test@example.com", "commit", "-q", "-m", message)
	return runTestGit(t, dir, "rev-parse", "HEAD")
}

func contractsByPath(contracts store.ArchitectureContracts) map[string]store.ArchitectureContract {
	byPath := make(map[string]store.ArchitectureContract, len(contracts.Contracts))
	for _, contract := range contracts.Contracts {
		byPath[contract.Path] = contract
	}
	return byPath
}

func TestCompareVersionedContractsFromGit(t *testing.T) {
	dir := t.TempDir()
	runTestGit(t, dir, "init", "-q")
	runTestGit(t, dir, "config", "core.autocrlf", "false")
	writeContractRepoFile(t, dir, "api/openapi.yaml", "openapi: 3.0.3\npaths: {}\n")
	writeContractRepoFile(t, dir, "api/config.json", `{"setting":true}`)
	writeContractRepoFile(t, dir, "api/broken.json", `{"setting":true}`)
	writeContractRepoFile(t, dir, "api/removed.proto", "syntax = \"proto3\";\nmessage Gone { string id = 1; }\n")
	baseSHA := commitContractRepo(t, dir, "base")

	writeContractRepoFile(t, dir, "api/openapi.yaml", "openapi: 3.0.3\npaths: {}\ninfo:\n  title: v2\n")
	writeContractRepoFile(t, dir, "api/config.json", `{"setting":false}`)
	writeContractRepoFile(t, dir, "api/broken.json", `{"setting":`)
	if err := os.Remove(filepath.Join(dir, "api", "removed.proto")); err != nil {
		t.Fatal(err)
	}
	headSHA := commitContractRepo(t, dir, "head")

	contracts := compareVersionedContracts(context.Background(), dir, baseSHA, headSHA)
	byPath := contractsByPath(contracts)

	if got, ok := byPath["api/openapi.yaml"]; !ok || got.Status != store.ContractCompatible ||
		got.Format != store.ContractFormatOpenAPI || got.Adapter != contractAdapterOpenAPIYAML {
		t.Errorf("changed YAML contract = %+v, want a compatible OpenAPI YAML entry", got)
	}
	if got, ok := byPath["api/broken.json"]; !ok || got.Status != store.ContractUnknown {
		t.Errorf("truncated JSON contract = %+v, want a kept unknown entry", got)
	}
	if _, ok := byPath["api/config.json"]; ok {
		t.Errorf("valid non-contract JSON was reported as a contract: %+v", byPath["api/config.json"])
	}
	if got, ok := byPath["api/removed.proto"]; !ok || got.Status != store.ContractIncompatible || got.Change != store.ContractChangeRemoved {
		t.Errorf("removed contract = %+v, want an incompatible removal", got)
	}
	if contracts.Status != store.ContractIncompatible {
		t.Errorf("aggregate status = %q, want incompatible", contracts.Status)
	}
}

func TestCompareVersionedContractsCaps(t *testing.T) {
	dir := t.TempDir()
	runTestGit(t, dir, "init", "-q")
	runTestGit(t, dir, "config", "core.autocrlf", "false")
	writeContractRepoFile(t, dir, "README.md", "fixtures\n")
	baseSHA := commitContractRepo(t, dir, "base")

	large := `{"openapi":"3.0.3","padding":"` + strings.Repeat("a", maxContractBytes+1024) + `"}`
	writeContractRepoFile(t, dir, "api/a-openapi.json", large)
	for i := 0; i < maxContractFiles+1; i++ {
		writeContractRepoFile(t, dir, "contracts/c"+string(rune('a'+i/26))+string(rune('a'+i%26))+".proto",
			"syntax = \"proto3\";\nmessage M { string id = 1; }\n")
	}
	headSHA := commitContractRepo(t, dir, "head")

	contracts := compareVersionedContracts(context.Background(), dir, baseSHA, headSHA)
	byPath := contractsByPath(contracts)

	if got, ok := byPath["api/a-openapi.json"]; !ok || got.Status != store.ContractUnknown ||
		!containsSubstring(got.Errors, "tamaño máximo") {
		t.Errorf("large blob = %+v, want rejected by size before reading", got)
	}
	limitSeen := false
	for _, contract := range contracts.Contracts {
		if containsSubstring(contract.Errors, "límite de contratos alcanzado") {
			limitSeen = true
		}
	}
	if !limitSeen {
		t.Errorf("file cap was not reported: %+v", contracts.Contracts)
	}
	if contracts.Status != store.ContractUnknown {
		t.Errorf("aggregate status = %q, want unknown", contracts.Status)
	}
}
