package codexupstream

import (
	"context"
	"encoding/json"
	"net/http"
)

type modelFields struct {
	Slug              string          `json:"slug"`
	DisplayName       string          `json:"display_name"`
	Visibility        string          `json:"visibility"`
	Shell             string          `json:"shell_type"`
	Reasoning         json.RawMessage `json:"supported_reasoning_levels"`
	Truncation        json.RawMessage `json:"truncation_policy"`
	Experimental      json.RawMessage `json:"experimental_supported_tools"`
	SupportsVerbosity *bool           `json:"support_verbosity"`
	Priority          *int            `json:"priority"`
	BaseInstructions  *string         `json:"base_instructions"`
	ModelMessages     struct {
		InstructionsTemplate *string `json:"instructions_template"`
	} `json:"model_messages"`
}

func (c *Client) ListModels(ctx context.Context, grant Grant, route Route) (Catalog, error) {
	if route.CatalogURL == "" {
		return Catalog{}, &UpstreamError{Code: "catalog_route_unverified", Stage: "catalog_route", StatusCode: http.StatusServiceUnavailable}
	}
	headers, err := c.RequestHeaders(grant, route, false)
	if err != nil {
		return Catalog{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, route.CatalogURL, nil)
	if err != nil {
		return Catalog{}, unavailable("catalog")
	}
	req.Header = headers
	req.Header.Set("Accept", "application/json")
	req.Header.Del("Content-Type")
	res, err := c.oauth.Do(req)
	if err != nil {
		return Catalog{}, transportError(ctx, "catalog")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return Catalog{}, responseError(res, "catalog", false)
	}
	var received Catalog
	if readJSON(res.Body, &received) != nil || received.Models == nil {
		return Catalog{}, invalidResponse("catalog")
	}
	catalog := Catalog{Models: make([]json.RawMessage, 0, len(received.Models))}
	seen := make(map[string]bool)
	for _, entry := range received.Models {
		var fields modelFields
		if json.Unmarshal(entry, &fields) != nil || fields.Visibility != "list" ||
			!validBearer(fields.Slug) || fields.DisplayName == "" || seen[fields.Slug] {
			continue
		}
		var model json.RawMessage
		if c.Channel() == ChannelCodex {
			model = nativeCodexModel(entry, fields)
		} else {
			model = chatGPTModel(fields, len(catalog.Models))
		}
		if model != nil {
			catalog.Models = append(catalog.Models, model)
			catalog.Slugs = append(catalog.Slugs, fields.Slug)
			seen[fields.Slug] = true
		}
	}
	if len(catalog.Models) == 0 {
		return Catalog{}, &UpstreamError{Code: "model_incompatible", Stage: "catalog", StatusCode: http.StatusConflict}
	}
	return catalog, nil
}

func nativeCodexModel(entry json.RawMessage, f modelFields) json.RawMessage {
	switch f.Shell {
	case "unified_exec", "default", "local", "shell_command", "disabled":
	default:
		return nil
	}
	var reasoning []struct {
		Effort      string `json:"effort"`
		Description string `json:"description"`
	}
	var experimental []string
	var truncation struct {
		Mode  string `json:"mode"`
		Limit int64  `json:"limit"`
	}
	if json.Unmarshal(f.Reasoning, &reasoning) != nil || reasoning == nil ||
		json.Unmarshal(f.Experimental, &experimental) != nil || experimental == nil ||
		json.Unmarshal(f.Truncation, &truncation) != nil || truncation.Limit <= 0 ||
		(truncation.Mode != "tokens" && truncation.Mode != "bytes") ||
		f.Priority == nil || f.SupportsVerbosity == nil ||
		(f.BaseInstructions == nil && f.ModelMessages.InstructionsTemplate == nil) {
		return nil
	}
	for _, choice := range reasoning {
		if choice.Effort == "" {
			return nil
		}
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(entry, &fields) != nil {
		return nil
	}
	// env_key providers filter their catalog by this field. It describes
	// availability through the gateway's provider after compatibility checks.
	fields["supported_in_api"] = json.RawMessage("true")
	model, _ := json.Marshal(fields)
	return model
}

func chatGPTModel(f modelFields, priority int) json.RawMessage {
	// Model capabilities come from the pinned Codex catalog. These two
	// slugs also have direct-plan Responses examples in the SIWC documentation.
	// Use local direct tools encoded as additional_tools, without tool_search.
	switch f.Slug {
	case "gpt-6.1-sol", "gpt-6.2-sol":
	default:
		return nil
	}
	model, _ := json.Marshal(map[string]any{
		"slug": f.Slug, "display_name": f.DisplayName, "visibility": "list",
		"model_messages": map[string]string{
			"instructions_template": "You are Codex, a coding agent. Complete the user's task using the available local tools.",
		},
		"supported_in_api": true, "priority": priority,
		"shell_type": "unified_exec", "apply_patch_tool_type": "freeform",
		"default_reasoning_level": "low", "supported_reasoning_levels": []map[string]string{
			{"effort": "low", "description": "Low reasoning effort"},
			{"effort": "medium", "description": "Medium reasoning effort"},
			{"effort": "high", "description": "High reasoning effort"},
			{"effort": "xhigh", "description": "Extra high reasoning effort"},
		},
		"default_reasoning_summary": "none", "supports_reasoning_summary_parameter": true,
		"support_verbosity": true, "context_window": 500000,
		"truncation_policy":            map[string]any{"mode": "tokens", "limit": 10000},
		"input_modalities":             []string{"text", "image"},
		"experimental_supported_tools": []string{}, "tool_mode": "direct",
		"supports_search_tool": false, "use_responses_lite": true,
	})
	return model
}
