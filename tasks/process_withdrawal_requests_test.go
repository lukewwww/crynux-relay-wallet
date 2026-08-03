package tasks

import (
	"context"
	"crynux_relay_wallet/config"
	"crynux_relay_wallet/models"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type withdrawalCallbackCounts struct {
	fulfill atomic.Int32
	reject  atomic.Int32
}

func setupWithdrawalCancellationTest(t *testing.T) (*gorm.DB, *withdrawalCallbackCounts) {
	t.Helper()

	counts := &withdrawalCallbackCounts{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case strings.HasSuffix(request.URL.Path, "/fulfill"):
			counts.fulfill.Add(1)
		case strings.HasSuffix(request.URL.Path, "/reject"):
			counts.reject.Add(1)
		default:
			http.NotFound(w, request)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)

	configDir := t.TempDir()
	configContent := fmt.Sprintf(`
environment: test
relay:
  api:
    host: %s
tasks:
  process_withdrawal_requests:
    cancellation_settlement_timeout_seconds: 1
`, server.URL)
	if err := os.WriteFile(filepath.Join(configDir, "config.yml"), []byte(configContent), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := config.InitConfig(configDir); err != nil {
		t.Fatalf("init config: %v", err)
	}
	privateKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate relay API private key: %v", err)
	}
	config.GetConfig().Relay.Api.PrivateKey = fmt.Sprintf("%x", crypto.FromECDSA(privateKey))

	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "withdrawal-cancellation.db")), &gorm.Config{})
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
	if err := db.AutoMigrate(&models.RelayAccount{}, &models.BlockchainTransaction{}, &models.WithdrawRecord{}); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	return db, counts
}

func createCancellationTestWithdrawal(
	t *testing.T,
	db *gorm.DB,
	status models.TransactionStatus,
) (*models.WithdrawRecord, *models.BlockchainTransaction) {
	t.Helper()

	transaction := &models.BlockchainTransaction{
		Network:     "testnet",
		Type:        "SendETH",
		Status:      status,
		FromAddress: "0x1111111111111111111111111111111111111111",
		ToAddress:   "0x2222222222222222222222222222222222222222",
		Value:       "10",
		MaxRetries:  3,
	}
	if err := transaction.Save(context.Background(), db); err != nil {
		t.Fatalf("save transaction: %v", err)
	}
	record := &models.WithdrawRecord{
		RemoteID:                uint(time.Now().UnixNano()),
		Address:                 "0x3333333333333333333333333333333333333333",
		BenefitAddress:          transaction.ToAddress,
		Amount:                  models.BigInt{Int: *big.NewInt(10)},
		WithdrawalFee:           models.BigInt{Int: *big.NewInt(1)},
		Network:                 transaction.Network,
		Status:                  models.WithdrawStatusPending,
		BlockchainTransactionID: sql.NullInt64{Int64: int64(transaction.ID), Valid: true},
	}
	if err := db.Create(record).Error; err != nil {
		t.Fatalf("save withdrawal: %v", err)
	}
	return record, transaction
}

func signedWithdrawalAuthorization(
	t *testing.T,
	privateKeyHex, address, amount, benefitAddress, network string,
	timestamp int64,
) string {
	t.Helper()
	message := buildWithdrawalAuthorizationMessage(address, amount, benefitAddress, network, timestamp)
	prefix := fmt.Sprintf("\x19Ethereum Signed Message:\n%d", len(message))
	privateKey, err := crypto.HexToECDSA(privateKeyHex)
	if err != nil {
		t.Fatalf("parse private key: %v", err)
	}
	signature, err := crypto.Sign(crypto.Keccak256([]byte(prefix+message)), privateKey)
	if err != nil {
		t.Fatalf("sign withdrawal authorization: %v", err)
	}
	signature[64] += 27
	return hexutil.Encode(signature)
}

func createWithdrawalAccount(t *testing.T, db *gorm.DB, address string, balance int64) {
	t.Helper()
	account := &models.RelayAccount{
		Address: address,
		Balance: models.BigInt{Int: *big.NewInt(balance)},
	}
	if err := db.Create(account).Error; err != nil {
		t.Fatalf("save relay account: %v", err)
	}
}

