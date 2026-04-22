// Command openapi-gen generates openapi.yaml from Go source types and route metadata.
//
// Run via: go generate ./internal/api/
package main

import (
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"

	"github.com/VoiceBlender/voiceblender/internal/api"
	"github.com/VoiceBlender/voiceblender/internal/events"
	"gopkg.in/yaml.v3"
)

// ── YAML ordered-map helpers ────────────────────────────────────────────

// omap is an ordered map backed by yaml.Node (kind MappingNode).
type omap struct{ node yaml.Node }

func newMap() *omap {
	return &omap{node: yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}}
}

// setQuotedKey is like set but forces the key to be single-quoted (e.g. '200').
func (m *omap) setQuotedKey(key string, val interface{}) *omap {
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Value: key, Tag: "!!str", Style: yaml.SingleQuotedStyle}
	m.setWithKeyNode(keyNode, val)
	return m
}

func (m *omap) set(key string, val interface{}) *omap {
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Value: key, Tag: "!!str"}
	m.setWithKeyNode(keyNode, val)
	return m
}

func (m *omap) setWithKeyNode(keyNode *yaml.Node, val interface{}) {
	var valNode *yaml.Node
	switch v := val.(type) {
	case *omap:
		valNode = &v.node
	case *seq:
		valNode = &v.node
	case *yaml.Node:
		valNode = v
	case string:
		valNode = &yaml.Node{Kind: yaml.ScalarNode, Value: v, Tag: "!!str"}
	case int:
		valNode = &yaml.Node{Kind: yaml.ScalarNode, Value: fmt.Sprintf("%d", v), Tag: "!!int"}
	case bool:
		valNode = &yaml.Node{Kind: yaml.ScalarNode, Value: fmt.Sprintf("%t", v), Tag: "!!bool"}
	case float64:
		valNode = &yaml.Node{Kind: yaml.ScalarNode, Value: fmt.Sprintf("%g", v), Tag: "!!float"}
	default:
		// Marshal through yaml then decode to node.
		b, _ := yaml.Marshal(v)
		valNode = &yaml.Node{}
		_ = yaml.Unmarshal(b, valNode)
		// yaml.Unmarshal wraps in a document node; unwrap.
		if valNode.Kind == yaml.DocumentNode && len(valNode.Content) > 0 {
			valNode = valNode.Content[0]
		}
	}
	m.node.Content = append(m.node.Content, keyNode, valNode)
}

type seq struct{ node yaml.Node }

func newSeq() *seq {
	return &seq{node: yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}}
}

func (s *seq) add(val interface{}) *seq {
	switch v := val.(type) {
	case *omap:
		s.node.Content = append(s.node.Content, &v.node)
	case *seq:
		s.node.Content = append(s.node.Content, &v.node)
	case string:
		s.node.Content = append(s.node.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: v, Tag: "!!str"})
	default:
		b, _ := yaml.Marshal(v)
		n := &yaml.Node{}
		_ = yaml.Unmarshal(b, n)
		if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
			n = n.Content[0]
		}
		s.node.Content = append(s.node.Content, n)
	}
	return s
}

// ── Schema generation ───────────────────────────────────────────────────

// Package-level enrichment data loaded from the api package.
var (
	schemaEnrichments       map[string]api.FieldEnrichment
	webhookFieldDescs       map[string]string
	webhookNestedFieldDescs map[string]string
)

// schemaRegistry collects named schemas and deduplicates.
var schemaRegistry = map[string]*omap{}

func schemaRef(name string) *omap {
	return newMap().set("$ref", "#/components/schemas/"+schemaDisplayName(name))
}

func typeName(t reflect.Type) string {
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return t.Name()
}

func goTypeToSchema(t reflect.Type) *omap {
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String:
		return newMap().set("type", "string")
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return newMap().set("type", "integer")
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return newMap().set("type", "integer")
	case reflect.Float32, reflect.Float64:
		return newMap().set("type", "number")
	case reflect.Bool:
		return newMap().set("type", "boolean")
	case reflect.Slice:
		elem := t.Elem()
		if elem.Kind() == reflect.Struct && elem.Name() != "" {
			registerSchema(elem)
			return newMap().set("type", "array").set("items", schemaRef(elem.Name()))
		}
		return newMap().set("type", "array").set("items", goTypeToSchema(elem))
	case reflect.Map:
		if t.Key().Kind() == reflect.String {
			return newMap().set("type", "object").set("additionalProperties", goTypeToSchema(t.Elem()))
		}
		return newMap().set("type", "object")
	case reflect.Struct:
		if t.Name() != "" {
			registerSchema(t)
			return schemaRef(t.Name())
		}
		return structToSchema(t)
	}
	return newMap().set("type", "string")
}

// responseSchemaTypes tracks types that represent API responses (not requests).
// These get instance_id prepended.
var responseSchemaTypes = map[string]bool{
	"LegView":  true,
	"RoomView": true,
}

