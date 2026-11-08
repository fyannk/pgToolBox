// Copyright 2026 The pgToolBox Authors.
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

// Package evidenceartifacts generates the committed v1alpha1 JSON Schema and
// deterministic wire examples from the dependency-light public Go model.
package evidenceartifacts

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	evidencev1alpha1 "github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1"
)

const (
	schemaDraft = "https://json-schema.org/draft/2020-12/schema"
	schemaID    = "https://github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1/schema.json"
)

var (
	timeType = reflect.TypeFor[time.Time]()
	apiPath  = reflect.TypeFor[evidencev1alpha1.RepositoryEvidenceSnapshot]().PkgPath()
)

// Artifact is one generated path relative to pkg/evidence/v1alpha1.
type Artifact struct {
	Path string
	Data []byte
}

// Generate returns every committed schema and wire-golden artifact.
func Generate() ([]Artifact, error) {
	schema, err := generateSchema()
	if err != nil {
		return nil, err
	}
	goldens, err := generateGoldens()
	if err != nil {
		return nil, err
	}
	return append([]Artifact{{Path: "schema.json", Data: schema}}, goldens...), nil
}

type jsonSchema map[string]any

type schemaGenerator struct {
	definitions map[string]any
	building    map[string]bool
}

func generateSchema() ([]byte, error) {
	generator := &schemaGenerator{
		definitions: make(map[string]any),
		building:    make(map[string]bool),
	}
	roots := []reflect.Type{
		reflect.TypeFor[evidencev1alpha1.RepositoryEvidenceSnapshot](),
		reflect.TypeFor[evidencev1alpha1.BarmanBackupPage](),
		reflect.TypeFor[evidencev1alpha1.BarmanWALRangePage](),
		reflect.TypeFor[evidencev1alpha1.BarmanWALGapPage](),
		reflect.TypeFor[evidencev1alpha1.BarmanRecoveryPathPage](),
		reflect.TypeFor[evidencev1alpha1.PGBackRestBackupPage](),
		reflect.TypeFor[evidencev1alpha1.PGBackRestWALRangePage](),
		reflect.TypeFor[evidencev1alpha1.PGBackRestWALGapPage](),
		reflect.TypeFor[evidencev1alpha1.PGBackRestRecoveryPathPage](),
		reflect.TypeFor[evidencev1alpha1.EvidenceAPIError](),
		reflect.TypeFor[evidencev1alpha1.ServiceStatus](),
	}
	rootReferences := make([]any, 0, len(roots))
	for _, root := range roots {
		generator.ensureDefinition(root)
		rootReferences = append(rootReferences, reference(root.Name()))
	}
	document := jsonSchema{
		"$schema":  schemaDraft,
		"$id":      schemaID,
		"$comment": "Generated from github.com/fyannk/pgToolBox/pkg/evidence/v1alpha1. Go Validate methods enforce cross-field, ordering, UTC, and UTF-8 byte-length invariants that JSON Schema cannot express portably.",
		"title":    "pgToolBox evidence agent API v1alpha1",
		"oneOf":    rootReferences,
		"$defs":    generator.definitions,
	}
	return marshalArtifact(document)
}

func (g *schemaGenerator) ensureDefinition(value reflect.Type) {
	for value.Kind() == reflect.Pointer {
		value = value.Elem()
	}
	if value.PkgPath() != apiPath || value.Name() == "" {
		return
	}
	name := value.Name()
	if _, exists := g.definitions[name]; exists || g.building[name] {
		return
	}
	g.building[name] = true
	var definition jsonSchema
	switch name {
	case "Details":
		definition = g.detailsDefinition()
	case "ServiceStatus":
		definition = serviceStatusDefinition()
	default:
		definition = g.definition(value)
	}
	g.definitions[name] = definition
	delete(g.building, name)
}

func (g *schemaGenerator) definition(value reflect.Type) jsonSchema {
	if enum := enumValues(value); enum != nil {
		return jsonSchema{"type": "string", "enum": enum}
	}
	switch value.Kind() {
	case reflect.Struct:
		return g.structDefinition(value)
	case reflect.String:
		return jsonSchema{"type": "string"}
	default:
		panic("unsupported public schema type " + value.String())
	}
}

func (g *schemaGenerator) structDefinition(value reflect.Type) jsonSchema {
	properties := make(map[string]any)
	required := make([]string, 0, value.NumField())
	g.collectFields(value, value, properties, &required)
	return objectDefinition(properties, required)
}

func (g *schemaGenerator) collectFields(owner, value reflect.Type, properties map[string]any, required *[]string) {
	for index := range value.NumField() {
		field := value.Field(index)
		tagParts := strings.Split(field.Tag.Get("json"), ",")
		jsonName := tagParts[0]
		if jsonName == "-" || field.PkgPath != "" {
			continue
		}
		if field.Anonymous && jsonName == "" {
			g.collectFields(owner, field.Type, properties, required)
			continue
		}
		if jsonName == "" {
			jsonName = field.Name
		}
		property := g.schemaFor(field.Type)
		applyFieldConstraints(owner.Name(), field.Name, property)
		properties[jsonName] = property
		// An omitempty field is an additive optional member: older producers
		// never send it, so it cannot be required.
		if !slices.Contains(tagParts[1:], "omitempty") {
			*required = append(*required, jsonName)
		}
	}
}

func (g *schemaGenerator) schemaFor(value reflect.Type) jsonSchema {
	if value == timeType {
		return jsonSchema{"type": "string", "format": "date-time"}
	}
	if value.Kind() == reflect.Pointer {
		return jsonSchema{"anyOf": []any{g.schemaFor(value.Elem()), jsonSchema{"type": "null"}}}
	}
	if value.Name() != "" && value.PkgPath() == apiPath {
		g.ensureDefinition(value)
		return reference(value.Name())
	}
	switch value.Kind() {
	case reflect.String:
		return jsonSchema{"type": "string"}
	case reflect.Bool:
		return jsonSchema{"type": "boolean"}
	case reflect.Uint64:
		return jsonSchema{"type": "integer", "minimum": uint64(0), "maximum": ^uint64(0)}
	case reflect.Slice:
		return jsonSchema{"type": "array", "items": g.schemaFor(value.Elem())}
	default:
		panic("unsupported public schema field " + value.String())
	}
}

