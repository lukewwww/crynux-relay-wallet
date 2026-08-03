package blockchain

import (
	"context"
	"crynux_relay_wallet/config"
	"crynux_relay_wallet/models"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/crypto"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newConfirmerTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "confirmer.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get sql db: %v", err)
	}
	t.Cleanup(func() {
		_ = sqlDB.Close()
	})
	if err := db.AutoMigrate(&models.BlockchainTransaction{}); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	return db
}

func initConfirmerTestConfig(t *testing.T) {
	t.Helper()
	configDir := t.TempDir()
	privateKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	address := crypto.PubkeyToAddress(privateKey.PublicKey).Hex()
	privateKeyHex := fmt.Sprintf("%x", crypto.FromECDSA(privateKey))
	privateKeyPath := filepath.Join(configDir, "blockchain_privkey.txt")
	if err := os.WriteFile(privateKeyPath, []byte(privateKeyHex), 0o600); err != nil {
		t.Fatalf("write private key: %v", err)
	}
	relayKeyPath := filepath.Join(configDir, "relay_api_privkey.txt")
	if err := os.WriteFile(relayKeyPath, []byte(privateKeyHex), 0o600); err != nil {
		t.Fatalf("write relay key: %v", err)
	}
	configContent := fmt.Sprintf(`
environment: debug
blockchains:
  testnet:
    token_type: native
    gas_limit_buffer_percent: 20
    receipt_confirmation_blocks: 1
    max_withdrawals_per_day: 10
    retry_interval: 1
    max_retries: 3
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
`, address, filepath.ToSlash(privateKeyPath), filepath.ToSlash(relayKeyPath))
	if err := os.WriteFile(filepath.Join(configDir, "config.yml"), []byte(configContent), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := config.InitConfig(configDir); err != nil {
		t.Fatalf("init config: %v", err)
	}
}

func TestHandleDelayedReceiptKeepsSentWithoutRetry(t *testing.T) {
	db := newConfirmerTestDB(t)
	transaction := &models.BlockchainTransaction{
		Network:     "testnet",
		Type:        "SendETH",
		Status:      models.TransactionStatusSent,
		FromAddress: "0x1111111111111111111111111111111111111111",
		ToAddress:   "0x2222222222222222222222222222222222222222",
		Value:       "1",
		TxHash:      sql.NullString{String: "0xabc", Valid: true},
		Nonce:       sql.NullInt64{Int64: 4, Valid: true},
		SentAt:      sql.NullTime{Time: time.Now().Add(-time.Hour), Valid: true},
		MaxRetries:  3,
	}
	if err := transaction.Save(context.Background(), db); err != nil {
		t.Fatalf("save transaction: %v", err)
	}

	confirmer := NewTransactionConfirmer(db)
	if err := confirmer.handleDelayedReceipt(context.Background(), transaction); err != nil {
		t.Fatalf("handle delayed receipt: %v", err)
	}
	if err := confirmer.handleDelayedReceipt(context.Background(), transaction); err != nil {
		t.Fatalf("repeat delayed receipt: %v", err)
	}

	var saved models.BlockchainTransaction
	if err := db.First(&saved, transaction.ID).Error; err != nil {
		t.Fatalf("load transaction: %v", err)
	}
	if saved.Status != models.TransactionStatusSent {
		t.Fatalf("delayed receipt must keep sent status, got %d", saved.Status)
	}
	if saved.StatusMessage.String != models.TransactionReceiptDelayedMessage {
		t.Fatalf("unexpected status message: %q", saved.StatusMessage.String)
	}
	if saved.TxHash.String != "0xabc" {
		t.Fatalf("tx hash changed: %q", saved.TxHash.String)
	}

	retries, err := models.GetRetryTransactionsByID(context.Background(), db, transaction.ID)
	if err != nil {
		t.Fatalf("load retries: %v", err)
	}
	if len(retries) != 0 {
		t.Fatalf("delayed receipt must not create retries, got %d", len(retries))
	}
}

func TestReceiptHasRequiredConfirmations(t *testing.T) {
	if receiptHasRequiredConfirmations(100, 100, 1) {
		t.Fatal("zero additional blocks must not confirm")
	}
	if !receiptHasRequiredConfirmations(101, 100, 1) {
		t.Fatal("one additional block must confirm when required=1")
	}
	if receiptHasRequiredConfirmations(105, 100, 12) {
		t.Fatal("insufficient confirmations must wait")
	}
	if !receiptHasRequiredConfirmations(112, 100, 12) {
		t.Fatal("exact required confirmations must pass")
	}
	if receiptHasRequiredConfirmations(99, 100, 1) {
		t.Fatal("reorg below receipt block must wait")
	}
	if !receiptHasRequiredConfirmations(100, 100, 0) {
		t.Fatal("required zero must confirm at receipt block")
	}
	if receiptHasRequiredConfirmations(99, 100, 0) {
		t.Fatal("required zero must still wait when latest is below receipt block")
	}
}

func TestHandleFailedTransactionCreatesSingleRetry(t *testing.T) {
	initConfirmerTestConfig(t)
	db := newConfirmerTestDB(t)

	transaction := &models.BlockchainTransaction{
		Network:     "testnet",
		Type:        "SendETH",
		Status:      models.TransactionStatusSent,
		FromAddress: "0x1111111111111111111111111111111111111111",
		ToAddress:   "0x2222222222222222222222222222222222222222",
		Value:       "1",
		TxHash:      sql.NullString{String: "0xabc", Valid: true},
		Nonce:       sql.NullInt64{Int64: 4, Valid: true},
		SentAt:      sql.NullTime{Time: time.Now().Add(-time.Hour), Valid: true},
		MaxRetries:  3,
	}
	if err := transaction.Save(context.Background(), db); err != nil {
		t.Fatalf("save transaction: %v", err)
	}

	createRetryOnce := func() error {
		return db.Transaction(func(tx *gorm.DB) error {
			marked, err := transaction.MarkFailedFromSent(context.Background(), tx, 100, 21000, "1", "Transaction failed with status 0")
			if err != nil {
				return err
			}
			if !marked {
				return nil
			}
			if transaction.RetryCount < transaction.MaxRetries {
				return transaction.CreateRetryTransaction(context.Background(), tx)
			}
			return nil
		})
	}
	if err := createRetryOnce(); err != nil {
		t.Fatalf("first failed handling: %v", err)
	}
	if err := createRetryOnce(); err != nil {
		t.Fatalf("second failed handling: %v", err)
	}

	retries, err := models.GetRetryTransactionsByID(context.Background(), db, transaction.ID)
	if err != nil {
		t.Fatalf("load retries: %v", err)
	}
	if len(retries) != 1 {
		t.Fatalf("expected exactly one retry, got %d", len(retries))
	}
}

func TestIsAlreadyKnownTransactionError(t *testing.T) {
	if !isAlreadyKnownTransactionError(errors.New("already known")) {
		t.Fatal("expected already known detection")
	}
	if !isAlreadyKnownTransactionError(errors.New("known transaction")) {
		t.Fatal("expected known transaction detection")
	}
	if isAlreadyKnownTransactionError(errors.New("nonce too low")) {
		t.Fatal("nonce error must not be treated as already known")
	}
}