func structToSchema(t reflect.Type) *omap {
	parentName := t.Name()
	props := newMap()
	required := newSeq()
	hasRequired := false

	// Add instance_id as the first property for response types.
	if responseSchemaTypes[parentName] {
		props.set("instance_id", newMap().set("type", "string").set("description", "Instance identifier"))
	}

	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		// Handle embedded structs by flattening their fields.
		if f.Anonymous {
			embT := f.Type
			if embT.Kind() == reflect.Ptr {
				embT = embT.Elem()
			}
			for j := 0; j < embT.NumField(); j++ {
				ef := embT.Field(j)
				if !ef.IsExported() {
					continue
				}
				jsonName, omit := parseJSONTag(ef)
				if jsonName == "-" {
					continue
				}
				props.set(jsonName, enrichedFieldSchema(parentName, jsonName, ef))
				if !omit {
					required.add(jsonName)
					hasRequired = true
				}
			}
			continue
		}
		jsonName, omit := parseJSONTag(f)
		if jsonName == "-" {
			continue
		}
		props.set(jsonName, enrichedFieldSchema(parentName, jsonName, f))
		if !omit {
			required.add(jsonName)
			hasRequired = true
		}
	}

	schema := newMap().set("type", "object").set("properties", props)
	if hasRequired {
		schema.set("required", required)
	}

	// Add oneOf constraint for PlaybackRequest.
	if parentName == "PlaybackRequest" {
		oneOf := newSeq()
		oneOf.add(newMap().set("required", newSeq().add("url").add("mime_type")))
		oneOf.add(newMap().set("required", newSeq().add("tone")))
		schema.set("oneOf", oneOf)
	}

	return schema
}

// enrichedFieldSchema generates the schema for a struct field, then applies
// any enrichment metadata (description, enum, format, constraints).
func enrichedFieldSchema(parentType, jsonName string, f reflect.StructField) *omap {
	schema := fieldSchema(f)

	// Look up enrichment.
	key := parentType + "." + jsonName
	enrich, ok := schemaEnrichments[key]
	if !ok {
		return schema
	}

	// If the schema is a $ref, wrap with description via allOf.
	if isRef(schema) {
		wrapper := newMap()
		if enrich.Description != "" {
			wrapper.set("description", enrich.Description)
		}
		wrapper.set("allOf", newSeq().add(schema))
		return wrapper
	}

	// If the schema uses nullable+allOf (pointer-to-struct), add description at top.
	if hasAllOf(schema) {
		if enrich.Description != "" {
			// Insert description before the allOf node.
			insertDescription(schema, enrich.Description)
		}
		return schema
	}

	if enrich.Description != "" {
		schema.set("description", enrich.Description)
	}
	if len(enrich.Enum) > 0 {
		enumSeq := newSeq()
		for _, v := range enrich.Enum {
			enumSeq.add(v)
		}
		schema.set("enum", enumSeq)
	}
	if enrich.Format != "" {
		schema.set("format", enrich.Format)
	}
	if enrich.Default != nil {
		schema.set("default", enrich.Default)
	}
	if enrich.Minimum != nil {
		schema.set("minimum", *enrich.Minimum)
	}
	if enrich.Maximum != nil {
		schema.set("maximum", *enrich.Maximum)
	}

	// Special case: codecs items enum.
	if parentType == "CreateLegRequest" && jsonName == "codecs" {
		addCodecsItemEnum(schema, api.CodecsItemEnum)
	}

	return schema
}

// isRef checks if a schema node is a $ref.
func isRef(m *omap) bool {
	for i := 0; i < len(m.node.Content)-1; i += 2 {
		if m.node.Content[i].Value == "$ref" {
			return true
		}
	}
	return false
}

// hasAllOf checks if a schema has an allOf key.
func hasAllOf(m *omap) bool {
	for i := 0; i < len(m.node.Content)-1; i += 2 {
		if m.node.Content[i].Value == "allOf" {
			return true
		}
	}
	return false
}

// insertDescription adds a description node right after any existing first node pairs.
func insertDescription(m *omap, desc string) {
	// Prepend description as the first key-value pair.
	descKey := &yaml.Node{Kind: yaml.ScalarNode, Value: "description", Tag: "!!str"}
	descVal := &yaml.Node{Kind: yaml.ScalarNode, Value: desc, Tag: "!!str"}
	m.node.Content = append([]*yaml.Node{descKey, descVal}, m.node.Content...)
}

// addCodecsItemEnum finds the "items" child of an array schema and adds an enum.
func addCodecsItemEnum(schema *omap, enumValues []string) {
	for i := 0; i < len(schema.node.Content)-1; i += 2 {
		if schema.node.Content[i].Value == "items" {
			items := schema.node.Content[i+1]
			enumSeq := newSeq()
			for _, v := range enumValues {
				enumSeq.add(v)
			}
			items.Content = append(items.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Value: "enum", Tag: "!!str"},
				&enumSeq.node)
			return
		}
	}
}

