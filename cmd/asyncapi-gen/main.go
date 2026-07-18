// Command asyncapi-gen generates asyncapi.yaml from the VSI command and
// event registries in internal/api.
//
// Run via: go generate ./internal/api/
//
// The generated spec covers the /v1/vsi WebSocket: every VSI command (and
// the lifecycle frames `connected`, `ping`, `pong`, `stop`,
// `events_dropped`, `error`) appears as a separate message, and every event
// type from internal/events appears as a server-sent message. Rich shared
// schemas (LegView, RoomView, CreateLegRequest, …) are $ref'd into
// openapi.yaml so the two specs cannot disagree about field shapes.
package main

import (
	"fmt"
	"os"
	"reflect"
	"sort"

	"github.com/VoiceBlender/voiceblender/internal/api"
	"gopkg.in/yaml.v3"
)

// ── YAML ordered-map helpers (duplicated from cmd/openapi-gen) ──────────

type omap struct{ node yaml.Node }

func newMap() *omap {
	return &omap{node: yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}}
}

func (m *omap) set(key string, val interface{}) *omap {
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Value: key, Tag: "!!str"}
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
	default:
		b, _ := yaml.Marshal(v)
		valNode = &yaml.Node{}
		_ = yaml.Unmarshal(b, valNode)
		if valNode.Kind == yaml.DocumentNode && len(valNode.Content) > 0 {
			valNode = valNode.Content[0]
		}
	}
	m.node.Content = append(m.node.Content, keyNode, valNode)
	return m
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
	case int:
		s.node.Content = append(s.node.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: fmt.Sprintf("%d", v), Tag: "!!int"})
	}
	return s
}

// ── Go reflection → JSON Schema (minimal, no enrichments) ───────────────
//
// Rich shared types (LegView, CreateLegRequest, …) get a $ref into
// openapi.yaml#/components/schemas/<Name> so we don't reimplement all the
// enrichment metadata that openapi-gen carries.

// sharedSchemas is the set of Go type names that already have a curated
// JSON Schema in openapi.yaml. asyncapi-gen emits a $ref instead of a fresh
// inline schema for these.
var sharedSchemas = map[string]bool{
	"LegView":           true,
	"RoomView":          true,
	"CreateLegRequest":  true,
	"CreateRoomRequest": true,
}

// localSchemas accumulates inline VSI-only payload shapes (idPayload,
// dtmfPayload, rttPayload, …) that don't appear in openapi.yaml.
var localSchemas = map[string]*omap{}

func sharedRef(name string) *omap {
	return newMap().set("$ref", "openapi.yaml#/components/schemas/"+name)
}

func localRef(name string) *omap {
	return newMap().set("$ref", "#/components/schemas/"+name)
}

// goTypeToSchema converts a Go type to a JSON Schema fragment. Named structs
// are registered (locally or as a shared $ref); slice/map/scalar are inlined.
func goTypeToSchema(t reflect.Type) *omap {
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String:
		return newMap().set("type", "string")
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return newMap().set("type", "integer")
	case reflect.Float32, reflect.Float64:
		return newMap().set("type", "number")
	case reflect.Bool:
		return newMap().set("type", "boolean")
	case reflect.Slice:
		elem := t.Elem()
		if elem.Kind() == reflect.Struct && elem.Name() != "" {
			return newMap().set("type", "array").set("items", structRefOrInline(elem))
		}
		return newMap().set("type", "array").set("items", goTypeToSchema(elem))
	case reflect.Map:
		if t.Key().Kind() == reflect.String {
			return newMap().set("type", "object").set("additionalProperties", goTypeToSchema(t.Elem()))
		}
		return newMap().set("type", "object")
	case reflect.Struct:
		if t.Name() == "" {
			return inlineStructToSchema(t)
		}
		return structRefOrInline(t)
	case reflect.Interface:
		return newMap()
	}
	return newMap().set("type", "string")
}

func structRefOrInline(t reflect.Type) *omap {
	name := t.Name()
	if sharedSchemas[name] {
		return sharedRef(name)
	}
	if _, seen := localSchemas[name]; !seen {
		localSchemas[name] = nil // mark as in-progress to break recursion
		localSchemas[name] = inlineStructToSchema(t)
	}
	return localRef(name)
}

