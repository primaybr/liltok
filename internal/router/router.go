package router

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/primaybr/liltok/internal/config"
	"github.com/primaybr/liltok/internal/provider"
	"github.com/primaybr/liltok/internal/provider/anthropic"
	"github.com/primaybr/liltok/internal/provider/gemini"
	"github.com/primaybr/liltok/internal/provider/openai"
	"github.com/primaybr/liltok/internal/telemetry"
)

// TargetSpec defines a provider and specific upstream model in a fallback chain.
type TargetSpec struct {
	ProviderName  string
	UpstreamModel string
}

// defaultGroqActiveModels provides the verified baseline active models from https://console.groq.com/docs/models
var defaultGroqActiveModels = []provider.ModelInfo{
	{ID: "openai/gpt-oss-120b", Provider: "groq", Active: true, ContextWindow: 131072, OwnedBy: "openai"},
	{ID: "openai/gpt-oss-20b", Provider: "groq", Active: true, ContextWindow: 131072, OwnedBy: "openai"},
	{ID: "qwen/qwen3.8-27b", Provider: "groq", Active: true, ContextWindow: 131042, OwnedBy: "qwen"},
	{ID: "groq/compound", Provider: "groq", Active: true, ContextWindow: 131072, OwnedBy: "groq"},
	{ID: "groq/compound-mini", Provider: "groq", Active: true, ContextWindow: 131072, OwnedBy: "groq"},
	{ID: "openai/gpt-oss-safeguard-20b", Provider: "groq", Active: true, ContextWindow: 131072, OwnedBy: "openai"},
	{ID: "allam-2-7b", Provider: "groq", Active: true, ContextWindow: 4096, OwnedBy: "allam"},
	{ID: "canopylabs/orpheus-v1-english", Provider: "groq", Active: true, ContextWindow: 4000, OwnedBy: "canopylabs"},
	{ID: "canopylabs/orpheus-arabic-saudi", Provider: "groq", Active: true, ContextWindow: 4000, OwnedBy: "canopylabs"},
	{ID: "meta-llama/llama-prompt-guard-2-22m", Provider: "groq", Active: true, ContextWindow: 512, OwnedBy: "meta-llama"},
	{ID: "meta-llama/llama-prompt-guard-2-86m", Provider: "groq", Active: true, ContextWindow: 512, OwnedBy: "meta-llama"},
	{ID: "whisper-large-v3", Provider: "groq", Active: true, ContextWindow: 448, OwnedBy: "openai"},
	{ID: "whisper-large-v3-turbo", Provider: "groq", Active: true, ContextWindow: 448, OwnedBy: "openai"},
}

// groqDeprecatedModelReplacements maps decommissioned Groq models to recommended active replacements.
var groqDeprecatedModelReplacements = map[string]string{
	"llama-3.3-70b-versatile":                       "openai/gpt-oss-120b",
	"llama-3.1-8b-instant":                          "openai/gpt-oss-20b",
	"llama3-70b-8192":                               "openai/gpt-oss-120b",
	"llama3-8b-8192":                                "openai/gpt-oss-20b",
	"mixtral-8x7b-32768":                            "openai/gpt-oss-120b",
	"gemma2-9b-it":                                  "openai/gpt-oss-20b",
	"gemma-7b-it":                                   "openai/gpt-oss-20b",
	"deepseek-r1-distill-llama-70b":                 "openai/gpt-oss-120b",
	"deepseek-r1-distill-llama-70b-specdec":         "openai/gpt-oss-120b",
	"deepseek-r1-distill-qwen-32b":                  "openai/gpt-oss-120b",
	"qwen/qwen3-32b":                                "openai/gpt-oss-120b",
	"qwen-qwq-32b":                                  "openai/gpt-oss-120b",
	"qwen-2.5-32b":                                  "openai/gpt-oss-120b",
	"qwen-2.5-coder-32b":                            "openai/gpt-oss-120b",
	"meta-llama/llama-guard-4-12b":                  "openai/gpt-oss-safeguard-20b",
	"llama-guard-3-8b":                              "openai/gpt-oss-safeguard-20b",
	"meta-llama/llama-4-scout-17b-16e-instruct":     "openai/gpt-oss-120b",
	"meta-llama/llama-4-maverick-17b-128e-instruct": "openai/gpt-oss-120b",
	"playai-tts":                                    "canopylabs/orpheus-v1-english",
	"playai-tts-arabic":                             "canopylabs/orpheus-arabic-saudi",
	"distil-whisper-large-v3-en":                    "whisper-large-v3-turbo",
}

// RemapGroqModel translates deprecated Groq model names to active replacement model IDs.
func RemapGroqModel(model string) (string, bool) {
	lower := strings.ToLower(strings.TrimSpace(model))
	lower = strings.TrimPrefix(lower, "groq/")
	if strings.HasSuffix(lower, ":free") {
		return model, false
	}
	if repl, ok := groqDeprecatedModelReplacements[lower]; ok {
		return repl, true
	}
	for dep, repl := range groqDeprecatedModelReplacements {
		if strings.HasPrefix(lower, dep) {
			return repl, true
		}
	}
	return model, false
}

func isDeprecatedGroq(model string) bool {
	if strings.HasSuffix(strings.ToLower(model), ":free") {
		return false
	}
	_, ok := RemapGroqModel(model)
	return ok
}

// defaultNVIDIANIMActiveModels provides the verified baseline active free reasoning and chat models from build.nvidia.com
var defaultNVIDIANIMActiveModels = []provider.ModelInfo{
	{ID: "deepseek-ai/deepseek-v4-flash-0731", Provider: "nvidianim", Active: true, ContextWindow: 131072, OwnedBy: "deepseek-ai"},
	{ID: "google/gemma-4-31b-it", Provider: "nvidianim", Active: true, ContextWindow: 131072, OwnedBy: "google"},
	{ID: "nvidia/nemotron-3.5-lightning-30b-a3b", Provider: "nvidianim", Active: true, ContextWindow: 131072, OwnedBy: "nvidia"},
	{ID: "nvidia/nemotron-3-super-120b-a12b", Provider: "nvidianim", Active: true, ContextWindow: 131072, OwnedBy: "nvidia"},
	{ID: "nvidia/nemotron-3-nano-omni-30b-a3b-reasoning", Provider: "nvidianim", Active: true, ContextWindow: 131072, OwnedBy: "nvidia"},
	{ID: "poolside/laguna-xs-2.1", Provider: "nvidianim", Active: true, ContextWindow: 65536, OwnedBy: "poolside"},
	{ID: "meta/llama-3.2-11b-vision-instruct", Provider: "nvidianim", Active: true, ContextWindow: 131072, OwnedBy: "meta"},
	{ID: "meta/llama-3.2-90b-vision-instruct", Provider: "nvidianim", Active: true, ContextWindow: 131072, OwnedBy: "meta"},
	{ID: "google/diffusiongemma-26b-a4b-it", Provider: "nvidianim", Active: true, ContextWindow: 32768, OwnedBy: "google"},
	{ID: "nvidia/nemotron-3-ultra-550b-a55b", Provider: "nvidianim", Active: true, ContextWindow: 131072, OwnedBy: "nvidia"},
	{ID: "openai/gpt-oss-20b", Provider: "nvidianim", Active: true, ContextWindow: 131072, OwnedBy: "openai"},
	{ID: "mistralai/mistral-nemotron", Provider: "nvidianim", Active: true, ContextWindow: 131072, OwnedBy: "mistralai"},
	{ID: "z-ai/glm-5.3-flash", Provider: "nvidianim", Active: true, ContextWindow: 131072, OwnedBy: "z-ai"},
	{ID: "z-ai/glm-5.3", Provider: "nvidianim", Active: true, ContextWindow: 131072, OwnedBy: "z-ai"},
	{ID: "moonshotai/kimi-k3", Provider: "nvidianim", Active: true, ContextWindow: 32768, OwnedBy: "moonshotai"},
}