func fieldSchema(f reflect.StructField) *omap {
	ft := f.Type
	isPtr := ft.Kind() == reflect.Ptr
	if isPtr {
		ft = ft.Elem()
	}
	if ft.Kind() == reflect.Struct && ft.Name() != "" {
		registerSchema(ft)
		s := schemaRef(ft.Name())
		if isPtr {
			return newMap().set("nullable", true).set("allOf", newSeq().add(s))
		}
		return s
	}
	return goTypeToSchema(ft)
}

func registerSchema(t reflect.Type) {
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	name := t.Name()
	if name == "" || schemaRegistry[name] != nil {
		return
	}
	// Placeholder to prevent infinite recursion.
	schemaRegistry[name] = newMap()
	schema := structToSchema(t)
	schemaRegistry[name] = schema
}

func parseJSONTag(f reflect.StructField) (name string, omitempty bool) {
	tag := f.Tag.Get("json")
	if tag == "" || tag == "-" {
		return f.Name, false
	}
	parts := strings.Split(tag, ",")
	name = parts[0]
	if name == "" {
		name = f.Name
	}
	for _, p := range parts[1:] {
		if p == "omitempty" {
			omitempty = true
		}
	}
	return name, omitempty
}

// ── Config vars ─────────────────────────────────────────────────────────

type configVar struct {
	Name        string `yaml:"name"`
	Default     string `yaml:"default"`
	Description string `yaml:"description"`
}

func configVars() *seq {
	vars := []configVar{
		{Name: "INSTANCE_ID", Default: "(auto-generated UUID)", Description: "Instance identifier included in all API responses and webhook events"},
		{Name: "HTTP_ADDR", Default: ":8080", Description: "REST API listen address"},
		{Name: "SIP_BIND_IP", Default: "127.0.0.1", Description: "IP used in SDP, Contact, and Via headers"},
		{Name: "SIP_LISTEN_IP", Default: "(same as SIP_BIND_IP)", Description: "UDP socket bind IP"},
		{Name: "SIP_PORT", Default: "5060", Description: "SIP listen port"},
		{Name: "SIP_HOST", Default: "voiceblender", Description: "SIP User-Agent name"},
		{Name: "ICE_SERVERS", Default: "stun:stun.l.google.com:19302", Description: "STUN/TURN URLs for WebRTC ICE, comma-separated"},
		{Name: "RECORDING_DIR", Default: "/tmp/recordings", Description: "Local directory for recording output files"},
		{Name: "LOG_LEVEL", Default: "info", Description: "Log verbosity: debug, info, warn, error"},
		{Name: "WEBHOOK_URL", Default: "", Description: "Global webhook URL for event delivery (fallback when no per-leg or per-room webhook is set)"},
		{Name: "WEBHOOK_SECRET", Default: "", Description: "HMAC-SHA256 signing secret for the global webhook"},
		{Name: "ELEVENLABS_API_KEY", Default: "", Description: "API key for ElevenLabs TTS, STT, and Agent provider"},
		{Name: "VAPI_API_KEY", Default: "", Description: "API key for VAPI Agent provider"},
		{Name: "S3_BUCKET", Default: "", Description: "S3 bucket name for recording uploads"},
		{Name: "S3_REGION", Default: "us-east-1", Description: "AWS region for S3"},
		{Name: "S3_ENDPOINT", Default: "", Description: "Custom S3-compatible endpoint (e.g. MinIO)"},
		{Name: "S3_PREFIX", Default: "", Description: "Key prefix applied to all S3 objects"},
		{Name: "TTS_CACHE_ENABLED", Default: "false", Description: "Enable disk-backed TTS audio cache; cached audio persists across restarts"},
		{Name: "TTS_CACHE_DIR", Default: "/tmp/tts_cache", Description: "Directory for cached TTS audio files (used when TTS_CACHE_ENABLED=true)"},
		{Name: "TTS_CACHE_INCLUDE_API_KEY", Default: "false", Description: "Include API key in TTS cache key; set true if different keys map to different voice clones"},
		{Name: "SIP_JITTER_BUFFER_MS", Default: "0", Description: "SIP ingress jitter buffer target delay in ms (0 = disabled passthrough). Applies to every SIP leg."},
		{Name: "SIP_JITTER_BUFFER_MAX_MS", Default: "300", Description: "Maximum depth of the SIP ingress jitter buffer in ms. Frames beyond this are dropped oldest-first to catch up after a stall."},
		{Name: "SIP_REFER_AUTO_DIAL", Default: "false", Description: "When true, accept incoming SIP REFER requests and automatically originate the transferred call. Default-deny: stays off unless the SIP edge is locked down (IP allow-lists, digest auth) because auto-dialing arbitrary Refer-To URIs is a classic toll-fraud vector. Outbound transfers initiated via the REST API are unaffected by this flag."},
	}
	s := newSeq()
	for _, v := range vars {
		s.add(newMap().set("name", v.Name).set("default", v.Default).set("description", v.Description))
	}
	return s
}

