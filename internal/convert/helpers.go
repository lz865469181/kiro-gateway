package convert

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

func asMap(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	return m, ok
}

func asSlice(v any) ([]any, bool) {
	s, ok := v.([]any)
	if ok {
		return s, true
	}
	// Useful when callers construct blocks directly instead of unmarshalling JSON.
	switch x := v.(type) {
	case []map[string]any:
		r := make([]any, len(x))
		for i := range x {
			r[i] = x[i]
		}
		return r, true
	default:
		return nil, false
	}
}

func stringValue(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func ExtractTextContent(content any) string {
	if content == nil {
		return ""
	}
	if s, ok := content.(string); ok {
		return s
	}
	items, ok := asSlice(content)
	if !ok {
		return fmt.Sprint(content)
	}
	var b strings.Builder
	for _, item := range items {
		if s, ok := item.(string); ok {
			b.WriteString(s)
			continue
		}
		m, ok := asMap(item)
		if !ok {
			continue
		}
		typ, _ := m["type"].(string)
		if typ == "image" || typ == "image_url" || typ == "tool_reference" {
			continue
		}
		if text, ok := m["text"].(string); ok {
			b.WriteString(text)
		}
	}
	return b.String()
}

func ExtractImagesFromContent(content any) []map[string]any {
	items, ok := asSlice(content)
	if !ok {
		return nil
	}
	var images []map[string]any
	for _, item := range items {
		m, ok := asMap(item)
		if !ok {
			continue
		}
		switch m["type"] {
		case "image_url":
			u, _ := asMap(m["image_url"])
			url, _ := u["url"].(string)
			if !strings.HasPrefix(url, "data:") {
				continue
			}
			parts := strings.SplitN(url, ",", 2)
			if len(parts) != 2 || parts[1] == "" {
				continue
			}
			media := strings.TrimPrefix(strings.SplitN(parts[0], ";", 2)[0], "data:")
			images = append(images, map[string]any{"media_type": media, "data": parts[1]})
		case "image":
			source, _ := asMap(m["source"])
			if source["type"] != "base64" {
				continue
			}
			data, _ := source["data"].(string)
			if data == "" {
				continue
			}
			media, _ := source["media_type"].(string)
			if media == "" {
				media = "image/jpeg"
			}
			images = append(images, map[string]any{"media_type": media, "data": data})
		}
	}
	return images
}

func ConvertImagesToKiroFormat(images []map[string]any) []map[string]any {
	var out []map[string]any
	for _, img := range images {
		media, _ := img["media_type"].(string)
		if media == "" {
			media = "image/jpeg"
		}
		data, _ := img["data"].(string)
		if data == "" {
			continue
		}
		if strings.HasPrefix(data, "data:") {
			parts := strings.SplitN(data, ",", 2)
			if len(parts) == 2 {
				media = strings.TrimPrefix(strings.SplitN(parts[0], ";", 2)[0], "data:")
				data = parts[1]
			}
		}
		format := media
		if i := strings.LastIndex(media, "/"); i >= 0 {
			format = media[i+1:]
		}
		out = append(out, map[string]any{"format": format, "source": map[string]any{"bytes": data}})
	}
	return out
}

func SanitizeJSONSchema(schema map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range schema {
		if key == "additionalProperties" {
			continue
		}
		if key == "required" {
			if x, ok := asSlice(value); ok && len(x) == 0 {
				continue
			}
			if x, ok := value.([]string); ok && len(x) == 0 {
				continue
			}
		}
		switch x := value.(type) {
		case map[string]any:
			out[key] = SanitizeJSONSchema(x)
		case []any:
			r := make([]any, len(x))
			for i, item := range x {
				if m, ok := asMap(item); ok {
					r[i] = SanitizeJSONSchema(m)
				} else {
					r[i] = item
				}
			}
			out[key] = r
		case []map[string]any:
			r := make([]any, len(x))
			for i := range x {
				r[i] = SanitizeJSONSchema(x[i])
			}
			out[key] = r
		default:
			out[key] = value
		}
	}
	return out
}

var (
	standardModel = regexp.MustCompile(`^(claude-(?:haiku|sonnet|opus)-\d+)-(\d{1,2})(?:-(?:\d{8}|latest|\d+))?$`)
	noMinorModel  = regexp.MustCompile(`^(claude-(?:haiku|sonnet|opus)-\d+)(?:-\d{8})?$`)
	legacyModel   = regexp.MustCompile(`^(claude)-(\d+)-(\d+)-(haiku|sonnet|opus)(?:-(?:\d{8}|latest|\d+))?$`)
	dotDateModel  = regexp.MustCompile(`^(claude-(?:\d+\.\d+-)?(?:haiku|sonnet|opus)(?:-\d+\.\d+)?)-\d{8}$`)
	invertedModel = regexp.MustCompile(`^claude-(\d+)\.(\d+)-(haiku|sonnet|opus)-(.+)$`)
	contextSuffix = regexp.MustCompile(`(?i)\[\d+[mk]\]$`)
)

func NormalizeModelName(name string) string {
	if name == "" {
		return name
	}
	name = contextSuffix.ReplaceAllString(name, "")
	lower := strings.ToLower(name)
	if m := standardModel.FindStringSubmatch(lower); m != nil {
		return m[1] + "." + m[2]
	}
	if m := noMinorModel.FindStringSubmatch(lower); m != nil {
		return m[1]
	}
	if m := legacyModel.FindStringSubmatch(lower); m != nil {
		return m[1] + "-" + m[2] + "." + m[3] + "-" + m[4]
	}
	if m := dotDateModel.FindStringSubmatch(lower); m != nil {
		return m[1]
	}
	if m := invertedModel.FindStringSubmatch(lower); m != nil {
		return "claude-" + m[3] + "-" + m[1] + "." + m[2]
	}
	return name
}

func ResolveModel(name string, hidden map[string]string) string {
	n := NormalizeModelName(name)
	if v, ok := hidden[n]; ok {
		return v
	}
	return n
}

func decodeArguments(v any) any {
	s, ok := v.(string)
	if !ok {
		if v == nil {
			return map[string]any{}
		}
		return v
	}
	if s == "" {
		return map[string]any{}
	}
	var out any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return map[string]any{}
	}
	return out
}
