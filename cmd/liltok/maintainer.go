package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/primaybr/liltok/internal/crypto"
	"github.com/spf13/cobra"
)

func newMaintainerCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "maintainer",
		Short: "Tools for repository maintainers (key generation, moderation)",
	}

	var keygenOut string

	keygenCmd := &cobra.Command{
		Use:   "keygen",
		Short: "Generate an X25519 asymmetric key pair for private cache submissions",
		Long: `Generates a cryptographic key pair for maintainer moderation.
The private key is saved locally with restrictive file permissions (0600).
The public key can be configured in liltok.yaml or distributed to community members for encrypted submissions.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if keygenOut == "" {
				home, err := os.UserHomeDir()
				if err != nil {
					return fmt.Errorf("failed to resolve home directory: %w", err)
				}
				keygenOut = filepath.Join(home, ".liltok", "maintainer.key")
			}

			pair, err := crypto.GenerateKeyPair()
			if err != nil {
				return fmt.Errorf("failed to generate key pair: %w", err)
			}

			dir := filepath.Dir(keygenOut)
			if err := os.MkdirAll(dir, 0755); err != nil {
				return fmt.Errorf("failed to create directory %s: %w", dir, err)
			}

			if err := os.WriteFile(keygenOut, []byte(pair.PrivateKeyStr+"\n"), 0600); err != nil {
				return fmt.Errorf("failed to write private key to %s: %w", keygenOut, err)
			}

			fmt.Println("==================================================================")
			fmt.Println(" Liltok Maintainer Key Pair Generated")
			fmt.Println("==================================================================")
			fmt.Printf(" Private Key File: %s (mode 0600, keep secret!)\n", keygenOut)
			fmt.Printf(" Public Key:      %s\n", pair.PublicKeyStr)
			fmt.Println("==================================================================")
			fmt.Println("To enable maintainer moderation mode, add this to ~/.liltok/liltok.yaml:")
			fmt.Println()
			fmt.Println("maintainer:")
			fmt.Println("  enabled: true")
			fmt.Printf("  private_key_file: %q\n", keygenOut)
			fmt.Printf("  public_key: %q\n", pair.PublicKeyStr)
			fmt.Println("==================================================================")

			return nil
		},
	}
	keygenCmd.Flags().StringVarP(&keygenOut, "out", "o", "", "Destination path for private key (default: ~/.liltok/maintainer.key)")

	cmd.AddCommand(keygenCmd)
	return cmd
}