func (g *schemaGenerator) detailsDefinition() jsonSchema {
	g.ensureDefinition(reflect.TypeFor[evidencev1alpha1.BarmanCloudSummary]())
	g.ensureDefinition(reflect.TypeFor[evidencev1alpha1.PGBackRestSummary]())
	known := objectDefinition(map[string]any{
		"type":         jsonSchema{"const": evidencev1alpha1.BarmanDetailsType},
		"barman_cloud": reference("BarmanCloudSummary"),
	}, []string{"type", "barman_cloud"})
	pgbackrest := objectDefinition(map[string]any{
		"type":       jsonSchema{"const": evidencev1alpha1.PGBackRestDetailsType},
		"pgbackrest": reference("PGBackRestSummary"),
	}, []string{"type", "pgbackrest"})
	unknown := objectDefinition(map[string]any{
		"type": boundedText(64),
	}, []string{"type"})
	unknown["not"] = jsonSchema{
		"properties": jsonSchema{"type": jsonSchema{"enum": []string{evidencev1alpha1.BarmanDetailsType, evidencev1alpha1.PGBackRestDetailsType}}},
		"required":   []string{"type"},
	}
	return jsonSchema{"oneOf": []any{known, pgbackrest, unknown}}
}

func serviceStatusDefinition() jsonSchema {
	branches := make([]any, 0, 3)
	for _, pair := range [][2]string{
		{evidencev1alpha1.HealthKind, evidencev1alpha1.HealthLive},
		{evidencev1alpha1.ReadinessKind, evidencev1alpha1.ReadinessReady},
		{evidencev1alpha1.ReadinessKind, evidencev1alpha1.ReadinessNotReady},
	} {
		branches = append(branches, objectDefinition(map[string]any{
			"api_version": jsonSchema{"const": evidencev1alpha1.APIVersion},
			"kind":        jsonSchema{"const": pair[0]},
			"status":      jsonSchema{"const": pair[1]},
		}, []string{"api_version", "kind", "status"}))
	}
	return jsonSchema{"oneOf": branches}
}

func objectDefinition(properties map[string]any, required []string) jsonSchema {
	return jsonSchema{
		"type":                 "object",
		"properties":           properties,
		"required":             required,
		"additionalProperties": true,
	}
}

func reference(name string) jsonSchema {
	return jsonSchema{"$ref": "#/$defs/" + name}
}

func enumValues(value reflect.Type) []any {
	var values []string
	switch value.Name() {
	case "State":
		values = []string{string(evidencev1alpha1.Healthy), string(evidencev1alpha1.Warning), string(evidencev1alpha1.Unhealthy), string(evidencev1alpha1.Unknown)}
	case "Support":
		values = []string{string(evidencev1alpha1.Supported), string(evidencev1alpha1.Unsupported), string(evidencev1alpha1.SupportUnknown)}
	case "Completeness":
		values = []string{string(evidencev1alpha1.Complete), string(evidencev1alpha1.Incomplete), string(evidencev1alpha1.Unscanned)}
	case "CapabilityID":
		values = []string{
			string(evidencev1alpha1.ObjectInventory), string(evidencev1alpha1.CatalogListing), string(evidencev1alpha1.StructuralValidation),
			string(evidencev1alpha1.DependencyValidation), string(evidencev1alpha1.WALContinuity), string(evidencev1alpha1.TimelineTraversal),
			string(evidencev1alpha1.EncryptedMetadata), string(evidencev1alpha1.RecoveryCoverage), string(evidencev1alpha1.RetentionExpectation),
		}
	case "FailureCategory":
		values = []string{
			string(evidencev1alpha1.FailureCanceled), string(evidencev1alpha1.FailureTimeout), string(evidencev1alpha1.FailureInvalidConfig),
			string(evidencev1alpha1.FailureAuthentication), string(evidencev1alpha1.FailureAuthorization), string(evidencev1alpha1.FailureThrottled),
			string(evidencev1alpha1.FailureUnavailable), string(evidencev1alpha1.FailureNotFound), string(evidencev1alpha1.FailureIncompatibleFormat),
			string(evidencev1alpha1.FailureSafetyLimit),
		}
	case "GapStatus":
		values = []string{string(evidencev1alpha1.GapCandidate), string(evidencev1alpha1.GapConfirmed)}
	case "CoverageStop":
		values = []string{
			string(evidencev1alpha1.CoverageFrontier), string(evidencev1alpha1.CoverageCandidateLimited),
			string(evidencev1alpha1.CoverageGapLimited), string(evidencev1alpha1.CoverageUnknownLimited),
		}
	case "ErrorCode":
		values = []string{
			string(evidencev1alpha1.ErrorInvalidRequest), string(evidencev1alpha1.ErrorUnauthenticated), string(evidencev1alpha1.ErrorNotFound),
			string(evidencev1alpha1.ErrorMethodNotAllowed), string(evidencev1alpha1.ErrorPublicationChanged), string(evidencev1alpha1.ErrorResponseLimit),
			string(evidencev1alpha1.ErrorBusy), string(evidencev1alpha1.ErrorInvalidPublication),
			string(evidencev1alpha1.ErrorForbidden), string(evidencev1alpha1.ErrorAuthenticationUnavailable),
			string(evidencev1alpha1.ErrorAudienceMismatch),
		}
	default:
		return nil
	}
	result := make([]any, len(values))
	for index, item := range values {
		result[index] = item
	}
	return result
}

