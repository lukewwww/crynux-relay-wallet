package models

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newBlockchainTransactionTestDB(t *testing.T) *gorm.DB {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(&BlockchainTransaction{}); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	return db
}

func createBlockchainTransactionTestRecord(t *testing.T, db *gorm.DB) *BlockchainTransaction {
	t.Helper()

	transaction := &BlockchainTransaction{
		Network:     "testnet",
		Type:        "SendETH",
		Status:      TransactionStatusPending,
		FromAddress: "0x1111111111111111111111111111111111111111",
		ToAddress:   "0x2222222222222222222222222222222222222222",
		Value:       "1",
	}
	if err := transaction.Save(context.Background(), db); err != nil {
		t.Fatalf("save transaction: %v", err)
	}
	return transaction
}

func loadBlockchainTransactionTestRecord(t *testing.T, db *gorm.DB, id uint) BlockchainTransaction {
	t.Helper()

	var transaction BlockchainTransaction
	if err := db.First(&transaction, id).Error; err != nil {
		t.Fatalf("load transaction: %v", err)
	}
	return transaction
}

func TestBlockchainTransactionRequestBeforeClaim(t *testing.T) {
	ctx := context.Background()
	db := newBlockchainTransactionTestDB(t)
	transaction := createBlockchainTransactionTestRecord(t, db)

	requested, err := transaction.RequestCancellation(ctx, db, "test cancellation")
	if err != nil || !requested {
		t.Fatalf("request cancellation: requested=%v err=%v", requested, err)
	}
	requestedAt := transaction.CancellationRequestedAt.Time

	time.Sleep(time.Millisecond)
	requested, err = transaction.RequestCancellation(ctx, db, "new reason")
	if err != nil {
		t.Fatalf("repeat cancellation request: %v", err)
	}
	if requested {
		t.Fatalf("repeat cancellation request must not rewrite intent")
	}

	claimed, err := transaction.ClaimForSending(ctx, db)
	if err != nil || claimed {
		t.Fatalf("transaction with cancellation intent must not be claimed: claimed=%v err=%v", claimed, err)
	}

	cancelled, err := transaction.CancelRequestedUnbroadcasted(ctx, db)
	if err != nil || !cancelled {
		t.Fatalf("cancel requested transaction: cancelled=%v err=%v", cancelled, err)
	}

	saved := loadBlockchainTransactionTestRecord(t, db, transaction.ID)
	if !saved.CancellationRequestedAt.Valid || !saved.CancellationRequestedAt.Time.Equal(requestedAt) {
		t.Fatalf("cancellation request time changed: got %v want %v", saved.CancellationRequestedAt, requestedAt)
	}
	if saved.StatusMessage.String != "test cancellation" {
		t.Fatalf("cancellation reason changed: %q", saved.StatusMessage.String)
	}
}

func TestRecoverLegacySendingTransactionReturnsPending(t *testing.T) {
	db := newBlockchainTransactionTestDB(t)
	transaction := createBlockchainTransactionTestRecord(t, db)
	if err := db.Model(transaction).Update("status", TransactionStatusSending).Error; err != nil {
		t.Fatalf("set sending status: %v", err)
	}

	if err := RecoverLegacySendingTransactions(context.Background(), db); err != nil {
		t.Fatalf("recover sending transactions: %v", err)
	}
	saved := loadBlockchainTransactionTestRecord(t, db, transaction.ID)
	if saved.Status != TransactionStatusPending {
		t.Fatalf("expected pending status, got %d", saved.Status)
	}
}