// nvidianimDeprecatedModelReplacements maps decommissioned or alias NVIDIA NIM models to active reasoning/chat replacements.
var nvidianimDeprecatedModelReplacements = map[string]string{
	"meta/llama-3.1-70b-instruct":             "nvidia/nemotron-3.5-lightning-30b-a3b",
	"llama-3.1-70b-instruct":                  "nvidia/nemotron-3.5-lightning-30b-a3b",
	"meta/llama-3.1-8b-instruct":              "meta/llama-3.2-11b-vision-instruct",
	"llama-3.1-8b-instruct":                   "meta/llama-3.2-11b-vision-instruct",
	"meta/llama-3.1-405b-instruct":            "nvidia/nemotron-3-ultra-550b-a55b",
	"llama-3.1-405b-instruct":                 "nvidia/nemotron-3-ultra-550b-a55b",
	"meta/llama3-70b-instruct":                "nvidia/nemotron-3.5-lightning-30b-a3b",
	"meta/llama3-8b-instruct":                 "meta/llama-3.2-11b-vision-instruct",
	"deepseek-ai/deepseek-r1":                 "deepseek-ai/deepseek-v4-flash-0731",
	"deepseek-r1":                             "deepseek-ai/deepseek-v4-flash-0731",
	"deepseek-ai/deepseek-v3":                 "deepseek-ai/deepseek-v4-flash-0731",
	"deepseek-v3":                             "deepseek-ai/deepseek-v4-flash-0731",
	"deepseek-ai/deepseek-coder-6.7b-instruct": "deepseek-ai/deepseek-v4-flash-0731",
	"nvidia/nemotron-4-340b-instruct":         "nvidia/nemotron-3-ultra-550b-a55b",
	"nvidia/llama-3.1-nemotron-70b-instruct":  "nvidia/nemotron-3.5-lightning-30b-a3b",
	"nvidia/llama-3.1-nemotron-51b-instruct":  "nvidia/nemotron-3.5-lightning-30b-a3b",
	"nvidia/llama-3.1-nemotron-ultra-253b-v1": "nvidia/nemotron-3-ultra-550b-a55b",
	"meta/muse-glimmer-30b":                   "deepseek-ai/deepseek-v4-flash-0731",
	"google/gemma-3-12b-it":                   "google/gemma-4-31b-it",
	"google/gemma-3-4b-it":                    "google/gemma-4-31b-it",
	"google/gemma-2-9b-it":                    "google/gemma-4-31b-it",
	"google/codegemma-7b":                     "google/gemma-4-31b-it",
	"meta/codellama-70b":                      "nvidia/nemotron-3.5-lightning-30b-a3b",
	"mistralai/mistral-large-2-instruct":      "mistralai/mistral-nemotron",
	"mistralai/mixtral-8x22b-v0.1":            "nvidia/nemotron-3-super-120b-a12b",
	"writer/palmyra-creative-122b":            "nvidia/nemotron-3-super-120b-a12b",
}

// RemapNVIDIANIMModel translates deprecated NVIDIA NIM model names to active replacement model IDs.
func RemapNVIDIANIMModel(model string) (string, bool) {
	lower := strings.ToLower(strings.TrimSpace(model))
	lower = strings.TrimPrefix(lower, "nvidianim/")
	if strings.HasSuffix(lower, ":free") {
		return model, false
	}
	if repl, ok := nvidianimDeprecatedModelReplacements[lower]; ok {
		return repl, true
	}
	for dep, repl := range nvidianimDeprecatedModelReplacements {
		if strings.HasSuffix(dep, lower) || strings.HasPrefix(lower, dep) {
			return repl, true
		}
	}
	return model, false
}

func isDeprecatedNVIDIANIM(model string) bool {
	if strings.HasSuffix(strings.ToLower(model), ":free") {
		return false
	}
	_, ok := RemapNVIDIANIMModel(model)
	return ok
}

// defaultOpenRouterActiveModels provides the verified baseline active free reasoning and chat models from openrouter.ai/models
var defaultOpenRouterActiveModels = []provider.ModelInfo{
	{ID: "deepseek/deepseek-v4-flash-0731:free", Provider: "openrouter", Active: true, ContextWindow: 1048576, OwnedBy: "deepseek"},
	{ID: "google/gemma-4-31b-it:free", Provider: "openrouter", Active: true, ContextWindow: 262144, OwnedBy: "google"},
	{ID: "qwen/qwen3.8-27b:free", Provider: "openrouter", Active: true, ContextWindow: 262144, OwnedBy: "qwen"},
	{ID: "nvidia/nemotron-3.5-lightning:free", Provider: "openrouter", Active: true, ContextWindow: 1000000, OwnedBy: "nvidia"},
	{ID: "nvidia/nemotron-3-super-120b-a12b:free", Provider: "openrouter", Active: true, ContextWindow: 262144, OwnedBy: "nvidia"},
	{ID: "nvidia/nemotron-3-ultra-550b-a55b:free", Provider: "openrouter", Active: true, ContextWindow: 1000000, OwnedBy: "nvidia"},
	{ID: "nvidia/nemotron-3-nano-omni-30b-a3b-reasoning:free", Provider: "openrouter", Active: true, ContextWindow: 256000, OwnedBy: "nvidia"},
	{ID: "poolside/laguna-xs-2.1:free", Provider: "openrouter", Active: true, ContextWindow: 262144, OwnedBy: "poolside"},
	{ID: "poolside/laguna-s-2.1:free", Provider: "openrouter", Active: true, ContextWindow: 262144, OwnedBy: "poolside"},
	{ID: "thinkingmachines/inkling:free", Provider: "openrouter", Active: true, ContextWindow: 1048576, OwnedBy: "thinkingmachines"},
	{ID: "thinkingmachines/inkling-small:free", Provider: "openrouter", Active: true, ContextWindow: 1048576, OwnedBy: "thinkingmachines"},
	{ID: "nex-agi/nex-n2.5-pro:free", Provider: "openrouter", Active: true, ContextWindow: 262144, OwnedBy: "nex-agi"},
	{ID: "nex-agi/nex-n2.5-mini:free", Provider: "openrouter", Active: true, ContextWindow: 262144, OwnedBy: "nex-agi"},
	{ID: "cohere/north-mini-code:free", Provider: "openrouter", Active: true, ContextWindow: 256000, OwnedBy: "cohere"},
	{ID: "dots-studio/dots-3-note-preview:free", Provider: "openrouter", Active: true, ContextWindow: 512000, OwnedBy: "dots-studio"},
	{ID: "liquid/lfm-2.5-2.6b:free", Provider: "openrouter", Active: true, ContextWindow: 65536, OwnedBy: "liquid"},
	{ID: "inclusionai/ling-3.0-flash-vl:free", Provider: "openrouter", Active: true, ContextWindow: 262144, OwnedBy: "inclusionai"},
	{ID: "inclusionai/ling-3.0-flash-sante:free", Provider: "openrouter", Active: true, ContextWindow: 262144, OwnedBy: "inclusionai"},
	{ID: "inclusionai/ling-3.0-flash-fin:free", Provider: "openrouter", Active: true, ContextWindow: 262144, OwnedBy: "inclusionai"},
	{ID: "google/gemma-4-26b-a4b-it:free", Provider: "openrouter", Active: true, ContextWindow: 262144, OwnedBy: "google"},
	{ID: "z-ai/glm-5.2:free", Provider: "openrouter", Active: true, ContextWindow: 32768, OwnedBy: "z-ai"},
	{ID: "openrouter/free", Provider: "openrouter", Active: true, ContextWindow: 200000, OwnedBy: "openrouter"},
}

