package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/primaybr/liltok/internal/config"
	"github.com/spf13/cobra"
)

func newRouteCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "route",
		Short: "Inspect, switch and edit AI routing strategies",
		Long:  "Manage Liltok routing strategies (auto-resilient, free-first, premium-only) dynamically, and create, replace or reset named routes on the running gateway.",
	}

	cmd.AddCommand(newRouteStatusCommand())
	cmd.AddCommand(newRouteSwitchCommand())
	cmd.AddCommand(newRouteSetCommand())
	cmd.AddCommand(newRouteResetCommand())

	return cmd
}

func newRouteStatusCommand() *cobra.Command {
	var gatewayURL string

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show active routing strategy and circuit breaker health",
		RunE: func(cmd *cobra.Command, args []string) error {
			gatewayURL = strings.TrimRight(gatewayURL, "/")
			client := &http.Client{Timeout: 5 * time.Second}
			resp, err := client.Get(gatewayURL + "/api/v1/routes")
			if err != nil {
				cfg, loadErr := config.Load(resolveConfigPath(configPath))
				if loadErr == nil {
					fmt.Printf("Liltok Gateway: OFFLINE\nConfigured Strategy: %s (from liltok.yaml)\n", cfg.Routes.DefaultStrategy)
					return nil
				}
				return fmt.Errorf("failed to connect to gateway at %s: %w", gatewayURL, err)
			}
			defer resp.Body.Close()

			var data struct {
				DefaultStrategy string      `json:"default_strategy"`
				Routes          []routeInfo `json:"routes"`
				CircuitBreakers []struct {
					Provider string `json:"provider"`
					State    string `json:"state"`
				} `json:"circuit_breakers"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
				return fmt.Errorf("failed to parse routes response: %w", err)
			}

			fmt.Println("==================================================")
			fmt.Printf("Active Routing Strategy: %s\n", strings.ToUpper(data.DefaultStrategy))
			fmt.Println("==================================================")
			fmt.Println("\nConfigured Route Profiles:")
			for _, r := range data.Routes {
				marker := "  "
				if r.ID == data.DefaultStrategy {
					marker = "* "
				}
				fmt.Printf("%s[%s] %s%s\n", marker, r.ID, r.Description, r.tag())
				fmt.Printf("    Strategy: %s\n", r.strategy())
				fmt.Printf("    Targets: %s\n", strings.Join(r.Targets, " -> "))
			}

			if len(data.CircuitBreakers) > 0 {
				fmt.Println("\nCircuit Breakers:")
				for _, cb := range data.CircuitBreakers {
					fmt.Printf("  - %-12s: %s\n", cb.Provider, cb.State)
				}
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "http://localhost:8080", "Liltok gateway HTTP endpoint")
	return cmd
}

func newRouteSwitchCommand() *cobra.Command {
	var gatewayURL string

	cmd := &cobra.Command{
		Use:   "switch <strategy>",
		Short: "Switch active routing strategy (auto-resilient, free-first, premium-only)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			strategy := strings.ToLower(strings.TrimSpace(args[0]))
			if strategy != "auto-resilient" && strategy != "free-first" && strategy != "premium-only" {
				return fmt.Errorf("invalid strategy %q. Choose one of: auto-resilient, free-first, premium-only", strategy)
			}

			gatewayURL = strings.TrimRight(gatewayURL, "/")
			payload, _ := json.Marshal(map[string]string{"strategy": strategy})

			client := &http.Client{Timeout: 5 * time.Second}
			resp, err := client.Post(gatewayURL+"/api/v1/routes/strategy", "application/json", bytes.NewReader(payload))
			if err != nil {
				targetCfg := resolveConfigPath(configPath)
				if persistErr := config.PersistDefaultStrategy(targetCfg, strategy); persistErr == nil {
					disp := targetCfg
					if disp == "" {
						disp = "~/.liltok/liltok.yaml"
					}
					fmt.Printf("Gateway offline. Updated %s strategy to %q\n", disp, strategy)
					return nil
				}
				return fmt.Errorf("failed to connect to gateway at %s and failed to update config: %w", gatewayURL, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("gateway returned HTTP %d", resp.StatusCode)
			}

			fmt.Printf("Successfully switched active routing priority to: %s\n", strings.ToUpper(strategy))
			if strategy == "free-first" {
				fmt.Println("Zero-cost routing active: requests will route to Groq, Gemini Free, NVIDIA NIM, and OpenRouter Free ($0.00 spend).")
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "http://localhost:8080", "Liltok gateway HTTP endpoint")
	return cmd
}