func TestProcessWithdrawalExecutionBalanceGateRejectsWithoutTransaction(t *testing.T) {
	db, counts := setupWithdrawalCancellationTest(t)
	privateKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate user key: %v", err)
	}
	address := crypto.PubkeyToAddress(privateKey.PublicKey).Hex()
	benefitAddress := "0x2222222222222222222222222222222222222222"
	timestamp := time.Now().Unix()
	signature := signedWithdrawalAuthorization(
		t,
		fmt.Sprintf("%x", crypto.FromECDSA(privateKey)),
		address,
		"60",
		benefitAddress,
		"testnet",
		timestamp,
	)
	createWithdrawalAccount(t, db, address, 40)
	record := &models.WithdrawRecord{
		RemoteID:       uint(time.Now().UnixNano()),
		Address:        address,
		BenefitAddress: benefitAddress,
		Amount:         models.BigInt{Int: *big.NewInt(60)},
		WithdrawalFee:  models.BigInt{Int: *big.NewInt(0)},
		Network:        "testnet",
		Status:         models.WithdrawStatusPending,
		Timestamp:      sql.NullInt64{Int64: timestamp, Valid: true},
		Signature:      sql.NullString{String: signature, Valid: true},
	}
	if err := db.Create(record).Error; err != nil {
		t.Fatalf("save withdrawal: %v", err)
	}

	if err := processWithdrawalRecord(context.Background(), db, record); err != nil {
		t.Fatalf("process withdrawal: %v", err)
	}
	var transactionCount int64
	if err := db.Model(&models.BlockchainTransaction{}).Count(&transactionCount).Error; err != nil {
		t.Fatalf("count blockchain transactions: %v", err)
	}
	if transactionCount != 0 {
		t.Fatalf("insufficient execution balance created %d blockchain transactions", transactionCount)
	}
	if counts.reject.Load() != 1 || counts.fulfill.Load() != 0 {
		t.Fatalf("unexpected callbacks: reject=%d fulfill=%d", counts.reject.Load(), counts.fulfill.Load())
	}
}

func TestProcessWithdrawalRejectsMissingAuthorization(t *testing.T) {
	db, counts := setupWithdrawalCancellationTest(t)
	record := &models.WithdrawRecord{
		RemoteID:       uint(time.Now().UnixNano()),
		Address:        "0x3333333333333333333333333333333333333333",
		BenefitAddress: "0x2222222222222222222222222222222222222222",
		Amount:         models.BigInt{Int: *big.NewInt(10)},
		WithdrawalFee:  models.BigInt{Int: *big.NewInt(1)},
		Network:        "testnet",
		Status:         models.WithdrawStatusPending,
	}
	if err := db.Create(record).Error; err != nil {
		t.Fatalf("save withdrawal: %v", err)
	}

	if err := processWithdrawalRecord(context.Background(), db, record); err != nil {
		t.Fatalf("process withdrawal: %v", err)
	}
	if counts.reject.Load() != 1 || counts.fulfill.Load() != 0 {
		t.Fatalf("unexpected callbacks: reject=%d fulfill=%d", counts.reject.Load(), counts.fulfill.Load())
	}
}