// openrouterDeprecatedModelReplacements maps decommissioned or alias OpenRouter models to active free replacements.
var openrouterDeprecatedModelReplacements = map[string]string{
	"openrouter/auto":                        "openrouter/free",
	"auto":                                   "openrouter/free",
	"deepseek/deepseek-r1:free":              "deepseek/deepseek-v4-flash-0731:free",
	"deepseek-r1:free":                       "deepseek/deepseek-v4-flash-0731:free",
	"deepseek/deepseek-r1":                   "deepseek/deepseek-v4-flash-0731:free",
	"deepseek-r1":                            "deepseek/deepseek-v4-flash-0731:free",
	"deepseek/deepseek-chat:free":            "deepseek/deepseek-v4-flash-0731:free",
	"deepseek-chat:free":                     "deepseek/deepseek-v4-flash-0731:free",
	"meta-llama/llama-3.3-70b-instruct:free": "nvidia/nemotron-3.5-lightning:free",
	"meta-llama/llama-3.1-70b-instruct:free": "nvidia/nemotron-3.5-lightning:free",
	"meta-llama/llama-3.1-8b-instruct:free":  "qwen/qwen3.8-27b:free",
	"meta-llama/llama-3.2-1b-instruct:free":  "qwen/qwen3.8-27b:free",
	"meta-llama/llama-3.2-3b-instruct:free":  "qwen/qwen3.8-27b:free",
	"mistralai/mistral-7b-instruct:free":     "qwen/qwen3.8-27b:free",
	"mistralai/mistral-nemo:free":            "qwen/qwen3.8-27b:free",
	"google/gemma-2-9b-it:free":              "google/gemma-4-31b-it:free",
	"google/gemma-3-12b-it:free":             "google/gemma-4-31b-it:free",
	"google/gemma-3-4b-it:free":              "google/gemma-4-31b-it:free",
	"qwen/qwen-2.5-72b-instruct:free":        "qwen/qwen3.8-27b:free",
	"qwen/qwen-2.5-coder-32b-instruct:free":  "qwen/qwen3.8-27b:free",
}

// RemapOpenRouterModel translates deprecated OpenRouter model names to active replacement model IDs.
func RemapOpenRouterModel(model string) (string, bool) {
	lower := strings.ToLower(strings.TrimSpace(model))
	lower = strings.TrimPrefix(lower, "openrouter/")
	if repl, ok := openrouterDeprecatedModelReplacements[lower]; ok {
		return repl, true
	}
	for dep, repl := range openrouterDeprecatedModelReplacements {
		if strings.HasSuffix(dep, lower) || strings.HasPrefix(lower, dep) {
			return repl, true
		}
	}
	return model, false
}

func isDeprecatedOpenRouter(model string) bool {
	_, ok := RemapOpenRouterModel(model)
	return ok
}

// defaultKiloActiveModels provides the verified baseline active free models from api.kilo.ai/api/gateway/models
var defaultKiloActiveModels = []provider.ModelInfo{
	{ID: "kilo-auto/free", Provider: "kilo", Active: true, ContextWindow: 256000, OwnedBy: "kilo"},
	{ID: "deepseek/deepseek-v4-flash-0731:free", Provider: "kilo", Active: true, ContextWindow: 1048576, OwnedBy: "deepseek"},
	{ID: "nvidia/nemotron-3.5-lightning:free", Provider: "kilo", Active: true, ContextWindow: 1000000, OwnedBy: "nvidia"},
	{ID: "qwen/qwen3.8-27b:free", Provider: "kilo", Active: true, ContextWindow: 262144, OwnedBy: "qwen"},
	{ID: "nvidia/nemotron-3-ultra-550b-a55b:free", Provider: "kilo", Active: true, ContextWindow: 1000000, OwnedBy: "nvidia"},
	{ID: "cohere/north-mini-code:free", Provider: "kilo", Active: true, ContextWindow: 256000, OwnedBy: "cohere"},
	{ID: "poolside/laguna-xs-2.1:free", Provider: "kilo", Active: true, ContextWindow: 262144, OwnedBy: "poolside"},
	{ID: "poolside/laguna-s-2.1:free", Provider: "kilo", Active: true, ContextWindow: 262144, OwnedBy: "poolside"},
	{ID: "dots-studio/dots-3-note-preview:free", Provider: "kilo", Active: true, ContextWindow: 512000, OwnedBy: "dots-studio"},
	{ID: "thinkingmachines/inkling-small:free", Provider: "kilo", Active: true, ContextWindow: 1048576, OwnedBy: "thinkingmachines"},
	{ID: "stepfun/step-3.7-flash:free", Provider: "kilo", Active: true, ContextWindow: 262144, OwnedBy: "stepfun"},
	{ID: "nex-agi/nex-n2.5-pro:free", Provider: "kilo", Active: true, ContextWindow: 262144, OwnedBy: "nex-agi"},
	{ID: "nex-agi/nex-n2.5-mini:free", Provider: "kilo", Active: true, ContextWindow: 262144, OwnedBy: "nex-agi"},
	{ID: "inclusionai/ling-3.0-flash-vl:free", Provider: "kilo", Active: true, ContextWindow: 262144, OwnedBy: "inclusionai"},
	{ID: "openrouter/free", Provider: "kilo", Active: true, ContextWindow: 200000, OwnedBy: "openrouter"},
}

// kiloDeprecatedModelReplacements maps shorthand or alias Kilo model names to active free models.
var kiloDeprecatedModelReplacements = map[string]string{
	"kilo/auto":                 "kilo-auto/free",
	"auto":                      "kilo-auto/free",
	"kilo-auto":                 "kilo-auto/free",
	"free":                      "kilo-auto/free",
	"kilo/free":                 "kilo-auto/free",
	"deepseek-r1":               "deepseek/deepseek-v4-flash-0731:free",
	"deepseek-r1:free":          "deepseek/deepseek-v4-flash-0731:free",
	"deepseek/deepseek-r1":      "deepseek/deepseek-v4-flash-0731:free",
	"deepseek/deepseek-r1:free": "deepseek/deepseek-v4-flash-0731:free",
}

// RemapKiloModel translates shorthand Kilo model names to active free model IDs.
func RemapKiloModel(model string) (string, bool) {
	lower := strings.ToLower(strings.TrimSpace(model))
	lower = strings.TrimPrefix(lower, "kilo/")
	if repl, ok := kiloDeprecatedModelReplacements[lower]; ok {
		return repl, true
	}
	for dep, repl := range kiloDeprecatedModelReplacements {
		if strings.HasSuffix(dep, lower) || strings.HasPrefix(lower, dep) {
			return repl, true
		}
	}
	return model, false
}

func isDeprecatedKilo(model string) bool {
	_, ok := RemapKiloModel(model)
	return ok
}

// defaultMistralActiveModels provides the verified baseline active models from api.mistral.ai on the free experimentation tier.
var defaultMistralActiveModels = []provider.ModelInfo{
	{ID: "codestral-latest", Provider: "mistral", Active: true, ContextWindow: 256000, OwnedBy: "mistralai"},
	{ID: "codestral-2508", Provider: "mistral", Active: true, ContextWindow: 256000, OwnedBy: "mistralai"},
	{ID: "mistral-code-latest", Provider: "mistral", Active: true, ContextWindow: 256000, OwnedBy: "mistralai"},
	{ID: "ministral-8b-latest", Provider: "mistral", Active: true, ContextWindow: 262144, OwnedBy: "mistralai"},
	{ID: "ministral-8b-2512", Provider: "mistral", Active: true, ContextWindow: 262144, OwnedBy: "mistralai"},
	{ID: "ministral-3b-latest", Provider: "mistral", Active: true, ContextWindow: 131072, OwnedBy: "mistralai"},
	{ID: "ministral-3b-2512", Provider: "mistral", Active: true, ContextWindow: 131072, OwnedBy: "mistralai"},
	{ID: "ministral-14b-latest", Provider: "mistral", Active: true, ContextWindow: 262144, OwnedBy: "mistralai"},
	{ID: "ministral-14b-2512", Provider: "mistral", Active: true, ContextWindow: 262144, OwnedBy: "mistralai"},
	{ID: "voxtral-small-latest", Provider: "mistral", Active: true, ContextWindow: 32768, OwnedBy: "mistralai"},
}

