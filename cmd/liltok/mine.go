package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/primaybr/liltok/internal/config"
	"github.com/primaybr/liltok/internal/db"
	"github.com/primaybr/liltok/internal/miner"
	"github.com/spf13/cobra"
)

func newMineCommand() *cobra.Command {
	var (
		providerFlag     string
		categoryFlag     string
		apiKeyFlag       string
		baseURLFlag      string
		modelFlag        string
		rateLimitFlag    int
		workersFlag      int
		fileFlag         string
		workspaceFlag    string
		exportFlag       string
		targetModelsFlag string
	)

	mineCmd := &cobra.Command{
		Use:   "mine",
		Short: "Mine and pre-warm cache entries using free-tier LLM providers",
		Long: `Liltok Cache Miner uses generous free-tier providers (Groq, NVIDIA NIM, or local Ollama)
to pre-populate your local SQLite cache with canonical responses for common programming queries,
compiler/runtime errors, algorithms, and workspace-specific libraries at $0.00 cost.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			sigChan := make(chan os.Signal, 1)
			signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
			go func() {
				<-sigChan
				fmt.Println("\nReceived interrupt signal, stopping miner gracefully...")
				cancel()
			}()

			// 1. Resolve configuration and database path
			dbPath := resolveDBPath()
			database, err := db.Open(dbPath)
			if err != nil {
				return fmt.Errorf("failed to open database at %s: %w", dbPath, err)
			}
			defer database.Close()

			// Load config to check for provider keys
			cfgPath := resolveConfigPath(configPath)
			cfg, _ := config.Load(cfgPath)

			// 2. Resolve provider API key
			providerName := strings.ToLower(providerFlag)
			apiKey := apiKeyFlag
			if apiKey == "" && cfg != nil {
				switch providerName {
				case "openrouter":
					apiKey = cfg.Providers.OpenRouter.APIKey
					if apiKey == "" {
						apiKey = os.Getenv("OPENROUTER_API_KEY")
					}
				case "groq":
					apiKey = cfg.Providers.Groq.APIKey
					if apiKey == "" {
						apiKey = os.Getenv("GROQ_API_KEY")
					}
				case "nvidianim":
					apiKey = cfg.Providers.NVIDIANIM.APIKey
					if apiKey == "" {
						apiKey = os.Getenv("NVIDIA_API_KEY")
					}
				}
			}

			if apiKey == "" && providerName != "ollama" {
				fmt.Printf("Notice: No API key found for %s. You can provide one via --api-key or set it in ~/.liltok/liltok.yaml\n", providerName)
				fmt.Println("Tip: Free Groq keys can be created instantly at https://console.groq.com/keys")
				return fmt.Errorf("missing API key for free provider %s", providerName)
			}

			// 3. Assemble prompts to mine
			var prompts []miner.PromptItem
			if fileFlag != "" {
				filePrompts, err := miner.LoadPromptsFromFile(fileFlag)
				if err != nil {
					return fmt.Errorf("failed to load prompts from file: %w", err)
				}
				prompts = append(prompts, filePrompts...)
				fmt.Printf("Loaded %d prompts from file: %s\n", len(filePrompts), fileFlag)
			}

			if workspaceFlag != "" {
				wsPrompts := miner.ScanWorkspaceGenerators(workspaceFlag)
				prompts = append(prompts, wsPrompts...)
				fmt.Printf("Synthesized %d contextual prompts from workspace: %s\n", len(wsPrompts), workspaceFlag)
			}

			if fileFlag == "" && workspaceFlag == "" {
				curated := miner.GetCuratedPrompts(categoryFlag)
				prompts = append(prompts, curated...)
				fmt.Printf("Loaded %d curated prompts for category '%s'\n", len(curated), categoryFlag)
			}

			if len(prompts) == 0 {
				fmt.Println("No prompts to mine.")
				return nil
			}

			// 4. Configure Target Models
			var targetModels []string
			if targetModelsFlag != "" {
				for _, m := range strings.Split(targetModelsFlag, ",") {
					m = strings.TrimSpace(m)
					if m != "" {
						targetModels = append(targetModels, m)
					}
				}
			}
			if len(targetModels) == 0 {
				targetModels = []string{
					"claude-sonnet-4-5",
					"claude-haiku-4-5",
					"claude-sonnet-5",
					"claude-opus-5",
					"gpt-4o",
					"gpt-4o-mini",
					"deepseek-chat",
				}
			}

			minerCfg := miner.MinerConfig{
				Provider:     providerName,
				APIKey:       apiKey,
				BaseURL:      baseURLFlag,
				Model:        modelFlag,
				Workers:      workersFlag,
				RateLimitRPM: rateLimitFlag,
				TargetModels: targetModels,
			}

			cacheMiner := miner.NewCacheMiner(minerCfg, database, nil)

			fmt.Println("==================================================================")
			fmt.Printf(" Liltok Cache Miner (%s)\n", providerName)
			fmt.Printf(" Generator Model: %s\n", minerCfg.Model)
			fmt.Printf(" Target Models:   %s\n", strings.Join(targetModels, ", "))
			fmt.Printf(" Rate Limit:      %d RPM across %d worker(s)\n", rateLimitFlag, workersFlag)
			fmt.Printf(" Total Prompts:   %d\n", len(prompts))
			fmt.Println("==================================================================")
			fmt.Println("Mining in progress (Ctrl+C to abort)...")

			start := time.Now()
			stats, err := cacheMiner.MinePrompts(ctx, prompts)
			if err != nil {
				return fmt.Errorf("mining error: %w", err)
			}

			duration := time.Since(start).Round(time.Millisecond)

			fmt.Println("\n==================================================================")
			fmt.Println(" Mining Session Complete:")
			fmt.Printf("   Prompts Processed: %d / %d\n", stats.Completed, stats.TotalPrompts)
			fmt.Printf("   New Cache Entries: %d\n", stats.CacheEntries)
			fmt.Printf("   Tokens Generated:  %d\n", stats.TokensGenerated)
			fmt.Printf("   Errors / Retries:  %d\n", stats.Errors)
			fmt.Printf("   Elapsed Time:      %s\n", duration)
			fmt.Println("==================================================================")

			// 5. Optional Export
			if exportFlag != "" {
				outFile, err := os.Create(exportFlag)
				if err != nil {
					return fmt.Errorf("failed to create export file %s: %w", exportFlag, err)
				}
				defer outFile.Close()

				exported, err := miner.ExportCacheToGz(database, outFile)
				if err != nil {
					return fmt.Errorf("failed to export cache: %w", err)
				}
				fmt.Printf("Successfully exported %d cache entries to %s\n", exported, exportFlag)
			}

			return nil
		},
	}

	mineCmd.Flags().StringVarP(&providerFlag, "provider", "p", "groq", "Free provider ('groq', 'nvidianim', 'ollama')")
	mineCmd.Flags().StringVar(&categoryFlag, "category", "all", "Curated prompt category ('all', 'errors', 'coding', 'devops', 'git')")
	mineCmd.Flags().StringVarP(&apiKeyFlag, "api-key", "k", "", "Override provider API key")
	mineCmd.Flags().StringVarP(&baseURLFlag, "base-url", "u", "", "Custom provider base URL")
	mineCmd.Flags().StringVarP(&modelFlag, "model", "m", "", "Generator model name")
	mineCmd.Flags().IntVarP(&rateLimitFlag, "rate-limit", "r", 30, "Rate limit throttle in requests per minute (RPM)")
	mineCmd.Flags().IntVarP(&workersFlag, "workers", "w", 2, "Number of concurrent mining workers")
	mineCmd.Flags().StringVarP(&fileFlag, "file", "f", "", "Path to text file containing custom prompts to mine")
	mineCmd.Flags().StringVar(&workspaceFlag, "workspace", "", "Path to codebase for contextual dependency scanning")
	mineCmd.Flags().StringVarP(&exportFlag, "export", "e", "", "Path to export mined cache bundle as .json.gz")
	mineCmd.Flags().StringVar(&targetModelsFlag, "target-models", "", "Comma-separated target models to synthesize cache for")

	return mineCmd
}
