package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/primaybr/liltok/internal/router"
	"github.com/spf13/cobra"
)

// routeInfo mirrors one entry of the admin API's GET /api/v1/routes "routes" list and the body
// returned by PUT /api/v1/routes.
type routeInfo struct {
	ID          string   `json:"id"`
	Description string   `json:"description"`
	Strategy    string   `json:"strategy"`
	Targets     []string `json:"targets"`
	BuiltIn     bool     `json:"built_in"`
	Customized  bool     `json:"customized"`
}

// strategy returns the route's strategy, treating an empty value (older gateways) as fallback.
func (r routeInfo) strategy() string {
	if r.Strategy == "" {
		return router.StrategyFallback
	}
	return r.Strategy
}

// tag returns a suffix marking custom routes and edited built-in routes, or "" for a built-in
// route that still has its shipped definition. Gateways that predate route editing send neither
// built_in nor strategy, so a route without a strategy is never tagged custom.
func (r routeInfo) tag() string {
	switch {
	case r.BuiltIn && r.Customized:
		return "  (edited)"
	case !r.BuiltIn && r.Strategy != "":
		return "  (custom)"
	}
	return ""
}

// routeTargetSpec is one {"provider","model"} entry of a PUT /api/v1/routes body.
type routeTargetSpec struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// parseRouteTarget splits "provider/model" at the first slash. Model names may contain further
// slashes and suffixes such as ":free", so everything after the first slash is the model.
func parseRouteTarget(raw string) (routeTargetSpec, error) {
	s := strings.TrimSpace(raw)
	provider, model, ok := strings.Cut(s, "/")
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	if !ok || provider == "" || model == "" {
		return routeTargetSpec{}, fmt.Errorf("invalid target %q: expected provider/model, e.g. groq/llama-3.3-70b-versatile", raw)
	}
	return routeTargetSpec{Provider: provider, Model: model}, nil
}

// gatewayOfflineError explains that route edits need the running gateway, which validates them
// against its registered providers and applies them live.
func gatewayOfflineError(gatewayURL string, err error) error {
	return fmt.Errorf("gateway at %s is not reachable (%v); route editing requires a running gateway, start it with 'liltok start' and retry", gatewayURL, err)
}

// routeAPIError builds an error from a non-2xx admin API response. The admin API reports errors
// as {"error": "message"}; {"error": {"message": "..."}} is accepted too.
func routeAPIError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &envelope) == nil && len(envelope.Error) > 0 {
		var msg string
		if json.Unmarshal(envelope.Error, &msg) == nil && msg != "" {
			return fmt.Errorf("gateway rejected the request (HTTP %d): %s", resp.StatusCode, msg)
		}
		var obj struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(envelope.Error, &obj) == nil && obj.Message != "" {
			return fmt.Errorf("gateway rejected the request (HTTP %d): %s", resp.StatusCode, obj.Message)
		}
	}
	return fmt.Errorf("gateway returned HTTP %d", resp.StatusCode)
}

// printRoute prints one route in the same layout as route status.
func printRoute(r routeInfo) {
	fmt.Printf("  [%s] %s%s\n", r.ID, r.Description, r.tag())
	fmt.Printf("    Strategy: %s\n", r.strategy())
	fmt.Printf("    Targets: %s\n", strings.Join(r.Targets, " -> "))
}