// mistralDeprecatedModelReplacements maps shorthand or alias Mistral model names to working models.
var mistralDeprecatedModelReplacements = map[string]string{
	"codestral":            "codestral-latest",
	"mistral/codestral":    "codestral-latest",
	"ministral-8b":         "ministral-8b-latest",
	"mistral/ministral-8b": "ministral-8b-latest",
	"ministral-3b":         "ministral-3b-latest",
	"mistral/ministral-3b": "ministral-3b-latest",
	"mistral-small":        "ministral-8b-latest",
	"mistral-small-latest": "ministral-8b-latest",
	"open-mistral-7b":      "ministral-8b-latest",
	"mistral-tiny":         "ministral-3b-latest",
}

// RemapMistralModel translates deprecated or shorthand Mistral model names to active working models.
func RemapMistralModel(model string) (string, bool) {
	lower := strings.ToLower(strings.TrimSpace(model))
	lower = strings.TrimPrefix(lower, "mistral/")
	if strings.HasSuffix(lower, ":free") {
		return model, false
	}
	if repl, ok := mistralDeprecatedModelReplacements[lower]; ok {
		return repl, true
	}
	for dep, repl := range mistralDeprecatedModelReplacements {
		if strings.HasSuffix(dep, lower) || strings.HasPrefix(lower, dep) {
			return repl, true
		}
	}
	return model, false
}

func isDeprecatedMistral(model string) bool {
	if strings.HasSuffix(strings.ToLower(model), ":free") {
		return false
	}
	_, ok := RemapMistralModel(model)
	return ok
}

// Route defines an ordered fallback sequence of provider targets.
type Route struct {
	ID       string
	Strategy string
	Targets  []TargetSpec
}

// Router orchestrates multi-provider dispatching, circuit breaking, and resilient fallbacks.
type Router struct {
	mu           sync.RWMutex
	cfg          *config.Config
	providers    map[string]provider.ProviderClient
	breakers     map[string]*CircuitBreaker
	routes       map[string]Route
	translator   *Translator
	activeModels map[string][]provider.ModelInfo
	modelsMu     sync.RWMutex
}

// NewRouter initializes the router with configured provider clients and default fallback routes.
func NewRouter(cfg *config.Config) *Router {
	r := &Router{
		cfg:          cfg,
		providers:    make(map[string]provider.ProviderClient),
		breakers:     make(map[string]*CircuitBreaker),
		routes:       make(map[string]Route),
		translator:   NewTranslator(),
		activeModels: make(map[string][]provider.ModelInfo),
	}

	// Register Standard Providers
	r.registerProvider(openai.NewAdapter("openai", provider.TierPremium, cfg.Providers.OpenAI.BaseURL, cfg.Providers.OpenAI.APIKey))
	r.registerProvider(anthropic.NewAdapter(cfg.Providers.Anthropic.BaseURL, cfg.Providers.Anthropic.APIKey))

	nimURL := cfg.Providers.NVIDIANIM.BaseURL
	if nimURL == "" {
		nimURL = "https://integrate.api.nvidia.com/v1"
	}
	r.registerProvider(openai.NewAdapter("nvidianim", provider.TierFree, nimURL, cfg.Providers.NVIDIANIM.APIKey))

	groqURL := cfg.Providers.Groq.BaseURL
	if groqURL == "" {
		groqURL = "https://api.groq.com/openai/v1"
	}
	r.registerProvider(openai.NewAdapter("groq", provider.TierFree, groqURL, cfg.Providers.Groq.APIKey))

	r.registerProvider(gemini.NewAdapter(cfg.Providers.Gemini.BaseURL, cfg.Providers.Gemini.APIKey))

	orURL := cfg.Providers.OpenRouter.BaseURL
	if orURL == "" {
		orURL = "https://openrouter.ai/api/v1"
	}
	r.registerProvider(openai.NewOpenRouterAdapter(cfg.Providers.OpenRouter.APIKey, orURL))

	r.registerProvider(openai.NewOllamaAdapter(cfg.Providers.Ollama.BaseURL))

	kiloURL := cfg.Providers.Kilo.BaseURL
	if kiloURL == "" {
		kiloURL = "https://api.kilo.ai/api/gateway"
	}
	r.registerProvider(openai.NewKiloAdapter(cfg.Providers.Kilo.APIKey, kiloURL))

	mistralURL := cfg.Providers.Mistral.BaseURL
	if mistralURL == "" {
		mistralURL = "https://api.mistral.ai/v1"
	}
	r.registerProvider(openai.NewMistralAdapter(cfg.Providers.Mistral.APIKey, mistralURL))

	// Register Default Fallback Routes
	r.initDefaultRoutes()

	// Seed Groq active models and start non-blocking discovery
	r.seedActiveModels()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = r.SyncAllProviderModels(ctx)
	}()

	return r
}

func (r *Router) registerProvider(client provider.ProviderClient) {
	name := client.Name()
	r.providers[name] = client
	r.breakers[name] = NewCircuitBreaker(name)
}

func (r *Router) initDefaultRoutes() {
	// 1. auto-resilient: Claude -> Groq (Qwen -> GPT-120B -> GPT-20B) -> Gemini (3.8 -> 3.7 -> 3.6 -> 3.5-lite) -> NVIDIA NIM (Llama-11B -> Nemotron 30B/120B -> Poolside -> GPT-20B -> Nemotron Omni/550B) -> OpenRouter -> Mistral -> Kilo
	r.routes["auto-resilient"] = Route{
		ID:       "auto-resilient",
		Strategy: "fallback",
		Targets: []TargetSpec{
			{ProviderName: "anthropic", UpstreamModel: "claude-sonnet-5"},
			{ProviderName: "groq", UpstreamModel: "qwen/qwen3.8-27b"},
			{ProviderName: "groq", UpstreamModel: "openai/gpt-oss-120b"},
			{ProviderName: "groq", UpstreamModel: "openai/gpt-oss-20b"},
			{ProviderName: "gemini", UpstreamModel: "gemini-3.8-flash"},
			{ProviderName: "gemini", UpstreamModel: "gemini-3.7-flash"},
			{ProviderName: "gemini", UpstreamModel: "gemini-3.6-flash"},
			{ProviderName: "gemini", UpstreamModel: "gemini-3.5-flash-lite"},
			{ProviderName: "nvidianim", UpstreamModel: "deepseek-ai/deepseek-v4-flash-0731"},
			{ProviderName: "nvidianim", UpstreamModel: "google/gemma-4-31b-it"},
			{ProviderName: "nvidianim", UpstreamModel: "nvidia/nemotron-3.5-lightning-30b-a3b"},
			{ProviderName: "nvidianim", UpstreamModel: "nvidia/nemotron-3-super-120b-a12b"},
			{ProviderName: "nvidianim", UpstreamModel: "nvidia/nemotron-3-nano-omni-30b-a3b-reasoning"},
			{ProviderName: "nvidianim", UpstreamModel: "meta/llama-3.2-11b-vision-instruct"},
			{ProviderName: "nvidianim", UpstreamModel: "poolside/laguna-xs-2.1"},
			{ProviderName: "nvidianim", UpstreamModel: "google/diffusiongemma-26b-a4b-it"},
			{ProviderName: "nvidianim", UpstreamModel: "openai/gpt-oss-20b"},
			{ProviderName: "nvidianim", UpstreamModel: "nvidia/nemotron-3-ultra-550b-a55b"},
			{ProviderName: "openrouter", UpstreamModel: "deepseek/deepseek-v4-flash-0731:free"},
			{ProviderName: "openrouter", UpstreamModel: "google/gemma-4-31b-it:free"},
			{ProviderName: "openrouter", UpstreamModel: "openrouter/free"},
			{ProviderName: "mistral", UpstreamModel: "codestral-latest"},
			{ProviderName: "mistral", UpstreamModel: "ministral-8b-latest"},
			{ProviderName: "kilo", UpstreamModel: "kilo-auto/free"},
			{ProviderName: "kilo", UpstreamModel: "deepseek/deepseek-v4-flash-0731:free"},
		},
	}

	// 2. free-first: Groq -> Gemini Free (3 Keys) -> NVIDIA NIM -> OpenRouter Free -> Mistral Free -> Kilo Free
	r.routes["free-first"] = Route{
		ID:       "free-first",
		Strategy: "free_first",
		Targets: []TargetSpec{
			{ProviderName: "groq", UpstreamModel: "qwen/qwen3.8-27b"},
			{ProviderName: "groq", UpstreamModel: "openai/gpt-oss-120b"},
			{ProviderName: "groq", UpstreamModel: "openai/gpt-oss-20b"},
			{ProviderName: "gemini", UpstreamModel: "gemini-3.8-flash"},
			{ProviderName: "gemini", UpstreamModel: "gemini-3.7-flash"},
			{ProviderName: "gemini", UpstreamModel: "gemini-3.6-flash"},
			{ProviderName: "gemini", UpstreamModel: "gemini-3.5-flash-lite"},
			{ProviderName: "nvidianim", UpstreamModel: "deepseek-ai/deepseek-v4-flash-0731"},
			{ProviderName: "nvidianim", UpstreamModel: "google/gemma-4-31b-it"},
			{ProviderName: "nvidianim", UpstreamModel: "nvidia/nemotron-3.5-lightning-30b-a3b"},
			{ProviderName: "nvidianim", UpstreamModel: "nvidia/nemotron-3-super-120b-a12b"},
			{ProviderName: "nvidianim", UpstreamModel: "nvidia/nemotron-3-nano-omni-30b-a3b-reasoning"},
			{ProviderName: "nvidianim", UpstreamModel: "meta/llama-3.2-11b-vision-instruct"},
			{ProviderName: "nvidianim", UpstreamModel: "poolside/laguna-xs-2.1"},
			{ProviderName: "nvidianim", UpstreamModel: "google/diffusiongemma-26b-a4b-it"},
			{ProviderName: "nvidianim", UpstreamModel: "openai/gpt-oss-20b"},
			{ProviderName: "nvidianim", UpstreamModel: "nvidia/nemotron-3-ultra-550b-a55b"},
			{ProviderName: "openrouter", UpstreamModel: "deepseek/deepseek-v4-flash-0731:free"},
			{ProviderName: "openrouter", UpstreamModel: "google/gemma-4-31b-it:free"},
			{ProviderName: "openrouter", UpstreamModel: "openrouter/free"},
			{ProviderName: "mistral", UpstreamModel: "codestral-latest"},
			{ProviderName: "mistral", UpstreamModel: "ministral-8b-latest"},
			{ProviderName: "kilo", UpstreamModel: "kilo-auto/free"},
			{ProviderName: "kilo", UpstreamModel: "deepseek/deepseek-v4-flash-0731:free"},
		},
	}

	// 3. premium-only: Direct Frontier API
	r.routes["premium-only"] = Route{
		ID:       "premium-only",
		Strategy: "fallback",
		Targets: []TargetSpec{
			{ProviderName: "anthropic", UpstreamModel: "claude-opus-5"},
			{ProviderName: "openai", UpstreamModel: "gpt-4o"},
		},
	}

	// Initialize target-level circuit breakers for all targets in routes
	for _, route := range r.routes {
		for _, target := range route.Targets {
			key := target.ProviderName + "/" + target.UpstreamModel
			if _, exists := r.breakers[key]; !exists {
				r.breakers[key] = NewCircuitBreaker(key)
			}
		}
	}
}