func applyFieldConstraints(owner, field string, schema jsonSchema) {
	if strings.HasPrefix(owner, "PGBackRest") {
		applyPGBackRestConstraints(owner, field, schema)
		return
	}
	key := owner + "." + field
	switch key {
	case "RepositoryEvidenceSnapshot.APIVersion", "BarmanBackupPage.APIVersion", "BarmanWALRangePage.APIVersion", "BarmanWALGapPage.APIVersion", "BarmanRecoveryPathPage.APIVersion", "EvidenceAPIError.APIVersion":
		replaceSchema(schema, jsonSchema{"const": evidencev1alpha1.APIVersion})
	case "RepositoryEvidenceSnapshot.Kind":
		replaceSchema(schema, jsonSchema{"const": evidencev1alpha1.SnapshotKind})
	case "BarmanBackupPage.Kind":
		replaceSchema(schema, jsonSchema{"const": evidencev1alpha1.BarmanBackupPageKind})
	case "BarmanWALRangePage.Kind":
		replaceSchema(schema, jsonSchema{"const": evidencev1alpha1.BarmanWALRangePageKind})
	case "BarmanWALGapPage.Kind":
		replaceSchema(schema, jsonSchema{"const": evidencev1alpha1.BarmanWALGapPageKind})
	case "BarmanRecoveryPathPage.Kind":
		replaceSchema(schema, jsonSchema{"const": evidencev1alpha1.BarmanRecoveryPageKind})
	case "EvidenceAPIError.Kind":
		replaceSchema(schema, jsonSchema{"const": evidencev1alpha1.ErrorKind})
	case "Producer.Name":
		replaceSchema(schema, jsonSchema{"const": evidencev1alpha1.ProducerName})
	case "RepositoryIdentity.Provider":
		replaceSchema(schema, jsonSchema{"type": "string", "enum": []string{"s3", "azure", "gcs"}})
	case "RepositoryIdentity.Format":
		replaceSchema(schema, jsonSchema{"type": "string", "enum": []string{evidencev1alpha1.FormatBarmanCloud, evidencev1alpha1.FormatPGBackRest}})
	case "ScopeIdentity.Kind":
		replaceSchema(schema, jsonSchema{"type": "string", "enum": []string{evidencev1alpha1.ScopeKindBarmanServer, evidencev1alpha1.ScopeKindStanza}})
	case "Reason.Code":
		setText(schema, 64, `^[a-z0-9]+(?:-[a-z0-9]+)*$`)
	case "Reason.Message", "EvidenceAPIError.Message":
		setText(schema, 256, "")
	case "Producer.Version":
		setText(schema, 64, "")
	case "RepositoryEvidenceSnapshot.ProducerInstance":
		setText(schema, evidencev1alpha1.ProducerInstanceLength, `^[0-9a-f]{32}$`)
		schema["minLength"] = evidencev1alpha1.ProducerInstanceLength
	case "RepositoryEvidenceSnapshot.ProducerEpoch":
		setPositive(schema)
	case "ClusterIdentity.Namespace":
		setText(schema, 63, "")
	case "ClusterIdentity.UID":
		setText(schema, 128, "")
	case "ClusterIdentity.Name":
		setText(schema, 253, "")
	case "RepositoryIdentity.DestinationFingerprint":
		setText(schema, 71, `^sha256:[0-9a-f]{64}$`)
	case "ScopeIdentity.Name", "BarmanBackup.Server", "BarmanWALRange.Server", "BarmanWALGap.Server", "BarmanRecoveryPath.Server":
		setText(schema, 256, `^[^/\\]+$`)
	case "BarmanBackup.BackupID", "BarmanRecoveryPath.BackupID":
		setText(schema, 256, "")
	case "BarmanBackup.Status", "BarmanBackup.BackupType", "BarmanBackup.Compression", "BarmanBackup.Encryption":
		setText(schema, 64, "")
	case "BarmanBackup.SystemID":
		setText(schema, 64, `^[0-9]+$`)
	case "BarmanBackup.BeginWAL", "BarmanBackup.EndWAL", "BarmanWALRange.FirstWAL", "BarmanWALRange.LastWAL", "BarmanWALGap.FirstWAL", "BarmanWALGap.LastWAL", "BarmanRecoveryPath.StartWAL", "BarmanRecoveryPath.FrontierWAL":
		setText(schema, 24, `^[0-9A-F]{24}$`)
	case "BarmanBackup.BeginLSN", "BarmanBackup.EndLSN":
		setText(schema, 17, `^[0-9A-F]{1,8}/[0-9A-F]{1,8}$`)
	case "BarmanBackup.WALSegmentSizeBytes":
		setEnum(schema, walSegmentSizes())
	case "BarmanBackup.Timeline", "BarmanWALRange.Timeline", "BarmanWALGap.Timeline", "BarmanRecoveryPath.TargetTimeline", "BarmanRecoveryPath.StartTimeline", "BarmanRecoveryPath.FrontierTimeline", "BarmanWALRange.SegmentCount", "BarmanWALGap.SegmentCount", "BarmanWALGap.FirstObservedGeneration":
		setPositive(schema)
	case "BarmanBackupPage.Revision", "BarmanBackupPage.EvidenceGeneration", "BarmanWALRangePage.Revision", "BarmanWALRangePage.EvidenceGeneration", "BarmanWALGapPage.Revision", "BarmanWALGapPage.EvidenceGeneration", "BarmanRecoveryPathPage.Revision", "BarmanRecoveryPathPage.EvidenceGeneration":
		setPositive(schema)
	case "BarmanBackupPage.NextCursor", "BarmanWALRangePage.NextCursor", "BarmanWALGapPage.NextCursor", "BarmanRecoveryPathPage.NextCursor":
		setText(schema, 4096, "")
	case "RepositoryEvidenceSnapshot.Capabilities":
		schema["minItems"], schema["maxItems"] = 9, 9
	case "BarmanBackupPage.Items", "BarmanWALRangePage.Items", "BarmanWALGapPage.Items", "BarmanRecoveryPathPage.Items":
		schema["maxItems"] = 200
	case "BarmanRecoveryPath.AssumptionCodes":
		schema["maxItems"], schema["uniqueItems"] = 32, true
		if items, ok := schema["items"].(jsonSchema); ok {
			setText(items, 64, `^[a-z0-9]+(?:-[a-z0-9]+)*$`)
		}
	}
}

const (
	codePattern      = `^[a-z0-9]+(?:-[a-z0-9]+)*$`
	labelPattern     = `^[0-9]{8}-[0-9]{6}F(?:_[0-9]{8}-[0-9]{6}[DI])?$`
	archiveIDPattern = `^[1-9][0-9]?(?:\.[0-9])?-[1-9][0-9]*$`
	walNamePattern   = `^[0-9A-F]{24}$`
	lsnPattern       = `^[0-9A-F]{1,8}/[0-9A-F]{1,8}$`
	// stanzaPattern is a superset of ValidPGBackRestStanza: the schema
	// refuses a separator, and its maxLength the length, but not "." or
	// "..", a control character or surrounding space (an ECMA-262 pattern
	// for those needs a lookahead, which RE2-based validators reject). The
	// Go Validate methods are the authority.
	stanzaPattern = `^[^/\\]+$`
)