// ── Path generation ─────────────────────────────────────────────────────

func buildPaths(routes []api.RouteMeta) *omap {
	paths := newMap()

	// Group routes by path to support multiple methods on the same path.
	type pathEntry struct {
		path    string
		methods []*api.RouteMeta
	}
	pathOrder := []string{}
	pathMap := map[string]*[]*api.RouteMeta{}
	for i := range routes {
		r := &routes[i]
		if pathMap[r.Path] == nil {
			pathOrder = append(pathOrder, r.Path)
			methods := []*api.RouteMeta{}
			pathMap[r.Path] = &methods
		}
		*pathMap[r.Path] = append(*pathMap[r.Path], r)
	}

	for _, path := range pathOrder {
		methods := *pathMap[path]
		pathItem := newMap()

		// Add shared path-level parameters.
		params := extractPathParams(path)
		if len(params) > 0 {
			paramSeq := newSeq()
			for _, p := range params {
				paramSeq.add(paramRefForPath(p, path))
			}
			pathItem.set("parameters", paramSeq)
		}

		for _, r := range methods {
			op := buildOperation(r)
			pathItem.set(strings.ToLower(r.Method), op)
		}

		paths.set(path, pathItem)
	}

	return paths
}

func paramRefForPath(name, path string) *omap {
	switch name {
	case "id":
		if strings.HasPrefix(path, "/rooms") {
			return newMap().set("$ref", "#/components/parameters/RoomId")
		}
		return newMap().set("$ref", "#/components/parameters/LegId")
	case "playbackID":
		return newMap().set("$ref", "#/components/parameters/PlaybackId")
	case "legID":
		return newMap().set("name", "legID").set("in", "path").set("required", true).
			set("schema", newMap().set("type", "string")).set("description", "Leg ID")
	}
	return newMap().set("name", name).set("in", "path").set("required", true).
		set("schema", newMap().set("type", "string"))
}

func extractPathParams(path string) []string {
	var params []string
	for _, seg := range strings.Split(path, "/") {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			params = append(params, seg[1:len(seg)-1])
		}
	}
	return params
}

func buildOperation(r *api.RouteMeta) *omap {
	op := newMap()
	op.set("operationId", r.OperationID)
	op.set("summary", r.Summary)
	if r.Description != "" {
		op.set("description", foldDescription(r.Description))
	}
	if len(r.Tags) > 0 {
		tags := newSeq()
		for _, t := range r.Tags {
			tags.add(t)
		}
		op.set("tags", tags)
	}

	// Request body.
	if r.RequestType != nil {
		rt := reflect.TypeOf(r.RequestType)
		if rt.Kind() == reflect.Ptr {
			rt = rt.Elem()
		}
		registerSchema(rt)
		reqBody := newMap()
		if !r.OptionalBody {
			reqBody.set("required", true)
		}
		reqBody.set("content", newMap().set("application/json",
			newMap().set("schema", schemaRef(rt.Name()))))
		op.set("requestBody", reqBody)
	}

	// Responses.
	responses := newMap()
	codes := sortedCodes(r.Responses)
	for _, code := range codes {
		resp := r.Responses[code]
		respObj := newMap().set("description", resp.Description)
		if resp.Type != nil {
			rt := reflect.TypeOf(resp.Type)
			var schemaNode *omap
			if rt.Kind() == reflect.Slice {
				elem := rt.Elem()
				if elem.Kind() == reflect.Struct && elem.Name() != "" {
					registerSchema(elem)
					schemaNode = newMap().set("type", "array").set("items", schemaRef(elem.Name()))
				}
			} else if rt.Kind() == reflect.Struct {
				registerSchema(rt)
				schemaNode = schemaRef(rt.Name())
			}
			if schemaNode != nil {
				respObj.set("content", newMap().set("application/json",
					newMap().set("schema", schemaNode)))
			}
		} else if code >= 400 {
			// Error responses.
			respObj.set("content", newMap().set("application/json",
				newMap().set("schema", schemaRef("Error"))))
		} else if code == 200 || code == 201 {
			// Status responses for endpoints without a typed response.
			respObj.set("content", newMap().set("application/json",
				newMap().set("schema", schemaRef("StatusResponse"))))
		}
		responses.setQuotedKey(fmt.Sprintf("%d", code), respObj)
	}
	op.set("responses", responses)

	return op
}

func foldDescription(s string) *yaml.Node {
	return &yaml.Node{
		Kind:  yaml.ScalarNode,
		Value: s,
		Tag:   "!!str",
		Style: yaml.FlowStyle,
	}
}

func sortedCodes(m map[int]api.ResponseMeta) []int {
	codes := make([]int, 0, len(m))
	for c := range m {
		codes = append(codes, c)
	}
	sort.Ints(codes)
	return codes
}

// ── Webhook generation ──────────────────────────────────────────────────

type webhookEventMeta struct {
	eventType events.EventType
	summary   string
	dataType  reflect.Type
}

