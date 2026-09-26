package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/primaybr/liltok/internal/config"
	"github.com/primaybr/liltok/internal/db"
	"github.com/primaybr/liltok/internal/miner"
	"github.com/primaybr/liltok/internal/share"
	"github.com/spf13/cobra"
)

// skipCurated counts entries that are answers to curated corpus prompts: they already ship in the
// starter pack, so offering them again would only fill the review list.
const skipCurated = "skip_curated"

// currentDenyEnv supplies the machine identity for automatic deny terms; tests replace it.
var currentDenyEnv = share.CurrentDenyEnv

func newShareCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "share",
		Short: "Offer questions from your cache for the shared answer pack (reviewed by you, never automatic)",
	}
	cmd.AddCommand(newShareScanCommand())
	return cmd
}

func newShareScanCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "scan",
		Short: "Find standalone questions in your cache that pass the privacy gate",
		Long: `Reads your local cache, keeps standalone questions that pass the privacy gate (no secrets, personal
data, paths, private URLs, client context, identifying names or pasted code), and stores them as
pending candidates for review. Nothing leaves this machine, and the report shows counts only.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(resolveConfigPath(configPath))
			if err != nil {
				return fmt.Errorf("failed to load config: %w", err)
			}
			// Resolve the path the same way the rest of the CLI does and refuse a missing, empty-path,
			// or zero-byte database outright, rather than letting db.Open silently create and seed a
			// new, empty one at that path: a mistyped or not-yet-initialized storage.db_path would
			// otherwise report zero pending candidates with no indication anything was wrong.
			dbPath := config.ExpandHomeDir(cfg.Storage.DBPath)
			if strings.TrimSpace(dbPath) == "" {
				return fmt.Errorf("storage.db_path is empty")
			}
			info, err := os.Stat(dbPath)
			if err != nil {
				if os.IsNotExist(err) {
					return fmt.Errorf("database not found at %s: run the gateway first or check storage.db_path", dbPath)
				}
				return fmt.Errorf("failed to check database at %s: %w", dbPath, err)
			}
			if info.Size() == 0 {
				return fmt.Errorf("database at %s is empty: run the gateway first or check storage.db_path", dbPath)
			}
			database, err := db.Open(dbPath)
			if err != nil {
				return fmt.Errorf("failed to open database at %s: %w", dbPath, err)
			}
			defer database.Close()

			autoTerms := share.AutoDenyTerms(currentDenyEnv())
			gate := share.NewGate(share.GateConfig{
				DenyTerms:    append(append([]string{}, cfg.Share.DenyTerms...), autoTerms...),
				URLAllowlist: cfg.Share.URLAllowlist,
			})
			ctx := context.Background()
			rep, survivors, err := scanCache(ctx, database, gate)
			if err != nil {
				return err
			}
			res, err := database.ReconcileShareCandidates(ctx, survivors, share.GateVersion)
			if err != nil {
				return fmt.Errorf("failed to store candidates: %w", err)
			}
			counts, err := database.CountShareCandidates(ctx)
			if err != nil {
				return fmt.Errorf("failed to count candidates: %w", err)
			}

			fmt.Println("==================================================================")
			fmt.Println(" liltok Share Scan (nothing leaves this machine)")
			fmt.Printf(" Database:                %s\n", dbPath)
			fmt.Printf(" Deny terms:              %d configured, %d automatic\n", len(cfg.Share.DenyTerms), len(autoTerms))
			fmt.Printf(" Cache entries scanned:   %d\n", rep.scanned)
			fmt.Printf(" Skipped:                 %d\n", total(rep.skips))
			printCounts(rep.skips)
			fmt.Printf(" Rejected by gate:        %d  (gate v%d)\n", total(rep.rejects), share.GateVersion)
			printCounts(rep.rejects)
			fmt.Printf(" Unique candidates:       %d\n", len(survivors))
			fmt.Printf("   %-22s %d\n", "added:", res.Added)
			fmt.Printf("   %-22s %d\n", "requeued:", res.Requeued)
			fmt.Printf("   %-22s %d\n", "dropped:", res.Dropped)
			fmt.Printf(" Pending review:          %d\n", counts[db.ShareStatusPending])
			fmt.Println("==================================================================")
			return nil
		},
	}
}

type scanReport struct {
	scanned int
	skips   map[string]int
	rejects map[string]int
}

// scanCache streams cache entries one at a time (old databases can hold multi-megabyte prompts)
// and returns the unique survivors, keyed by question ID; the first entry seen for an ID wins.
func scanCache(ctx context.Context, database *db.DB, gate *share.Gate) (scanReport, []db.ShareCandidate, error) {
	rep := scanReport{skips: map[string]int{}, rejects: map[string]int{}}
	curated := miner.NewStarterFilter(nil)
	seen := map[string]bool{}
	var survivors []db.ShareCandidate

	rows, err := database.QueryContext(ctx, `SELECT hash, model, normalized_prompt, response_payload FROM cache_entries`)
	if err != nil {
		return rep, nil, fmt.Errorf("failed to read cache entries: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var hash, model, prompt string
		var payload []byte
		if err := rows.Scan(&hash, &model, &prompt, &payload); err != nil {
			return rep, nil, fmt.Errorf("failed to read cache entry: %w", err)
		}
		rep.scanned++
		c, skip := share.Extract(share.ExtractInput{NormalizedPrompt: prompt, ResponsePayload: string(payload)})
		if skip != "" {
			rep.skips[skip]++
			continue
		}
		if v := gate.Check(c.Question); !v.Passed() {
			rep.rejects[v.Rule]++
			continue
		}
		if curated.Check(miner.CacheExportItem{Hash: hash, Model: model, NormalizedPrompt: prompt, ResponsePayload: string(payload)}) == "" {
			rep.skips[skipCurated]++
			continue
		}
		if seen[c.ID] {
			continue
		}
		seen[c.ID] = true
		survivors = append(survivors, db.ShareCandidate{ID: c.ID, Question: c.Question, SourceHash: hash})
	}
	return rep, survivors, rows.Err()
}

func total(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

func printCounts(m map[string]int) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("   %-22s %d\n", k+":", m[k])
	}
}
