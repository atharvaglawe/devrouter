package mcpsource

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"github.com/atharva-ag/devrouter/internal/prompt"
)

// mapper turns a tool's textual result into generic DocEntries. The
// rest of the pipeline only sees prompt.DocEntry.
//
// The default mapper is "generic": a shape-agnostic normalizer that
// works for any MCP structuredContent / REST JSON payload, so a new
// tool needs no bespoke code. The named mappers (cmdocs, gitlab) are
// retained only for back-compat with existing config; new tools should
// leave Config.Mapper empty.
type mapper func(text string) ([]prompt.DocEntry, error)

// mappers is the registry keyed by Config.Mapper. The empty key
// resolves to "generic" in source.New, so new tools need no entry here.
var mappers = map[string]mapper{
	"generic": genericMapper,
	"cmdocs":  cmdocsMapper,
	"gitlab":  gitlabMapper,
}

const maxDocContent = 4000 // per-DocEntry content cap (chars)

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ---------------------------------------------------------------------------
// generic — shape-agnostic normalizer (default for every new tool)
// ---------------------------------------------------------------------------

// containerKeys are the field names under which tools commonly nest the
// result array inside a wrapper object.
var containerKeys = []string{
	"items", "results", "data", "documents", "docs",
	"issues", "merge_requests", "nodes", "sections", "hits", "matches",
}

// fieldAliases maps each DocEntry field to the source keys we accept,
// in priority order. Lets one normalizer absorb cmdocs / gitlab / wiki /
// arbitrary REST shapes without per-tool code.
var (
	idAliases      = []string{"id", "iid", "doc_id", "key", "number"}
	titleAliases   = []string{"title", "name", "summary", "heading", "doc_name"}
	urlAliases     = []string{"url", "web_url", "html_url", "link", "permalink"}
	collAliases    = []string{"collection", "project", "list", "repository", "namespace"}
	contentAliases = []string{"content", "description", "body", "text", "snippet", "excerpt"}
)

// genericMapper normalizes any tool payload into DocEntries. It handles,
// in order: a JSON array of objects, a JSON object wrapping an array
// (containerKeys), a single JSON object, and finally plain text (surfaced
// as one entry so nothing is silently dropped). Source is left empty and
// stamped by Source.Search from Config.Name.
func genericMapper(text string) ([]prompt.DocEntry, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, nil
	}
	items := extractItems([]byte(text))
	if items == nil {
		return []prompt.DocEntry{{Content: clip(text, maxDocContent)}}, nil
	}
	out := make([]prompt.DocEntry, 0, len(items))
	for _, it := range items {
		d := itemToDoc(it)
		if d.Title == "" && d.Content == "" && d.URL == "" {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

// extractItems pulls a list of objects out of the common container
// shapes. Returns nil when the payload is not JSON or carries no
// recognisable items (caller falls back to a plain-text entry).
func extractItems(raw []byte) []map[string]any {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	switch raw[0] {
	case '[':
		var arr []map[string]any
		if err := decodeAny(raw, &arr); err == nil {
			return arr
		}
		return nil
	case '{':
		var obj map[string]json.RawMessage
		if err := decodeAny(raw, &obj); err != nil {
			return nil
		}
		for _, key := range containerKeys {
			if v, ok := obj[key]; ok {
				var arr []map[string]any
				if err := decodeAny(v, &arr); err == nil && len(arr) > 0 {
					return arr
				}
			}
		}
		var single map[string]any
		if err := decodeAny(raw, &single); err == nil {
			return []map[string]any{single}
		}
	}
	return nil
}

// itemToDoc applies the field-alias heuristics to one object.
func itemToDoc(it map[string]any) prompt.DocEntry {
	d := prompt.DocEntry{
		ID:         firstString(it, idAliases...),
		Title:      firstString(it, titleAliases...),
		URL:        firstString(it, urlAliases...),
		Collection: firstString(it, collAliases...),
	}
	if d.Collection == "" {
		d.Collection = nestedString(it, "references", "full")
	}
	content := firstString(it, contentAliases...)
	if content == "" {
		content = joinSections(it)
	}
	d.Content = clip(content, maxDocContent)
	return d
}

// decodeAny unmarshals with UseNumber so integer IDs don't lose
// precision through the float64 default.
func decodeAny(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return dec.Decode(v)
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s := anyToString(v); s != "" {
				return s
			}
		}
	}
	return ""
}

func anyToString(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case json.Number:
		return t.String()
	case float64:
		if t == math.Trunc(t) {
			return strconv.FormatInt(int64(t), 10)
		}
		return strconv.FormatFloat(t, 'g', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	}
	return ""
}

func nestedString(m map[string]any, outer, inner string) string {
	if v, ok := m[outer]; ok {
		if sub, ok := v.(map[string]any); ok {
			return anyToString(sub[inner])
		}
	}
	return ""
}

