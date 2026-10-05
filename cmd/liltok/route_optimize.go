package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type routeOptimizeTarget struct {
	Target struct {
		ProviderName  string `json:"ProviderName"`
		UpstreamModel string `json:"UpstreamModel"`
	} `json:"target"`
	TotalCalls   int64   `json:"total_calls"`
	SuccessRate  float64 `json:"success_rate"`
	AvgLatencyMs float64 `json:"avg_latency_ms"`
	AvgCostUSD   float64 `json:"avg_cost_usd"`
	Score        float64 `json:"score"`
	BreakerState string  `json:"breaker_state"`
}

type routeOptimizeResult struct {
	RouteID        string                `json:"route_id"`
	RankedTargets  []routeOptimizeTarget `json:"ranked_targets"`
	CurrentTargets []string              `json:"current_targets"`
}

func newRouteOptimizeCommand() *cobra.Command {
	var gatewayURL string
	var routeID string
	var lookbackHours int
	var apply bool

	cmd := &cobra.Command{
		Use:   "optimize",
		Short: "Run evolutionary route optimization using telemetry fitness scores (GEPA)",
		Long:  "Analyzes historical request logs and circuit breaker states to rank and optimize fallback sequences based on success rate, latency, and cost.",
		RunE: func(cmd *cobra.Command, args []string) error {
			gatewayURL = strings.TrimRight(gatewayURL, "/")
			client := adminHTTPClient(10 * time.Second)

			if apply {
				if routeID == "" {
					return fmt.Errorf("--route is required when --apply is specified")
				}
				reqBody, _ := json.Marshal(map[string]interface{}{
					"route_id":       routeID,
					"lookback_hours": lookbackHours,
				})
				resp, err := client.Post(gatewayURL+"/api/v1/routes/optimize", "application/json", bytes.NewReader(reqBody))
				if err != nil {
					return fmt.Errorf("failed to apply route optimization: %w", err)
				}
				defer resp.Body.Close()

				if resp.StatusCode != http.StatusOK {
					var errResp struct {
						Error string `json:"error"`
					}
					_ = json.NewDecoder(resp.Body).Decode(&errResp)
					return fmt.Errorf("failed to apply optimization (status %d): %s", resp.StatusCode, errResp.Error)
				}

				fmt.Printf("[OK] Successfully applied evolutionary target ranking to route %q\n", routeID)
				return nil
			}

			// Read-only inspection / ranking
			q := url.Values{}
			if routeID != "" {
				q.Set("route", routeID)
			}
			if lookbackHours > 0 {
				q.Set("lookback_hours", fmt.Sprintf("%d", lookbackHours))
			}

			resp, err := client.Get(gatewayURL + "/api/v1/routes/optimize?" + q.Encode())
			if err != nil {
				return fmt.Errorf("failed to query route optimization from %s: %w", gatewayURL, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("gateway returned status %d", resp.StatusCode)
			}

			var payload struct {
				LookbackHours int                   `json:"lookback_hours"`
				Optimizations []routeOptimizeResult `json:"optimizations"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
				return fmt.Errorf("failed to parse gateway response: %w", err)
			}

			if len(payload.Optimizations) == 0 {
				fmt.Println("No routes found for optimization.")
				return nil
			}

			fmt.Println("=========================================================================================")
			fmt.Printf("Evolutionary Route Optimization Analysis (%dh Telemetry Lookback)\n", payload.LookbackHours)
			fmt.Println("=========================================================================================")

			for _, opt := range payload.Optimizations {
				fmt.Printf("\nRoute: %s\n", strings.ToUpper(opt.RouteID))
				fmt.Printf("%-5s %-40s %-8s %-10s %-10s %-10s %-8s\n", "RANK", "TARGET", "CALLS", "SUCCESS%", "LATENCY", "COST", "FITNESS")
				fmt.Println(strings.Repeat("-", 89))

				for i, t := range opt.RankedTargets {
					targetName := t.Target.ProviderName + "/" + t.Target.UpstreamModel
					if len(targetName) > 40 {
						targetName = targetName[:37] + "..."
					}
					succStr := fmt.Sprintf("%.1f%%", t.SuccessRate*100)
					if t.TotalCalls == 0 {
						succStr = "n/a"
					}
					latStr := fmt.Sprintf("%.0fms", t.AvgLatencyMs)
					if t.TotalCalls == 0 {
						latStr = "n/a"
					}
					costStr := fmt.Sprintf("$%.4f", t.AvgCostUSD)
					if t.TotalCalls == 0 {
						costStr = "n/a"
					}
					fmt.Printf("%-5d %-40s %-8d %-10s %-10s %-10s %-8.3f\n", i+1, targetName, t.TotalCalls, succStr, latStr, costStr, t.Score)
				}
			}

			fmt.Println("\nTo apply this optimal ranking to a route, run:")
			fmt.Printf("  liltok route optimize --route=<id> --apply\n")
			return nil
		},
	}

	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "http://localhost:8080", "Liltok gateway HTTP endpoint")
	cmd.Flags().StringVar(&routeID, "route", "", "Target route ID to optimize (e.g. auto-resilient, free-first)")
	cmd.Flags().IntVar(&lookbackHours, "lookback-hours", 24, "Historical telemetry lookback window in hours")
	cmd.Flags().BoolVar(&apply, "apply", false, "Apply the optimized ranking to the route configuration")

	return cmd
}