// applyPGBackRestConstraints bounds the pgBackRest resources;
// the Go Validate methods enforce the same and the cross-field rules.
func applyPGBackRestConstraints(owner, field string, schema jsonSchema) {
	page := strings.HasSuffix(owner, "Page")
	switch {
	case page && field == "APIVersion":
		replaceSchema(schema, jsonSchema{"const": evidencev1alpha1.APIVersion})
	case page && field == "Kind":
		replaceSchema(schema, jsonSchema{"const": map[string]string{
			"PGBackRestBackupPage":       evidencev1alpha1.PGBackRestBackupPageKind,
			"PGBackRestWALRangePage":     evidencev1alpha1.PGBackRestWALRangePageKind,
			"PGBackRestWALGapPage":       evidencev1alpha1.PGBackRestWALGapPageKind,
			"PGBackRestRecoveryPathPage": evidencev1alpha1.PGBackRestRecoveryPageKind,
		}[owner]})
	case page && (field == "Revision" || field == "EvidenceGeneration"):
		setPositive(schema)
	case page && field == "NextCursor":
		setText(schema, 4096, "")
	case page && field == "Items":
		schema["maxItems"] = 200
	case owner == "PGBackRestRetention" && field == "FullType":
		replaceSchema(schema, jsonSchema{"type": "string", "enum": []string{evidencev1alpha1.RetentionFullCount, evidencev1alpha1.RetentionFullTime}})
	case owner == "PGBackRestRetention" && field == "ArchiveType":
		replaceSchema(schema, jsonSchema{"type": "string", "enum": []string{evidencev1alpha1.PGBackRestFull, evidencev1alpha1.PGBackRestDifferential, evidencev1alpha1.PGBackRestIncremental}})
	case owner == "PGBackRestRetention" && (field == "Full" || field == "Diff" || field == "Archive" || field == "EffectiveArchive"):
		applyNonNull(schema, func(value jsonSchema) {
			value["minimum"], value["maximum"] = uint64(1), uint64(evidencev1alpha1.MaxPGBackRestRetention)
		})
	case field == "Stanza" || field == "ArchiveStanza":
		setText(schema, 128, stanzaPattern)
	case field == "Label" || field == "Prior" || field == "OldestBackup" || field == "NewestBackup":
		setText(schema, 33, labelPattern)
	case field == "References":
		schema["maxItems"], schema["uniqueItems"] = 1000, true
		if items, ok := schema["items"].(jsonSchema); ok {
			setText(items, 33, labelPattern)
		}
	case field == "ArchiveID":
		setText(schema, 32, archiveIDPattern)
	case field == "ArchiveIDs":
		schema["maxItems"], schema["uniqueItems"] = 256, true
		if items, ok := schema["items"].(jsonSchema); ok {
			setText(items, 32, archiveIDPattern)
		}
	case owner == "PGBackRestBackup" && field == "Type":
		replaceSchema(schema, jsonSchema{"type": "string", "enum": []string{evidencev1alpha1.PGBackRestFull, evidencev1alpha1.PGBackRestDifferential, evidencev1alpha1.PGBackRestIncremental}})
	case strings.HasSuffix(field, "WAL") || field == "ArchiveStart" || field == "ArchiveStop":
		setText(schema, 24, walNamePattern)
	case strings.HasSuffix(field, "LSN"):
		setText(schema, 17, lsnPattern)
	case field == "SystemID":
		setText(schema, 20, `^[0-9]+$`)
	case owner == "PGBackRestDatabase" && field == "Version":
		setText(schema, 4, `^[1-9][0-9]?(?:\.[0-9])?$`)
	case field == "PGBackRestVersion":
		setText(schema, 64, "")
	case field == "CNPGBackupName":
		setText(schema, 256, "")
	case owner == "PGBackRestSummary" && field == "Repository":
		setPositive(schema)
		schema["maximum"] = uint64(evidencev1alpha1.MaxPGBackRestRepository)
	case owner == "PGBackRestSummary" && field == "RepositoryFormat":
		setPositive(schema)
		applyNonNull(schema, func(value jsonSchema) { value["maximum"] = uint64(99) })
	case field == "WALSegmentSizeBytes":
		setEnum(schema, walSegmentSizes())
	case field == "WALSegmentSizeSource":
		setEnum(schema, []any{evidencev1alpha1.SegmentSizeFromBackupLSN, evidencev1alpha1.SegmentSizeAssumedDefault})
	case field == "Timeline" || field == "TargetTimeline" || field == "EndTimeline" || field == "FrontierTimeline" || field == "SegmentCount" || field == "FirstObservedGeneration" || field == "DBID" || field == "UsableBackups" && owner == "PGBackRestRecoveryWindow":
		setPositive(schema)
	case field == "Timelines":
		schema["maxItems"] = 128
		if items, ok := schema["items"].(jsonSchema); ok {
			items["minimum"] = uint64(1)
		}
	case field == "AssumptionCodes" || field == "UnusableReasons":
		schema["maxItems"], schema["uniqueItems"] = 32, true
		if items, ok := schema["items"].(jsonSchema); ok {
			setText(items, 64, codePattern)
		}
	case field == "Findings":
		schema["maxItems"] = 200
	case owner == "PGBackRestFinding" && field == "Code":
		setText(schema, 64, `^[A-Z][A-Za-z0-9]*$`)
	case owner == "PGBackRestFinding" && field == "Severity":
		replaceSchema(schema, jsonSchema{"type": "string", "enum": []string{evidencev1alpha1.FindingWarning, evidencev1alpha1.FindingError}})
	case owner == "PGBackRestFinding" && field == "Subject":
		setText(schema, 1024, "")
	case owner == "PGBackRestFinding" && field == "Message":
		setText(schema, 256, "")
	case field == "Annotations":
		schema["maxItems"] = 32
	case owner == "PGBackRestAnnotation" && field == "Key":
		setText(schema, 256, "")
	case owner == "PGBackRestAnnotation" && field == "Value":
		applyNonNull(schema, func(value jsonSchema) { value["maxLength"], value["x-max-utf8-bytes"] = 256, 256 })
	}
}

func replaceSchema(target, replacement jsonSchema) {
	clear(target)
	for key, value := range replacement {
		target[key] = value
	}
}

func setText(schema jsonSchema, maximum int, pattern string) {
	applyNonNull(schema, func(value jsonSchema) {
		value["minLength"] = 1
		value["maxLength"] = maximum
		value["x-max-utf8-bytes"] = maximum
		if pattern != "" {
			value["pattern"] = pattern
		}
	})
}

