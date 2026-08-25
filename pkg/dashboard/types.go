// Package dashboard translates the query expressions in a Grafana dashboard,
// leaving everything else exactly as it found it.
//
// A dashboard is a large JSON document of which PolyQL understands a sliver:
// the expressions inside panel targets. Everything else — layout, datasource
// references, field configuration, thresholds, annotations, templating,
// whatever a future Grafana adds — is carried through untouched. That is the
// whole design constraint here, because a migration tool that reformats a
// dashboard while translating it produces a diff nobody can review.
package dashboard

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// rawObject is a JSON object that remembers its keys and their order.
//
// Marshaling a map would sort the keys and rewrite every line of the document,
// burying the handful of expressions that actually changed. Keeping the order
// means a translated dashboard diffs against its original in just the places
// the translation touched.
type rawObject struct {
	keys   []string
	values map[string]json.RawMessage
}

func (o *rawObject) UnmarshalJSON(data []byte) error {
	o.keys = nil
	o.values = map[string]json.RawMessage{}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()

	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return fmt.Errorf("expected a JSON object, got %v", token)
	}

	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return err
		}
		key, ok := keyToken.(string)
		if !ok {
			return fmt.Errorf("expected an object key, got %v", keyToken)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return fmt.Errorf("reading %q: %w", key, err)
		}
		// A duplicate key keeps its first position and its last value, which is
		// what encoding/json does.
		if _, seen := o.values[key]; !seen {
			o.keys = append(o.keys, key)
		}
		o.values[key] = value
	}

	if _, err := decoder.Token(); err != nil {
		return err
	}
	return nil
}

func (o rawObject) MarshalJSON() ([]byte, error) {
	if o.values == nil {
		return []byte("{}"), nil
	}
	var b bytes.Buffer
	b.WriteByte('{')
	for i, key := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		encoded, err := marshalRaw(key)
		if err != nil {
			return nil, err
		}
		b.Write(encoded)
		b.WriteByte(':')
		b.Write(o.values[key])
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// marshalRaw encodes a value without HTML escaping.
//
// encoding/json rewrites &, < and > as escape sequences by default, and does so
// even to the output of a custom marshaller. A dashboard is full of ampersands
// in panel titles and annotation names; escaping them would rewrite lines the
// translation never touched and bury the expressions that actually changed.
func marshalRaw(value any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	// Encode appends a newline that a nested value must not carry.
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// set stores a value, appending the key when it is new so that a field the
// input did not carry lands at the end rather than in the middle.
func (o *rawObject) set(key string, value any) error {
	encoded, err := marshalRaw(value)
	if err != nil {
		return fmt.Errorf("encoding %q: %w", key, err)
	}
	if o.values == nil {
		o.values = map[string]json.RawMessage{}
	}
	if _, seen := o.values[key]; !seen {
		o.keys = append(o.keys, key)
	}
	o.values[key] = encoded
	return nil
}

// has reports whether the object carried a key, which decides whether a field
// is written back out at all.
func (o rawObject) has(key string) bool {
	_, ok := o.values[key]
	return ok
}

// Dashboard is a Grafana dashboard: the parts PolyQL reads, plus everything
// else exactly as it arrived.
type Dashboard struct {
	Title  string  `json:"title"`
	Panels []Panel `json:"panels"`

	// remaining holds the whole document, so marshaling can rebuild it with
	// only the translated fields replaced.
	remaining rawObject
}

func (d *Dashboard) UnmarshalJSON(data []byte) error {
	if err := d.remaining.UnmarshalJSON(data); err != nil {
		return fmt.Errorf("dashboard: %w", err)
	}
	if raw, ok := d.remaining.values["title"]; ok {
		if err := json.Unmarshal(raw, &d.Title); err != nil {
			return fmt.Errorf("dashboard: title: %w", err)
		}
	}
	if raw, ok := d.remaining.values["panels"]; ok {
		if err := json.Unmarshal(raw, &d.Panels); err != nil {
			return fmt.Errorf("dashboard: panels: %w", err)
		}
	}
	return nil
}

func (d Dashboard) MarshalJSON() ([]byte, error) {
	out := d.remaining
	// Only write back what the input had. Adding "panels" to a dashboard that
	// carried none would change the document rather than preserve it.
	if out.has("title") {
		if err := out.set("title", d.Title); err != nil {
			return nil, err
		}
	}
	if out.has("panels") {
		if err := out.set("panels", d.Panels); err != nil {
			return nil, err
		}
	}
	return out.MarshalJSON()
}

// Panel is one panel. A row panel carries the panels inside it, so the type is
// recursive.
type Panel struct {
	ID      int      `json:"id"`
	Title   string   `json:"title"`
	Type    string   `json:"type"`
	Targets []Target `json:"targets"`
	Panels  []Panel  `json:"panels,omitempty"`

	remaining rawObject
}

func (p *Panel) UnmarshalJSON(data []byte) error {
	if err := p.remaining.UnmarshalJSON(data); err != nil {
		return fmt.Errorf("panel: %w", err)
	}
	for key, target := range map[string]any{
		"id":      &p.ID,
		"title":   &p.Title,
		"type":    &p.Type,
		"targets": &p.Targets,
		"panels":  &p.Panels,
	} {
		raw, ok := p.remaining.values[key]
		if !ok {
			continue
		}
		if err := json.Unmarshal(raw, target); err != nil {
			return fmt.Errorf("panel: %s: %w", key, err)
		}
	}
	return nil
}

func (p Panel) MarshalJSON() ([]byte, error) {
	out := p.remaining
	fields := []struct {
		key   string
		value any
	}{
		{"id", p.ID},
		{"title", p.Title},
		{"type", p.Type},
		{"targets", p.Targets},
		{"panels", p.Panels},
	}
	for _, field := range fields {
		if !out.has(field.key) {
			continue
		}
		if err := out.set(field.key, field.value); err != nil {
			return nil, err
		}
	}
	return out.MarshalJSON()
}

// Target is one query within a panel.
type Target struct {
	Expr       string          `json:"expr"`
	RefID      string          `json:"refId"`
	Datasource json.RawMessage `json:"datasource,omitempty"`

	remaining rawObject
}

func (t *Target) UnmarshalJSON(data []byte) error {
	if err := t.remaining.UnmarshalJSON(data); err != nil {
		return fmt.Errorf("target: %w", err)
	}
	if raw, ok := t.remaining.values["expr"]; ok {
		if err := json.Unmarshal(raw, &t.Expr); err != nil {
			return fmt.Errorf("target: expr: %w", err)
		}
	}
	if raw, ok := t.remaining.values["refId"]; ok {
		if err := json.Unmarshal(raw, &t.RefID); err != nil {
			return fmt.Errorf("target: refId: %w", err)
		}
	}
	if raw, ok := t.remaining.values["datasource"]; ok {
		t.Datasource = raw
	}
	return nil
}

func (t Target) MarshalJSON() ([]byte, error) {
	out := t.remaining
	if out.has("expr") {
		if err := out.set("expr", t.Expr); err != nil {
			return nil, err
		}
	}
	if out.has("refId") {
		if err := out.set("refId", t.RefID); err != nil {
			return nil, err
		}
	}
	// The datasource is carried as raw bytes and never rewritten: a translated
	// dashboard still points at whatever it pointed at, and repointing it is a
	// decision for whoever installs it.
	return out.MarshalJSON()
}
