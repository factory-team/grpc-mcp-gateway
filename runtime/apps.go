package runtime

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCP Apps (SEP-1865) identifiers.
const (
	AppsExtensionID      = "io.modelcontextprotocol/ui"
	AppResourceMIMEType  = "text/html;profile=mcp-app"
	appResourceURIScheme = "ui://"
)

// ToolUI links a tool to the ui:// resource that renders its result.
type ToolUI struct {
	ResourceURI string
	// Visibility lists "model" and/or "app". Empty means both.
	Visibility []string
}

// ResourceCSP declares the external origins a UI resource needs. Leave it nil
// when the view loads nothing external, so hosts apply their strict default.
type ResourceCSP struct {
	ConnectDomains  []string
	ResourceDomains []string
	FrameDomains    []string
	BaseURIDomains  []string
}

// Resource is a static MCP resource, typically an MCP Apps ui:// view.
type Resource struct {
	URI         string
	Name        string
	Title       string
	Description string
	MIMEType    string
	Text        string
	CSP         *ResourceCSP
	// PrefersBorder is sent as _meta.ui.prefersBorder when set.
	PrefersBorder *bool
	// Meta is merged into the resource _meta, e.g. host-specific keys such as
	// openai/widgetDescription.
	Meta map[string]any
}

// RegisterResource registers a resource served by resources/list and
// resources/read. App resources must use the ui:// scheme. Register resources
// before the first request: the resources capability and the MCP Apps
// extension are advertised from what is registered at that point.
func (mux *MCPServeMux) RegisterResource(res *Resource) {
	if res == nil || res.URI == "" {
		panic("mcp resource requires a uri")
	}
	if res.MIMEType == AppResourceMIMEType && !strings.HasPrefix(res.URI, appResourceURIScheme) {
		panic(fmt.Sprintf("mcp app resource %q must use the ui:// scheme", res.URI))
	}
	mux.mu.Lock()
	defer mux.mu.Unlock()
	mux.resources[res.URI] = res
	if mux.server != nil {
		addResource(mux.server, res)
		return
	}
	addResource(mux.probe, res)
}

// MissingUIResources returns the ui:// URIs that registered tools reference
// but no registered resource serves. Call it at startup: hosts fail to render
// a tool whose view does not exist.
func (mux *MCPServeMux) MissingUIResources() []string {
	mux.mu.RLock()
	defer mux.mu.RUnlock()
	seen := map[string]bool{}
	var missing []string
	for _, tool := range mux.tools {
		if tool.UI == nil || tool.UI.ResourceURI == "" {
			continue
		}
		uri := tool.UI.ResourceURI
		if _, ok := mux.resources[uri]; ok || seen[uri] {
			continue
		}
		seen[uri] = true
		missing = append(missing, uri)
	}
	sort.Strings(missing)
	return missing
}

func (mux *MCPServeMux) hasAppResources() bool {
	for _, res := range mux.resources {
		if res.MIMEType == AppResourceMIMEType {
			return true
		}
	}
	return false
}

func toolMeta(ui *ToolUI) map[string]any {
	if ui == nil || ui.ResourceURI == "" {
		return nil
	}
	uiMeta := map[string]any{"resourceUri": ui.ResourceURI}
	if len(ui.Visibility) > 0 {
		uiMeta["visibility"] = ui.Visibility
	}
	return map[string]any{
		"ui": uiMeta,
		// Deprecated flat key, still read by hosts built before the nested form.
		"ui/resourceUri": ui.ResourceURI,
		// ChatGPT (OpenAI Apps SDK) alias of ui.resourceUri.
		"openai/outputTemplate": ui.ResourceURI,
	}
}

func resourceMeta(res *Resource) map[string]any {
	meta := map[string]any{}
	for k, v := range res.Meta {
		meta[k] = v
	}
	uiMeta := map[string]any{}
	if res.CSP != nil {
		csp := map[string]any{}
		addDomains := func(key string, domains []string) {
			if len(domains) > 0 {
				csp[key] = domains
			}
		}
		addDomains("connectDomains", res.CSP.ConnectDomains)
		addDomains("resourceDomains", res.CSP.ResourceDomains)
		addDomains("frameDomains", res.CSP.FrameDomains)
		addDomains("baseUriDomains", res.CSP.BaseURIDomains)
		if len(csp) > 0 {
			uiMeta["csp"] = csp
		}
	}
	if res.PrefersBorder != nil {
		uiMeta["prefersBorder"] = *res.PrefersBorder
	}
	if len(uiMeta) > 0 {
		meta["ui"] = uiMeta
	}
	if len(meta) == 0 {
		return nil
	}
	return meta
}

// addResource registers res on an SDK server.
func addResource(server *mcp.Server, res *Resource) {
	meta := resourceMeta(res)
	server.AddResource(&mcp.Resource{
		URI:         res.URI,
		Name:        res.Name,
		Title:       res.Title,
		Description: res.Description,
		MIMEType:    res.MIMEType,
		Meta:        meta,
	}, func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
			URI: res.URI, MIMEType: res.MIMEType, Text: res.Text, Meta: meta,
		}}}, nil
	})
}