func boundedText(maximum int) jsonSchema {
	result := jsonSchema{"type": "string"}
	setText(result, maximum, "")
	return result
}

// setPositive requires a number of at least 1.
func setPositive(schema jsonSchema) {
	applyNonNull(schema, func(value jsonSchema) { value["minimum"] = uint64(1) })
}

func setEnum(schema jsonSchema, values []any) {
	applyNonNull(schema, func(value jsonSchema) { value["enum"] = values })
}

func applyNonNull(schema jsonSchema, apply func(jsonSchema)) {
	if alternatives, ok := schema["anyOf"].([]any); ok {
		for _, alternative := range alternatives {
			candidate, ok := alternative.(jsonSchema)
			if ok && candidate["type"] != "null" {
				apply(candidate)
				return
			}
		}
	}
	apply(schema)
}

func walSegmentSizes() []any {
	result := make([]any, 0, 11)
	for value := uint64(1 << 20); value <= 1<<30; value <<= 1 {
		result = append(result, value)
	}
	return result
}

func generateGoldens() ([]Artifact, error) {
	started := time.Date(2026, 7, 28, 10, 0, 0, 123000000, time.UTC)
	completed := started.Add(3 * time.Second)
	archiveReceipt := completed.Add(2 * time.Minute)
	fingerprint, err := evidencev1alpha1.FingerprintS3(evidencev1alpha1.S3FingerprintInput{
		Bucket: "synthetic-bucket", Prefix: "cluster",
		Format: "barman-cloud", ScopeKind: "barman-server", ScopeName: "orders",
	})
	if err != nil {
		return nil, err
	}
	reason := evidencev1alpha1.Reason{Code: "evidence-evaluated", Message: "structural evidence evaluated"}
	result := evidencev1alpha1.StateReason{State: evidencev1alpha1.Healthy, Reason: reason}
	backupCount, rangeCount, gapCount, pathCount := uint64(1), uint64(1), uint64(1), uint64(1)
	zero, one, two := uint64(0), uint64(1), uint64(2)
	clusterName := "orders"
	snapshot := evidencev1alpha1.RepositoryEvidenceSnapshot{
		APIVersion:       evidencev1alpha1.APIVersion,
		Kind:             evidencev1alpha1.SnapshotKind,
		Producer:         evidencev1alpha1.Producer{Name: evidencev1alpha1.ProducerName, Version: "1.0.0"},
		ProducerInstance: "0123456789abcdef0123456789abcdef",
		ProducerEpoch:    7,
		Identity: evidencev1alpha1.Identity{
			Cluster: evidencev1alpha1.ClusterIdentity{Namespace: "database-team", UID: "2f12b7d1-7e8d-4c37-a68f-233efc5f3191", Name: &clusterName},
			Repository: evidencev1alpha1.RepositoryIdentity{
				Provider: "s3", Format: "barman-cloud", DestinationFingerprint: fingerprint,
				Scope: evidencev1alpha1.ScopeIdentity{Kind: "barman-server", Name: "orders"},
			},
		},
		Revision: 42, EvidenceGeneration: 42, StartedAt: &started, CompletedAt: &completed, LastAttemptAt: &started,
		Completeness: evidencev1alpha1.Complete, State: evidencev1alpha1.Healthy, Reason: reason,
		Capabilities: goldenCapabilities(reason),
		Inventory: evidencev1alpha1.InventorySummary{
			Known: true, ObjectCount: &backupCount, StoredBytes: new(uint64(4096)), UnscopedObjectCount: &zero,
			PagesExamined: one, ObjectsExamined: backupCount,
		},
		Details: evidencev1alpha1.Details{Type: evidencev1alpha1.BarmanDetailsType, BarmanCloud: &evidencev1alpha1.BarmanCloudSummary{
			BackupItems: &backupCount, WALRangeItems: &rangeCount, WALGapItems: &gapCount, RecoveryPathItems: &pathCount,
			StructurallyUsableBackups: &backupCount,
			BackupStates:              &evidencev1alpha1.StateCounts{Healthy: one},
			WALCounts:                 &evidencev1alpha1.BarmanWALCounts{Segment: two},
			WAL:                       result,
			Timeline:                  result,
			Coverage:                  result,
			Retention: evidencev1alpha1.BarmanRetentionSummary{
				VisibleBackups: &backupCount, StructurallyUsableBackups: &backupCount,
				OldestCompletionAt: &completed, NewestCompletionAt: &completed, State: evidencev1alpha1.Healthy, Reason: reason,
			},
			LatestArchiveReceiptAt: &archiveReceipt,
		}},
	}
	header := func(kind string) evidencev1alpha1.PageHeader {
		return evidencev1alpha1.PageHeader{
			APIVersion: evidencev1alpha1.APIVersion, Kind: kind, Revision: 42, EvidenceGeneration: 42, TotalItems: &one,
		}
	}
	status, backupType, systemID := "DONE", "full", "7396242311909787798"
	walSize, timeline := uint64(16<<20), uint64(1)
	beginWAL, endWAL := "000000010000000000000001", "000000010000000000000002"
	beginLSN, endLSN := "0/1000000", "0/2000000"
	compression, encryption := "gzip", "none"
	logical, stored := uint64(8192), uint64(4096)
	backupPage := evidencev1alpha1.BarmanBackupPage{
		PageHeader: header(evidencev1alpha1.BarmanBackupPageKind),
		Items: []evidencev1alpha1.BarmanBackup{{
			Server: "orders", BackupID: "20260728T100000", Status: &status, BackupType: &backupType,
			State: evidencev1alpha1.Healthy, Reason: reason, SystemID: &systemID, PostgreSQLVersion: new(uint64(170000)),
			Timeline: &timeline, WALSegmentSizeBytes: &walSize, BeginWAL: &beginWAL, EndWAL: &endWAL,
			BeginLSN: &beginLSN, EndLSN: &endLSN, BeginAt: &started, EndAt: &completed,
			LogicalBytes: &logical, StoredArtifactBytes: &stored, Compression: &compression, Encryption: &encryption,
			ArtifactCount: &one, TablespaceCount: &zero,
		}},
	}
	rangePage := evidencev1alpha1.BarmanWALRangePage{
		PageHeader: header(evidencev1alpha1.BarmanWALRangePageKind),
		Items: []evidencev1alpha1.BarmanWALRange{{
			Server: "orders", Timeline: one, StartPosition: one, EndPosition: two, SegmentCount: two,
			FirstWAL: beginWAL, LastWAL: endWAL, LatestReceiptAt: &archiveReceipt, EndReceiptAt: &archiveReceipt,
		}},
	}
	gapPage := evidencev1alpha1.BarmanWALGapPage{
		PageHeader: header(evidencev1alpha1.BarmanWALGapPageKind),
		Items: []evidencev1alpha1.BarmanWALGap{{
			Server: "orders", Timeline: one, StartPosition: 3, EndPosition: 3, SegmentCount: one,
			FirstWAL: "000000010000000000000003", LastWAL: "000000010000000000000003",
			Status: evidencev1alpha1.GapCandidate, FirstObservedGeneration: 42, LastObservedGeneration: 42,
		}},
	}
	recoveryPage := evidencev1alpha1.BarmanRecoveryPathPage{
		PageHeader: header(evidencev1alpha1.BarmanRecoveryPageKind),
		Items: []evidencev1alpha1.BarmanRecoveryPath{{
			Server: "orders", BackupID: "20260728T100000", TargetTimeline: one,
			State: evidencev1alpha1.Healthy, Reason: reason, Stop: evidencev1alpha1.CoverageFrontier,
			LowerBoundAt: &completed, StartTimeline: &one, StartPosition: &one, StartWAL: &beginWAL,
			FrontierTimeline: &one, FrontierPosition: &two, FrontierWAL: &endWAL, FrontierReceiptAt: &archiveReceipt,
			AssumptionCodes: []string{"segment-name-presence-only", "wal-bytes-and-restore-not-verified"},
		}},
	}
	pgbr, err := pgbackrestGoldens(snapshot, header)
	if err != nil {
		return nil, err
	}
	values := []struct {
		path  string
		value any
	}{
		{"testdata/wire/health.json", evidencev1alpha1.ServiceStatus{APIVersion: evidencev1alpha1.APIVersion, Kind: evidencev1alpha1.HealthKind, Status: evidencev1alpha1.HealthLive}},
		{"testdata/wire/readiness-ready.json", evidencev1alpha1.ServiceStatus{APIVersion: evidencev1alpha1.APIVersion, Kind: evidencev1alpha1.ReadinessKind, Status: evidencev1alpha1.ReadinessReady}},
		{"testdata/wire/readiness-not-ready.json", evidencev1alpha1.ServiceStatus{APIVersion: evidencev1alpha1.APIVersion, Kind: evidencev1alpha1.ReadinessKind, Status: evidencev1alpha1.ReadinessNotReady}},
		{"testdata/wire/snapshot.json", snapshot},
		{"testdata/wire/backups.json", backupPage},
		{"testdata/wire/wal-ranges.json", rangePage},
		{"testdata/wire/wal-gaps.json", gapPage},
		{"testdata/wire/recovery-paths.json", recoveryPage},
		{"testdata/wire/pgbackrest-snapshot.json", pgbr.snapshot},
		{"testdata/wire/pgbackrest-backups.json", pgbr.backups},
		{"testdata/wire/pgbackrest-wal-ranges.json", pgbr.ranges},
		{"testdata/wire/pgbackrest-wal-gaps.json", pgbr.gaps},
		{"testdata/wire/pgbackrest-recovery-paths.json", pgbr.paths},
		{"testdata/wire/error.json", evidencev1alpha1.EvidenceAPIError{APIVersion: evidencev1alpha1.APIVersion, Kind: evidencev1alpha1.ErrorKind, Code: evidencev1alpha1.ErrorPublicationChanged, Message: "repository evidence changed; restart from snapshot"}},
	}
	artifacts := make([]Artifact, 0, len(values))
	for _, value := range values {
		if err := validateGolden(value.value); err != nil {
			return nil, fmt.Errorf("generate %s: %w", value.path, err)
		}
		data, err := marshalArtifact(value.value)
		if err != nil {
			return nil, fmt.Errorf("generate %s: %w", value.path, err)
		}
		artifacts = append(artifacts, Artifact{Path: value.path, Data: data})
	}
	return artifacts, nil
}