// joinSections concatenates the content of a "sections" array (cmdocs
// shape: [{page, content}]).
func joinSections(m map[string]any) string {
	v, ok := m["sections"]
	if !ok {
		return ""
	}
	arr, ok := v.([]any)
	if !ok {
		return ""
	}
	var sb strings.Builder
	for _, e := range arr {
		sec, ok := e.(map[string]any)
		if !ok {
			continue
		}
		c := anyToString(sec["content"])
		if c == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(c)
	}
	return sb.String()
}

// ---------------------------------------------------------------------------
// cmdocs — PageIndex pageindex_search JSON
// ---------------------------------------------------------------------------

type cmdocsResponse struct {
	Results []struct {
		DocID      string `json:"doc_id"`
		DocName    string `json:"doc_name"`
		Collection string `json:"collection"`
		Sections   []struct {
			Page    int    `json:"page"`
			Content string `json:"content"`
		} `json:"sections"`
	} `json:"results"`
}

func cmdocsMapper(text string) ([]prompt.DocEntry, error) {
	var resp cmdocsResponse
	if err := json.Unmarshal([]byte(text), &resp); err != nil {
		return nil, err
	}
	out := make([]prompt.DocEntry, 0, len(resp.Results))
	for _, r := range resp.Results {
		var sb strings.Builder
		for _, s := range r.Sections {
			if s.Content == "" {
				continue
			}
			if sb.Len() > 0 {
				sb.WriteString("\n\n")
			}
			sb.WriteString(s.Content)
		}
		content := clip(sb.String(), maxDocContent)
		if content == "" {
			continue
		}
		out = append(out, prompt.DocEntry{
			Source:     "cmdocs",
			ID:         r.DocID,
			Title:      r.DocName,
			Collection: r.Collection,
			Content:    content,
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// gitlab — issue / MR search results (tolerant of shape variation)
// ---------------------------------------------------------------------------

// gitlabItem captures the common fields across GitLab issue/MR/search
// payloads. All optional — different MCP servers and endpoints name
// things slightly differently, so we read several aliases.
type gitlabItem struct {
	IID         json.Number `json:"iid"`
	ID          json.Number `json:"id"`
	Title       string      `json:"title"`
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Body        string      `json:"body"`
	WebURL      string      `json:"web_url"`
	URL         string      `json:"url"`
	State       string      `json:"state"`
	References  struct {
		Full string `json:"full"`
	} `json:"references"`
}

func (it gitlabItem) toDoc() prompt.DocEntry {
	title := it.Title
	if title == "" {
		title = it.Name
	}
	id := it.IID.String()
	if id == "" {
		id = it.ID.String()
	}
	url := it.WebURL
	if url == "" {
		url = it.URL
	}
	body := it.Description
	if body == "" {
		body = it.Body
	}
	return prompt.DocEntry{
		Source:     "gitlab",
		ID:         id,
		Title:      title,
		Collection: it.References.Full,
		URL:        url,
		Content:    clip(strings.TrimSpace(strings.Join([]string{it.State, body}, " ")), maxDocContent),
	}
}

func gitlabMapper(text string) ([]prompt.DocEntry, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, nil
	}
	items := extractGitlabItems([]byte(text))
	if items == nil {
		// Unknown shape — surface the raw payload as a single entry so
		// the agent still sees something rather than silently dropping it.
		return []prompt.DocEntry{{
			Source:  "gitlab",
			Title:   "gitlab result",
			Content: clip(text, maxDocContent),
		}}, nil
	}
	out := make([]prompt.DocEntry, 0, len(items))
	for _, it := range items {
		d := it.toDoc()
		if d.Title == "" && d.Content == "" {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

// extractGitlabItems pulls a list of items out of the common container
// shapes: a bare array, or an object with items/results/data/issues/
// merge_requests holding the array. Returns nil if none matched.
func extractGitlabItems(raw []byte) []gitlabItem {
	raw = []byte(strings.TrimSpace(string(raw)))
	if len(raw) == 0 {
		return nil
	}
	if raw[0] == '[' {
		var arr []gitlabItem
		if err := json.Unmarshal(raw, &arr); err == nil {
			return arr
		}
		return nil
	}
	if raw[0] == '{' {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil
		}
		for _, key := range []string{"items", "results", "data", "issues", "merge_requests", "nodes"} {
			if v, ok := obj[key]; ok {
				var arr []gitlabItem
				if err := json.Unmarshal(v, &arr); err == nil && len(arr) > 0 {
					return arr
				}
			}
		}
		// Single object — treat as one item.
		var single gitlabItem
		if err := json.Unmarshal(raw, &single); err == nil {
			return []gitlabItem{single}
		}
	}
	return nil
}