func inlineStructToSchema(t reflect.Type) *omap {
	props := newMap()
	required := newSeq()
	hasRequired := false

	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
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
				name, omit := parseJSONTag(ef)
				if name == "-" {
					continue
				}
				props.set(name, fieldSchema(ef))
				if !omit {
					required.add(name)
					hasRequired = true
				}
			}
			continue
		}
		name, omit := parseJSONTag(f)
		if name == "-" {
			continue
		}
		props.set(name, fieldSchema(f))
		if !omit {
			required.add(name)
			hasRequired = true
		}
	}

	out := newMap().set("type", "object").set("properties", props)
	if hasRequired {
		out.set("required", required)
	}
	return out
}

func fieldSchema(f reflect.StructField) *omap {
	t := f.Type
	if t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	return goTypeToSchema(t)
}

func parseJSONTag(f reflect.StructField) (name string, omitempty bool) {
	tag := f.Tag.Get("json")
	if tag == "" {
		return f.Name, false
	}
	for i, part := range splitComma(tag) {
		if i == 0 {
			name = part
			if name == "" {
				name = f.Name
			}
		}
		if part == "omitempty" {
			omitempty = true
		}
	}
	return
}

func splitComma(s string) []string {
	out := []string{}
	cur := ""
	for _, c := range s {
		if c == ',' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(c)
	}
	out = append(out, cur)
	return out
}

// ── AsyncAPI document construction ──────────────────────────────────────

const (
	asyncAPIVersion = "3.0.0"
	channelAddress  = "/v1/vsi"
)

func main() {
	doc := newMap()
	doc.set("asyncapi", asyncAPIVersion)
	doc.set("info", buildInfo())
	doc.set("channels", buildChannels())
	doc.set("operations", buildOperations())
	doc.set("components", buildComponents())

	out, err := yaml.Marshal(&doc.node)
	if err != nil {
		fmt.Fprintln(os.Stderr, "marshal:", err)
		os.Exit(1)
	}

	header := []byte("# Generated by cmd/asyncapi-gen — DO NOT EDIT BY HAND.\n" +
		"# Run `make asyncapi` (or `go generate ./internal/api/`) to regenerate.\n")
	if err := os.WriteFile("../../asyncapi.yaml", append(header, out...), 0644); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}
	fmt.Printf("Generated ../../asyncapi.yaml (%d bytes)\n", len(out)+len(header))
}

func buildInfo() *omap {
	return newMap().
		set("title", "VoiceBlender Streaming Interface (VSI)").
		set("version", "1.0.0").
		set("description",
			"WebSocket protocol for real-time event streaming and command dispatch on /v1/vsi. "+
				"All frames are JSON text. The server sends a `connected` frame on connect, then "+
				"event frames whose shape is identical to webhook payloads (see openapi.yaml "+
				"x-webhooks). Clients may issue commands at any time; each command receives a "+
				"matching `<command>.result` or `error` frame echoing the request_id.\n\n"+
				"This file is generated from internal/api/vsi_meta.go — every VSI command and "+
				"every event in internal/events MUST be present in the registries there or this "+
				"spec is incomplete.")
}

// channelMessageRefs returns the ordered list of $ref entries that go under
// channels.vsi.messages — one per concrete message defined in components.
func channelMessageRefs() *omap {
	out := newMap()
	for _, c := range api.VSICommandsMetadata() {
		out.set(c.Name, newMap().set("$ref", "#/components/messages/"+c.Name))
		out.set(c.Name+".result", newMap().set("$ref", "#/components/messages/"+resultMsgName(c.Name)))
	}
	for _, ev := range api.EventsMetadata() {
		out.set(string(ev.Type), newMap().set("$ref", "#/components/messages/"+eventMsgName(string(ev.Type))))
	}
	for _, lf := range api.VSILifecycleFramesMetadata() {
		out.set(lf.Name, newMap().set("$ref", "#/components/messages/"+lifecycleMsgName(lf.Name)))
	}
	return out
}

