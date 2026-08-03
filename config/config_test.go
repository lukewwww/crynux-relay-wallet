package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
)

func writeConfigFile(t *testing.T, dir string, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "config.yml"), []byte(content), 0o600); err != nil {
		t.Fatalf("write config.yml: %v", err)
	}
}

func writePrivateKeyFile(t *testing.T, dir string, filename string, privateKey string) string {
	t.Helper()
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, []byte(privateKey), 0o600); err != nil {
		t.Fatalf("write private key file %s: %v", filename, err)
	}
	return path
}

func generateAccount(t *testing.T) (string, string) {
	t.Helper()
	privateKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate private key: %v", err)
	}
	address := crypto.PubkeyToAddress(privateKey.PublicKey).Hex()
	privateKeyHex := fmt.Sprintf("%x", crypto.FromECDSA(privateKey))
	return privateKeyHex, address
}

func TestInitConfigLoadsAndDeletesMultiplePrivateKeyFiles(t *testing.T) {
	configDir := t.TempDir()
	networkAKey, networkAAddress := generateAccount(t)
	networkBKey, networkBAddress := generateAccount(t)
	relayKey, _ := generateAccount(t)

	networkAPath := writePrivateKeyFile(t, configDir, "network_a_privkey.txt", "0x"+networkAKey+"\n")
	networkBPath := writePrivateKeyFile(t, configDir, "network_b_privkey.txt", networkBKey+"\n")
	relayPath := writePrivateKeyFile(t, configDir, "relay_api_privkey.txt", relayKey+"\n")

	configContent := fmt.Sprintf(`
environment: debug
blockchains:
  network_a:
    token_type: native
    gas_limit_buffer_percent: 20
    receipt_confirmation_blocks: 1
    max_withdrawals_per_day: 10
    contracts:
      benefit_address: "0x0000000000000000000000000000000000000001"
    account:
      address: %s
      private_key_file: %s
  network_b:
    token_type: erc20
    token_address: "0x0000000000000000000000000000000000000002"
    gas_limit_buffer_percent: 20
    receipt_confirmation_blocks: 1
    max_withdrawals_per_day: 20
    contracts:
      benefit_address: "0x0000000000000000000000000000000000000003"
    account:
      address: %s
      private_key_file: %s
relay:
  api:
    private_key_file: %s
tasks:
  process_withdrawal_requests:
    cancellation_settlement_timeout_seconds: 30
`, networkAAddress, filepath.ToSlash(networkAPath), networkBAddress, filepath.ToSlash(networkBPath), filepath.ToSlash(relayPath))
	writeConfigFile(t, configDir, configContent)

	if err := InitConfig(configDir); err != nil {
		t.Fatalf("InitConfig failed: %v", err)
	}

	cfg := GetConfig()
	if cfg.Blockchains["network_a"].Account.PrivateKey != networkAKey {
		t.Fatalf("network_a private key mismatch")
	}
	if cfg.Blockchains["network_b"].Account.PrivateKey != networkBKey {
		t.Fatalf("network_b private key mismatch")
	}
	if cfg.Blockchains["network_a"].MaxWithdrawalsPerDay != 10 {
		t.Fatalf("network_a max withdrawals per day mismatch")
	}
	if cfg.Blockchains["network_b"].MaxWithdrawalsPerDay != 20 {
		t.Fatalf("network_b max withdrawals per day mismatch")
	}
	if cfg.Relay.Api.PrivateKey != relayKey {
		t.Fatalf("relay api private key mismatch")
	}

	for _, path := range []string{networkAPath, networkBPath, relayPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("expected private key file to be deleted: %s, err: %v", path, err)
		}
	}
}