func newRouteSetCommand() *cobra.Command {
	var (
		gatewayURL  string
		targets     []string
		strategy    string
		description string
	)

	cmd := &cobra.Command{
		Use:   "set <id>",
		Short: "Create or replace a named route on the running gateway",
		Long: "Create or replace a named route. Targets are tried in the order given; each is\n" +
			"provider/model and splits at the first slash, so models may contain slashes\n" +
			"(openrouter/deepseek/deepseek-v4-flash-0731:free). Saving a built-in route ID\n" +
			"(auto-resilient, free-first, premium-only) overrides it until 'liltok route reset'.\n" +
			"Requires the running gateway, which validates the route and applies it live.",
		Example: "  liltok route set cheap --target groq/llama-3.3-70b-versatile --target openrouter/deepseek/deepseek-v4-flash-0731:free --strategy least_cost",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := strings.TrimSpace(args[0])
			if id == "" {
				return fmt.Errorf("route id must not be empty")
			}
			if len(targets) == 0 {
				return fmt.Errorf("at least one --target provider/model is required")
			}
			specs := make([]routeTargetSpec, 0, len(targets))
			for _, raw := range targets {
				spec, err := parseRouteTarget(raw)
				if err != nil {
					return err
				}
				specs = append(specs, spec)
			}
			strategy = strings.TrimSpace(strategy)
			if !router.IsKnownStrategy(strategy) {
				return fmt.Errorf("invalid strategy %q. Choose one of: %s", strategy, strings.Join(router.KnownStrategies, ", "))
			}

			payload, err := json.Marshal(map[string]interface{}{
				"id":          id,
				"description": description,
				"strategy":    strategy,
				"targets":     specs,
			})
			if err != nil {
				return fmt.Errorf("encode route: %w", err)
			}

			gatewayURL = strings.TrimRight(gatewayURL, "/")
			req, err := http.NewRequest(http.MethodPut, gatewayURL+"/api/v1/routes", bytes.NewReader(payload))
			if err != nil {
				return fmt.Errorf("build request: %w", err)
			}
			req.Header.Set("Content-Type", "application/json")
			client := adminHTTPClient(5 * time.Second)
			resp, err := client.Do(req)
			if err != nil {
				return gatewayOfflineError(gatewayURL, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return routeAPIError(resp)
			}

			var saved routeInfo
			if err := json.NewDecoder(resp.Body).Decode(&saved); err != nil {
				return fmt.Errorf("failed to parse saved route: %w", err)
			}
			fmt.Printf("Saved route %s:\n", saved.ID)
			printRoute(saved)
			return nil
		},
	}

	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "http://localhost:8080", "Liltok gateway HTTP endpoint")
	cmd.Flags().StringArrayVar(&targets, "target", nil, "route target as provider/model, repeat in priority order (required)")
	cmd.Flags().StringVar(&strategy, "strategy", "", "target ordering: "+strings.Join(router.KnownStrategies, ", ")+" (default fallback)")
	cmd.Flags().StringVar(&description, "description", "", "human-readable route description")
	return cmd
}

func newRouteResetCommand() *cobra.Command {
	var gatewayURL string

	cmd := &cobra.Command{
		Use:   "reset <id>",
		Short: "Restore a built-in route or delete a custom route on the running gateway",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := strings.TrimSpace(args[0])
			if id == "" {
				return fmt.Errorf("route id must not be empty")
			}
			gatewayURL = strings.TrimRight(gatewayURL, "/")
			client := adminHTTPClient(5 * time.Second)

			// Look the route up first: the DELETE response does not say whether a built-in route
			// was restored or a custom one removed.
			resp, err := client.Get(gatewayURL + "/api/v1/routes")
			if err != nil {
				return gatewayOfflineError(gatewayURL, err)
			}
			var list struct {
				Routes []routeInfo `json:"routes"`
			}
			if resp.StatusCode != http.StatusOK {
				apiErr := routeAPIError(resp)
				resp.Body.Close()
				return apiErr
			}
			decodeErr := json.NewDecoder(resp.Body).Decode(&list)
			resp.Body.Close()
			if decodeErr != nil {
				return fmt.Errorf("failed to parse routes response: %w", decodeErr)
			}
			var existing *routeInfo
			for i := range list.Routes {
				if list.Routes[i].ID == id {
					existing = &list.Routes[i]
					break
				}
			}

			req, err := http.NewRequest(http.MethodDelete, gatewayURL+"/api/v1/routes/"+url.PathEscape(id), nil)
			if err != nil {
				return fmt.Errorf("build request: %w", err)
			}
			resp, err = client.Do(req)
			if err != nil {
				return gatewayOfflineError(gatewayURL, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return routeAPIError(resp)
			}

			switch {
			case existing == nil:
				fmt.Printf("Reset route %s\n", id)
			case existing.BuiltIn && existing.Customized:
				fmt.Printf("Restored built-in route %s to its shipped definition\n", id)
			case existing.BuiltIn:
				fmt.Printf("Built-in route %s was not edited; it keeps its shipped definition\n", id)
			default:
				fmt.Printf("Deleted custom route %s\n", id)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&gatewayURL, "gateway-url", "http://localhost:8080", "Liltok gateway HTTP endpoint")
	return cmd
}