func TestRecoverLegacySendingTransactionWithCancellationBecomesCancelled(t *testing.T) {
	db := newBlockchainTransactionTestDB(t)
	transaction := createBlockchainTransactionTestRecord(t, db)
	if err := db.Model(transaction).Updates(map[string]interface{}{
		"status":                    TransactionStatusSending,
		"cancellation_requested_at": time.Now(),
	}).Error; err != nil {
		t.Fatalf("set sending cancellation state: %v", err)
	}

	if err := RecoverLegacySendingTransactions(context.Background(), db); err != nil {
		t.Fatalf("recover sending transactions: %v", err)
	}
	saved := loadBlockchainTransactionTestRecord(t, db, transaction.ID)
	if saved.Status != TransactionStatusCancelled || !saved.FailedAt.Valid {
		t.Fatalf("unexpected recovered cancellation: status=%d failed_at=%v", saved.Status, saved.FailedAt)
	}
}

func TestRecoverLegacySendingTransactionsPreservesCommittedBroadcast(t *testing.T) {
	db := newBlockchainTransactionTestDB(t)
	transaction := createBlockchainTransactionTestRecord(t, db)
	if err := db.Model(transaction).Updates(map[string]interface{}{
		"status":        TransactionStatusSending,
		"tx_hash":       "0xhash",
		"signed_raw_tx": "0xraw",
	}).Error; err != nil {
		t.Fatalf("set committed sending state: %v", err)
	}
	broadcasting := createBlockchainTransactionTestRecord(t, db)
	if err := db.Model(broadcasting).Updates(map[string]interface{}{
		"status":        TransactionStatusBroadcasting,
		"tx_hash":       "0xother",
		"signed_raw_tx": "0xotherraw",
	}).Error; err != nil {
		t.Fatalf("set broadcasting state: %v", err)
	}

	if err := RecoverLegacySendingTransactions(context.Background(), db); err != nil {
		t.Fatalf("recover sending transactions: %v", err)
	}
	savedSending := loadBlockchainTransactionTestRecord(t, db, transaction.ID)
	if savedSending.Status != TransactionStatusSending {
		t.Fatalf("committed sending status changed to %d", savedSending.Status)
	}
	savedBroadcasting := loadBlockchainTransactionTestRecord(t, db, broadcasting.ID)
	if savedBroadcasting.Status != TransactionStatusBroadcasting {
		t.Fatalf("broadcasting status changed to %d", savedBroadcasting.Status)
	}
}

func TestBlockchainTransactionRequestBeforeReleaseCancelsSending(t *testing.T) {
	ctx := context.Background()
	db := newBlockchainTransactionTestDB(t)
	transaction := createBlockchainTransactionTestRecord(t, db)

	claimed, err := transaction.ClaimForSending(ctx, db)
	if err != nil || !claimed {
		t.Fatalf("claim transaction: claimed=%v err=%v", claimed, err)
	}
	requested, err := transaction.RequestCancellation(ctx, db, "test cancellation")
	if err != nil || !requested {
		t.Fatalf("request cancellation: requested=%v err=%v", requested, err)
	}
	if err := transaction.ReleaseSending(ctx, db, "temporary send error"); err != nil {
		t.Fatalf("release transaction: %v", err)
	}

	saved := loadBlockchainTransactionTestRecord(t, db, transaction.ID)
	if saved.Status != TransactionStatusCancelled {
		t.Fatalf("expected cancelled status, got %d", saved.Status)
	}
	if saved.StatusMessage.String != "test cancellation" {
		t.Fatalf("release overwrote cancellation reason: %q", saved.StatusMessage.String)
	}
	claimed, err = transaction.ClaimForSending(ctx, db)
	if err != nil || claimed {
		t.Fatalf("cancelled transaction must not be reclaimed: claimed=%v err=%v", claimed, err)
	}
}