type pgbackrestWire struct {
	snapshot evidencev1alpha1.RepositoryEvidenceSnapshot
	backups  evidencev1alpha1.PGBackRestBackupPage
	ranges   evidencev1alpha1.PGBackRestWALRangePage
	gaps     evidencev1alpha1.PGBackRestWALGapPage
	paths    evidencev1alpha1.PGBackRestRecoveryPathPage
}

// pgbackrestGoldens is the pgBackRest profile on the Barman golden's
// envelope: one stanza, a full and an incremental backup (the incremental
// is not usable: its manifest does not verify), one WAL range, one candidate
// gap after it, and the recovery window of the full backup.
func pgbackrestGoldens(barman evidencev1alpha1.RepositoryEvidenceSnapshot, header func(string) evidencev1alpha1.PageHeader) (pgbackrestWire, error) {
	fingerprint, err := evidencev1alpha1.FingerprintS3(evidencev1alpha1.S3FingerprintInput{
		Bucket: "synthetic-bucket", Prefix: "pgbr/orders",
		Format: evidencev1alpha1.FormatPGBackRest, ScopeKind: evidencev1alpha1.ScopeKindStanza, ScopeName: "orders",
	})
	if err != nil {
		return pgbackrestWire{}, err
	}
	started := *barman.StartedAt
	fullStart, fullStop := started.Add(-2*time.Hour), started.Add(-2*time.Hour+30*time.Second)
	incrStart, incrStop := started.Add(-time.Hour), started.Add(-time.Hour+10*time.Second)
	receipt := started.Add(-time.Minute)
	const retentionMessage = "the backups and WAL match the retention of 2 full backups"
	healthy := func(code, message string) evidencev1alpha1.StateReason {
		return evidencev1alpha1.StateReason{State: evidencev1alpha1.Healthy, Reason: evidencev1alpha1.Reason{Code: code, Message: message}}
	}
	one, two, five, sixteen := uint64(1), uint64(2), uint64(5), uint64(16<<20)
	version, systemID, pgVersion := "2.60.0", "7570185463461748506", uint64(170000)
	source, archiveID, encrypted := evidencev1alpha1.SegmentSizeFromBackupLSN, "17-1", true
	full, incr := "20261107-081054F", "20261107-081054F_20261107-091108I"
	firstWAL, lastWAL, gapWAL := "000000010000000000000003", "000000010000000000000008", "000000010000000000000009"
	snapshot := barman
	snapshot.Identity.Repository = evidencev1alpha1.RepositoryIdentity{
		Provider: "s3", Format: evidencev1alpha1.FormatPGBackRest, DestinationFingerprint: fingerprint,
		Scope: evidencev1alpha1.ScopeIdentity{Kind: evidencev1alpha1.ScopeKindStanza, Name: "orders"},
	}
	snapshot.State = evidencev1alpha1.Unhealthy
	snapshot.Reason = evidencev1alpha1.Reason{Code: "snapshot-unhealthy", Message: "pgBackRest catalog, WAL, timeline or recovery evidence is not healthy"}
	capabilities := make([]evidencev1alpha1.Capability, 0, len(barman.Capabilities))
	for _, capability := range barman.Capabilities {
		switch capability.ID {
		case evidencev1alpha1.RetentionExpectation:
			capability.Reason = evidencev1alpha1.Reason{Code: "capability-retention-expectation-comparison-healthy", Message: retentionMessage}
		case evidencev1alpha1.CatalogListing, evidencev1alpha1.StructuralValidation:
			capability.State = evidencev1alpha1.Unhealthy
			capability.Reason = evidencev1alpha1.Reason{Code: "capability-" + string(capability.ID) + "-unhealthy", Message: "a backup is not usable"}
		case evidencev1alpha1.WALContinuity, evidencev1alpha1.RecoveryCoverage:
			capability.State = evidencev1alpha1.Warning
			capability.Reason = evidencev1alpha1.Reason{Code: "capability-" + string(capability.ID) + "-warning", Message: "a candidate WAL gap follows the archived WAL"}
		case evidencev1alpha1.ObjectInventory, evidencev1alpha1.DependencyValidation, evidencev1alpha1.TimelineTraversal, evidencev1alpha1.EncryptedMetadata:
			// Healthy, as in the Barman golden.
		}
		capabilities = append(capabilities, capability)
	}
	snapshot.Capabilities = capabilities
	window := &evidencev1alpha1.PGBackRestRecoveryWindow{
		OldestBackup: full, StartAt: fullStop, NewestBackup: full, NewestStopAt: fullStop,
		EndTimeline: one, EndWAL: lastWAL, EndLSN: "0/9000000", EndReceiptAt: &receipt, UsableBackups: one,
		AssumptionCodes: []string{"window-end-at-segment-upload-time"},
	}
	snapshot.Details = evidencev1alpha1.Details{Type: evidencev1alpha1.PGBackRestDetailsType, PGBackRest: &evidencev1alpha1.PGBackRestSummary{
		Stanza: "orders", ArchiveStanza: "orders", Repository: one, RepositoryFormat: &five, Encrypted: &encrypted,
		PGBackRestVersion: &version,
		Database:          &evidencev1alpha1.PGBackRestDatabase{DBID: one, SystemID: systemID, Version: "17", PostgreSQLVersion: &pgVersion},
		ArchiveIDs:        []string{archiveID}, WALSegmentSizeBytes: &sixteen, WALSegmentSizeSource: &source,
		BackupItems: &two, WALRangeItems: &one, WALGapItems: &one, RecoveryPathItems: &two, UsableBackups: &one,
		BackupStates:           &evidencev1alpha1.StateCounts{Healthy: one, Unhealthy: one},
		WALCounts:              &evidencev1alpha1.PGBackRestWALCounts{Segment: 6, BackupHistory: two},
		Catalog:                evidencev1alpha1.StateReason{State: evidencev1alpha1.Unhealthy, Reason: evidencev1alpha1.Reason{Code: "catalog-unhealthy", Message: "a backup is not usable"}},
		WAL:                    evidencev1alpha1.StateReason{State: evidencev1alpha1.Warning, Reason: evidencev1alpha1.Reason{Code: "wal-warning", Message: "a candidate WAL gap was observed"}},
		Timeline:               healthy("timeline-healthy", "timeline history evaluated"),
		Coverage:               evidencev1alpha1.StateReason{State: evidencev1alpha1.Warning, Reason: evidencev1alpha1.Reason{Code: "coverage-warning", Message: "a recovery path stops at a candidate WAL gap"}},
		RecoveryWindow:         window,
		LatestArchiveReceiptAt: &receipt,
		Retention: &evidencev1alpha1.PGBackRestRetention{
			Configured: true, Full: &two, FullType: evidencev1alpha1.RetentionFullCount, ArchiveType: evidencev1alpha1.PGBackRestFull,
			EffectiveArchive: &two, FullBackups: one, IncrementalBackups: one, EvaluatedAt: &incrStop,
			State: evidencev1alpha1.Healthy, Reason: evidencev1alpha1.Reason{Code: "retention-healthy", Message: retentionMessage},
		},
		Findings: []evidencev1alpha1.PGBackRestFinding{
			{Code: evidencev1alpha1.FindingChecksumMismatch, Severity: evidencev1alpha1.FindingError, Subject: "backup/orders/" + incr + "/backup.manifest", Message: "content does not match its backrest-checksum"},
			{Code: evidencev1alpha1.FindingWALGap, Severity: evidencev1alpha1.FindingWarning, Subject: "archive/orders/17-1/" + gapWAL, Message: "1 segment(s) missing on timeline 1, " + gapWAL + " to " + gapWAL + " (candidate)"},
		},
	}}
	stopLSN, startLSN := "0/4000100", "0/3000028"
	timeline := one
	annotation := "orders-20261107081054"
	fullPath := evidencev1alpha1.PGBackRestRecoveryPath{
		Stanza: "orders", Label: full, ArchiveID: &archiveID, TargetTimeline: one,
		State: evidencev1alpha1.Warning, Reason: evidencev1alpha1.Reason{Code: "coverage-path-warning", Message: "WAL is contiguous to " + lastWAL + "; a candidate gap follows"},
		Stop: evidencev1alpha1.CoverageCandidateLimited, Timelines: []uint64{one},
		StartWAL: &firstWAL, ConsistentWAL: new("000000010000000000000004"), Consistent: true,
		FrontierTimeline: &timeline, FrontierPosition: new(uint64(8)), FrontierWAL: &lastWAL, FrontierReceiptAt: &receipt,
		AssumptionCodes: []string{"segment-name-presence-only", "wal-bytes-and-restore-not-verified"},
	}
	incrPath := fullPath
	incrPath.Label, incrPath.StartWAL, incrPath.ConsistentWAL = incr, new("000000010000000000000007"), new("000000010000000000000007")
	return pgbackrestWire{
		snapshot: snapshot,
		backups: evidencev1alpha1.PGBackRestBackupPage{
			PageHeader: withTotal(header(evidencev1alpha1.PGBackRestBackupPageKind), two),
			Items: []evidencev1alpha1.PGBackRestBackup{
				{
					Stanza: "orders", Label: full, Type: evidencev1alpha1.PGBackRestFull, References: []string{},
					State: evidencev1alpha1.Healthy, Reason: evidencev1alpha1.Reason{Code: "backup-healthy", Message: "chain, manifests and WAL to consistency are present and verified"},
					Usable: true, UnusableReasons: []string{}, ArchiveID: &archiveID, SystemID: &systemID, PostgreSQLVersion: &pgVersion, Timeline: &timeline,
					StartAt: &fullStart, StopAt: &fullStop, StartLSN: &startLSN, StopLSN: &stopLSN,
					ArchiveStart: &firstWAL, ArchiveStop: new("000000010000000000000004"),
					SizeBytes: 23_000_000, DeltaBytes: 23_000_000, RepositorySizeBytes: 3_000_000, RepositoryDeltaBytes: 3_000_000,
					Error: new(false), PGBackRestVersion: &version, CNPGBackupName: &annotation,
					Annotations: []evidencev1alpha1.PGBackRestAnnotation{{Key: "cnpg-backup-name", Value: annotation}},
				},
				{
					Stanza: "orders", Label: incr, Type: evidencev1alpha1.PGBackRestIncremental, Prior: &full, References: []string{full},
					State: evidencev1alpha1.Unhealthy, Reason: evidencev1alpha1.Reason{Code: "backup-unhealthy", Message: "manifest does not verify"},
					UnusableReasons: []string{"manifest-unhealthy"}, ArchiveID: &archiveID, SystemID: &systemID, PostgreSQLVersion: &pgVersion, Timeline: &timeline,
					StartAt: &incrStart, StopAt: &incrStop, StartLSN: new("0/7000028"), StopLSN: new("0/7000100"),
					ArchiveStart: new("000000010000000000000007"), ArchiveStop: new("000000010000000000000007"),
					SizeBytes: 23_000_000, DeltaBytes: 400_000, RepositorySizeBytes: 3_000_000, RepositoryDeltaBytes: 60_000,
					Error: new(false), PGBackRestVersion: &version, Annotations: []evidencev1alpha1.PGBackRestAnnotation{},
				},
			},
		},
		ranges: evidencev1alpha1.PGBackRestWALRangePage{
			PageHeader: header(evidencev1alpha1.PGBackRestWALRangePageKind),
			Items: []evidencev1alpha1.PGBackRestWALRange{{
				Stanza: "orders", ArchiveID: archiveID, Timeline: one, StartPosition: 3, EndPosition: 8, SegmentCount: 6,
				FirstWAL: firstWAL, LastWAL: lastWAL, LatestReceiptAt: &receipt, EndReceiptAt: &receipt,
			}},
		},
		gaps: evidencev1alpha1.PGBackRestWALGapPage{
			PageHeader: header(evidencev1alpha1.PGBackRestWALGapPageKind),
			Items: []evidencev1alpha1.PGBackRestWALGap{{
				Stanza: "orders", ArchiveID: archiveID, Timeline: one, StartPosition: 9, EndPosition: 9, SegmentCount: one,
				FirstWAL: gapWAL, LastWAL: gapWAL, Status: evidencev1alpha1.GapCandidate, FirstObservedGeneration: 42, LastObservedGeneration: 42,
			}},
		},
		paths: evidencev1alpha1.PGBackRestRecoveryPathPage{
			PageHeader: withTotal(header(evidencev1alpha1.PGBackRestRecoveryPageKind), two),
			Items:      []evidencev1alpha1.PGBackRestRecoveryPath{fullPath, incrPath},
		},
	}, nil
}

