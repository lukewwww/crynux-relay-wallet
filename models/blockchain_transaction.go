package models

import (
	"context"
	"crynux_relay_wallet/config"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

var ErrRetryAncestorsUnsafe = errors.New("retry ancestors lack proven on-chain failure")

// TransactionStatus represents the status of a blockchain transaction.
type TransactionStatus uint8

const (
	TransactionStatusPending TransactionStatus = iota
	TransactionStatusSent
	TransactionStatusConfirmed
	TransactionStatusFailed
	TransactionStatusSending
	TransactionStatusCancelled
	TransactionStatusBroadcasting
)

const (
	TransactionReceiptDelayedMessage = "Transaction receipt delayed"
	TransactionTimedOutMessage       = "Transaction timed out"
)

// BlockchainTransaction represents a blockchain transaction that needs to be sent
type BlockchainTransaction struct {
	gorm.Model
	Network                 string            `json:"network" gorm:"index;not null"`
	Type                    string            `json:"type" gorm:"index;not null"`
	Status                  TransactionStatus `json:"status" gorm:"index;not null;default:0"`
	FromAddress             string            `json:"from_address" gorm:"not null"`
	ToAddress               string            `json:"to_address" gorm:"not null"`
	Value                   string            `json:"value" gorm:"not null;default:'0'"`
	Data                    sql.NullString    `json:"data" gorm:"null"`
	TxHash                  sql.NullString    `json:"tx_hash" gorm:"null;uniqueIndex"`
	BlockNumber             sql.NullInt64     `json:"block_number" gorm:"null"`
	Nonce                   sql.NullInt64     `json:"nonce" gorm:"null"`
	GasUsed                 sql.NullInt64     `json:"gas_used" gorm:"null"`
	EffectiveGasPrice       sql.NullString    `json:"effective_gas_price" gorm:"null"`
	StatusMessage           sql.NullString    `json:"status_message" gorm:"null"`
	RetryCount              uint8             `json:"retry_count" gorm:"not null;default:0"`
	MaxRetries              uint8             `json:"max_retries" gorm:"not null;default:0"`
	RetryTransactionID      sql.NullInt64     `json:"retry_transaction_id" gorm:"null;index"`
	NextRetryAt             sql.NullTime      `json:"next_retry_at" gorm:"null"`
	SentAt                  sql.NullTime      `json:"sent_at" gorm:"null"`
	ConfirmedAt             sql.NullTime      `json:"confirmed_at" gorm:"null"`
	FailedAt                sql.NullTime      `json:"failed_at" gorm:"null"`
	CancellationRequestedAt sql.NullTime      `json:"cancellation_requested_at" gorm:"null"`
	SignedRawTx             sql.NullString    `json:"signed_raw_tx" gorm:"type:mediumtext;null"`
}

// TableName returns the table name for BlockchainTransaction
func (BlockchainTransaction) TableName() string {
	return "blockchain_transactions"
}

// Save saves the blockchain transaction to database
func (tx *BlockchainTransaction) Save(ctx context.Context, db *gorm.DB) error {
	dbCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.WithContext(dbCtx).Save(&tx).Error; err != nil {
		return err
	}
	return nil
}

func (tx *BlockchainTransaction) Sync(ctx context.Context, db *gorm.DB) error {
	dbCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.WithContext(dbCtx).Model(tx).First(tx).Error; err != nil {
		return err
	}
	return nil
}

// Update updates the blockchain transaction in database
func (tx *BlockchainTransaction) Update(ctx context.Context, db *gorm.DB, values map[string]interface{}) error {
	if tx.ID == 0 {
		return gorm.ErrRecordNotFound
	}
	dbCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := db.WithContext(dbCtx).Model(tx).Updates(values).Error; err != nil {
		return err
	}
	return nil
}

// GetPendingTransactions gets all pending transactions from database
func GetPendingTransactions(ctx context.Context, db *gorm.DB, offset, limit int) ([]BlockchainTransaction, error) {
	dbCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var transactions []BlockchainTransaction
	if err := db.WithContext(dbCtx).Where("status = ?", TransactionStatusPending).Order("id").Offset(offset).Limit(limit).Find(&transactions).Error; err != nil {
		return nil, err
	}
	return transactions, nil
}

// GetBroadcastingTransactions gets transactions that have a signed payload awaiting broadcast acknowledgment.
func GetBroadcastingTransactions(ctx context.Context, db *gorm.DB, offset, limit int) ([]BlockchainTransaction, error) {
	dbCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var transactions []BlockchainTransaction
	if err := db.WithContext(dbCtx).
		Where("status = ?", TransactionStatusBroadcasting).
		Order("id").
		Offset(offset).
		Limit(limit).
		Find(&transactions).Error; err != nil {
		return nil, err
	}
	return transactions, nil
}

func RecoverLegacySendingTransactions(ctx context.Context, db *gorm.DB) error {
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&BlockchainTransaction{}).
			Where("status = ? AND tx_hash IS NULL AND signed_raw_tx IS NULL AND cancellation_requested_at IS NULL", TransactionStatusSending).
			Update("status", TransactionStatusPending).Error; err != nil {
			return err
		}
		return tx.Model(&BlockchainTransaction{}).
			Where("status = ? AND tx_hash IS NULL AND signed_raw_tx IS NULL AND cancellation_requested_at IS NOT NULL", TransactionStatusSending).
			Updates(map[string]interface{}{
				"status":    TransactionStatusCancelled,
				"failed_at": time.Now(),
			}).Error
	})
}