func TestBlockchainTransactionReleaseBeforeRequestCannotBeReclaimed(t *testing.T) {
	ctx := context.Background()
	db := newBlockchainTransactionTestDB(t)
	transaction := createBlockchainTransactionTestRecord(t, db)

	claimed, err := transaction.ClaimForSending(ctx, db)
	if err != nil || !claimed {
		t.Fatalf("claim transaction: claimed=%v err=%v", claimed, err)
	}
	if err := transaction.ReleaseSending(ctx, db, "temporary send error"); err != nil {
		t.Fatalf("release transaction: %v", err)
	}
	requested, err := transaction.RequestCancellation(ctx, db, "test cancellation")
	if err != nil || !requested {
		t.Fatalf("request cancellation: requested=%v err=%v", requested, err)
	}

	claimed, err = transaction.ClaimForSending(ctx, db)
	if err != nil || claimed {
		t.Fatalf("requested transaction must not be reclaimed: claimed=%v err=%v", claimed, err)
	}
	cancelled, err := transaction.CancelRequestedUnbroadcasted(ctx, db)
	if err != nil || !cancelled {
		t.Fatalf("cancel requested transaction: cancelled=%v err=%v", cancelled, err)
	}
}

func TestBlockchainTransactionPrepareBroadcastWinsOverCancellation(t *testing.T) {
	ctx := context.Background()
	db := newBlockchainTransactionTestDB(t)
	transaction := createBlockchainTransactionTestRecord(t, db)

	claimed, err := transaction.ClaimForSending(ctx, db)
	if err != nil || !claimed {
		t.Fatalf("claim transaction: claimed=%v err=%v", claimed, err)
	}
	prepared, err := transaction.PrepareBroadcast(ctx, db, "0x1234", 7, "0xraw")
	if err != nil || !prepared {
		t.Fatalf("prepare broadcast: prepared=%v err=%v", prepared, err)
	}
	requested, err := transaction.RequestCancellation(ctx, db, "test cancellation")
	if err != nil || requested {
		t.Fatalf("broadcasting transaction must not accept cancellation: requested=%v err=%v", requested, err)
	}
	marked, err := transaction.MarkSent(ctx, db)
	if err != nil || !marked {
		t.Fatalf("mark sent: marked=%v err=%v", marked, err)
	}

	saved := loadBlockchainTransactionTestRecord(t, db, transaction.ID)
	if saved.Status != TransactionStatusSent || saved.TxHash.String != "0x1234" || saved.SignedRawTx.String != "0xraw" {
		t.Fatalf("unexpected sent transaction state: %+v", saved)
	}
}

func TestBlockchainTransactionCancellationBeforePrepareBlocksBroadcast(t *testing.T) {
	ctx := context.Background()
	db := newBlockchainTransactionTestDB(t)
	transaction := createBlockchainTransactionTestRecord(t, db)

	claimed, err := transaction.ClaimForSending(ctx, db)
	if err != nil || !claimed {
		t.Fatalf("claim transaction: claimed=%v err=%v", claimed, err)
	}
	requested, err := transaction.RequestCancellation(ctx, db, "test cancellation")
	if err != nil || !requested {
		t.Fatalf("request cancellation: requested=%v err=%v", requested, err)
	}
	prepared, err := transaction.PrepareBroadcast(ctx, db, "0x1234", 7, "0xraw")
	if err != nil || prepared {
		t.Fatalf("cancelled sending must not prepare broadcast: prepared=%v err=%v", prepared, err)
	}
	if err := transaction.ReleaseSending(ctx, db, "cancelled before broadcast"); err != nil {
		t.Fatalf("release sending: %v", err)
	}

	saved := loadBlockchainTransactionTestRecord(t, db, transaction.ID)
	if saved.Status != TransactionStatusCancelled || saved.TxHash.Valid || saved.SignedRawTx.Valid {
		t.Fatalf("unexpected cancelled state: %+v", saved)
	}
}

func TestBlockchainTransactionReleaseRequiresNoBroadcastPayload(t *testing.T) {
	ctx := context.Background()
	db := newBlockchainTransactionTestDB(t)
	transaction := createBlockchainTransactionTestRecord(t, db)

	claimed, err := transaction.ClaimForSending(ctx, db)
	if err != nil || !claimed {
		t.Fatalf("claim transaction: claimed=%v err=%v", claimed, err)
	}
	prepared, err := transaction.PrepareBroadcast(ctx, db, "0x1234", 7, "0xraw")
	if err != nil || !prepared {
		t.Fatalf("prepare broadcast: prepared=%v err=%v", prepared, err)
	}
	if err := transaction.ReleaseSending(ctx, db, "must not release"); err != nil {
		t.Fatalf("release sending: %v", err)
	}

	saved := loadBlockchainTransactionTestRecord(t, db, transaction.ID)
	if saved.Status != TransactionStatusBroadcasting {
		t.Fatalf("prepared broadcast must remain broadcasting, got %d", saved.Status)
	}
}

