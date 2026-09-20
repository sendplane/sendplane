package render

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/sendplane/sendplane/store"
)

// ErrInvalidBundle is returned by ImportYAML for a bundle that cannot be used:
// malformed YAML, or a translation value that is not valid Liquid.
var ErrInvalidBundle = errors.New("render: invalid i18n bundle")

// yamlIndent keeps the exported file readable and matches architecture 6.2.
const yamlIndent = 2

// ExportYAML renders a bundle in the interchange format of architecture 6.2:
//
//	default_locale: en
//	locales:
//	  en:
//	    welcome.title: "Welcome, {{ recipient.name }}"
//	  ko-KR: {}
//
// Locales and keys are sorted, so the output is stable and diffable.
func ExportYAML(b store.I18nBundle) ([]byte, error) {
	root := &yaml.Node{Kind: yaml.MappingNode}
	root.Content = append(root.Content, scalar("default_locale"), scalar(b.DefaultLocale))

	locales := &yaml.Node{Kind: yaml.MappingNode}
	for _, loc := range sortedKeys(b.Locales) {
		kv := b.Locales[loc]
		entry := &yaml.Node{Kind: yaml.MappingNode}
		if len(kv) == 0 {
			entry.Style = yaml.FlowStyle
		}
		for _, k := range sortedKeys(kv) {
			value := scalar(kv[k])
			value.Style = yaml.DoubleQuotedStyle
			entry.Content = append(entry.Content, scalar(k), value)
		}
		locales.Content = append(locales.Content, scalar(loc), entry)
	}
	root.Content = append(root.Content, scalar("locales"), locales)

	var b2 strings.Builder
	enc := yaml.NewEncoder(&b2)
	enc.SetIndent(yamlIndent)
	if err := enc.Encode(root); err != nil {
		return nil, fmt.Errorf("render: export i18n: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("render: export i18n: %w", err)
	}
	return []byte(b2.String()), nil
}

func scalar(s string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

type yamlBundle struct {
	DefaultLocale string                       `yaml:"default_locale"`
	Locales       map[string]map[string]string `yaml:"locales"`
}

// ImportYAML parses the format ExportYAML writes. Every translation value is
// parsed as Liquid, because a translator can break a tag and the failure would
// otherwise only show up at send time (ADR-0004).
//
// Warnings cover survivable oddities: an empty locale map (which falls back to
// its language) and a default_locale with no translations.
func ImportYAML(data []byte) (store.I18nBundle, []Warning, error) {
	var y yamlBundle
	if err := yaml.Unmarshal(data, &y); err != nil {
		return store.I18nBundle{}, nil, fmt.Errorf("%w: %w", ErrInvalidBundle, err)
	}

	bundle := store.I18nBundle{
		DefaultLocale: strings.TrimSpace(y.DefaultLocale),
		Locales:       map[string]map[string]string{},
	}

	eng := NewEngine()
	var warnings []Warning
	var bad []string
	for _, loc := range sortedKeys(y.Locales) {
		kv := y.Locales[loc]
		out := make(map[string]string, len(kv))
		for _, k := range sortedKeys(kv) {
			v := kv[k]
			if err := eng.Validate(v); err != nil {
				bad = append(bad, fmt.Sprintf("%s/%s: %v", loc, k, err))
				continue
			}
			out[k] = v
		}
		bundle.Locales[loc] = out
		if len(out) == 0 {
			warnings = append(warnings, Warning{Code: WarnMissingTranslation, Locale: loc, Detail: "no translations; falls back to the language or the default locale"})
		}
	}
	if len(bad) > 0 {
		return store.I18nBundle{}, warnings, fmt.Errorf("%w: %s", ErrInvalidBundle, strings.Join(bad, "; "))
	}
	if bundle.DefaultLocale == "" {
		return store.I18nBundle{}, warnings, fmt.Errorf("%w: default_locale is required", ErrInvalidBundle)
	}
	if len(bundle.Locales[bundle.DefaultLocale]) == 0 {
		warnings = append(warnings, Warning{Code: WarnMissingTranslation, Locale: bundle.DefaultLocale, Detail: "default locale has no translations"})
	}
	return bundle, warnings, nil
}
