package main

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/primaybr/liltok/internal/config"
	"github.com/primaybr/liltok/internal/db"
	"github.com/primaybr/liltok/internal/ledger"
	"github.com/spf13/cobra"
)

func resolveDBPath() string {
	cfgPath := resolveConfigPath(configPath)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return "~/.liltok/liltok.db"
	}
	return cfg.Storage.DBPath
}

func newKeysCommand() *cobra.Command {
	keysCmd := &cobra.Command{
		Use:   "keys",
		Short: "Manage virtual API keys, rate limits, and monthly and daily spend quotas",
	}

	var name string
	var budget float64
	var dailyBudget float64
	var rpm int
	var tpm int

	createCmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new virtual API key",
		RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" {
				name = "default-key"
			}

			dbPath := resolveDBPath()
			database, err := db.Open(dbPath)
			if err != nil {
				return fmt.Errorf("failed to open database at %s: %w", dbPath, err)
			}
			defer database.Close()

			km := ledger.NewKeyManager(database)
			rawKey, keyObj, err := km.CreateKeyWithOptions(context.Background(), ledger.KeyOptions{
				Name:             name,
				MonthlyBudgetUSD: budget,
				DailyBudgetUSD:   dailyBudget,
				RPM:              rpm,
				TPM:              tpm,
			})
			if err != nil {
				return fmt.Errorf("failed to create api key: %w", err)
			}

			fmt.Println("==================================================================")
			fmt.Println(" Created Virtual API Key:")
			fmt.Printf("   ID:              %s\n", keyObj.ID)
			fmt.Printf("   Name:            %s\n", keyObj.Name)
			fmt.Printf("   Secret Key:      %s\n", rawKey)
			if keyObj.MonthlyBudgetUSD > 0 {
				fmt.Printf("   Monthly Budget:  $%.2f USD\n", keyObj.MonthlyBudgetUSD)
			} else {
				fmt.Println("   Monthly Budget:  Unlimited")
			}
			if keyObj.DailyBudgetUSD > 0 {
				fmt.Printf("   Daily Budget:    $%.2f USD (resets 00:00 UTC)\n", keyObj.DailyBudgetUSD)
			} else {
				fmt.Println("   Daily Budget:    Unlimited")
			}
			fmt.Printf("   Rate Limits:     %d RPM / %d TPM\n", keyObj.RPM, keyObj.TPM)
			fmt.Println("==================================================================")
			fmt.Println(" Keep this secret key safe! It will not be displayed again.")
			return nil
		},
	}
	createCmd.Flags().StringVarP(&name, "name", "n", "default", "Friendly name for the virtual key")
	createCmd.Flags().Float64VarP(&budget, "budget", "b", 0.0, "Monthly spend cap in USD (0.0 for unlimited)")
	createCmd.Flags().Float64Var(&dailyBudget, "daily-budget", 0.0, "Daily spend cap in USD per UTC day (0.0 for unlimited)")
	createCmd.Flags().IntVar(&rpm, "rpm", 60, "Maximum requests per minute")
	createCmd.Flags().IntVar(&tpm, "tpm", 100000, "Maximum tokens per minute")

	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List all virtual API keys",
		RunE: func(cmd *cobra.Command, args []string) error {
			dbPath := resolveDBPath()
			database, err := db.Open(dbPath)
			if err != nil {
				return fmt.Errorf("failed to open database at %s: %w", dbPath, err)
			}
			defer database.Close()

			km := ledger.NewKeyManager(database)
			keys, err := km.ListKeys(context.Background())
			if err != nil {
				return fmt.Errorf("failed to list keys: %w", err)
			}

			if len(keys) == 0 {
				fmt.Println("No virtual API keys found. Create one with: liltok keys create")
				return nil
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
			_, _ = fmt.Fprintln(w, "ID\tNAME\tSTATUS\tCURRENT SPEND\tMONTHLY BUDGET\tTODAY SPEND\tDAILY BUDGET\tRPM\tTPM")
			for _, k := range keys {
				status := "ACTIVE"
				if !k.IsActive {
					status = "REVOKED"
				}
				budgetStr := "UNLIMITED"
				if k.MonthlyBudgetUSD > 0 {
					budgetStr = fmt.Sprintf("$%.2f", k.MonthlyBudgetUSD)
				}
				dailyStr := "UNLIMITED"
				if k.DailyBudgetUSD > 0 {
					dailyStr = fmt.Sprintf("$%.2f", k.DailyBudgetUSD)
				}
				_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t$%.4f\t%s\t$%.4f\t%s\t%d\t%d\n",
					k.ID, k.Name, status, k.CurrentSpendUSD, budgetStr, k.DailySpendUSD, dailyStr, k.RPM, k.TPM)
			}
			_ = w.Flush()
			return nil
		},
	}

	revokeCmd := &cobra.Command{
		Use:   "revoke [key-id]",
		Short: "Revoke/deactivate a virtual API key",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			keyID := args[0]
			dbPath := resolveDBPath()
			database, err := db.Open(dbPath)
			if err != nil {
				return fmt.Errorf("failed to open database at %s: %w", dbPath, err)
			}
			defer database.Close()

			km := ledger.NewKeyManager(database)
			if err := km.RevokeKey(context.Background(), keyID); err != nil {
				return fmt.Errorf("failed to revoke key %s: %w", keyID, err)
			}

			fmt.Printf("Successfully revoked virtual API key: %s\n", keyID)
			return nil
		},
	}

	keysCmd.AddCommand(createCmd, listCmd, revokeCmd)
	return keysCmd
}