// GetSentTransactions gets all sent transactions that need confirmation from database
func GetSentTransactions(ctx context.Context, db *gorm.DB, offset, limit int) ([]BlockchainTransaction, error) {
	dbCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var transactions []BlockchainTransaction
	if err := db.WithContext(dbCtx).Where("status = ?", TransactionStatusSent).Order("id").Offset(offset).Limit(limit).Find(&transactions).Error; err != nil {
		return nil, err
	}
	return transactions, nil
}

func GetSentTransactionCountByNetwork(ctx context.Context, db *gorm.DB, network string) (int64, error) {
	dbCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var count int64
	if err := db.WithContext(dbCtx).Model(&BlockchainTransaction{}).
		Where("network = ?", network).
		Where("status IN ?", []TransactionStatus{TransactionStatusSent, TransactionStatusBroadcasting}).
		Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

func GetBroadcastingTransactionCountByNetwork(ctx context.Context, db *gorm.DB, network string) (int64, error) {
	dbCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var count int64
	if err := db.WithContext(dbCtx).Model(&BlockchainTransaction{}).
		Where("network = ? AND status = ?", network, TransactionStatusBroadcasting).
		Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

// GetTransactionByHash gets a transaction by its hash
func GetTransactionByHash(ctx context.Context, db *gorm.DB, txHash string) (*BlockchainTransaction, error) {
	dbCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var transaction BlockchainTransaction
	if err := db.WithContext(dbCtx).Where("tx_hash = ?", txHash).First(&transaction).Error; err != nil {
		return nil, err
	}
	return &transaction, nil
}

func GetTransactionByID(ctx context.Context, db *gorm.DB, id uint) (*BlockchainTransaction, error) {
	var transaction BlockchainTransaction
	if err := db.WithContext(ctx).First(&transaction, id).Error; err != nil {
		return nil, err
	}
	return &transaction, nil
}

func (tx *BlockchainTransaction) MarkSent(ctx context.Context, db *gorm.DB) (bool, error) {
	if tx.ID == 0 {
		return false, gorm.ErrRecordNotFound
	}
	dbCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	sentAt := time.Now()
	result := db.WithContext(dbCtx).Model(&BlockchainTransaction{}).
		Where("id = ? AND status = ? AND tx_hash IS NOT NULL AND signed_raw_tx IS NOT NULL", tx.ID, TransactionStatusBroadcasting).
		Updates(map[string]interface{}{
			"status":  TransactionStatusSent,
			"sent_at": sentAt,
		})
	if result.Error != nil {
		return false, result.Error
	}
	if result.RowsAffected == 0 {
		if err := tx.Sync(ctx, db); err != nil {
			return false, err
		}
		return tx.Status == TransactionStatusSent, nil
	}
	tx.Status = TransactionStatusSent
	tx.SentAt = sql.NullTime{Time: sentAt, Valid: true}
	return true, nil
}

func (tx *BlockchainTransaction) ClaimForSending(ctx context.Context, db *gorm.DB) (bool, error) {
	if tx.ID == 0 {
		return false, gorm.ErrRecordNotFound
	}

	var claimed bool
	err := db.WithContext(ctx).Transaction(func(txDB *gorm.DB) error {
		if tx.RetryTransactionID.Valid {
			chain, err := ListTransactionChain(ctx, txDB, uint(tx.RetryTransactionID.Int64))
			if err != nil {
				return err
			}
			for i := range chain {
				ancestor := &chain[i]
				if ancestor.ID == tx.ID {
					break
				}
				if !ancestor.IsProvenOnChainFailure() {
					return ErrRetryAncestorsUnsafe
				}
			}
		}

		dbCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		result := txDB.WithContext(dbCtx).Model(&BlockchainTransaction{}).
			Where("id = ? AND status = ? AND tx_hash IS NULL AND signed_raw_tx IS NULL AND cancellation_requested_at IS NULL", tx.ID, TransactionStatusPending).
			Updates(map[string]interface{}{
				"status": TransactionStatusSending,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return nil
		}
		claimed = true
		tx.Status = TransactionStatusSending
		return nil
	})
	if err != nil {
		return false, err
	}
	return claimed, nil
}

func (tx *BlockchainTransaction) ReleaseSending(ctx context.Context, db *gorm.DB, errorMsg string) error {
	if tx.ID == 0 {
		return gorm.ErrRecordNotFound
	}
	dbCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	now := time.Now()
	updates := map[string]interface{}{
		"status": gorm.Expr(
			"CASE WHEN cancellation_requested_at IS NULL THEN ? ELSE ? END",
			TransactionStatusPending,
			TransactionStatusCancelled,
		),
		"failed_at": gorm.Expr(
			"CASE WHEN cancellation_requested_at IS NULL THEN failed_at ELSE ? END",
			now,
		),
	}
	if errorMsg != "" {
		updates["status_message"] = gorm.Expr(
			"CASE WHEN cancellation_requested_at IS NULL THEN ? ELSE status_message END",
			errorMsg,
		)
	}
	result := db.WithContext(dbCtx).Model(&BlockchainTransaction{}).
		Where("id = ? AND status = ? AND tx_hash IS NULL AND signed_raw_tx IS NULL", tx.ID, TransactionStatusSending).
		Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected > 0 {
		return tx.Sync(ctx, db)
	}
	return nil
}

func (tx *BlockchainTransaction) PrepareBroadcast(ctx context.Context, db *gorm.DB, txHash string, nonce int64, signedRawTx string) (bool, error) {
	if tx.ID == 0 {
		return false, gorm.ErrRecordNotFound
	}
	if txHash == "" || signedRawTx == "" {
		return false, fmt.Errorf("broadcast payload is incomplete")
	}
	dbCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	result := db.WithContext(dbCtx).Model(&BlockchainTransaction{}).
		Where(
			"id = ? AND status = ? AND tx_hash IS NULL AND signed_raw_tx IS NULL AND cancellation_requested_at IS NULL",
			tx.ID,
			TransactionStatusSending,
		).
		Updates(map[string]interface{}{
			"status":        TransactionStatusBroadcasting,
			"tx_hash":       txHash,
			"nonce":         nonce,
			"signed_raw_tx": signedRawTx,
		})
	if result.Error != nil {
		return false, result.Error
	}
	if result.RowsAffected == 0 {
		if err := tx.Sync(ctx, db); err != nil {
			return false, err
		}
		return false, nil
	}
	tx.Status = TransactionStatusBroadcasting
	tx.TxHash = sql.NullString{String: txHash, Valid: true}
	tx.Nonce = sql.NullInt64{Int64: nonce, Valid: true}
	tx.SignedRawTx = sql.NullString{String: signedRawTx, Valid: true}
	return true, nil
}

func (tx *BlockchainTransaction) RequestCancellation(ctx context.Context, db *gorm.DB, reason string) (bool, error) {
	if tx.ID == 0 {
		return false, gorm.ErrRecordNotFound
	}
	dbCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	requestedAt := time.Now()
	result := db.WithContext(dbCtx).Model(&BlockchainTransaction{}).
		Where(
			"id = ? AND status IN ? AND tx_hash IS NULL AND signed_raw_tx IS NULL AND cancellation_requested_at IS NULL",
			tx.ID,
			[]TransactionStatus{TransactionStatusPending, TransactionStatusSending},
		).
		Updates(map[string]interface{}{
			"cancellation_requested_at": requestedAt,
			"status_message":            reason,
		})
	if result.Error != nil {
		return false, result.Error
	}
	if result.RowsAffected == 0 {
		return false, nil
	}
	tx.CancellationRequestedAt = sql.NullTime{Time: requestedAt, Valid: true}
	tx.StatusMessage = sql.NullString{String: reason, Valid: true}
	return true, nil
}

func (tx *BlockchainTransaction) CancelRequestedUnbroadcasted(ctx context.Context, db *gorm.DB) (bool, error) {
	if tx.ID == 0 {
		return false, gorm.ErrRecordNotFound
	}
	dbCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	updates := map[string]interface{}{
		"status":    TransactionStatusCancelled,
		"failed_at": time.Now(),
	}
	result := db.WithContext(dbCtx).Model(&BlockchainTransaction{}).
		Where(
			"id = ? AND status = ? AND tx_hash IS NULL AND signed_raw_tx IS NULL AND cancellation_requested_at IS NOT NULL",
			tx.ID,
			TransactionStatusPending,
		).
		Updates(updates)
	if result.Error != nil {
		return false, result.Error
	}
	if result.RowsAffected == 0 {
		return false, nil
	}
	tx.Status = TransactionStatusCancelled
	tx.FailedAt = sql.NullTime{Time: updates["failed_at"].(time.Time), Valid: true}
	return true, nil
}

func (tx *BlockchainTransaction) MarkConfirmed(ctx context.Context, db *gorm.DB, blockNumber, gasUsed int64, effectiveGasPrice string) (bool, error) {
	if tx.ID == 0 {
		return false, gorm.ErrRecordNotFound
	}
	dbCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	confirmedAt := time.Now()
	result := db.WithContext(dbCtx).Model(&BlockchainTransaction{}).
		Where("id = ? AND status = ?", tx.ID, TransactionStatusSent).
		Updates(map[string]interface{}{
			"status":              TransactionStatusConfirmed,
			"confirmed_at":        confirmedAt,
			"block_number":        blockNumber,
			"gas_used":            gasUsed,
			"effective_gas_price": effectiveGasPrice,
		})
	if result.Error != nil {
		return false, result.Error
	}
	if result.RowsAffected == 0 {
		if err := tx.Sync(ctx, db); err != nil {
			return false, err
		}
		return tx.Status == TransactionStatusConfirmed, nil
	}
	tx.Status = TransactionStatusConfirmed
	tx.ConfirmedAt = sql.NullTime{Time: confirmedAt, Valid: true}
	tx.BlockNumber = sql.NullInt64{Int64: blockNumber, Valid: true}
	tx.GasUsed = sql.NullInt64{Int64: gasUsed, Valid: true}
	tx.EffectiveGasPrice = sql.NullString{String: effectiveGasPrice, Valid: true}
	return true, nil
}

func (tx *BlockchainTransaction) MarkFailed(ctx context.Context, db *gorm.DB, blockNumber, gasUsed int64, effectiveGasPrice string, errorMsg string) error {
	updates := map[string]interface{}{
		"status":              TransactionStatusFailed,
		"failed_at":           time.Now(),
		"block_number":        blockNumber,
		"gas_used":            gasUsed,
		"effective_gas_price": effectiveGasPrice,
		"status_message":      errorMsg,
	}

	return tx.Update(ctx, db, updates)
}

// MarkFailedFromSent atomically records a receipt status=0 failure from sent.
// Only the first successful transition MAY create a retry.
func (tx *BlockchainTransaction) MarkFailedFromSent(ctx context.Context, db *gorm.DB, blockNumber, gasUsed int64, effectiveGasPrice string, errorMsg string) (bool, error) {
	if tx.ID == 0 {
		return false, gorm.ErrRecordNotFound
	}
	dbCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	failedAt := time.Now()
	result := db.WithContext(dbCtx).Model(&BlockchainTransaction{}).
		Where("id = ? AND status = ? AND tx_hash IS NOT NULL", tx.ID, TransactionStatusSent).
		Updates(map[string]interface{}{
			"status":              TransactionStatusFailed,
			"failed_at":           failedAt,
			"block_number":        blockNumber,
			"gas_used":            gasUsed,
			"effective_gas_price": effectiveGasPrice,
			"status_message":      errorMsg,
		})
	if result.Error != nil {
		return false, result.Error
	}
	if result.RowsAffected == 0 {
		return false, nil
	}
	tx.Status = TransactionStatusFailed
	tx.FailedAt = sql.NullTime{Time: failedAt, Valid: true}
	tx.BlockNumber = sql.NullInt64{Int64: blockNumber, Valid: true}
	tx.GasUsed = sql.NullInt64{Int64: gasUsed, Valid: true}
	tx.EffectiveGasPrice = sql.NullString{String: effectiveGasPrice, Valid: true}
	tx.StatusMessage = sql.NullString{String: errorMsg, Valid: true}
	return true, nil
}

func (tx *BlockchainTransaction) MarkReceiptDelayed(ctx context.Context, db *gorm.DB) (bool, error) {
	if tx.ID == 0 {
		return false, gorm.ErrRecordNotFound
	}
	dbCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	result := db.WithContext(dbCtx).Model(&BlockchainTransaction{}).
		Where(
			"id = ? AND status = ? AND (status_message IS NULL OR status_message <> ?)",
			tx.ID,
			TransactionStatusSent,
			TransactionReceiptDelayedMessage,
		).
		Updates(map[string]interface{}{
			"status_message": TransactionReceiptDelayedMessage,
		})
	if result.Error != nil {
		return false, result.Error
	}
	if result.RowsAffected == 0 {
		return false, nil
	}
	tx.StatusMessage = sql.NullString{String: TransactionReceiptDelayedMessage, Valid: true}
	return true, nil
}

func (tx *BlockchainTransaction) CreateRetryTransaction(ctx context.Context, db *gorm.DB) error {
	appConfig := config.GetConfig()
	blockchain, ok := appConfig.Blockchains[tx.Network]
	if !ok {
		return fmt.Errorf("network %s not found", tx.Network)
	}
	var retryTransactionID sql.NullInt64
	if tx.RetryTransactionID.Valid {
		retryTransactionID = tx.RetryTransactionID
	} else {
		retryTransactionID = sql.NullInt64{Int64: int64(tx.ID), Valid: true}
	}
	nextTransaction := &BlockchainTransaction{
		Network:            tx.Network,
		Type:               tx.Type,
		Status:             TransactionStatusPending,
		FromAddress:        tx.FromAddress,
		ToAddress:          tx.ToAddress,
		Value:              tx.Value,
		Data:               tx.Data,
		RetryCount:         tx.RetryCount + 1,
		MaxRetries:         tx.MaxRetries,
		NextRetryAt:        sql.NullTime{Time: time.Now().Add(time.Duration(blockchain.RetryInterval) * time.Second)},
		RetryTransactionID: retryTransactionID,
	}
	if err := nextTransaction.Save(ctx, db); err != nil {
		return err
	}
	return nil
}

func GetRetryTransactionsByID(ctx context.Context, db *gorm.DB, id uint) ([]BlockchainTransaction, error) {
	var transactions []BlockchainTransaction
	if err := db.WithContext(ctx).Model(&BlockchainTransaction{}).Where("retry_transaction_id = ?", id).Order("id").Find(&transactions).Error; err != nil {
		return nil, err
	}
	return transactions, nil
}

func ListTransactionChain(ctx context.Context, db *gorm.DB, rootID uint) ([]BlockchainTransaction, error) {
	root, err := GetTransactionByID(ctx, db, rootID)
	if err != nil {
		return nil, err
	}
	transactions := []BlockchainTransaction{*root}
	retries, err := GetRetryTransactionsByID(ctx, db, rootID)
	if err != nil {
		return nil, err
	}
	transactions = append(transactions, retries...)
	return transactions, nil
}

func (tx *BlockchainTransaction) IsReceiptTimeoutFailure() bool {
	return tx.Status == TransactionStatusFailed &&
		tx.StatusMessage.Valid &&
		tx.StatusMessage.String == TransactionTimedOutMessage
}

func (tx *BlockchainTransaction) HasCommittedBroadcast() bool {
	return tx.TxHash.Valid || tx.SignedRawTx.Valid ||
		tx.Status == TransactionStatusBroadcasting ||
		tx.Status == TransactionStatusSent
}

func (tx *BlockchainTransaction) IsProvenUnbroadcastedCancellation() bool {
	return tx.Status == TransactionStatusCancelled && !tx.HasCommittedBroadcast()
}

func (tx *BlockchainTransaction) IsProvenOnChainFailure() bool {
	return tx.Status == TransactionStatusFailed &&
		tx.TxHash.Valid &&
		tx.BlockNumber.Valid &&
		tx.BlockNumber.Int64 > 0 &&
		!tx.IsReceiptTimeoutFailure()
}

func (tx *BlockchainTransaction) IsProvenTerminalFailure() bool {
	return tx.IsProvenUnbroadcastedCancellation() || tx.IsProvenOnChainFailure()
}

func (tx *BlockchainTransaction) IsBlockingUncertainty() bool {
	if tx.IsProvenTerminalFailure() || tx.Status == TransactionStatusConfirmed {
		return false
	}
	if tx.Status == TransactionStatusPending || tx.Status == TransactionStatusSending {
		return false
	}
	return true
}

type TransactionChainOutcome struct {
	ConfirmedCount int
	Blocking       *BlockchainTransaction
	AllProvenFail  bool
}

func ClassifyTransactionChain(transactions []BlockchainTransaction) TransactionChainOutcome {
	outcome := TransactionChainOutcome{AllProvenFail: true}
	for i := range transactions {
		transaction := &transactions[i]
		if transaction.Status == TransactionStatusConfirmed {
			outcome.ConfirmedCount++
			outcome.AllProvenFail = false
			continue
		}
		if transaction.IsProvenTerminalFailure() {
			continue
		}
		outcome.AllProvenFail = false
		if transaction.IsBlockingUncertainty() ||
			transaction.Status == TransactionStatusPending ||
			transaction.Status == TransactionStatusSending {
			if outcome.Blocking == nil {
				outcome.Blocking = transaction
			}
		}
	}
	return outcome
}

func RootTransactionID(tx *BlockchainTransaction) uint {
	if tx.RetryTransactionID.Valid {
		return uint(tx.RetryTransactionID.Int64)
	}
	return tx.ID
}
