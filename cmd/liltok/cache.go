package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	cachesync "github.com/liltok/liltok/internal/cache/sync"
	"github.com/liltok/liltok/internal/config"
	"github.com/liltok/liltok/internal/db"
	"github.com/liltok/liltok/internal/miner"
	"github.com/spf13/cobra"
)

func newCacheCommand() *cobra.Command {
	cacheCmd := &cobra.Command{
		Use:   "cache",
		Short: "Inspect and manage the local Liltok cache",
	}

	statsCmd := &cobra.Command{
		Use:   "stats",
		Short: "Display summary statistics of the cache",
		RunE: func(cmd *cobra.Command, args []string) error {
			dbPath := resolveDBPath()
			database, err := db.Open(dbPath)
			if err != nil {
				return fmt.Errorf("failed to open database at %s: %w", dbPath, err)
			}
			defer database.Close()

			var totalEntries, exactHits, totalHits int64
			row := database.QueryRowContext(context.Background(), `
				SELECT 
					COUNT(*),
					COALESCE(SUM(CASE WHEN is_semantic = 0 THEN hit_count ELSE 0 END), 0),
					COALESCE(SUM(hit_count), 0)
				FROM cache_entries
			`)
			_ = row.Scan(&totalEntries, &exactHits, &totalHits)

			fmt.Println("==================================================================")
			fmt.Println(" liltok Cache Statistics:")
			fmt.Printf("   Database:         %s\n", dbPath)
			fmt.Printf("   Active Entries:   %d\n", totalEntries)
			fmt.Printf("   Exact Cache Hits: %d\n", exactHits)
			fmt.Printf("   Total Hits:       %d\n", totalHits)
			fmt.Println("==================================================================")
			return nil
		},
	}

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List cached prompt entries",
		RunE: func(cmd *cobra.Command, args []string) error {
			dbPath := resolveDBPath()
			database, err := db.Open(dbPath)
			if err != nil {
				return fmt.Errorf("failed to open database at %s: %w", dbPath, err)
			}
			defer database.Close()

			rows, err := database.QueryContext(context.Background(), `
				SELECT hash, model, normalized_prompt, hit_count, last_accessed_at, is_semantic
				FROM cache_entries
				ORDER BY last_accessed_at DESC
				LIMIT 50
			`)
			if err != nil {
				return fmt.Errorf("failed to query cache entries: %w", err)
			}
			defer rows.Close()

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
			_, _ = fmt.Fprintln(w, "HASH\tMODEL\tTYPE\tHITS\tLAST ACCESSED\tPROMPT PREVIEW")

			count := 0
			for rows.Next() {
				var hash, model, prompt, lastAccess string
				var hits, isSem int
				if err := rows.Scan(&hash, &model, &prompt, &hits, &lastAccess, &isSem); err == nil {
					count++
					shortHash := hash
					if len(shortHash) > 12 {
						shortHash = shortHash[:12] + "..."
					}
					typeStr := "EXACT"
					if isSem == 1 {
						typeStr = "SEMANTIC"
					}
					preview := prompt
					if len(preview) > 50 {
						preview = preview[:50] + "..."
					}
					_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\n",
						shortHash, model, typeStr, hits, lastAccess, preview)
				}
			}
			_ = w.Flush()

			if count == 0 {
				fmt.Println("Cache is currently empty.")
			}
			return nil
		},
	}

	var purgeAll bool
	var purgeModel string

	purgeCmd := &cobra.Command{
		Use:   "purge",
		Short: "Purge entries from the local cache",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !purgeAll && purgeModel == "" {
				return fmt.Errorf("must specify --all or --model <name>")
			}

			dbPath := resolveDBPath()
			database, err := db.Open(dbPath)
			if err != nil {
				return fmt.Errorf("failed to open database at %s: %w", dbPath, err)
			}
			defer database.Close()

			var query string
			var qArgs []interface{}

			if purgeAll {
				query = "DELETE FROM cache_entries"
			} else {
				query = "DELETE FROM cache_entries WHERE model = ?"
				qArgs = append(qArgs, purgeModel)
			}

			res, err := database.ExecContext(context.Background(), query, qArgs...)
			if err != nil {
				return fmt.Errorf("failed to purge cache: %w", err)
			}

			affected, _ := res.RowsAffected()
			fmt.Printf("Successfully purged %d cache entries.\n", affected)
			return nil
		},
	}
	purgeCmd.Flags().BoolVar(&purgeAll, "all", false, "Purge all cache entries")
	purgeCmd.Flags().StringVar(&purgeModel, "model", "", "Purge entries matching specific model name")

	var (
		syncURLFlag string
		forceFlag   bool
	)

	updateCmd := &cobra.Command{
		Use:   "update",
		Short: "Synchronize local cache with latest remote community cache pack",
		Long: `Fetches the latest pre-mined community cache pack from GitHub Releases or remote CDN
using HTTP conditional GET (ETag). Merges new entries seamlessly into your local SQLite store.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			dbPath := resolveDBPath()
			database, err := db.Open(dbPath)
			if err != nil {
				return fmt.Errorf("failed to open database at %s: %w", dbPath, err)
			}
			defer database.Close()

			syncURL := syncURLFlag
			if syncURL == "" {
				cfgPath := configPath
				if cfgPath == "" {
					home, err := os.UserHomeDir()
					if err == nil {
						candidate := filepath.Join(home, ".liltok", "liltok.yaml")
						if _, err := os.Stat(candidate); err == nil {
							cfgPath = candidate
						}
					}
				}
				if cfg, err := config.Load(cfgPath); err == nil && cfg.Cache.SyncURL != "" {
					syncURL = cfg.Cache.SyncURL
				} else {
					syncURL = "https://github.com/liltok/liltok/releases/latest/download/starter_cache.json.gz"
				}
			}

			fmt.Println("==================================================================")
			fmt.Println(" Liltok Cache OTA Synchronizer")
			fmt.Printf(" Remote Source: %s\n", syncURL)
			if forceFlag {
				fmt.Println(" Mode:          Force re-sync (bypassing local ETag)")
			} else {
				fmt.Println(" Mode:          Conditional Check (ETag / 304 Not Modified)")
			}
			fmt.Println("==================================================================")
			fmt.Println("Connecting to remote release...")

			syncer := cachesync.NewCacheSyncer(database, syncURL, nil)
			res, err := syncer.Sync(cmd.Context(), forceFlag)
			if err != nil {
				return fmt.Errorf("sync failed: %w", err)
			}

			if res.UpToDate {
				fmt.Printf("\nYour cache is already up to date! (HTTP 304 Not Modified)\n")
				fmt.Printf("Checked in %s\n", res.Duration.Round(time.Millisecond))
			} else {
				fmt.Printf("\nSuccessfully synchronized %d new cache entries in %s!\n",
					res.NewEntries, res.Duration.Round(time.Millisecond))
				if res.ETag != "" {
					fmt.Printf("Current Release ETag: %s\n", res.ETag)
				}
			}

			var totalCount int
			_ = database.QueryRowContext(cmd.Context(), "SELECT COUNT(*) FROM cache_entries").Scan(&totalCount)
			fmt.Printf("Total Active Cache: %d entries\n", totalCount)
			fmt.Println("==================================================================")

			return nil
		},
	}
	updateCmd.Flags().StringVarP(&syncURLFlag, "url", "u", "", "Override remote sync URL")
	updateCmd.Flags().BoolVarP(&forceFlag, "force", "f", false, "Force download and re-sync even if ETag matches")

	var (
		exportOut      string
		exportMinHits  int
		exportModel    string
		exportSanitize bool
	)

	exportCmd := &cobra.Command{
		Use:   "export",
		Short: "Export sanitized local cache entries for community sharing or backup",
		Long: `Exports high-value cache entries to a compressed .json.gz file.
With --sanitize, private API keys, user home directory paths, and private IPs are scrubbed automatically.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if exportOut == "" {
				exportOut = "community_cache_pack.json.gz"
			}

			dbPath := resolveDBPath()
			database, err := db.Open(dbPath)
			if err != nil {
				return fmt.Errorf("failed to open database at %s: %w", dbPath, err)
			}
			defer database.Close()

			outFile, err := os.Create(exportOut)
			if err != nil {
				return fmt.Errorf("failed to create output file %s: %w", exportOut, err)
			}
			defer outFile.Close()

			opts := miner.ExportOptions{
				MinHits:  exportMinHits,
				Model:    exportModel,
				Sanitize: exportSanitize,
			}

			count, err := miner.ExportCacheWithOptions(database, outFile, opts)
			if err != nil {
				return fmt.Errorf("export failed: %w", err)
			}

			fmt.Println("==================================================================")
			fmt.Println(" Liltok Cache Exporter")
			fmt.Printf(" Output File:       %s\n", exportOut)
			fmt.Printf(" Exported Entries:  %d\n", count)
			fmt.Printf(" Min Hits Filter:   %d\n", exportMinHits)
			fmt.Printf(" Privacy Sanitized: %v\n", exportSanitize)
			fmt.Println("==================================================================")
			return nil
		},
	}
	exportCmd.Flags().StringVarP(&exportOut, "out", "o", "community_cache_pack.json.gz", "Output gzip file path")
	exportCmd.Flags().IntVar(&exportMinHits, "min-hits", 0, "Minimum hits required for export")
	exportCmd.Flags().StringVar(&exportModel, "model", "", "Filter entries by model name")
	exportCmd.Flags().BoolVar(&exportSanitize, "sanitize", true, "Scrub API keys, personal paths, and private IPs")

	var importIn string
	importCmd := &cobra.Command{
		Use:   "import [file.json.gz]",
		Short: "Import a community cache pack into local cache",
		RunE: func(cmd *cobra.Command, args []string) error {
			filePath := importIn
			if filePath == "" && len(args) > 0 {
				filePath = args[0]
			}
			if filePath == "" {
				return fmt.Errorf("must specify path to .json.gz cache file via argument or --file")
			}

			f, err := os.Open(filePath)
			if err != nil {
				return fmt.Errorf("failed to open cache pack %s: %w", filePath, err)
			}
			defer f.Close()

			dbPath := resolveDBPath()
			database, err := db.Open(dbPath)
			if err != nil {
				return fmt.Errorf("failed to open database at %s: %w", dbPath, err)
			}
			defer database.Close()

			imported, err := miner.ImportCacheFromGz(database, f)
			if err != nil {
				return fmt.Errorf("import failed: %w", err)
			}

			fmt.Printf("Successfully imported %d new cache entries from %s!\n", imported, filePath)
			return nil
		},
	}
	importCmd.Flags().StringVarP(&importIn, "file", "f", "", "Path to .json.gz file")

	seedCmd := &cobra.Command{
		Use:   "seed",
		Short: "Seed or update embedded starter cache entries into the local database",
		Long:  `Unpacks the embedded starter cache pack and inserts/updates all canonical entries using INSERT OR REPLACE.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			dbPath := resolveDBPath()
			database, err := db.Open(dbPath)
			if err != nil {
				return fmt.Errorf("failed to open database at %s: %w", dbPath, err)
			}
			defer database.Close()

			seeded, err := database.ForceSeedStarterCache()
			if err != nil {
				return fmt.Errorf("failed to seed starter cache: %w", err)
			}

			var totalCount int
			_ = database.QueryRowContext(cmd.Context(), "SELECT COUNT(*) FROM cache_entries").Scan(&totalCount)

			fmt.Println("==================================================================")
			fmt.Println(" liltok Starter Cache Seeder")
			fmt.Printf(" Database:               %s\n", dbPath)
			fmt.Printf(" Seeded/Updated Entries: %d\n", seeded)
			fmt.Printf(" Total Active Entries:   %d\n", totalCount)
			fmt.Println("==================================================================")
			return nil
		},
	}

	cacheCmd.AddCommand(statsCmd, listCmd, purgeCmd, updateCmd, exportCmd, importCmd, seedCmd)
	return cacheCmd
}
