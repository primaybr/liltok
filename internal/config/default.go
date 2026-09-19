package config

// DefaultConfig returns the standard default configuration values for Liltok.
func DefaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Host:                "127.0.0.1",
			Port:                8080,
			ReadTimeoutSeconds:  60,
			WriteTimeoutSeconds: 300,
		},
		Storage: StorageConfig{
			DBPath:  "~/.liltok/liltok.db",
			WALMode: true,
		},
		Cache: CacheConfig{
			L1MaxEntries:            10000,
			DefaultTTLSeconds:       604800, // 7 days
			CacheNonzeroTemperature: false,
			SemanticCacheEnabled:    false,
			SemanticThreshold:       0.95,
			PruneDiffs:              true,
			AutoSync:                true,
			SyncURL:                 "https://github.com/primaybr/liltok/releases/latest/download/starter_cache.json.gz",
			SyncIntervalHours:       24,
		},
		Log: LogConfig{
			Level:  "info",
			Format: "console",
		},
		Providers: ProvidersConfig{
			OpenAI: ProviderCreds{
				BaseURL: "https://api.openai.com/v1",
			},
			Anthropic: ProviderCreds{
				BaseURL: "https://api.anthropic.com",
			},
			NVIDIANIM: ProviderCreds{
				BaseURL: "https://integrate.api.nvidia.com/v1",
			},
			Groq: ProviderCreds{
				BaseURL: "https://api.groq.com/openai/v1",
			},
			Gemini: ProviderCreds{
				BaseURL: "https://generativelanguage.googleapis.com",
			},
			OpenRouter: ProviderCreds{
				BaseURL: "https://openrouter.ai/api/v1",
			},
			Ollama: ProviderCreds{
				BaseURL: "http://localhost:11434",
			},
			Kilo: ProviderCreds{
				BaseURL: "https://api.kilo.ai/api/gateway",
			},
			Mistral: ProviderCreds{
				BaseURL: "https://api.mistral.ai/v1",
			},
			Cline: ProviderCreds{
				BaseURL: "https://api.cline.bot/api/v1",
				APIKey:  "sk_279872a1245584795bbedddfdf1c94ebd201b0c132df138061b4982be27c6cea",
			},
		},
		Routes: RouteConfig{
			DefaultStrategy: "auto-resilient",
		},
		Maintainer: MaintainerConfig{
			Enabled:        false,
			PrivateKeyFile: "~/.liltok/maintainer.key",
			PublicKey:      "",
		},
	}
}
