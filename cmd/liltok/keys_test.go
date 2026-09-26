package main

import (
	"strings"
	"testing"
)

func TestKeysLifecycle(t *testing.T) {
	env := newTestEnv(t, "")

	out, err := env.run(t, "keys", "list")
	if err != nil {
		t.Fatalf("keys list (empty): %v", err)
	}
	assertContains(t, out, "No virtual API keys found")

	out, err = env.run(t, "keys", "create", "--name", "ci-bot", "--budget", "12.5", "--rpm", "30", "--tpm", "5000")
	if err != nil {
		t.Fatalf("keys create: %v", err)
	}
	assertContains(t, out,
		"Name:            ci-bot",
		"Monthly Budget:  $12.50 USD",
		"Rate Limits:     30 RPM / 5000 TPM",
		"It will not be displayed again",
	)
	keyID := fieldAfter(t, out, "ID:")
	if secret := fieldAfter(t, out, "Secret Key:"); secret == "" || secret == keyID {
		t.Errorf("secret key %q not distinct from id %q", secret, keyID)
	}

	out, err = env.run(t, "keys", "create", "-n", "free-tier")
	if err != nil {
		t.Fatalf("keys create unlimited: %v", err)
	}
	assertContains(t, out, "Monthly Budget:  Unlimited", "Rate Limits:     60 RPM / 100000 TPM")

	out, err = env.run(t, "keys", "list")
	if err != nil {
		t.Fatalf("keys list: %v", err)
	}
	assertContains(t, out, "ID", "NAME", "STATUS", keyID, "ci-bot", "free-tier", "$12.50", "UNLIMITED")
	if strings.Contains(out, "REVOKED") {
		t.Errorf("no key should be revoked yet:\n%s", out)
	}

	out, err = env.run(t, "keys", "revoke", keyID)
	if err != nil {
		t.Fatalf("keys revoke: %v", err)
	}
	assertContains(t, out, "Successfully revoked virtual API key: "+keyID)

	out, err = env.run(t, "keys", "list")
	if err != nil {
		t.Fatalf("keys list after revoke: %v", err)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, keyID) && !strings.Contains(line, "REVOKED") {
			t.Errorf("revoked key still listed as active: %q", line)
		}
	}
}

func TestKeysDailyBudget(t *testing.T) {
	env := newTestEnv(t, "")

	out, err := env.run(t, "keys", "create", "--name", "daily-bot", "--budget", "40", "--daily-budget", "3.25")
	if err != nil {
		t.Fatalf("keys create --daily-budget: %v", err)
	}
	assertContains(t, out, "Monthly Budget:  $40.00 USD", "Daily Budget:    $3.25 USD (resets 00:00 UTC)")

	out, err = env.run(t, "keys", "create", "--name", "no-daily")
	if err != nil {
		t.Fatalf("keys create: %v", err)
	}
	assertContains(t, out, "Daily Budget:    Unlimited")

	out, err = env.run(t, "keys", "list")
	if err != nil {
		t.Fatalf("keys list: %v", err)
	}
	assertContains(t, out, "TODAY SPEND", "DAILY BUDGET", "$3.25")
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "daily-bot") && !strings.Contains(line, "$3.25") {
			t.Errorf("daily-bot row lacks its daily budget: %q", line)
		}
	}
}

func TestKeysRevokeErrors(t *testing.T) {
	env := newTestEnv(t, "")

	if _, err := env.run(t, "keys", "revoke"); err == nil {
		t.Error("revoke without an id should fail argument validation")
	}
	_, err := env.run(t, "keys", "revoke", "no-such-key")
	if err == nil || !strings.Contains(err.Error(), "failed to revoke key no-such-key") {
		t.Errorf("err = %v, want revoke failure for unknown key", err)
	}
}