func buildChannels() *omap {
	return newMap().set("vsi", newMap().
		set("address", channelAddress).
		set("title", "VSI WebSocket").
		set("description", "Bidirectional WebSocket. Server is /v1/vsi.").
		set("messages", channelMessageRefs()))
}

func buildOperations() *omap {
	ops := newMap()

	// One receive-from-client operation per command.
	for _, c := range api.VSICommandsMetadata() {
		op := newMap().
			set("action", "receive").
			set("channel", newMap().set("$ref", "#/channels/vsi")).
			set("summary", c.Summary)
		if c.Description != "" {
			op.set("description", c.Description)
		}
		op.set("messages", newSeq().add(newMap().set("$ref",
			"#/channels/vsi/messages/"+c.Name)))
		// reply (success or error)
		replyMsgs := newSeq()
		replyMsgs.add(newMap().set("$ref", "#/channels/vsi/messages/"+c.Name+".result"))
		replyMsgs.add(newMap().set("$ref", "#/channels/vsi/messages/error"))
		op.set("reply", newMap().
			set("channel", newMap().set("$ref", "#/channels/vsi")).
			set("messages", replyMsgs))
		ops.set("recv_"+c.Name, op)
	}

	// One send-to-client operation per event.
	for _, ev := range api.EventsMetadata() {
		key := string(ev.Type)
		op := newMap().
			set("action", "send").
			set("channel", newMap().set("$ref", "#/channels/vsi")).
			set("summary", ev.Summary).
			set("messages", newSeq().add(newMap().set("$ref",
				"#/channels/vsi/messages/"+key)))
		ops.set("send_"+sanitizeOpKey(key), op)
	}

	// Lifecycle frames as their own operations.
	for _, lf := range api.VSILifecycleFramesMetadata() {
		op := newMap().
			set("action", lf.Direction).
			set("channel", newMap().set("$ref", "#/channels/vsi")).
			set("summary", lf.Description).
			set("messages", newSeq().add(newMap().set("$ref",
				"#/channels/vsi/messages/"+lf.Name)))
		ops.set(lf.Direction+"_"+lf.Name, op)
	}

	return ops
}

func buildComponents() *omap {
	comps := newMap()
	comps.set("messages", buildMessages())
	comps.set("schemas", buildSchemas())
	return comps
}

func buildMessages() *omap {
	msgs := newMap()

	// Commands: one inbound + one .result outbound per command.
	for _, c := range api.VSICommandsMetadata() {
		msgs.set(c.Name, commandRequestMessage(c))
		msgs.set(resultMsgName(c.Name), commandResultMessage(c))
	}

	// Events.
	for _, ev := range api.EventsMetadata() {
		msgs.set(eventMsgName(string(ev.Type)), eventMessage(ev))
	}

	// Lifecycle frames.
	for _, lf := range api.VSILifecycleFramesMetadata() {
		msgs.set(lifecycleMsgName(lf.Name), lifecycleMessage(lf))
	}

	return msgs
}

func commandRequestMessage(c api.VSICommandMeta) *omap {
	payload := newMap().
		set("type", "object").
		set("properties", newMap().
			set("type", newMap().set("const", c.Name)).
			set("request_id", newMap().set("type", "string").set("description", "Echoed back on the matching .result/error frame; clients use it to correlate.")).
			set("payload", commandPayloadSchema(c))).
		set("required", newSeq().add("type"))
	return newMap().
		set("name", c.Name).
		set("title", c.Summary).
		set("summary", c.Summary).
		set("payload", payload)
}

func commandPayloadSchema(c api.VSICommandMeta) *omap {
	if c.PayloadType == nil {
		return newMap().set("type", "null").set("description", "No payload.")
	}
	t := reflect.TypeOf(c.PayloadType)
	return goTypeToSchema(t)
}

