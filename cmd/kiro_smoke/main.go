// Package main is a one-shot smoke test for the Kiro AWS Builder ID device-code
// flow. It runs RegisterClient + StartDeviceAuthorization against the real AWS
// SSO OIDC service and prints the verification URL that a real Kiro login would
// open. It does NOT poll for the token — exiting early is fine, AWS will simply
// expire the device code.
//
// Build & run:
//
//	go run ./internal/api/handlers/management/cmd/kiro_smoke
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/kiro"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	log "github.com/sirupsen/logrus"
)

func main() {
	log.SetLevel(log.InfoLevel)

	cfg := &config.Config{}
	client := kiro.NewSSOOIDCClient(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	fmt.Println("=== Kiro AWS Builder ID device-code smoke test ===")

	fmt.Println("Step 1: RegisterClient on us-east-1...")
	regResp, err := client.RegisterClient(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "RegisterClient failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  ClientID:     %s\n", regResp.ClientID)
	fmt.Printf("  ClientSecret: %s...(redacted)\n", regResp.ClientSecret[:8])

	fmt.Println("Step 2: StartDeviceAuthorization...")
	authResp, err := client.StartDeviceAuthorization(ctx, regResp.ClientID, regResp.ClientSecret)
	if err != nil {
		fmt.Fprintf(os.Stderr, "StartDeviceAuthorization failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Println()
	fmt.Println("✅ AWS SSO OIDC handshake successful — Builder ID flow is live.")
	fmt.Println()
	fmt.Println("Verification URL (would open in browser):")
	fmt.Printf("  %s\n", authResp.VerificationURIComplete)
	fmt.Println()
	fmt.Println("User code (also embedded in the URL above):")
	fmt.Printf("  %s\n", authResp.UserCode)
	fmt.Println()
	fmt.Printf("Device code expires in: %d sec | Poll interval: %d sec | Region: us-east-1\n", authResp.ExpiresIn, authResp.Interval)
	fmt.Println()
	fmt.Println("(skipping CreateToken poll — this is just a smoke test of the discovery handshake)")
}