// DefaultStrategy returns the active default routing strategy.
func (r *Router) DefaultStrategy() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.cfg != nil && r.cfg.Routes.DefaultStrategy != "" {
		return r.cfg.Routes.DefaultStrategy
	}
	return "auto-resilient"
}

// SetDefaultStrategy dynamically updates the default routing strategy.
func (r *Router) SetDefaultStrategy(strategy string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.routes[strategy]; !exists {
		return fmt.Errorf("unknown routing strategy: %q", strategy)
	}

	if r.cfg != nil {
		r.cfg.Routes.DefaultStrategy = strategy
	}
	return nil
}

// ResolveTargets determines the ordered target list based on requested model and route alias.
func (r *Router) ResolveTargets(requestedModel, routeAlias string) []TargetSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// 1. Check explicit route alias header
	if routeAlias != "" {
		if route, exists := r.routes[routeAlias]; exists {
			return route.Targets
		}
	}

	// 2. Check if requested model matches a route name (e.g. "free-first", "premium-only", "auto-resilient")
	if route, exists := r.routes[requestedModel]; exists {
		return route.Targets
	}

	strategy := "auto-resilient"
	if r.cfg != nil && r.cfg.Routes.DefaultStrategy != "" {
		strategy = r.cfg.Routes.DefaultStrategy
	}

	lowerModel := strings.ToLower(requestedModel)

	// 3. Explicit provider prefix matching (e.g. "groq/...", "nvidianim/...", "openrouter/...")
	if strings.HasPrefix(lowerModel, "groq/") || isDeprecatedGroq(requestedModel) {
		actualModel := requestedModel
		if strings.HasPrefix(lowerModel, "groq/") {
			candidate := strings.TrimPrefix(requestedModel, "groq/")
			for _, m := range r.GetProviderActiveModels("groq") {
				if strings.EqualFold(m.ID, candidate) {
					actualModel = m.ID
					break
				}
			}
		}
		if repl, isDep := RemapGroqModel(actualModel); isDep {
			actualModel = repl
		}
		targets := []TargetSpec{
			{ProviderName: "groq", UpstreamModel: actualModel},
		}
		for _, t := range r.routes["free-first"].Targets {
			if t.ProviderName == "groq" && t.UpstreamModel == actualModel {
				continue
			}
			targets = append(targets, t)
		}
		return targets
	}

	if strings.HasPrefix(lowerModel, "nvidianim/") || isDeprecatedNVIDIANIM(requestedModel) {
		actualModel := requestedModel
		if strings.HasPrefix(lowerModel, "nvidianim/") {
			candidate := strings.TrimPrefix(requestedModel, "nvidianim/")
			for _, m := range r.GetProviderActiveModels("nvidianim") {
				if strings.EqualFold(m.ID, candidate) {
					actualModel = m.ID
					break
				}
			}
		}
		if repl, isDep := RemapNVIDIANIMModel(actualModel); isDep {
			actualModel = repl
		}
		targets := []TargetSpec{
			{ProviderName: "nvidianim", UpstreamModel: actualModel},
		}
		for _, t := range r.routes["free-first"].Targets {
			if t.ProviderName == "nvidianim" && t.UpstreamModel == actualModel {
				continue
			}
			targets = append(targets, t)
		}
		return targets
	}

	if strings.HasPrefix(lowerModel, "openrouter/") || strings.HasSuffix(lowerModel, ":free") || isDeprecatedOpenRouter(requestedModel) {
		actualModel := requestedModel
		if strings.HasPrefix(lowerModel, "openrouter/") {
			candidate := strings.TrimPrefix(requestedModel, "openrouter/")
			for _, m := range r.GetProviderActiveModels("openrouter") {
				if strings.EqualFold(m.ID, candidate) {
					actualModel = m.ID
					break
				}
			}
		}
		if repl, isDep := RemapOpenRouterModel(actualModel); isDep {
			actualModel = repl
		}
		targets := []TargetSpec{
			{ProviderName: "openrouter", UpstreamModel: actualModel},
		}
		for _, t := range r.routes["free-first"].Targets {
			if t.ProviderName == "openrouter" && t.UpstreamModel == actualModel {
				continue
			}
			targets = append(targets, t)
		}
		return targets
	}

	if strings.HasPrefix(lowerModel, "kilo/") || lowerModel == "kilo-auto/free" || isDeprecatedKilo(requestedModel) {
		actualModel := requestedModel
		if strings.HasPrefix(lowerModel, "kilo/") {
			candidate := strings.TrimPrefix(requestedModel, "kilo/")
			for _, m := range r.GetProviderActiveModels("kilo") {
				if strings.EqualFold(m.ID, candidate) {
					actualModel = m.ID
					break
				}
			}
		}
		if repl, isDep := RemapKiloModel(actualModel); isDep {
			actualModel = repl
		}
		targets := []TargetSpec{
			{ProviderName: "kilo", UpstreamModel: actualModel},
		}
		for _, t := range r.routes["free-first"].Targets {
			if t.ProviderName == "kilo" && t.UpstreamModel == actualModel {
				continue
			}
			targets = append(targets, t)
		}
		return targets
	}

	if strings.HasPrefix(lowerModel, "mistral/") || isDeprecatedMistral(requestedModel) {
		actualModel := requestedModel
		if strings.HasPrefix(lowerModel, "mistral/") {
			candidate := strings.TrimPrefix(requestedModel, "mistral/")
			for _, m := range r.GetProviderActiveModels("mistral") {
				if strings.EqualFold(m.ID, candidate) {
					actualModel = m.ID
					break
				}
			}
		}
		if repl, isDep := RemapMistralModel(actualModel); isDep {
			actualModel = repl
		}
		targets := []TargetSpec{
			{ProviderName: "mistral", UpstreamModel: actualModel},
		}
		for _, t := range r.routes["free-first"].Targets {
			if t.ProviderName == "mistral" && t.UpstreamModel == actualModel {
				continue
			}
			targets = append(targets, t)
		}
		return targets
	}

	// 4. If default strategy is explicitly free-first, use free-first targets to reduce token consumption
	if strategy == "free-first" {
		return r.routes["free-first"].Targets
	}

	// 5. If default strategy is premium-only, return premium-only targets
	if strategy == "premium-only" {
		return r.routes["premium-only"].Targets
	}

	// 6. Specific model matching heuristics for auto-resilient mode
	if strings.Contains(lowerModel, "claude") {
		// Target Anthropic first, fallback to all rolling free targets
		targets := []TargetSpec{
			{ProviderName: "anthropic", UpstreamModel: requestedModel},
		}
		targets = append(targets, r.routes["free-first"].Targets...)
		return targets
	}

	if r.IsActiveModel("groq", requestedModel) {
		targets := []TargetSpec{
			{ProviderName: "groq", UpstreamModel: requestedModel},
		}
		for _, t := range r.routes["free-first"].Targets {
			if t.ProviderName == "groq" && t.UpstreamModel == requestedModel {
				continue
			}
			targets = append(targets, t)
		}
		return targets
	}

	if r.IsActiveModel("nvidianim", requestedModel) {
		targets := []TargetSpec{
			{ProviderName: "nvidianim", UpstreamModel: requestedModel},
		}
		for _, t := range r.routes["free-first"].Targets {
			if t.ProviderName == "nvidianim" && t.UpstreamModel == requestedModel {
				continue
			}
			targets = append(targets, t)
		}
		return targets
	}

	if r.IsActiveModel("openrouter", requestedModel) {
		actualModel := requestedModel
		if repl, isDep := RemapOpenRouterModel(actualModel); isDep {
			actualModel = repl
		}
		targets := []TargetSpec{
			{ProviderName: "openrouter", UpstreamModel: actualModel},
		}
		for _, t := range r.routes["free-first"].Targets {
			if t.ProviderName == "openrouter" && t.UpstreamModel == actualModel {
				continue
			}
			targets = append(targets, t)
		}
		return targets
	}

	if r.IsActiveModel("kilo", requestedModel) {
		actualModel := requestedModel
		if repl, isDep := RemapKiloModel(actualModel); isDep {
			actualModel = repl
		}
		targets := []TargetSpec{
			{ProviderName: "kilo", UpstreamModel: actualModel},
		}
		for _, t := range r.routes["free-first"].Targets {
			if t.ProviderName == "kilo" && t.UpstreamModel == actualModel {
				continue
			}
			targets = append(targets, t)
		}
		return targets
	}

	if r.IsActiveModel("mistral", requestedModel) {
		actualModel := requestedModel
		if repl, isDep := RemapMistralModel(actualModel); isDep {
			actualModel = repl
		}
		targets := []TargetSpec{
			{ProviderName: "mistral", UpstreamModel: actualModel},
		}
		for _, t := range r.routes["free-first"].Targets {
			if t.ProviderName == "mistral" && t.UpstreamModel == actualModel {
				continue
			}
			targets = append(targets, t)
		}
		return targets
	}

	if strings.Contains(lowerModel, "llama") || strings.Contains(lowerModel, "free") {
		return r.routes["free-first"].Targets
	}

	// 6. Default to OpenAI target + fallback
	return []TargetSpec{
		{ProviderName: "openai", UpstreamModel: requestedModel},
		{ProviderName: "nvidianim", UpstreamModel: "deepseek-ai/deepseek-v4-flash-0731"},
		{ProviderName: "nvidianim", UpstreamModel: "google/gemma-4-31b-it"},
		{ProviderName: "nvidianim", UpstreamModel: "nvidia/nemotron-3.5-lightning-30b-a3b"},
		{ProviderName: "nvidianim", UpstreamModel: "meta/llama-3.2-11b-vision-instruct"},
		{ProviderName: "nvidianim", UpstreamModel: "poolside/laguna-xs-2.1"},
		{ProviderName: "nvidianim", UpstreamModel: "nvidia/nemotron-3-super-120b-a12b"},
		{ProviderName: "nvidianim", UpstreamModel: "openai/gpt-oss-20b"},
	}
}