func commandResultMessage(c api.VSICommandMeta) *omap {
	props := newMap().
		set("type", newMap().set("const", c.Name+".result")).
		set("request_id", newMap().set("type", "string"))
	if c.ResultType != nil {
		props.set("data", goTypeToSchema(reflect.TypeOf(c.ResultType)))
	}
	payload := newMap().
		set("type", "object").
		set("properties", props).
		set("required", newSeq().add("type"))
	return newMap().
		set("name", resultMsgName(c.Name)).
		set("title", c.Summary+" — success response").
		set("payload", payload)
}

func eventMessage(ev api.EventMeta) *omap {
	// Events on VSI carry the same JSON shape as webhooks. Reference the
	// rich schema in openapi.yaml so we don't duplicate every field.
	openapiPath := "openapi.yaml#/x-webhooks/" + string(ev.Type) + "/post/requestBody/content/application~1json/schema"
	return newMap().
		set("name", eventMsgName(string(ev.Type))).
		set("title", ev.Summary).
		set("summary", ev.Summary).
		set("description", "Event payload shape is documented in openapi.yaml under x-webhooks/"+string(ev.Type)+". Full envelope: {type, timestamp, event_id, instance_id, ...event-specific fields}.").
		set("payload", newMap().set("$ref", openapiPath))
}

func lifecycleMessage(lf api.VSILifecycleFrame) *omap {
	switch lf.Name {
	case "connected":
		return newMap().
			set("name", lifecycleMsgName(lf.Name)).
			set("title", lf.Description).
			set("payload", newMap().
				set("type", "object").
				set("properties", newMap().
					set("type", newMap().set("const", lf.Name))).
				set("required", newSeq().add("type")))
	case "ping":
		return newMap().
			set("name", lifecycleMsgName(lf.Name)).
			set("title", lf.Description).
			set("payload", newMap().
				set("type", "object").
				set("properties", newMap().
					set("type", newMap().set("const", "ping")).
					set("seq", newMap().set("type", "integer").set("description", "Per-connection monotonic counter starting at 1; resets on reconnect."))).
				set("required", newSeq().add("type").add("seq")))
	case "events_dropped":
		return newMap().
			set("name", lifecycleMsgName(lf.Name)).
			set("title", lf.Description).
			set("payload", newMap().
				set("type", "object").
				set("properties", newMap().
					set("type", newMap().set("const", "events_dropped")).
					set("count", newMap().set("type", "integer").set("description", "Number of events dropped since the last notification."))).
				set("required", newSeq().add("type").add("count")))
	case "error":
		return newMap().
			set("name", lifecycleMsgName(lf.Name)).
			set("title", lf.Description).
			set("payload", newMap().
				set("type", "object").
				set("properties", newMap().
					set("type", newMap().set("const", "error")).
					set("request_id", newMap().set("type", "string")).
					set("data", newMap().
						set("type", "object").
						set("properties", newMap().
							set("code", newMap().set("type", "integer")).
							set("message", newMap().set("type", "string"))).
						set("required", newSeq().add("code").add("message")))).
				set("required", newSeq().add("type").add("data")))
	case "pong", "stop":
		return newMap().
			set("name", lifecycleMsgName(lf.Name)).
			set("title", lf.Description).
			set("payload", newMap().
				set("type", "object").
				set("properties", newMap().
					set("type", newMap().set("const", lf.Name))).
				set("required", newSeq().add("type")))
	}
	return newMap()
}

func buildSchemas() *omap {
	out := newMap()
	// Emit local schemas in name order for stable output.
	names := make([]string, 0, len(localSchemas))
	for n := range localSchemas {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if localSchemas[n] != nil {
			out.set(n, localSchemas[n])
		}
	}
	return out
}

// ── helpers ─────────────────────────────────────────────────────────────

func resultMsgName(cmd string) string  { return cmd + "Result" }
func eventMsgName(evt string) string   { return "evt_" + sanitizeOpKey(evt) }
func lifecycleMsgName(n string) string { return "frame_" + n }

// sanitizeOpKey turns a dotted event name like "leg.ringing" into a token
// suitable for a YAML key without quoting (replaces "." with "_").
func sanitizeOpKey(s string) string {
	out := ""
	for _, c := range s {
		if c == '.' {
			out += "_"
			continue
		}
		out += string(c)
	}
	return out
}