func TestSyncMissingAuthorizationAdvancesCheckpointAndRejects(t *testing.T) {
	counts := &withdrawalCallbackCounts{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/v1/withdraw/list"):
			if request.URL.Query().Get("start_id") == "0" {
				_, _ = w.Write([]byte(`{"data":[{"id":1,"created_at":1,"address":"0x3333333333333333333333333333333333333333","benefit_address":"0x2222222222222222222222222222222222222222","amount":"10","withdrawal_fee":"1","network":"testnet","status":0,"relay_account_event_id":0,"timestamp":0,"signature":""}]}`))
			} else {
				_, _ = w.Write([]byte(`{"data":[]}`))
			}
		case strings.HasSuffix(request.URL.Path, "/reject"):
			counts.reject.Add(1)
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, request)
		}
	}))
	t.Cleanup(server.Close)

	configDir := t.TempDir()
	configContent := fmt.Sprintf(`
environment: test
db:
  driver: sqlite
  connection: file:withdrawal-sync-%d?mode=memory&cache=shared
  log:
    level: info
    output: stdout
relay:
  api:
    host: %s
tasks:
  sync_withdrawal_requests:
    batch_size: 10
  process_withdrawal_requests:
    cancellation_settlement_timeout_seconds: 1
`, time.Now().UnixNano(), server.URL)
	if err := os.WriteFile(filepath.Join(configDir, "config.yml"), []byte(configContent), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := config.InitConfig(configDir); err != nil {
		t.Fatalf("init config: %v", err)
	}
	privateKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate relay API private key: %v", err)
	}
	config.GetConfig().Relay.Api.PrivateKey = fmt.Sprintf("%x", crypto.FromECDSA(privateKey))

	if err := config.InitDB(config.GetConfig()); err != nil {
		t.Fatalf("init db: %v", err)
	}
	db := config.GetDB()
	if err := db.AutoMigrate(
		&models.WithdrawRecord{},
		&models.WithdrawalRequestCheckpoint{},
		&models.TaskFeeCheckpoint{},
	); err != nil {
		t.Fatalf("migrate db: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	syncResult := make(chan error, 1)
	go func() {
		syncResult <- syncWithdrawalRequests(ctx, 1)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for {
		var checkpoint models.WithdrawalRequestCheckpoint
		if err := db.First(&checkpoint).Error; err == nil && checkpoint.LatestWithdrawalRequestID == 1 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("withdrawal checkpoint did not advance")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-syncResult; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("sync withdrawal requests: %v", err)
	}

	var record models.WithdrawRecord
	if err := db.Where("remote_id = ?", 1).First(&record).Error; err != nil {
		t.Fatalf("load synced withdrawal: %v", err)
	}
	if record.Status != models.WithdrawStatusFailed {
		t.Fatalf("missing authorization status=%d, want failed", record.Status)
	}
	if err := processWithdrawalRecord(context.Background(), db, &record); err != nil {
		t.Fatalf("reject invalid withdrawal: %v", err)
	}
	if counts.reject.Load() != 1 {
		t.Fatalf("missing authorization reject callbacks=%d, want 1", counts.reject.Load())
	}
}

func TestProcessWithdrawalRejectsBadAuthorization(t *testing.T) {
	db, counts := setupWithdrawalCancellationTest(t)
	record := &models.WithdrawRecord{
		RemoteID:       uint(time.Now().UnixNano()),
		Address:        "0x3333333333333333333333333333333333333333",
		BenefitAddress: "0x2222222222222222222222222222222222222222",
		Amount:         models.BigInt{Int: *big.NewInt(10)},
		WithdrawalFee:  models.BigInt{Int: *big.NewInt(1)},
		Network:        "testnet",
		Status:         models.WithdrawStatusPending,
		Timestamp:      sql.NullInt64{Int64: time.Now().Unix(), Valid: true},
		Signature:      sql.NullString{String: "0xbad", Valid: true},
	}
	if err := db.Create(record).Error; err != nil {
		t.Fatalf("save withdrawal: %v", err)
	}

	if err := processWithdrawalRecord(context.Background(), db, record); err != nil {
		t.Fatalf("process withdrawal: %v", err)
	}
	if counts.reject.Load() != 1 || counts.fulfill.Load() != 0 {
		t.Fatalf("unexpected callbacks: reject=%d fulfill=%d", counts.reject.Load(), counts.fulfill.Load())
	}
}

func TestProcessWithdrawalRejectsReplayedAuthorization(t *testing.T) {
	db, counts := setupWithdrawalCancellationTest(t)
	privateKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate user key: %v", err)
	}
	address := crypto.PubkeyToAddress(privateKey.PublicKey).Hex()
	benefitAddress := "0x2222222222222222222222222222222222222222"
	timestamp := time.Now().Unix()
	signature := signedWithdrawalAuthorization(
		t,
		fmt.Sprintf("%x", crypto.FromECDSA(privateKey)),
		address,
		"10",
		benefitAddress,
		"testnet",
		timestamp,
	)
	fingerprint, err := validateWithdrawalAuthorization(
		address,
		"10",
		benefitAddress,
		"testnet",
		sql.NullInt64{Int64: timestamp, Valid: true},
		sql.NullString{String: signature, Valid: true},
	)
	if err != nil {
		t.Fatalf("validate authorization: %v", err)
	}
	original := &models.WithdrawRecord{
		RemoteID:                 uint(time.Now().UnixNano()),
		Address:                  address,
		BenefitAddress:           benefitAddress,
		Amount:                   models.BigInt{Int: *big.NewInt(10)},
		WithdrawalFee:            models.BigInt{Int: *big.NewInt(1)},
		Network:                  "testnet",
		Status:                   models.WithdrawStatusFinished,
		Timestamp:                sql.NullInt64{Int64: timestamp, Valid: true},
		Signature:                sql.NullString{String: signature, Valid: true},
		AuthorizationFingerprint: sql.NullString{String: fingerprint, Valid: true},
	}
	if err := db.Create(original).Error; err != nil {
		t.Fatalf("save original withdrawal: %v", err)
	}
	replay := *original
	replay.Model = gorm.Model{}
	replay.RemoteID++
	replay.Status = models.WithdrawStatusPending
	replay.AuthorizationFingerprint = sql.NullString{}
	if err := db.Create(&replay).Error; err != nil {
		t.Fatalf("save replay withdrawal: %v", err)
	}

	if err := processWithdrawalRecord(context.Background(), db, &replay); err != nil {
		t.Fatalf("process replay: %v", err)
	}
	if counts.reject.Load() != 1 || counts.fulfill.Load() != 0 {
		t.Fatalf("unexpected callbacks: reject=%d fulfill=%d", counts.reject.Load(), counts.fulfill.Load())
	}
}

func TestProcessBroadcastedUnsignedWithdrawalFulfills(t *testing.T) {
	db, counts := setupWithdrawalCancellationTest(t)
	address := "0x3333333333333333333333333333333333333333"
	createWithdrawalAccount(t, db, address, 100)
	record, transaction := createCancellationTestWithdrawal(t, db, models.TransactionStatusConfirmed)
	record.Address = address
	if err := db.Model(record).Updates(map[string]interface{}{
		"address": record.Address,
	}).Error; err != nil {
		t.Fatalf("update withdrawal address: %v", err)
	}
	if err := transaction.Update(context.Background(), db, map[string]interface{}{
		"tx_hash":       "0xconfirmed",
		"signed_raw_tx": "0xraw",
	}); err != nil {
		t.Fatalf("set committed broadcast evidence: %v", err)
	}

	if err := processWithdrawalRecord(context.Background(), db, record); err != nil {
		t.Fatalf("process withdrawal: %v", err)
	}
	if counts.reject.Load() != 0 || counts.fulfill.Load() != 1 {
		t.Fatalf("unexpected callbacks: reject=%d fulfill=%d", counts.reject.Load(), counts.fulfill.Load())
	}
}

func TestProcessUnbroadcastedUnsignedWithdrawalCancelsAndRejects(t *testing.T) {
	db, counts := setupWithdrawalCancellationTest(t)
	record, transaction := createCancellationTestWithdrawal(t, db, models.TransactionStatusPending)

	if err := processWithdrawalRecord(context.Background(), db, record); err != nil {
		t.Fatalf("process withdrawal: %v", err)
	}
	var saved models.BlockchainTransaction
	if err := db.First(&saved, transaction.ID).Error; err != nil {
		t.Fatalf("load blockchain transaction: %v", err)
	}
	if saved.Status != models.TransactionStatusCancelled || saved.HasCommittedBroadcast() {
		t.Fatalf("unexpected transaction state: status=%d committed=%v", saved.Status, saved.HasCommittedBroadcast())
	}
	if counts.reject.Load() != 1 || counts.fulfill.Load() != 0 {
		t.Fatalf("unexpected callbacks: reject=%d fulfill=%d", counts.reject.Load(), counts.fulfill.Load())
	}
}

func TestProcessSuccessfulWithdrawalAfterRestartDoesNotDebitAgain(t *testing.T) {
	db, counts := setupWithdrawalCancellationTest(t)
	address := "0x3333333333333333333333333333333333333333"
	createWithdrawalAccount(t, db, address, 100)
	record, transaction := createCancellationTestWithdrawal(t, db, models.TransactionStatusConfirmed)
	record.Status = models.WithdrawStatusSuccess
	if err := db.Model(record).Update("status", record.Status).Error; err != nil {
		t.Fatalf("set successful withdrawal: %v", err)
	}
	if err := transaction.Update(context.Background(), db, map[string]interface{}{
		"tx_hash":       "0xconfirmed",
		"signed_raw_tx": "0xraw",
	}); err != nil {
		t.Fatalf("set committed broadcast evidence: %v", err)
	}

	if err := processWithdrawalRecord(context.Background(), db, record); err != nil {
		t.Fatalf("resume successful withdrawal: %v", err)
	}
	var account models.RelayAccount
	if err := db.Where("address = ?", address).First(&account).Error; err != nil {
		t.Fatalf("load relay account: %v", err)
	}
	if account.Balance.Cmp(big.NewInt(100)) != 0 {
		t.Fatalf("successful restart debited balance again: %s", account.Balance.String())
	}
	if counts.reject.Load() != 0 || counts.fulfill.Load() != 1 {
		t.Fatalf("unexpected callbacks: reject=%d fulfill=%d", counts.reject.Load(), counts.fulfill.Load())
	}
}

func TestHandleTimeoutCancelsPendingTransaction(t *testing.T) {
	db, counts := setupWithdrawalCancellationTest(t)
	record, transaction := createCancellationTestWithdrawal(t, db, models.TransactionStatusPending)

	completed, err := handleTimeoutWithdrawalRequest(context.Background(), db, record)
	if err != nil || !completed {
		t.Fatalf("handle timeout: completed=%v err=%v", completed, err)
	}

	var saved models.BlockchainTransaction
	if err := db.First(&saved, transaction.ID).Error; err != nil {
		t.Fatalf("load transaction: %v", err)
	}
	if saved.Status != models.TransactionStatusCancelled || !saved.CancellationRequestedAt.Valid {
		t.Fatalf("unexpected cancellation state: status=%d requested_at=%v", saved.Status, saved.CancellationRequestedAt)
	}
	if counts.reject.Load() != 1 || counts.fulfill.Load() != 0 {
		t.Fatalf("unexpected callbacks: reject=%d fulfill=%d", counts.reject.Load(), counts.fulfill.Load())
	}
}

func TestHandleTimeoutSendingFailureSettlesToCancellation(t *testing.T) {
	db, counts := setupWithdrawalCancellationTest(t)
	record, transaction := createCancellationTestWithdrawal(t, db, models.TransactionStatusSending)

	completed, err := handleTimeoutWithdrawalRequest(context.Background(), db, record)
	if err != nil || completed {
		t.Fatalf("first timeout check: completed=%v err=%v", completed, err)
	}
	if err := transaction.ReleaseSending(context.Background(), db, "send failed"); err != nil {
		t.Fatalf("release sending transaction: %v", err)
	}
	completed, err = handleTimeoutWithdrawalRequest(context.Background(), db, record)
	if err != nil || !completed {
		t.Fatalf("final timeout check: completed=%v err=%v", completed, err)
	}

	if counts.reject.Load() != 1 || counts.fulfill.Load() != 0 {
		t.Fatalf("unexpected callbacks: reject=%d fulfill=%d", counts.reject.Load(), counts.fulfill.Load())
	}
}

func TestHandleTimeoutSendingBroadcastSuccessDoesNotReject(t *testing.T) {
	db, counts := setupWithdrawalCancellationTest(t)
	record, transaction := createCancellationTestWithdrawal(t, db, models.TransactionStatusSending)

	prepared, err := transaction.PrepareBroadcast(context.Background(), db, "0x1234", 7, "0xraw")
	if err != nil || !prepared {
		t.Fatalf("prepare broadcast: prepared=%v err=%v", prepared, err)
	}
	marked, err := transaction.MarkSent(context.Background(), db)
	if err != nil || !marked {
		t.Fatalf("mark sent: marked=%v err=%v", marked, err)
	}
	_, err = handleTimeoutWithdrawalRequest(context.Background(), db, record)
	if !errors.Is(err, ErrWithdrawalRequestTransactionUnconfirmedTimeout) {
		t.Fatalf("expected fail-stop timeout, got %v", err)
	}
	if counts.reject.Load() != 0 || counts.fulfill.Load() != 0 {
		t.Fatalf("broadcast transaction must not trigger callbacks")
	}
}

func TestHandleTimeoutBroadcastingDoesNotReject(t *testing.T) {
	db, counts := setupWithdrawalCancellationTest(t)
	record, transaction := createCancellationTestWithdrawal(t, db, models.TransactionStatusPending)
	if err := transaction.Update(context.Background(), db, map[string]interface{}{
		"status":        models.TransactionStatusBroadcasting,
		"tx_hash":       "0xabcd",
		"nonce":         9,
		"signed_raw_tx": "0xraw",
	}); err != nil {
		t.Fatalf("set broadcasting: %v", err)
	}

	_, err := handleTimeoutWithdrawalRequest(context.Background(), db, record)
	if !errors.Is(err, ErrWithdrawalRequestTransactionUnconfirmedTimeout) {
		t.Fatalf("expected fail-stop timeout, got %v", err)
	}
	if counts.reject.Load() != 0 || counts.fulfill.Load() != 0 {
		t.Fatalf("broadcasting transaction must not trigger callbacks")
	}
}

func TestEnsureWithdrawalRejectSafeBlocksHistoricalTimeoutFailure(t *testing.T) {
	db, counts := setupWithdrawalCancellationTest(t)
	record, transaction := createCancellationTestWithdrawal(t, db, models.TransactionStatusPending)
	if err := transaction.Update(context.Background(), db, map[string]interface{}{
		"status":         models.TransactionStatusFailed,
		"tx_hash":        "0xdead",
		"nonce":          1,
		"status_message": models.TransactionTimedOutMessage,
		"retry_count":    3,
		"max_retries":    3,
	}); err != nil {
		t.Fatalf("set timed out failure: %v", err)
	}
	if err := transaction.Sync(context.Background(), db); err != nil {
		t.Fatalf("sync transaction: %v", err)
	}

	err := ensureWithdrawalRejectSafe(context.Background(), db, record)
	if !errors.Is(err, ErrWithdrawalRequestTransactionUnconfirmedTimeout) {
		t.Fatalf("expected fail-stop for historical timeout failure, got %v", err)
	}
	if counts.reject.Load() != 0 {
		t.Fatalf("historical timeout failure must not reject")
	}
}

func TestEnsureWithdrawalRejectSafeBlocksCancelledRetryWithTimeoutAncestor(t *testing.T) {
	db, counts := setupWithdrawalCancellationTest(t)
	record, root := createCancellationTestWithdrawal(t, db, models.TransactionStatusPending)
	if err := root.Update(context.Background(), db, map[string]interface{}{
		"status":         models.TransactionStatusFailed,
		"tx_hash":        "0xdead",
		"nonce":          1,
		"status_message": models.TransactionTimedOutMessage,
	}); err != nil {
		t.Fatalf("set timed out root: %v", err)
	}
	retry := &models.BlockchainTransaction{
		Network:            root.Network,
		Type:               root.Type,
		Status:             models.TransactionStatusCancelled,
		FromAddress:        root.FromAddress,
		ToAddress:          root.ToAddress,
		Value:              root.Value,
		RetryCount:         1,
		MaxRetries:         3,
		RetryTransactionID: sql.NullInt64{Int64: int64(root.ID), Valid: true},
	}
	if err := retry.Save(context.Background(), db); err != nil {
		t.Fatalf("save cancelled retry: %v", err)
	}

	err := ensureWithdrawalRejectSafe(context.Background(), db, record)
	if !errors.Is(err, ErrWithdrawalRequestTransactionUnconfirmedTimeout) {
		t.Fatalf("expected fail-stop for cancelled retry with timeout ancestor, got %v", err)
	}
	if counts.reject.Load() != 0 {
		t.Fatalf("cancelled retry with timeout ancestor must not reject")
	}
}

func TestEnsureWithdrawalRejectSafeBlocksConfirmedAncestor(t *testing.T) {
	db, counts := setupWithdrawalCancellationTest(t)
	record, root := createCancellationTestWithdrawal(t, db, models.TransactionStatusPending)
	if err := root.Update(context.Background(), db, map[string]interface{}{
		"status":  models.TransactionStatusConfirmed,
		"tx_hash": "0xok",
		"nonce":   1,
	}); err != nil {
		t.Fatalf("set confirmed root: %v", err)
	}
	retry := &models.BlockchainTransaction{
		Network:            root.Network,
		Type:               root.Type,
		Status:             models.TransactionStatusCancelled,
		FromAddress:        root.FromAddress,
		ToAddress:          root.ToAddress,
		Value:              root.Value,
		RetryCount:         1,
		MaxRetries:         3,
		RetryTransactionID: sql.NullInt64{Int64: int64(root.ID), Valid: true},
	}
	if err := retry.Save(context.Background(), db); err != nil {
		t.Fatalf("save cancelled retry: %v", err)
	}

	err := ensureWithdrawalRejectSafe(context.Background(), db, record)
	if !errors.Is(err, ErrWithdrawalRequestTransactionUnconfirmedTimeout) {
		t.Fatalf("expected fail-stop for confirmed ancestor, got %v", err)
	}
	if counts.reject.Load() != 0 {
		t.Fatalf("confirmed ancestor must not reject")
	}
}

func TestEnsureWithdrawalFulfillSafeRequiresSingleConfirmed(t *testing.T) {
	db, counts := setupWithdrawalCancellationTest(t)
	record, root := createCancellationTestWithdrawal(t, db, models.TransactionStatusPending)
	if err := root.Update(context.Background(), db, map[string]interface{}{
		"status":         models.TransactionStatusFailed,
		"tx_hash":        "0xdead",
		"nonce":          1,
		"status_message": models.TransactionTimedOutMessage,
	}); err != nil {
		t.Fatalf("set timed out root: %v", err)
	}
	retry := &models.BlockchainTransaction{
		Network:            root.Network,
		Type:               root.Type,
		Status:             models.TransactionStatusConfirmed,
		FromAddress:        root.FromAddress,
		ToAddress:          root.ToAddress,
		Value:              root.Value,
		TxHash:             sql.NullString{String: "0xretry", Valid: true},
		RetryCount:         1,
		MaxRetries:         3,
		RetryTransactionID: sql.NullInt64{Int64: int64(root.ID), Valid: true},
	}
	if err := retry.Save(context.Background(), db); err != nil {
		t.Fatalf("save confirmed retry: %v", err)
	}

	err := ensureWithdrawalFulfillSafe(context.Background(), db, record, retry)
	if !errors.Is(err, ErrWithdrawalRequestTransactionUnconfirmedTimeout) {
		t.Fatalf("expected fail-stop when timeout ancestor exists, got %v", err)
	}
	if counts.fulfill.Load() != 0 || counts.reject.Load() != 0 {
		t.Fatalf("unsafe fulfill must not callback")
	}
}

func TestEnsureWithdrawalFulfillSafeAllowsProvenFailureAncestor(t *testing.T) {
	db, _ := setupWithdrawalCancellationTest(t)
	record, root := createCancellationTestWithdrawal(t, db, models.TransactionStatusPending)
	if err := root.Update(context.Background(), db, map[string]interface{}{
		"status":         models.TransactionStatusFailed,
		"tx_hash":        "0xdead",
		"nonce":          1,
		"block_number":   10,
		"status_message": "Transaction failed with status 0",
	}); err != nil {
		t.Fatalf("set on-chain failure root: %v", err)
	}
	retry := &models.BlockchainTransaction{
		Network:            root.Network,
		Type:               root.Type,
		Status:             models.TransactionStatusConfirmed,
		FromAddress:        root.FromAddress,
		ToAddress:          root.ToAddress,
		Value:              root.Value,
		TxHash:             sql.NullString{String: "0xretry", Valid: true},
		RetryCount:         1,
		MaxRetries:         3,
		RetryTransactionID: sql.NullInt64{Int64: int64(root.ID), Valid: true},
	}
	if err := retry.Save(context.Background(), db); err != nil {
		t.Fatalf("save confirmed retry: %v", err)
	}

	if err := ensureWithdrawalFulfillSafe(context.Background(), db, record, retry); err != nil {
		t.Fatalf("proven failure ancestor must allow fulfill: %v", err)
	}
}

func TestHandleTimeoutCancelledRetryWithTimeoutAncestorDoesNotReject(t *testing.T) {
	db, counts := setupWithdrawalCancellationTest(t)
	record, root := createCancellationTestWithdrawal(t, db, models.TransactionStatusPending)
	if err := root.Update(context.Background(), db, map[string]interface{}{
		"status":         models.TransactionStatusFailed,
		"tx_hash":        "0xdead",
		"nonce":          1,
		"status_message": models.TransactionTimedOutMessage,
	}); err != nil {
		t.Fatalf("set timed out root: %v", err)
	}
	retry := &models.BlockchainTransaction{
		Network:            root.Network,
		Type:               root.Type,
		Status:             models.TransactionStatusPending,
		FromAddress:        root.FromAddress,
		ToAddress:          root.ToAddress,
		Value:              root.Value,
		RetryCount:         1,
		MaxRetries:         3,
		RetryTransactionID: sql.NullInt64{Int64: int64(root.ID), Valid: true},
	}
	if err := retry.Save(context.Background(), db); err != nil {
		t.Fatalf("save pending retry: %v", err)
	}

	_, err := handleTimeoutWithdrawalRequest(context.Background(), db, record)
	if !errors.Is(err, ErrWithdrawalRequestTransactionUnconfirmedTimeout) {
		t.Fatalf("expected fail-stop after cancelling retry with timeout ancestor, got %v", err)
	}
	if counts.reject.Load() != 0 {
		t.Fatalf("timeout reject path must scan full chain")
	}
}

func TestHandleTimeoutLongSendingFailsStop(t *testing.T) {
	db, counts := setupWithdrawalCancellationTest(t)
	record, transaction := createCancellationTestWithdrawal(t, db, models.TransactionStatusSending)
	oldRequestTime := time.Now().Add(-2 * time.Second)
	if err := db.Model(transaction).Updates(map[string]interface{}{
		"cancellation_requested_at": oldRequestTime,
		"status_message":            "Withdrawal request timed out before broadcast",
	}).Error; err != nil {
		t.Fatalf("persist cancellation request: %v", err)
	}

	_, err := handleTimeoutWithdrawalRequest(context.Background(), db, record)
	if !errors.Is(err, ErrWithdrawalRequestTransactionUnconfirmedTimeout) {
		t.Fatalf("expected fail-stop timeout, got %v", err)
	}
	if counts.reject.Load() != 0 || counts.fulfill.Load() != 0 {
		t.Fatalf("unsettled sending transaction must not trigger callbacks")
	}
}

func TestHandleTimeoutRestartDoesNotResetCancellationDeadline(t *testing.T) {
	db, _ := setupWithdrawalCancellationTest(t)
	record, transaction := createCancellationTestWithdrawal(t, db, models.TransactionStatusSending)

	completed, err := handleTimeoutWithdrawalRequest(context.Background(), db, record)
	if err != nil || completed {
		t.Fatalf("initial timeout check: completed=%v err=%v", completed, err)
	}
	var first models.BlockchainTransaction
	if err := db.First(&first, transaction.ID).Error; err != nil {
		t.Fatalf("load first cancellation request: %v", err)
	}
	if err := db.Model(transaction).Update(
		"cancellation_requested_at",
		first.CancellationRequestedAt.Time.Add(-2*time.Second),
	).Error; err != nil {
		t.Fatalf("age cancellation request: %v", err)
	}

	var restartedRecord models.WithdrawRecord
	if err := db.First(&restartedRecord, record.ID).Error; err != nil {
		t.Fatalf("reload withdrawal after restart: %v", err)
	}
	_, err = handleTimeoutWithdrawalRequest(context.Background(), db, &restartedRecord)
	if !errors.Is(err, ErrWithdrawalRequestTransactionUnconfirmedTimeout) {
		t.Fatalf("expected persisted deadline fail-stop, got %v", err)
	}
	var saved models.BlockchainTransaction
	if err := db.First(&saved, transaction.ID).Error; err != nil {
		t.Fatalf("load transaction after restart: %v", err)
	}
	expected := first.CancellationRequestedAt.Time.Add(-2 * time.Second)
	if !saved.CancellationRequestedAt.Time.Equal(expected) {
		t.Fatalf("cancellation deadline reset: got %v want %v", saved.CancellationRequestedAt.Time, expected)
	}
}