func allWebhookEvents() []webhookEventMeta {
	return []webhookEventMeta{
		{events.LegRinging, "SIP call ringing (inbound or outbound)", reflect.TypeOf(events.LegRingingData{})},
		{events.LegEarlyMedia, "Outbound leg received 183 Session Progress with SDP; media pipeline active", reflect.TypeOf(events.LegEarlyMediaData{})},
		{events.LegConnected, "Leg answered/connected", reflect.TypeOf(events.LegConnectedData{})},
		{events.LegDisconnected, "Leg hung up (CDR-style nested structure)", reflect.TypeOf(events.LegDisconnectedData{})},
		{events.LegJoinedRoom, "Leg added to a room", reflect.TypeOf(events.LegJoinedRoomData{})},
		{events.LegLeftRoom, "Leg removed from a room", reflect.TypeOf(events.LegLeftRoomData{})},
		{events.LegMuted, "Leg muted", reflect.TypeOf(events.LegMutedData{})},
		{events.LegUnmuted, "Leg unmuted", reflect.TypeOf(events.LegUnmutedData{})},
		{events.LegDeaf, "Leg deafened (stops receiving room audio)", reflect.TypeOf(events.LegDeafData{})},
		{events.LegUndeaf, "Leg undeafened (resumes receiving room audio)", reflect.TypeOf(events.LegUndeafData{})},
		{events.LegHold, "Leg put on hold (local or remote)", reflect.TypeOf(events.LegHoldData{})},
		{events.LegUnhold, "Leg taken off hold (local or remote)", reflect.TypeOf(events.LegUnholdData{})},
		{events.DTMFReceived, "DTMF digit received", reflect.TypeOf(events.DTMFReceivedData{})},
		{events.SpeakingStarted, "Participant started speaking", reflect.TypeOf(events.SpeakingData{})},
		{events.SpeakingStopped, "Participant stopped speaking", reflect.TypeOf(events.SpeakingData{})},
		{events.PlaybackStarted, "Playback began", reflect.TypeOf(events.PlaybackStartedData{})},
		{events.PlaybackFinished, "Playback ended", reflect.TypeOf(events.PlaybackFinishedData{})},
		{events.PlaybackError, "Playback failed", reflect.TypeOf(events.PlaybackErrorData{})},
		{events.TTSStarted, "TTS synthesis began playing", reflect.TypeOf(events.TTSStartedData{})},
		{events.TTSFinished, "TTS synthesis finished playing", reflect.TypeOf(events.TTSFinishedData{})},
		{events.TTSError, "TTS synthesis or playback failed", reflect.TypeOf(events.TTSErrorData{})},
		{events.RecordingStarted, "Recording began", reflect.TypeOf(events.RecordingStartedData{})},
		{events.RecordingFinished, "Recording ended", reflect.TypeOf(events.RecordingFinishedData{})},
		{events.RecordingPaused, "Recording paused (audio replaced with silence)", reflect.TypeOf(events.RecordingPausedData{})},
		{events.RecordingResumed, "Recording resumed from a paused state", reflect.TypeOf(events.RecordingResumedData{})},
		{events.LegTransferInitiated, "We sent a SIP REFER (transfer initiated by the operator)", reflect.TypeOf(events.LegTransferInitiatedData{})},
		{events.LegTransferRequested, "We received a SIP REFER from the peer", reflect.TypeOf(events.LegTransferRequestedData{})},
		{events.LegTransferProgress, "Transfer progress reported via NOTIFY sipfrag", reflect.TypeOf(events.LegTransferProgressData{})},
		{events.LegTransferCompleted, "Transfer reached terminal 2xx", reflect.TypeOf(events.LegTransferCompletedData{})},
		{events.LegTransferFailed, "Transfer failed (REFER rejected, sipfrag non-2xx, or local error)", reflect.TypeOf(events.LegTransferFailedData{})},
		{events.RoomCreated, "Room created", reflect.TypeOf(events.RoomCreatedData{})},
		{events.RoomDeleted, "Room deleted", reflect.TypeOf(events.RoomDeletedData{})},
		{events.STTText, "Speech-to-text transcript", reflect.TypeOf(events.STTTextData{})},
		{events.AgentConnected, "Agent connected to provider", reflect.TypeOf(events.AgentConnectedData{})},
		{events.AgentDisconnected, "Agent session ended", reflect.TypeOf(events.AgentDisconnectedData{})},
		{events.AgentUserTranscript, "User speech transcribed by agent", reflect.TypeOf(events.AgentTranscriptData{})},
		{events.AgentAgentResponse, "Agent generated a response", reflect.TypeOf(events.AgentResponseData{})},
		{events.AMDResult, "Answering machine detection completed", reflect.TypeOf(events.AMDResultData{})},
		{events.AMDBeep, "Voicemail beep tone detected after machine classification", reflect.TypeOf(events.AMDBeepData{})},
	}
}