func withTotal(header evidencev1alpha1.PageHeader, total uint64) evidencev1alpha1.PageHeader {
	header.TotalItems = &total
	return header
}

func goldenCapabilities(reason evidencev1alpha1.Reason) []evidencev1alpha1.Capability {
	ids := []evidencev1alpha1.CapabilityID{
		evidencev1alpha1.CatalogListing,
		evidencev1alpha1.DependencyValidation,
		evidencev1alpha1.EncryptedMetadata,
		evidencev1alpha1.ObjectInventory,
		evidencev1alpha1.RecoveryCoverage,
		evidencev1alpha1.RetentionExpectation,
		evidencev1alpha1.StructuralValidation,
		evidencev1alpha1.TimelineTraversal,
		evidencev1alpha1.WALContinuity,
	}
	result := make([]evidencev1alpha1.Capability, 0, len(ids))
	for _, id := range ids {
		result = append(result, evidencev1alpha1.Capability{ID: id, Support: evidencev1alpha1.Supported, State: evidencev1alpha1.Healthy, Reason: reason})
	}
	return result
}

func validateGolden(value any) error {
	var err error
	switch typed := value.(type) {
	case evidencev1alpha1.RepositoryEvidenceSnapshot:
		err = typed.Validate()
	case evidencev1alpha1.BarmanBackupPage:
		err = typed.Validate()
	case evidencev1alpha1.BarmanWALRangePage:
		err = typed.Validate()
	case evidencev1alpha1.BarmanWALGapPage:
		err = typed.Validate()
	case evidencev1alpha1.BarmanRecoveryPathPage:
		err = typed.Validate()
	case evidencev1alpha1.PGBackRestBackupPage:
		err = typed.Validate()
	case evidencev1alpha1.PGBackRestWALRangePage:
		err = typed.Validate()
	case evidencev1alpha1.PGBackRestWALGapPage:
		err = typed.Validate()
	case evidencev1alpha1.PGBackRestRecoveryPathPage:
		err = typed.Validate()
	case evidencev1alpha1.EvidenceAPIError:
		err = typed.Validate()
	case evidencev1alpha1.ServiceStatus:
		err = typed.Validate()
	default:
		err = errors.New("unsupported golden resource")
	}
	return err
}

func marshalArtifact(value any) ([]byte, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}
