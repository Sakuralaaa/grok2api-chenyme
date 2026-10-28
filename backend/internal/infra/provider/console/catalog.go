package console

import (
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

const (
	QuotaMode      = "console"
	QuotaModeImage = "console_image"
	QuotaModeVideo = "console_video"
)

type ModelSpec struct {
	PublicID                string
	UpstreamModel           string
	SupportsReasoning       bool
	SupportsReasoningEffort bool
	DefaultReasoningEffort  string
	MaxOutputTokens         int
	// NoSearchTools marks models whose upstream ignores the built-in
	// web_search / x_search tools. No current Console chat model needs it:
	// grok-4.5 was flagged on 2026-10-20, but live 2026-10-29 it called
	// web_search three times for one request.
	NoSearchTools bool
}

// Synced with the Console home SSR model list (auth_mgmt.ListModelsForTeamResponse)
// and live probes on 2026-10-29: 4.7/4.6/4.5 accept low/medium/high/xhigh with
// upstream default high; 4.3 accepts none..xhigh with upstream default low. All
// chat models accept web_search/x_search and max_output_tokens=1_000_000.
var catalog = []ModelSpec{
	{PublicID: "grok-4.7", UpstreamModel: "grok-4.7", SupportsReasoning: true, SupportsReasoningEffort: true, DefaultReasoningEffort: "high", MaxOutputTokens: 1_000_000},
	{PublicID: "grok-4.6", UpstreamModel: "grok-4.6", SupportsReasoning: true, SupportsReasoningEffort: true, DefaultReasoningEffort: "high", MaxOutputTokens: 1_000_000},
	{PublicID: "grok-4.3", UpstreamModel: "grok-4.3", SupportsReasoning: true, SupportsReasoningEffort: true, DefaultReasoningEffort: "low", MaxOutputTokens: 1_000_000},
	{PublicID: "grok-4.20-0309-reasoning", UpstreamModel: "grok-4.20-0309-reasoning", SupportsReasoning: true, MaxOutputTokens: 1_000_000},
	{PublicID: "grok-4.20-0309-non-reasoning", UpstreamModel: "grok-4.20-0309-non-reasoning", MaxOutputTokens: 1_000_000},
	{PublicID: "grok-4.20-multi-agent-0309", UpstreamModel: "grok-4.20-multi-agent-0309", SupportsReasoning: true, SupportsReasoningEffort: true, MaxOutputTokens: 1_000_000},
	{PublicID: "grok-4.5", UpstreamModel: "grok-4.5", SupportsReasoning: true, SupportsReasoningEffort: true, DefaultReasoningEffort: "high", MaxOutputTokens: 1_000_000},
	{PublicID: "grok-build-0.1", UpstreamModel: "grok-build-0.1", MaxOutputTokens: 256_000},
}

var mediaCatalog = []struct {
	PublicID      string
	UpstreamModel string
	Capabilities  []modeldomain.Capability
}{
	// Keep the three official Console image products distinct. The legacy and
	// quality routes intentionally precede 2.0 so catalog reconciliation can
	// restore stable route IDs created before 2.0 was introduced.
	{PublicID: "grok-imagine-image", UpstreamModel: "grok-imagine-image", Capabilities: []modeldomain.Capability{modeldomain.CapabilityImage, modeldomain.CapabilityImageEdit}},
	{PublicID: "grok-imagine-image-quality", UpstreamModel: "grok-imagine-image-quality", Capabilities: []modeldomain.Capability{modeldomain.CapabilityImage, modeldomain.CapabilityImageEdit}},
	{PublicID: "grok-imagine-image-2.0", UpstreamModel: "grok-imagine-image-2.0", Capabilities: []modeldomain.Capability{modeldomain.CapabilityImage, modeldomain.CapabilityImageEdit}},
	{PublicID: "grok-imagine-video", UpstreamModel: "grok-imagine-video", Capabilities: []modeldomain.Capability{modeldomain.CapabilityVideo}},
	// Free Console accounts can use the 1.5 video model on the official Console media API.
	{PublicID: "grok-imagine-video-1.5", UpstreamModel: "grok-imagine-video-1.5", Capabilities: []modeldomain.Capability{modeldomain.CapabilityVideo}},
	// Low-cost 1.5 tier: text/image-to-video only (no reference_images / reference_audios).
	{PublicID: "grok-imagine-video-1.5-lite", UpstreamModel: "grok-imagine-video-1.5-lite", Capabilities: []modeldomain.Capability{modeldomain.CapabilityVideo}},
	{PublicID: "grok-voice-latest", UpstreamModel: "grok-voice-latest", Capabilities: []modeldomain.Capability{modeldomain.CapabilityRealtime, modeldomain.CapabilityTTS}},
	{PublicID: "grok-voice-think-fast-2.0", UpstreamModel: "grok-voice-think-fast-2.0", Capabilities: []modeldomain.Capability{modeldomain.CapabilityRealtime, modeldomain.CapabilityTTS}},
	{PublicID: "grok-voice-think-fast-1.0", UpstreamModel: "grok-voice-think-fast-1.0", Capabilities: []modeldomain.Capability{modeldomain.CapabilityRealtime, modeldomain.CapabilityTTS}},
	{PublicID: "grok-stt", UpstreamModel: "grok-stt", Capabilities: []modeldomain.Capability{modeldomain.CapabilitySTT}},
}

// Effort-suffixed aliases only include levels each Provider/model combination
// actually supports (see domain/model.SupportedReasoningEffortsForProvider).
// No blanket none/low/medium/high/xhigh/max template.
var aliases = []provider.ModelAlias{
	// Compatibility for the temporary PR catalog name. The official quality
	// model itself remains a first-class route.
	consoleAlias("grok-imagine-image-quality-2.0", "grok-imagine-image-quality", "grok-imagine-image-quality", ""),
	consoleAlias("grok-4.7-console", "grok-4.7", "grok-4.7", ""),
	consoleAlias("grok-4.6-console", "grok-4.6", "grok-4.6", ""),
	consoleAlias("grok-4.3-console", "grok-4.3", "grok-4.3", ""),
	consoleAlias("grok-4.20-0309-reasoning-console", "grok-4.20-0309-reasoning", "grok-4.20-0309-reasoning", ""),
	consoleAlias("grok-4.20-0309-non-reasoning-console", "grok-4.20-0309-non-reasoning", "grok-4.20-0309-non-reasoning", ""),
	consoleAlias("grok-4.20-multi-agent-console", "grok-4.20-multi-agent-0309", "grok-4.20-multi-agent-0309", ""),
	consoleAlias("grok-4.5-console", "grok-4.5", "grok-4.5", ""),
	consoleAlias("grok-build-console", "grok-build-0.1", "grok-build-0.1", ""),
	consoleAlias("grok-4.3-low", "grok-4.3", "grok-4.3", "low"),
	consoleAlias("grok-4.3-medium", "grok-4.3", "grok-4.3", "medium"),
	consoleAlias("grok-4.3-high", "grok-4.3", "grok-4.3", "high"),
	consoleAlias("grok-4.3-xhigh", "grok-4.3", "grok-4.3", "xhigh"),
	consoleAlias("grok-4.20-multi-agent-low", "grok-4.20-multi-agent-0309", "grok-4.20-multi-agent-0309", "low"),
	consoleAlias("grok-4.20-multi-agent-medium", "grok-4.20-multi-agent-0309", "grok-4.20-multi-agent-0309", "medium"),
	consoleAlias("grok-4.20-multi-agent-high", "grok-4.20-multi-agent-0309", "grok-4.20-multi-agent-0309", "high"),
	consoleAlias("grok-4.20-multi-agent-xhigh", "grok-4.20-multi-agent-0309", "grok-4.20-multi-agent-0309", "xhigh"),
}

func consoleAlias(alias, publicModel, upstreamModel, effort string) provider.ModelAlias {
	canonical, _ := modeldomain.NormalizePublicID(account.ProviderConsole, publicModel)
	return provider.ModelAlias{
		Alias: alias, PublicModel: canonical, Provider: account.ProviderConsole,
		UpstreamModel: upstreamModel, ReasoningEffort: effort,
	}
}

func Catalog() []ModelSpec { return append([]ModelSpec(nil), catalog...) }

func Routes() []modeldomain.Route {
	capacity := len(catalog)
	for _, spec := range mediaCatalog {
		capacity += len(spec.Capabilities)
	}
	values := make([]modeldomain.Route, 0, capacity)
	for _, spec := range catalog {
		publicID, _ := modeldomain.NormalizePublicID(account.ProviderConsole, spec.PublicID)
		values = append(values, modeldomain.Route{
			PublicID: publicID, Provider: account.ProviderConsole, UpstreamModel: spec.UpstreamModel,
			Capability: modeldomain.CapabilityResponses, Enabled: true,
		})
	}
	for _, spec := range mediaCatalog {
		publicID, _ := modeldomain.NormalizePublicID(account.ProviderConsole, spec.PublicID)
		for _, capability := range spec.Capabilities {
			values = append(values, modeldomain.Route{
				PublicID: publicID, Provider: account.ProviderConsole, UpstreamModel: spec.UpstreamModel,
				Capability: capability, Enabled: true,
			})
		}
	}
	return values
}

func Resolve(upstreamModel string) (ModelSpec, bool) {
	for _, spec := range catalog {
		if spec.UpstreamModel == upstreamModel {
			return spec, true
		}
	}
	return ModelSpec{}, false
}

func ResolveMedia(upstreamModel string, capability modeldomain.Capability) bool {
	for _, spec := range mediaCatalog {
		if spec.UpstreamModel != upstreamModel {
			continue
		}
		for _, supported := range spec.Capabilities {
			if supported == capability {
				return true
			}
		}
	}
	return false
}

func allModels() []string {
	values := make([]string, 0, len(catalog)+len(mediaCatalog))
	for _, spec := range catalog {
		values = append(values, spec.UpstreamModel)
	}
	for _, spec := range mediaCatalog {
		values = append(values, spec.UpstreamModel)
	}
	return values
}

func Aliases() []provider.ModelAlias { return append([]provider.ModelAlias(nil), aliases...) }