// getTargetBreaker returns or initializes a circuit breaker for a specific provider/model target.
func (r *Router) getTargetBreaker(providerName, upstreamModel string) *CircuitBreaker {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := providerName
	if upstreamModel != "" {
		key = providerName + "/" + upstreamModel
	}

	cb, exists := r.breakers[key]
	if !exists {
		cb = NewCircuitBreaker(key)
		r.breakers[key] = cb
	}
	return cb
}

// DispatchChat executes non-streaming chat with automatic failover across target specifications.
func (r *Router) DispatchChat(ctx context.Context, req *provider.UnifiedChatRequest, routeAlias string) (*provider.UnifiedChatResponse, string, error) {
	targets := r.ResolveTargets(req.Model, routeAlias)
	approxTokens := len(req.RawPayload) / 4
	if approxTokens == 0 {
		promptChars := len(req.SystemPrompt)
		for _, m := range req.Messages {
			promptChars += len(m.Content)
		}
		approxTokens = promptChars / 4
	}

	// Filter and prioritize targets based on token context requirements
	var candidateTargets []TargetSpec
	if approxTokens > 25000 {
		// Prompts > 25k tokens: prioritize 1M-context Gemini models first
		for _, t := range targets {
			if t.ProviderName == "gemini" {
				candidateTargets = append(candidateTargets, t)
			}
		}
		// If prompt fits within 120k tokens, include Groq/NVIDIA as secondary fallback
		if approxTokens <= 120000 {
			for _, t := range targets {
				if t.ProviderName != "gemini" && t.ProviderName != "anthropic" {
					candidateTargets = append(candidateTargets, t)
				}
			}
		}
		// If no candidates selected, default to original targets
		if len(candidateTargets) == 0 {
			candidateTargets = targets
		}
	} else {
		// Prompts <= 25k tokens: use default sequence (Groq fast tier first, then Gemini, then NVIDIA NIM)
		candidateTargets = targets
	}

	var lastErr error
	for _, target := range candidateTargets {
		// Strictly bypass providers whose physical context window cannot accommodate prompt
		if approxTokens > 120000 && (target.ProviderName == "groq" || (target.ProviderName == "nvidianim" && target.UpstreamModel != "nvidia/nemotron-3-ultra-550b-a55b")) {
			telemetry.Log.Debug().
				Str("provider", target.ProviderName).
				Str("model", target.UpstreamModel).
				Int("approx_tokens", approxTokens).
				Msg("Prompt exceeds provider context window; bypassing to large-context target")
			continue
		}

		// Strictly ensure only active models are dispatched to Groq
		if target.ProviderName == "groq" {
			if repl, isDep := RemapGroqModel(target.UpstreamModel); isDep {
				telemetry.Log.Info().
					Str("deprecated_model", target.UpstreamModel).
					Str("replacement_model", repl).
					Msg("Remapping deprecated Groq model to active replacement")
				target.UpstreamModel = repl
			}
			if !r.IsActiveModel("groq", target.UpstreamModel) {
				telemetry.Log.Warn().
					Str("provider", "groq").
					Str("model", target.UpstreamModel).
					Msg("Groq model is not in active models catalog, skipping target")
				continue
			}
		}

		// Strictly ensure only active models are dispatched to NVIDIA NIM
		if target.ProviderName == "nvidianim" {
			if repl, isDep := RemapNVIDIANIMModel(target.UpstreamModel); isDep {
				telemetry.Log.Info().
					Str("deprecated_model", target.UpstreamModel).
					Str("replacement_model", repl).
					Msg("Remapping deprecated NVIDIA NIM model to active replacement")
				target.UpstreamModel = repl
			}
			if !r.IsActiveModel("nvidianim", target.UpstreamModel) {
				telemetry.Log.Warn().
					Str("provider", "nvidianim").
					Str("model", target.UpstreamModel).
					Msg("NVIDIA NIM model is not in active models catalog, skipping target")
				continue
			}
		}

		// Strictly ensure only active models are dispatched to OpenRouter
		if target.ProviderName == "openrouter" {
			if repl, isDep := RemapOpenRouterModel(target.UpstreamModel); isDep {
				telemetry.Log.Info().
					Str("deprecated_model", target.UpstreamModel).
					Str("replacement_model", repl).
					Msg("Remapping deprecated OpenRouter model to active replacement")
				target.UpstreamModel = repl
			}
			if !r.IsActiveModel("openrouter", target.UpstreamModel) {
				telemetry.Log.Warn().
					Str("provider", "openrouter").
					Str("model", target.UpstreamModel).
					Msg("OpenRouter model is not in active models catalog, skipping target")
				continue
			}
		}

		// Strictly ensure only active models are dispatched to Kilo
		if target.ProviderName == "kilo" {
			if repl, isDep := RemapKiloModel(target.UpstreamModel); isDep {
				telemetry.Log.Info().
					Str("deprecated_model", target.UpstreamModel).
					Str("replacement_model", repl).
					Msg("Remapping deprecated Kilo model to active replacement")
				target.UpstreamModel = repl
			}
			if !r.IsActiveModel("kilo", target.UpstreamModel) {
				telemetry.Log.Warn().
					Str("provider", "kilo").
					Str("model", target.UpstreamModel).
					Msg("Kilo model is not in active models catalog, skipping target")
				continue
			}
		}

		// Strictly ensure only active models are dispatched to Mistral
		if target.ProviderName == "mistral" {
			if repl, isDep := RemapMistralModel(target.UpstreamModel); isDep {
				telemetry.Log.Info().
					Str("deprecated_model", target.UpstreamModel).
					Str("replacement_model", repl).
					Msg("Remapping deprecated Mistral model to active replacement")
				target.UpstreamModel = repl
			}
			if !r.IsActiveModel("mistral", target.UpstreamModel) {
				telemetry.Log.Warn().
					Str("provider", "mistral").
					Str("model", target.UpstreamModel).
					Msg("Mistral model is not in active models catalog, skipping target")
				continue
			}
		}

		p, exists := r.providers[target.ProviderName]
		if !exists {
			continue
		}

		cb := r.getTargetBreaker(target.ProviderName, target.UpstreamModel)
		if !cb.Allow() {
			telemetry.Log.Warn().
				Str("provider", target.ProviderName).
				Str("model", target.UpstreamModel).
				Msg("Circuit breaker OPEN, skipping target in fallback chain")
			continue
		}

		// Adjust request model for the specific target
		targetReq := *req
		targetReq.Model = target.UpstreamModel

		resp, err := p.SendChat(ctx, &targetReq)
		if err == nil {
			cb.RecordSuccess()
			return resp, target.ProviderName, nil
		}

		if isCircuitBreakerError(err) {
			cb.RecordFailure()
			// If rate limited or quota exhausted (429/RESOURCE_EXHAUSTED), trip immediately to allow rapid rolling failover
			errLower := strings.ToLower(err.Error())
			if strings.Contains(errLower, "429") || strings.Contains(errLower, "resource_exhausted") || strings.Contains(errLower, "quota") {
				cb.TripImmediate()
			}
		}
		lastErr = err
		telemetry.Log.Warn().
			Str("failed_provider", target.ProviderName).
			Str("failed_model", target.UpstreamModel).
			Err(err).
			Msg("Provider target failed, failing over to next target in rolling sequence")
	}

	return nil, "", fmt.Errorf("all providers in fallback chain failed: %w", lastErr)
}