func buildWebhookEventType() *omap {
	s := newMap().set("type", "string")
	enumSeq := newSeq()
	for _, wh := range allWebhookEvents() {
		enumSeq.add(string(wh.eventType))
	}
	s.set("enum", enumSeq)
	return s
}

func buildWebhooks() *omap {
	webhooks := newMap()
	for _, wh := range allWebhookEvents() {
		evtType := string(wh.eventType)
		// Build inline properties from the data struct (flattened, including embedded).
		inlineProps := newMap()
		flattenDataFields(wh.dataType, inlineProps, evtType)

		allOfSeq := newSeq()
		allOfSeq.add(schemaRef("WebhookEvent"))
		if len(inlineProps.node.Content) > 0 {
			allOfSeq.add(newMap().set("properties", inlineProps))
		}

		webhooks.set(evtType, newMap().set("post",
			newMap().set("summary", wh.summary).
				set("requestBody", newMap().set("content",
					newMap().set("application/json",
						newMap().set("schema", newMap().set("allOf", allOfSeq)))))))
	}
	return webhooks
}

// flattenDataFields extracts all JSON-tagged fields from a struct type,
// recursing into embedded structs, and adds them to the omap as simple
// type definitions (for the inline webhook schema). evtType is the event
// type string (e.g. "leg.ringing") used to look up field descriptions.
func flattenDataFields(t reflect.Type, props *omap, evtType string) {
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		if f.Anonymous {
			flattenDataFields(f.Type, props, evtType)
			continue
		}
		jsonName, _ := parseJSONTag(f)
		if jsonName == "-" {
			continue
		}
		ft := f.Type
		if ft.Kind() == reflect.Ptr {
			ft = ft.Elem()
		}
		switch ft.Kind() {
		case reflect.Struct:
			// For nested structs in webhook data (like cdr, quality),
			// generate inline schema.
			schema := webhookNestedSchema(ft, f.Type.Kind() == reflect.Ptr, jsonName)
			// Add top-level description for special fields.
			if jsonName == "quality" {
				insertDescription(schema, api.QualityDescription)
			}
			props.set(jsonName, schema)
		default:
			schema := goTypeToSchema(ft)
			// Apply webhook field description if available.
			descKey := evtType + "." + jsonName
			if desc, ok := webhookFieldDescs[descKey]; ok {
				schema.set("description", desc)
			}
			props.set(jsonName, schema)
		}
	}
}

// webhookNestedSchema builds an inline object schema for nested structs
// in webhook event data (e.g. cdr, quality).
// parentFieldName is the JSON name of the parent field (e.g. "cdr").
func webhookNestedSchema(t reflect.Type, nullable bool, parentFieldName string) *omap {
	schema := newMap().set("type", "object")
	if nullable {
		schema.set("nullable", true)
	}
	requiredSeq := newSeq()
	hasRequired := false
	props := newMap()
	typeName := t.Name()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		jsonName, omit := parseJSONTag(f)
		if jsonName == "-" {
			continue
		}
		fieldSchema := goTypeToSchema(f.Type)
		// Apply nested field description if available.
		descKey := typeName + "." + jsonName
		if desc, ok := webhookNestedFieldDescs[descKey]; ok {
			fieldSchema.set("description", desc)
		}
		// Apply disconnect reason enum.
		if typeName == "CallCDR" && jsonName == "reason" {
			enumSeq := newSeq()
			for _, v := range api.DisconnectReasonEnum {
				enumSeq.add(v)
			}
			fieldSchema.set("enum", enumSeq)
		}
		props.set(jsonName, fieldSchema)
		if !omit {
			requiredSeq.add(jsonName)
			hasRequired = true
		}
	}
	schema.set("properties", props)
	if hasRequired {
		schema.set("required", requiredSeq)
	}
	return schema
}

// ── Main ────────────────────────────────────────────────────────────────