func TestBlockchainTransactionCancellationRequiresNoTxHash(t *testing.T) {
	ctx := context.Background()
	db := newBlockchainTransactionTestDB(t)
	transaction := createBlockchainTransactionTestRecord(t, db)
	if err := transaction.Update(ctx, db, map[string]interface{}{
		"tx_hash": sql.NullString{String: "0x1234", Valid: true},
	}); err != nil {
		t.Fatalf("set transaction hash: %v", err)
	}

	requested, err := transaction.RequestCancellation(ctx, db, "test cancellation")
	if err != nil || requested {
		t.Fatalf("hashed transaction must not accept cancellation: requested=%v err=%v", requested, err)
	}
}

func TestBlockchainTransactionMarkFailedPreservesTxHash(t *testing.T) {
	ctx := context.Background()
	db := newBlockchainTransactionTestDB(t)
	transaction := createBlockchainTransactionTestRecord(t, db)
	if err := transaction.Update(ctx, db, map[string]interface{}{
		"status":  TransactionStatusSent,
		"tx_hash": "0xabc",
		"nonce":   3,
		"sent_at": time.Now(),
	}); err != nil {
		t.Fatalf("prepare sent transaction: %v", err)
	}
	if err := transaction.Sync(ctx, db); err != nil {
		t.Fatalf("sync transaction: %v", err)
	}
	if err := transaction.MarkFailed(ctx, db, 10, 21000, "1", "Transaction failed with status 0"); err != nil {
		t.Fatalf("mark failed: %v", err)
	}

	saved := loadBlockchainTransactionTestRecord(t, db, transaction.ID)
	if saved.TxHash.String != "0xabc" {
		t.Fatalf("failed transaction hash rewritten: %q", saved.TxHash.String)
	}
}

func TestBlockchainTransactionMarkFailedFromSentIsAtomic(t *testing.T) {
	ctx := context.Background()
	db := newBlockchainTransactionTestDB(t)
	transaction := createBlockchainTransactionTestRecord(t, db)
	if err := transaction.Update(ctx, db, map[string]interface{}{
		"status":  TransactionStatusSent,
		"tx_hash": "0xabc",
		"nonce":   3,
		"sent_at": time.Now(),
	}); err != nil {
		t.Fatalf("prepare sent transaction: %v", err)
	}
	if err := transaction.Sync(ctx, db); err != nil {
		t.Fatalf("sync transaction: %v", err)
	}

	marked, err := transaction.MarkFailedFromSent(ctx, db, 10, 21000, "1", "Transaction failed with status 0")
	if err != nil || !marked {
		t.Fatalf("first mark failed from sent: marked=%v err=%v", marked, err)
	}
	marked, err = transaction.MarkFailedFromSent(ctx, db, 11, 21000, "1", "duplicate")
	if err != nil || marked {
		t.Fatalf("second mark failed from sent must be no-op: marked=%v err=%v", marked, err)
	}
	saved := loadBlockchainTransactionTestRecord(t, db, transaction.ID)
	if saved.BlockNumber.Int64 != 10 || saved.StatusMessage.String != "Transaction failed with status 0" {
		t.Fatalf("unexpected failed state: %+v", saved)
	}
}