// ResetCircuitBreakers resets all circuit breakers to CLOSED.
func (r *Router) ResetCircuitBreakers() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, cb := range r.breakers {
		cb.Reset()
	}
}

// ResetCircuitBreaker resets a single named circuit breaker to CLOSED.
func (r *Router) ResetCircuitBreaker(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cb, exists := r.breakers[strings.ToLower(name)]; exists && cb != nil {
		cb.Reset()
		return true
	}
	if cb, exists := r.breakers[name]; exists && cb != nil {
		cb.Reset()
		return true
	}
	return false
}

// isCircuitBreakerError returns true if the error indicates a downstream server outage,
// network timeout, or rate-limit exhaustion that should contribute to tripping the circuit breaker.
// Client errors (HTTP 400 Bad Request, 401 Unauthorized, 403 Forbidden, 404 Not Found)
// are client-side or payload issues, not provider infrastructure outages.
func isCircuitBreakerError(err error) bool {
	if err == nil {
		return false
	}
	errStr := strings.ToLower(err.Error())
	if strings.Contains(errStr, "status 400") ||
		strings.Contains(errStr, "error 400") ||
		strings.Contains(errStr, "status 401") ||
		strings.Contains(errStr, "status 403") ||
		strings.Contains(errStr, "status 404") ||
		strings.Contains(errStr, "invalid_argument") {
		return false
	}
	return true
}


// GetProvider retrieves a registered provider client.
func (r *Router) GetProvider(name string) (provider.ProviderClient, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.providers[name]
	return p, ok
}

// GetBreaker retrieves a provider's circuit breaker.
func (r *Router) GetBreaker(name string) (*CircuitBreaker, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	b, ok := r.breakers[name]
	return b, ok
}

// CircuitBreakers returns a snapshot map of all registered circuit breakers.
func (r *Router) CircuitBreakers() map[string]*CircuitBreaker {
	r.mu.RLock()
	defer r.mu.RUnlock()
	res := make(map[string]*CircuitBreaker, len(r.breakers))
	for k, v := range r.breakers {
		res[k] = v
	}
	return res
}

// CircuitBreakerSnapshots returns an ordered snapshot slice of all active circuit breakers.
func (r *Router) CircuitBreakerSnapshots() []CircuitBreakerSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	res := make([]CircuitBreakerSnapshot, 0, len(r.breakers))
	for _, cb := range r.breakers {
		if cb != nil {
			res = append(res, cb.Snapshot())
		}
	}
	sort.Slice(res, func(i, j int) bool {
		iHasSlash := strings.Contains(res[i].Name, "/")
		jHasSlash := strings.Contains(res[j].Name, "/")
		if iHasSlash != jHasSlash {
			return !iHasSlash
		}
		return res[i].Name < res[j].Name
	})
	return res
}