func main() {
	// Load enrichment data from api package.
	schemaEnrichments = api.SchemaEnrichments()
	webhookFieldDescs = api.WebhookFieldDescriptions()
	webhookNestedFieldDescs = api.WebhookNestedFieldDescriptions()

	routes := api.RoutesMetadata()

	// Pre-register core schemas that aren't derived from route types.
	schemaRegistry["Error"] = newMap().set("type", "object").
		set("properties", newMap().
			set("instance_id", newMap().set("type", "string").set("description", "Instance identifier")).
			set("error", newMap().set("type", "string").set("description", "Error message"))).
		set("required", newSeq().add("error"))

	schemaRegistry["StatusResponse"] = newMap().set("type", "object").
		set("properties", newMap().
			set("instance_id", newMap().set("type", "string").set("description", "Instance identifier")).
			set("status", newMap().set("type", "string"))).
		set("required", newSeq().add("status"))

	schemaRegistry["WebhookEvent"] = newMap().set("type", "object").
		set("description", "Event envelope delivered via HTTP POST to registered webhook URLs. "+
			"Event-specific fields are flattened into the top-level object (no \"data\" wrapper). "+
			"Includes X-Signature-256 header when a secret is configured.").
		set("properties", newMap().
			set("type", schemaRef("WebhookEventType")).
			set("timestamp", newMap().set("type", "string").set("format", "date-time")).
			set("instance_id", newMap().set("type", "string").set("description", "Instance identifier"))).
		set("required", newSeq().add("type").add("timestamp"))

	schemaRegistry["WebhookEventType"] = buildWebhookEventType()

	schemaRegistry["ICECandidateInit"] = newMap().set("type", "object").
		set("properties", newMap().
			set("candidate", newMap().set("type", "string").set("description", "ICE candidate string")).
			set("sdpMid", newMap().set("type", "string").set("description", "Media stream identification tag")).
			set("sdpMLineIndex", newMap().set("type", "integer").set("description", "Index of the media description"))).
		set("required", newSeq().add("candidate"))

	// Build paths — this triggers schema registration for request/response types.
	pathsNode := buildPaths(routes)

	// Add observability paths (static, not driven by route metadata).
	addObservabilityPaths(pathsNode)

	// Build the top-level document.
	doc := newMap()
	doc.set("openapi", "3.1.0")

	// Info.
	info := newMap()
	info.set("title", "VoiceBlender API")
	info.set("description", "VoiceBlender bridges SIP and WebRTC voice calls with multi-party audio "+
		"mixing, real-time speech-to-text, text-to-speech, AI agent integration, "+
		"recording, and webhook-based event delivery.\n")
	info.set("x-config-vars", configVars())
	info.set("version", "1.0.0")
	info.set("license", newMap().set("name", "MIT"))
	doc.set("info", info)

	// Servers.
	servers := newSeq().add(newMap().set("url", "http://localhost:8080/v1").set("description", "Local development server"))
	doc.set("servers", servers)

	// Tags.
	tags := newSeq().
		add(newMap().set("name", "Legs").set("description", "Voice call legs (SIP or WebRTC)")).
		add(newMap().set("name", "Rooms").set("description", "Multi-party audio conference rooms")).
		add(newMap().set("name", "WebRTC").set("description", "WebRTC peer connection establishment")).
		add(newMap().set("name", "Observability").set("description", "Metrics and health endpoints"))
	doc.set("tags", tags)

	// Paths.
	doc.set("paths", pathsNode)

	// Components.
	components := newMap()
	components.set("parameters", buildParameters())
	components.set("responses", buildResponses())
	components.set("schemas", buildSchemas())
	doc.set("components", components)

	// Webhooks.
	doc.set("x-webhooks", buildWebhooks())

	// Write output.
	out, err := yaml.Marshal(&doc.node)
	if err != nil {
		fmt.Fprintf(os.Stderr, "yaml marshal: %v\n", err)
		os.Exit(1)
	}

	outPath := "openapi.yaml"
	// When run via go generate from internal/api/, write to repo root.
	if _, err := os.Stat("../../openapi.yaml"); err == nil {
		outPath = "../../openapi.yaml"
	} else if _, err := os.Stat("openapi.yaml"); err != nil {
		// Try repo root relative to where we are.
		outPath = "../../openapi.yaml"
	}

	if err := os.WriteFile(outPath, out, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "write %s: %v\n", outPath, err)
		os.Exit(1)
	}
	fmt.Printf("Generated %s (%d bytes)\n", outPath, len(out))
}

func buildParameters() *omap {
	return newMap().
		set("LegId", newMap().set("name", "id").set("in", "path").set("required", true).
			set("schema", newMap().set("type", "string")).set("description", "Leg ID")).
		set("RoomId", newMap().set("name", "id").set("in", "path").set("required", true).
			set("schema", newMap().set("type", "string")).set("description", "Room ID")).
		set("PlaybackId", newMap().set("name", "playbackID").set("in", "path").set("required", true).
			set("schema", newMap().set("type", "string")).set("description", "Playback ID"))
}

func buildResponses() *omap {
	return newMap().
		set("LegNotFound", newMap().set("description", "Leg not found").
			set("content", newMap().set("application/json",
				newMap().set("schema", schemaRef("Error"))))).
		set("RoomNotFound", newMap().set("description", "Room not found").
			set("content", newMap().set("application/json",
				newMap().set("schema", schemaRef("Error")))))
}

func buildSchemas() *omap {
	schemas := newMap()

	// Emit schemas in a deterministic order.
	// Core resources first, then requests, then responses, then webhook types.
	order := []string{
		"LegView", "RoomView", "Error", "StatusResponse",
	}

	// Collect request types.
	requestTypes := []string{}
	responseTypes := []string{}
	webhookTypes := []string{"WebhookEvent", "WebhookEventType", "ICECandidateInit"}

	for name := range schemaRegistry {
		found := false
		for _, o := range order {
			if name == o {
				found = true
				break
			}
		}
		for _, o := range webhookTypes {
			if name == o {
				found = true
				break
			}
		}
		if found {
			continue
		}
		if strings.HasSuffix(name, "Request") {
			requestTypes = append(requestTypes, name)
		} else {
			responseTypes = append(responseTypes, name)
		}
	}
	sort.Strings(requestTypes)
	sort.Strings(responseTypes)

	order = append(order, requestTypes...)
	order = append(order, responseTypes...)
	order = append(order, webhookTypes...)

	for _, name := range order {
		schema, ok := schemaRegistry[name]
		if !ok {
			continue
		}

		// Rename schema names to match existing OpenAPI spec naming conventions.
		displayName := schemaDisplayName(name)
		schemas.set(displayName, schema)
	}

	return schemas
}

