package mcpsource

import (
	"encoding/json"
	"time"
)

// configJSON is the JSON-friendly form of Config used by the
// DEVROUTER_TOOLS_CONFIG file. It mirrors Config with snake_case keys
// and expresses the timeout in milliseconds. Only name/transport/
// endpoint are required; everything else is auto-discovered or
// defaulted, so a typical entry is a single line.
type configJSON struct {
	Name      string            `json:"name"`
	Transport string            `json:"transport"`
	Endpoint  string            `json:"endpoint"`
	Headers   map[string]string `json:"headers,omitempty"`
	Env       []string          `json:"env,omitempty"`
	ToolName  string            `json:"tool_name,omitempty"`
	QueryArg  string            `json:"query_arg,omitempty"`
	LimitArg  string            `json:"limit_arg,omitempty"`
	ExtraArgs map[string]any    `json:"extra_args,omitempty"`
	Mapper    string            `json:"mapper,omitempty"`
	MaxDocs   int               `json:"max_docs,omitempty"`
	TimeoutMS int               `json:"timeout_ms,omitempty"`
}

func (c configJSON) toConfig() Config {
	return Config{
		Name:      c.Name,
		Transport: c.Transport,
		Endpoint:  c.Endpoint,
		Headers:   c.Headers,
		Env:       c.Env,
		ToolName:  c.ToolName,
		QueryArg:  c.QueryArg,
		LimitArg:  c.LimitArg,
		ExtraArgs: c.ExtraArgs,
		Mapper:    c.Mapper,
		MaxDocs:   c.MaxDocs,
		Timeout:   time.Duration(c.TimeoutMS) * time.Millisecond,
	}
}

// ParseConfigs parses the DEVROUTER_TOOLS_CONFIG payload: a JSON array
// of tool configs. The caller constructs a Source per entry via New,
// which surfaces any per-tool misconfiguration.
func ParseConfigs(data []byte) ([]Config, error) {
	var raw []configJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	out := make([]Config, 0, len(raw))
	for _, c := range raw {
		out = append(out, c.toConfig())
	}
	return out, nil
}