func TestInitConfigLoadsSharedBlockchainPrivateKeyFileOnce(t *testing.T) {
	configDir := t.TempDir()
	sharedKey, sharedAddress := generateAccount(t)
	relayKey, _ := generateAccount(t)

	sharedPath := writePrivateKeyFile(t, configDir, "shared_blockchain_privkey.txt", sharedKey)
	relayPath := writePrivateKeyFile(t, configDir, "relay_api_privkey.txt", relayKey)

	configContent := fmt.Sprintf(`
environment: debug
blockchains:
  network_a:
    token_type: native
    gas_limit_buffer_percent: 20
    receipt_confirmation_blocks: 1
    max_withdrawals_per_day: 10
    contracts:
      benefit_address: "0x0000000000000000000000000000000000000001"
    account:
      address: %s
      private_key_file: %s
  network_b:
    token_type: native
    gas_limit_buffer_percent: 20
    receipt_confirmation_blocks: 1
    max_withdrawals_per_day: 10
    contracts:
      benefit_address: "0x0000000000000000000000000000000000000002"
    account:
      address: %s
      private_key_file: %s
relay:
  api:
    private_key_file: %s
tasks:
  process_withdrawal_requests:
    cancellation_settlement_timeout_seconds: 30
`, sharedAddress, filepath.ToSlash(sharedPath), sharedAddress, filepath.ToSlash(sharedPath), filepath.ToSlash(relayPath))
	writeConfigFile(t, configDir, configContent)

	if err := InitConfig(configDir); err != nil {
		t.Fatalf("InitConfig failed: %v", err)
	}

	cfg := GetConfig()
	if cfg.Blockchains["network_a"].Account.PrivateKey != sharedKey {
		t.Fatalf("network_a private key mismatch")
	}
	if cfg.Blockchains["network_b"].Account.PrivateKey != sharedKey {
		t.Fatalf("network_b private key mismatch")
	}

	for _, path := range []string{sharedPath, relayPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("expected private key file to be deleted: %s, err: %v", path, err)
		}
	}
}

func TestInitConfigReturnsErrorWhenPrivateKeyFileMissing(t *testing.T) {
	configDir := t.TempDir()
	networkKey, networkAddress := generateAccount(t)
	networkPath := writePrivateKeyFile(t, configDir, "network_privkey.txt", networkKey)
	missingRelayPath := filepath.Join(configDir, "missing_relay_key.txt")

	configContent := fmt.Sprintf(`
environment: debug
blockchains:
  network_a:
    token_type: native
    contracts:
      benefit_address: "0x0000000000000000000000000000000000000001"
    account:
      address: %s
      private_key_file: %s
relay:
  api:
    private_key_file: %s
`, networkAddress, filepath.ToSlash(networkPath), filepath.ToSlash(missingRelayPath))
	writeConfigFile(t, configDir, configContent)

	err := InitConfig(configDir)
	if err == nil {
		t.Fatalf("InitConfig expected error but got nil")
	}
}

func TestInitConfigRequiresCancellationSettlementTimeout(t *testing.T) {
	configDir := t.TempDir()
	writeConfigFile(t, configDir, "environment: test\n")

	err := InitConfig(configDir)
	if err == nil || err.Error() != "process withdrawal requests cancellation settlement timeout seconds not set" {
		t.Fatalf("expected cancellation settlement timeout error, got %v", err)
	}
}

func TestInitConfigRequiresReceiptConfirmationBlocks(t *testing.T) {
	configDir := t.TempDir()
	networkKey, networkAddress := generateAccount(t)
	networkPath := writePrivateKeyFile(t, configDir, "network_privkey.txt", networkKey)
	relayPath := writePrivateKeyFile(t, configDir, "relay_api_privkey.txt", networkKey)

	configContent := fmt.Sprintf(`
environment: debug
blockchains:
  network_a:
    token_type: native
    gas_limit_buffer_percent: 20
    contracts:
      benefit_address: "0x0000000000000000000000000000000000000001"
    account:
      address: %s
      private_key_file: %s
relay:
  api:
    private_key_file: %s
tasks:
  process_withdrawal_requests:
    cancellation_settlement_timeout_seconds: 30
`, networkAddress, filepath.ToSlash(networkPath), filepath.ToSlash(relayPath))
	writeConfigFile(t, configDir, configContent)

	err := InitConfig(configDir)
	if err == nil || err.Error() != "blockchain network_a receipt confirmation blocks not set" {
		t.Fatalf("expected receipt confirmation blocks error, got %v", err)
	}
}

func TestInitConfigRequiresMaxWithdrawalsPerDay(t *testing.T) {
	configDir := t.TempDir()
	writeConfigFile(t, configDir, `
environment: test
blockchains:
  network_a:
    token_type: native
    gas_limit_buffer_percent: 20
    receipt_confirmation_blocks: 1
    contracts:
      benefit_address: "0x0000000000000000000000000000000000000001"
tasks:
  process_withdrawal_requests:
    cancellation_settlement_timeout_seconds: 30
`)

	err := InitConfig(configDir)
	if err == nil || err.Error() != "blockchain network_a max withdrawals per day not set" {
		t.Fatalf("expected max withdrawals per day error, got %v", err)
	}
}