// schemaDisplayName maps Go type names to OpenAPI schema names matching
// the existing spec naming conventions.
func schemaDisplayName(goName string) string {
	nameMap := map[string]string{
		"LegView":                "Leg",
		"RoomView":               "Room",
		"CreateLegRequest":       "CreateLegRequest",
		"SIPAuth":                "SIPAuth",
		"CreateRoomRequest":      "RoomCreateRequest",
		"AddLegRequest":          "AddLegRequest",
		"PlaybackRequest":        "PlaybackRequest",
		"VolumeRequest":          "VolumeRequest",
		"DTMFRequest":            "DTMFRequest",
		"TTSRequest":             "TTSRequest",
		"STTRequest":             "STTRequest",
		"RecordRequest":          "RecordingRequest",
		"ElevenLabsAgentRequest": "ElevenLabsAgentRequest",
		"VAPIAgentRequest":       "VAPIAgentRequest",
		"PipecatAgentRequest":    "PipecatAgentRequest",
		"DeepgramAgentRequest":   "DeepgramAgentRequest",
		"AgentMessageRequest":    "AgentMessageRequest",
		"WebRTCOfferRequest":     "WebRTCOfferRequest",
	}
	if display, ok := nameMap[goName]; ok {
		return display
	}
	return goName
}

func addObservabilityPaths(paths *omap) {
	// /metrics (outside /v1 prefix)
	paths.set("/metrics", newMap().set("get",
		newMap().set("operationId", "getMetrics").
			set("summary", "Prometheus metrics").
			set("description", "Returns Prometheus-format metrics (text/plain exposition format). "+
				"Includes VoiceBlender-specific metrics and standard Go runtime metrics.\n").
			set("tags", newSeq().add("Observability")).
			set("responses", newMap().setQuotedKey("200",
				newMap().set("description", "Prometheus text exposition format").
					set("content", newMap().set("text/plain",
						newMap().set("schema", newMap().set("type", "string"))))))))

	// /debug/pprof/ endpoints
	paths.set("/debug/pprof/", newMap().set("get",
		newMap().set("operationId", "pprofIndex").
			set("summary", "pprof index").
			set("description", "Index of available Go runtime profiles. Only available when built with `-tags pprof` (e.g. `go build -tags pprof ./...`).\n").
			set("tags", newSeq().add("Observability")).
			set("responses", newMap().setQuotedKey("200",
				newMap().set("description", "HTML index page listing available profiles").
					set("content", newMap().set("text/html",
						newMap().set("schema", newMap().set("type", "string"))))))))

	paths.set("/debug/pprof/profile", newMap().set("get",
		newMap().set("operationId", "pprofCPU").
			set("summary", "CPU profile").
			set("description", "30-second CPU profile (duration configurable via ?seconds= query param). Only available when built with `-tags pprof`.\n").
			set("tags", newSeq().add("Observability")).
			set("parameters", newSeq().add(
				newMap().set("name", "seconds").set("in", "query").
					set("schema", newMap().set("type", "integer").set("default", 30)).
					set("description", "Profile duration in seconds"))).
			set("responses", newMap().setQuotedKey("200",
				newMap().set("description", "pprof binary profile").
					set("content", newMap().set("application/octet-stream",
						newMap().set("schema", newMap().set("type", "string").set("format", "binary"))))))))

	paths.set("/debug/pprof/heap", newMap().set("get",
		newMap().set("operationId", "pprofHeap").
			set("summary", "Heap memory profile").
			set("description", "Heap memory snapshot. Only available when built with `-tags pprof`.\n").
			set("tags", newSeq().add("Observability")).
			set("responses", newMap().setQuotedKey("200",
				newMap().set("description", "pprof binary profile").
					set("content", newMap().set("application/octet-stream",
						newMap().set("schema", newMap().set("type", "string").set("format", "binary"))))))))

	paths.set("/debug/pprof/goroutine", newMap().set("get",
		newMap().set("operationId", "pprofGoroutine").
			set("summary", "Goroutine stack traces").
			set("description", "All goroutine stack traces. Only available when built with `-tags pprof`.\n").
			set("tags", newSeq().add("Observability")).
			set("responses", newMap().setQuotedKey("200",
				newMap().set("description", "pprof binary profile").
					set("content", newMap().set("application/octet-stream",
						newMap().set("schema", newMap().set("type", "string").set("format", "binary"))))))))
}