// UpdateProvider dynamically updates credentials and base URL for a named provider.
func (r *Router) UpdateProvider(name string, creds config.ProviderCreds) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	name = strings.ToLower(name)
	if r.cfg != nil {
		switch name {
		case "openai":
			r.cfg.Providers.OpenAI = creds
		case "anthropic":
			r.cfg.Providers.Anthropic = creds
		case "nvidianim":
			r.cfg.Providers.NVIDIANIM = creds
		case "groq":
			r.cfg.Providers.Groq = creds
		case "gemini":
			r.cfg.Providers.Gemini = creds
		case "openrouter":
			r.cfg.Providers.OpenRouter = creds
		case "ollama":
			r.cfg.Providers.Ollama = creds
		case "kilo":
			r.cfg.Providers.Kilo = creds
		case "mistral":
			r.cfg.Providers.Mistral = creds
		}
	}

	p, exists := r.providers[name]
	if !exists {
		return fmt.Errorf("unknown provider: %s", name)
	}

	type keySetter interface {
		SetAPIKey(string)
	}
	type urlSetter interface {
		SetBaseURL(string)
	}

	if ks, ok := p.(keySetter); ok {
		ks.SetAPIKey(creds.APIKey)
	}
	if us, ok := p.(urlSetter); ok && creds.BaseURL != "" {
		us.SetBaseURL(creds.BaseURL)
	}

	// Reset circuit breaker to CLOSED when credentials are updated
	if cb, exists := r.breakers[name]; exists {
		cb.Reset()
	}

	// Trigger asynchronous resync of provider models if applicable
	go func(pName string) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = r.SyncProviderModels(ctx, pName)
	}(name)

	return nil
}

// TestProvider runs an active health check against a provider and returns round-trip latency.
func (r *Router) TestProvider(ctx context.Context, name string) (bool, int64, error) {
	r.mu.RLock()
	p, exists := r.providers[strings.ToLower(name)]
	r.mu.RUnlock()

	if !exists {
		return false, 0, fmt.Errorf("unknown provider: %s", name)
	}

	start := time.Now()
	ok, err := p.CheckHealth(ctx)
	latencyMs := time.Since(start).Milliseconds()

	if ok && err == nil {
		if cb, ok := r.GetBreaker(name); ok {
			cb.RecordSuccess()
		}
		return true, latencyMs, nil
	}

	return false, latencyMs, err
}

// Translator returns the cross-protocol translator instance.
func (r *Router) Translator() *Translator {
	return r.translator
}

func (r *Router) seedActiveModels() {
	r.modelsMu.Lock()
	defer r.modelsMu.Unlock()
	r.activeModels["groq"] = append([]provider.ModelInfo(nil), defaultGroqActiveModels...)
	r.activeModels["nvidianim"] = append([]provider.ModelInfo(nil), defaultNVIDIANIMActiveModels...)
	r.activeModels["openrouter"] = append([]provider.ModelInfo(nil), defaultOpenRouterActiveModels...)
	r.activeModels["kilo"] = append([]provider.ModelInfo(nil), defaultKiloActiveModels...)
	r.activeModels["mistral"] = append([]provider.ModelInfo(nil), defaultMistralActiveModels...)
}

// SyncProviderModels retrieves the current active models from the specified provider client.
func (r *Router) SyncProviderModels(ctx context.Context, providerName string) ([]provider.ModelInfo, error) {
	providerName = strings.ToLower(providerName)
	r.mu.RLock()
	p, exists := r.providers[providerName]
	r.mu.RUnlock()

	if !exists {
		return nil, fmt.Errorf("unknown provider %q", providerName)
	}

	lister, ok := p.(provider.ModelLister)
	if !ok {
		return r.GetProviderActiveModels(providerName), nil
	}

	models, err := lister.ListModels(ctx)
	if err != nil {
		telemetry.Log.Warn().
			Str("provider", providerName).
			Err(err).
			Msg("Failed to dynamically sync provider models, using cached/default catalog")
		return r.GetProviderActiveModels(providerName), err
	}

	// Strictly filter active models
	var filtered []provider.ModelInfo
	for _, m := range models {
		if providerName == "groq" && !m.Active {
			continue
		}
		filtered = append(filtered, m)
	}

	if len(filtered) > 0 {
		r.modelsMu.Lock()
		r.activeModels[providerName] = filtered
		r.modelsMu.Unlock()

		// Ensure circuit breakers exist for discovered active models
		for _, m := range filtered {
			key := providerName + "/" + m.ID
			r.mu.Lock()
			if _, cbExists := r.breakers[key]; !cbExists {
				r.breakers[key] = NewCircuitBreaker(key)
			}
			r.mu.Unlock()
		}

		telemetry.Log.Info().
			Str("provider", providerName).
			Int("active_models", len(filtered)).
			Msg("Provider active models dynamically synchronized")
		return filtered, nil
	}

	return r.GetProviderActiveModels(providerName), nil
}

// SyncAllProviderModels queries all registered providers for active models.
func (r *Router) SyncAllProviderModels(ctx context.Context) map[string][]provider.ModelInfo {
	r.mu.RLock()
	names := make([]string, 0, len(r.providers))
	for name := range r.providers {
		names = append(names, name)
	}
	r.mu.RUnlock()

	result := make(map[string][]provider.ModelInfo)
	for _, name := range names {
		models, _ := r.SyncProviderModels(ctx, name)
		if len(models) > 0 {
			result[name] = models
		}
	}
	return result
}

// GetProviderActiveModels returns the cached active models for a provider.
func (r *Router) GetProviderActiveModels(providerName string) []provider.ModelInfo {
	providerName = strings.ToLower(providerName)
	r.modelsMu.RLock()
	defer r.modelsMu.RUnlock()

	if list, ok := r.activeModels[providerName]; ok && len(list) > 0 {
		out := make([]provider.ModelInfo, len(list))
		copy(out, list)
		return out
	}

	if providerName == "groq" {
		out := make([]provider.ModelInfo, len(defaultGroqActiveModels))
		copy(out, defaultGroqActiveModels)
		return out
	}
	if providerName == "nvidianim" {
		out := make([]provider.ModelInfo, len(defaultNVIDIANIMActiveModels))
		copy(out, defaultNVIDIANIMActiveModels)
		return out
	}
	if providerName == "openrouter" {
		out := make([]provider.ModelInfo, len(defaultOpenRouterActiveModels))
		copy(out, defaultOpenRouterActiveModels)
		return out
	}
	if providerName == "kilo" {
		out := make([]provider.ModelInfo, len(defaultKiloActiveModels))
		copy(out, defaultKiloActiveModels)
		return out
	}
	if providerName == "mistral" {
		out := make([]provider.ModelInfo, len(defaultMistralActiveModels))
		copy(out, defaultMistralActiveModels)
		return out
	}

	return nil
}

// IsActiveModel checks whether a given model is actively supported by the provider.
func (r *Router) IsActiveModel(providerName, modelID string) bool {
	providerName = strings.ToLower(providerName)
	models := r.GetProviderActiveModels(providerName)
	for _, m := range models {
		if !m.Active {
			continue
		}
		if strings.EqualFold(m.ID, modelID) {
			return true
		}
		trimmedModel := strings.TrimPrefix(strings.ToLower(modelID), providerName+"/")
		trimmedMID := strings.TrimPrefix(strings.ToLower(m.ID), providerName+"/")
		if trimmedModel == trimmedMID {
			return true
		}
	}
	return false
}

// GetAllActiveModels returns a flat list of all active models across all configured providers.
func (r *Router) GetAllActiveModels(ctx context.Context) []provider.ModelInfo {
	r.modelsMu.RLock()
	var all []provider.ModelInfo
	for _, list := range r.activeModels {
		all = append(all, list...)
	}
	r.modelsMu.RUnlock()

	hasGroq := false
	hasNvidia := false
	hasOpenRouter := false
	hasKilo := false
	hasMistral := false
	for _, m := range all {
		if m.Provider == "groq" {
			hasGroq = true
		}
		if m.Provider == "nvidianim" {
			hasNvidia = true
		}
		if m.Provider == "openrouter" {
			hasOpenRouter = true
		}
		if m.Provider == "kilo" {
			hasKilo = true
		}
		if m.Provider == "mistral" {
			hasMistral = true
		}
	}
	if !hasGroq {
		all = append(all, defaultGroqActiveModels...)
	}
	if !hasNvidia {
		all = append(all, defaultNVIDIANIMActiveModels...)
	}
	if !hasOpenRouter {
		all = append(all, defaultOpenRouterActiveModels...)
	}
	if !hasKilo {
		all = append(all, defaultKiloActiveModels...)
	}
	if !hasMistral {
		all = append(all, defaultMistralActiveModels...)
	}
	return all
}