func TestBlockchainTransactionClaimBlocksUnsafeRetryAncestors(t *testing.T) {
	ctx := context.Background()
	db := newBlockchainTransactionTestDB(t)
	root := createBlockchainTransactionTestRecord(t, db)
	if err := root.Update(ctx, db, map[string]interface{}{
		"status":         TransactionStatusFailed,
		"tx_hash":        "0xdead",
		"nonce":          1,
		"status_message": TransactionTimedOutMessage,
	}); err != nil {
		t.Fatalf("set timeout failure: %v", err)
	}

	retry := &BlockchainTransaction{
		Network:            root.Network,
		Type:               root.Type,
		Status:             TransactionStatusPending,
		FromAddress:        root.FromAddress,
		ToAddress:          root.ToAddress,
		Value:              root.Value,
		RetryCount:         1,
		MaxRetries:         3,
		RetryTransactionID: sql.NullInt64{Int64: int64(root.ID), Valid: true},
	}
	if err := retry.Save(ctx, db); err != nil {
		t.Fatalf("save retry: %v", err)
	}

	claimed, err := retry.ClaimForSending(ctx, db)
	if !errors.Is(err, ErrRetryAncestorsUnsafe) || claimed {
		t.Fatalf("timeout ancestor must block claim: claimed=%v err=%v", claimed, err)
	}
}

func TestBlockchainTransactionClaimAllowsProvenFailureAncestors(t *testing.T) {
	ctx := context.Background()
	db := newBlockchainTransactionTestDB(t)
	root := createBlockchainTransactionTestRecord(t, db)
	if err := root.Update(ctx, db, map[string]interface{}{
		"status":         TransactionStatusFailed,
		"tx_hash":        "0xdead",
		"nonce":          1,
		"block_number":   42,
		"status_message": "Transaction failed with status 0",
	}); err != nil {
		t.Fatalf("set on-chain failure: %v", err)
	}

	retry := &BlockchainTransaction{
		Network:            root.Network,
		Type:               root.Type,
		Status:             TransactionStatusPending,
		FromAddress:        root.FromAddress,
		ToAddress:          root.ToAddress,
		Value:              root.Value,
		RetryCount:         1,
		MaxRetries:         3,
		RetryTransactionID: sql.NullInt64{Int64: int64(root.ID), Valid: true},
	}
	if err := retry.Save(ctx, db); err != nil {
		t.Fatalf("save retry: %v", err)
	}

	claimed, err := retry.ClaimForSending(ctx, db)
	if err != nil || !claimed {
		t.Fatalf("proven failure ancestor must allow claim: claimed=%v err=%v", claimed, err)
	}
}

func TestClassifyTransactionChainRejectAndFulfillOutcomes(t *testing.T) {
	timeoutRoot := BlockchainTransaction{
		Status:        TransactionStatusFailed,
		TxHash:        sql.NullString{String: "0x1", Valid: true},
		StatusMessage: sql.NullString{String: TransactionTimedOutMessage, Valid: true},
	}
	cancelledRetry := BlockchainTransaction{
		Status: TransactionStatusCancelled,
	}
	outcome := ClassifyTransactionChain([]BlockchainTransaction{timeoutRoot, cancelledRetry})
	if outcome.AllProvenFail || outcome.Blocking == nil || !outcome.Blocking.IsReceiptTimeoutFailure() {
		t.Fatalf("timeout ancestor must block reject classification: %+v", outcome)
	}

	failedRoot := BlockchainTransaction{
		Status:      TransactionStatusFailed,
		TxHash:      sql.NullString{String: "0x1", Valid: true},
		BlockNumber: sql.NullInt64{Int64: 10, Valid: true},
	}
	confirmedRetry := BlockchainTransaction{
		Status: TransactionStatusConfirmed,
		TxHash: sql.NullString{String: "0x2", Valid: true},
	}
	outcome = ClassifyTransactionChain([]BlockchainTransaction{failedRoot, confirmedRetry})
	if outcome.ConfirmedCount != 1 || outcome.AllProvenFail {
		t.Fatalf("expected single confirmed with proven failure ancestor: %+v", outcome)
	}
	if !failedRoot.IsProvenOnChainFailure() || confirmedRetry.IsBlockingUncertainty() {
		t.Fatal("unexpected proven failure classification")
	}
}
