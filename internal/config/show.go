package config

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"reflect"

	"gopkg.in/yaml.v3"
)

// Redacted replaces every non-empty Secret in output meant for humans or
// logs.
const Redacted = "[REDACTED]"

// String renders a Secret redacted, so that %s and %v formatting, slog
// attributes and error messages never carry the value (SE-010). Code that
// needs the value converts explicitly with string(s).
func (s Secret) String() string {
	if s == "" {
		return ""
	}
	return Redacted
}

// GoString renders a Secret redacted for %#v as well.
func (s Secret) GoString() string { return fmt.Sprintf("config.Secret(%q)", s.String()) }

// LogValue implements slog.LogValuer so a Secret logged as an attribute is
// redacted (SE-010).
func (s Secret) LogValue() slog.Value { return slog.StringValue(s.String()) }

var secretType = reflect.TypeFor[Secret]()

// Redacted returns a deep copy of c with every non-empty Secret replaced by
// the Redacted marker. The copy is found by walking the schema with
// reflection, so a Secret field added to types.go later is redacted without
// this function changing.
func (c *Config) Redacted() *Config {
	clone, err := cloneConfig(c)
	if err != nil {
		// The schema round-trips through YAML by construction; a failure here
		// is a programming error in the schema, not an operator input.
		panic(fmt.Sprintf("config: clone: %v", err))
	}
	redactValue(reflect.ValueOf(clone).Elem())
	return clone
}

// cloneConfig deep-copies c by a YAML round trip, which every field of the
// schema supports.
func cloneConfig(c *Config) (*Config, error) {
	data, err := yaml.Marshal(c)
	if err != nil {
		return nil, err
	}
	var out Config
	if err := yaml.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// redactValue replaces every settable Secret reachable from v.
func redactValue(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			redactValue(v.Elem())
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			redactValue(v.Field(i))
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			redactValue(v.Index(i))
		}
	case reflect.Map:
		if v.Type().Elem() != secretType {
			return
		}
		for _, k := range v.MapKeys() {
			if s := v.MapIndex(k); s.Len() > 0 {
				v.SetMapIndex(k, reflect.ValueOf(Secret(Redacted)))
			}
		}
	case reflect.String:
		if v.Type() == secretType && v.CanSet() && v.Len() > 0 {
			v.SetString(Redacted)
		}
	default:
	}
}

//= docs/requirements/07-configuration.md#file-and-precedence
//# The Runner SHALL provide a `config show` command that prints the
//# effective configuration with every secret value redacted.

// Show writes the effective configuration, that is c after environment
// overrides, Inventory resolution and defaults, as YAML with every secret
// redacted. It is the body of `flintlock-runner config show`. The Inventory
// is printed inline; when it came from a file, `inventory.file` is kept as
// provenance, so the output documents the running state rather than being
// a drop-in replacement for the source file.
func Show(w io.Writer, c *Config) error {
	var buf bytes.Buffer
	buf.WriteString("# flintlock-runner effective configuration; secrets are redacted.\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(c.Redacted()); err != nil {
		return fmt.Errorf("config: show: %w", err)
	}
	if err := enc.Close(); err != nil {
		return fmt.Errorf("config: show: %w", err)
	}
	if _, err := w.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("config: show: %w", err)
	}
	return nil
}
