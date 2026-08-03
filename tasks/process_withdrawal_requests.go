package tasks

import (
	"context"
	"crynux_relay_wallet/alert"
	"crynux_relay_wallet/blockchain"
	"crynux_relay_wallet/config"
	"crynux_relay_wallet/models"
	"crynux_relay_wallet/relay_api"
	"crynux_relay_wallet/utils"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type WithdrawalRequestError struct {
	Message string
}

func (e *WithdrawalRequestError) Error() string {
	return e.Message
}

func NewWithdrawalRequestError(message string) *WithdrawalRequestError {
	return &WithdrawalRequestError{Message: message}
}

func IsWithdrawalRequestError(err error) bool {
	var withdrawalRequestError *WithdrawalRequestError
	return errors.As(err, &withdrawalRequestError)
}

var ErrWithdrawalRequestStatusInvalid = NewWithdrawalRequestError("invalid withdrawal request status")
var ErrWithdrawalRequestAmountInvalid = NewWithdrawalRequestError("invalid withdrawal request amount")
var ErrWithdrawalRequestAddressNotExists = NewWithdrawalRequestError("withdrawal request address not exists")
var ErrWithdrawalRequestAmountTooLarge = NewWithdrawalRequestError("withdrawal request amount is too large")
var ErrWithdrawalRequestTaskFeeNotEnough = NewWithdrawalRequestError("withdrawal request task fee not enough")
var ErrWithdrawalRequestBeneficialAddressInvalid = NewWithdrawalRequestError("withdrawal request beneficial address is invalid")
var ErrWithdrawalRequestAmountTooSmall = NewWithdrawalRequestError("withdrawal request amount is too small")
var ErrWithdrawalRequestTransactionUnconfirmedTimeout = NewWithdrawalRequestError("withdrawal request transaction remains unconfirmed after timeout")
var ErrWithdrawalRequestDailyLimitExceeded = NewWithdrawalRequestError("withdrawal request count exceeds daily limit per address")
var ErrWithdrawalAuthorizationMissing = errors.New("withdrawal authorization is missing")
var ErrWithdrawalAuthorizationInvalid = errors.New("withdrawal authorization is invalid")
var ErrWithdrawalAuthorizationAddressMismatch = errors.New("withdrawal authorization address mismatch")
var ErrWithdrawalAuthorizationReplayed = errors.New("withdrawal authorization has already been used")

func parseWithdrawalAmount(amountText string) (*big.Int, error) {
	amount, ok := big.NewInt(0).SetString(amountText, 10)
	if !ok || amount.Sign() < 0 {
		return nil, ErrWithdrawalRequestAmountInvalid
	}
	return amount, nil
}

func withdrawalTotalAmount(amount, withdrawalFee *big.Int) *big.Int {
	return big.NewInt(0).Add(amount, withdrawalFee)
}

func buildWithdrawalAuthorizationMessage(address, amount, benefitAddress, network string, timestamp int64) string {
	action := fmt.Sprintf("Withdraw %s from %s to %s on %s", amount, address, benefitAddress, network)
	return fmt.Sprintf("Crynux Relay\nAction: %s\nAddress: %s\nTimestamp: %d", action, address, timestamp)
}

func validateWithdrawalAuthorization(
	address, amount, benefitAddress, network string,
	timestamp sql.NullInt64,
	signature sql.NullString,
) (string, error) {
	if !timestamp.Valid || !signature.Valid || strings.TrimSpace(signature.String) == "" {
		return "", ErrWithdrawalAuthorizationMissing
	}

	sigBytes, err := hexutil.Decode("0x" + strings.TrimPrefix(signature.String, "0x"))
	if err != nil || len(sigBytes) != 65 {
		return "", ErrWithdrawalAuthorizationInvalid
	}
	if sigBytes[64] == 27 || sigBytes[64] == 28 {
		sigBytes[64] -= 27
	}

	message := buildWithdrawalAuthorizationMessage(address, amount, benefitAddress, network, timestamp.Int64)
	prefix := fmt.Sprintf("\x19Ethereum Signed Message:\n%d", len(message))
	publicKey, err := crypto.SigToPub(crypto.Keccak256([]byte(prefix+message)), sigBytes)
	if err != nil {
		return "", ErrWithdrawalAuthorizationInvalid
	}
	if !strings.EqualFold(crypto.PubkeyToAddress(*publicKey).Hex(), address) {
		return "", ErrWithdrawalAuthorizationAddressMismatch
	}

	fingerprint := sha256.Sum256([]byte(message))
	return hex.EncodeToString(fingerprint[:]), nil
}

func withdrawalRequestAuthorization(request relay_api.WithdrawalRequest, amount *big.Int) (string, error) {
	return validateWithdrawalAuthorization(
		request.Address,
		amount.String(),
		request.BenefitAddress,
		request.Network,
		sql.NullInt64{Int64: request.Timestamp, Valid: request.Timestamp != 0},
		sql.NullString{String: request.Signature, Valid: request.Signature != ""},
	)
}

func withdrawalRecordAuthorization(record *models.WithdrawRecord) (string, error) {
	return validateWithdrawalAuthorization(
		record.Address,
		record.Amount.String(),
		record.BenefitAddress,
		record.Network,
		record.Timestamp,
		record.Signature,
	)
}

func ensureWithdrawalAuthorizationFingerprint(ctx context.Context, db *gorm.DB, record *models.WithdrawRecord) error {
	fingerprint, err := withdrawalRecordAuthorization(record)
	if err != nil {
		return err
	}
	if record.AuthorizationFingerprint.Valid {
		if record.AuthorizationFingerprint.String != fingerprint {
			return ErrWithdrawalAuthorizationInvalid
		}
		return nil
	}

	var existingCount int64
	if err := db.WithContext(ctx).Model(&models.WithdrawRecord{}).
		Where("authorization_fingerprint = ? AND id <> ?", fingerprint, record.ID).
		Count(&existingCount).Error; err != nil {
		return err
	}
	if existingCount > 0 {
		return ErrWithdrawalAuthorizationReplayed
	}
	record.AuthorizationFingerprint = sql.NullString{String: fingerprint, Valid: true}
	return db.WithContext(ctx).Model(record).
		Update("authorization_fingerprint", record.AuthorizationFingerprint).Error
}

func withdrawalChainHasCommittedBroadcast(ctx context.Context, db *gorm.DB, rootID uint) (bool, error) {
	transactions, err := models.ListTransactionChain(ctx, db, rootID)
	if err != nil {
		return false, err
	}
	for i := range transactions {
		if transactions[i].HasCommittedBroadcast() {
			return true, nil
		}
	}
	return false, nil
}

func withdrawalRequestLogFields(request relay_api.WithdrawalRequest) log.Fields {
	fields := log.Fields{
		"remote_id":              request.ID,
		"relay_account_event_id": request.RelayAccountEventID,
		"address":                request.Address,
		"benefit_address":        request.BenefitAddress,
		"network":                request.Network,
		"amount":                 request.Amount,
		"withdrawal_fee":         request.WithdrawalFee,
		"status":                 request.Status,
		"created_at":             request.CreatedAt,
	}
	amount, amountErr := parseWithdrawalAmount(request.Amount)
	withdrawalFee, feeErr := parseWithdrawalAmount(request.WithdrawalFee)
	if amountErr == nil && feeErr == nil {
		fields["total_debit"] = withdrawalTotalAmount(amount, withdrawalFee).String()
	}
	addBlockchainLogFields(fields, request.Network)
	return fields
}

func withdrawalRecordLogFields(record *models.WithdrawRecord) log.Fields {
	toAddress := record.BenefitAddress
	if toAddress == "" {
		toAddress = record.Address
	}
	fields := log.Fields{
		"record_id":      record.ID,
		"remote_id":      record.RemoteID,
		"address":        record.Address,
		"to_address":     toAddress,
		"network":        record.Network,
		"amount":         record.Amount.String(),
		"withdrawal_fee": record.WithdrawalFee.String(),
		"total_debit":    withdrawalTotalAmount(&record.Amount.Int, &record.WithdrawalFee.Int).String(),
	}
	addBlockchainLogFields(fields, record.Network)
	return fields
}

func logWithdrawalRequestsReceived(requests []relay_api.WithdrawalRequest) {
	for _, request := range requests {
		log.WithFields(withdrawalRequestLogFields(request)).Info("Withdrawal request received")
	}
}

func logWithdrawalValidationResults(requests []relay_api.WithdrawalRequest, validationErr error) {
	for _, request := range requests {
		fields := withdrawalRequestLogFields(request)
		if validationErr != nil {
			fields["error"] = validationErr.Error()
			log.WithFields(fields).Info("Withdrawal validation failed")
			continue
		}
		log.WithFields(fields).Info("Withdrawal validation succeeded")
	}
}

func logWithdrawalRequestFailures(requests []relay_api.WithdrawalRequest, processErr error) {
	for _, request := range requests {
		fields := withdrawalRequestLogFields(request)
		fields["error"] = processErr.Error()
		log.WithFields(fields).Info("Withdrawal failed")
	}
}

func logWithdrawalFulfilled(record *models.WithdrawRecord, blockchainTransaction *models.BlockchainTransaction) {
	fields := withdrawalRecordLogFields(record)
	if blockchainTransaction != nil {
		fields["blockchain_transaction_id"] = blockchainTransaction.ID
		fields["tx_hash"] = blockchainTransaction.TxHash.String
	}
	log.WithFields(fields).Info("Withdrawal fulfilled")
}

func logWithdrawalRecordFailed(record *models.WithdrawRecord, processErr error, blockchainTransaction *models.BlockchainTransaction) {
	fields := withdrawalRecordLogFields(record)
	fields["error"] = processErr.Error()
	if blockchainTransaction != nil {
		fields["blockchain_transaction_id"] = blockchainTransaction.ID
		fields["tx_hash"] = blockchainTransaction.TxHash.String
		fields["transaction_status"] = blockchainTransaction.Status
	}
	log.WithFields(fields).Info("Withdrawal failed")
}

func StartSyncWithdrawalRequests(ctx context.Context) error {
	intervalSeconds := config.GetConfig().Tasks.SyncWithdrawalRequests.IntervalSeconds
	interval := time.Duration(intervalSeconds) * time.Second

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Infoln("Sync withdrawal requests task is stopping")
			return nil
		case <-ticker.C:
			if err := syncWithdrawalRequests(ctx, intervalSeconds); err != nil {
				log.Errorf("Failed to sync withdrawal requests: %v", err)
				if IsWithdrawalRequestError(err) {
					return err
				}
			}
		}
	}
}

func StartProcessWithdrawalRequests(ctx context.Context) error {
	intervalSeconds := config.GetConfig().Tasks.ProcessWithdrawalRequests.IntervalSeconds
	interval := time.Duration(intervalSeconds) * time.Second

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Infoln("Process withdrawal requests task is stopping")
			return nil
		case <-ticker.C:
			if err := processWithdrawalRecords(ctx); err != nil {
				log.Errorf("Failed to process withdrawal requests: %v", err)
				if IsWithdrawalRequestError(err) {
					return err
				}
			}
		}
	}
}

func checkWithdrawalRequests(ctx context.Context, db *gorm.DB, requests []relay_api.WithdrawalRequest) error {
	appConfig := config.GetConfig()
	minWithdrawalAmount := utils.EtherToWei(big.NewInt(0).SetUint64(appConfig.Tasks.SyncWithdrawalRequests.MinWithdrawalAmount))
	for _, request := range requests {
		amount, err := parseWithdrawalAmount(request.Amount)
		if err != nil {
			return err
		}
		withdrawalFee, err := parseWithdrawalAmount(request.WithdrawalFee)
		if err != nil {
			return err
		}
		if amount.Cmp(minWithdrawalAmount) < 0 {
			return ErrWithdrawalRequestAmountTooSmall
		}
		if withdrawalTotalAmount(amount, withdrawalFee).Sign() <= 0 {
			return ErrWithdrawalRequestAmountInvalid
		}
	}

	amountMap := make(map[string]*big.Int)
	for _, request := range requests {
		if request.Status != relay_api.WithdrawStatusPending {
			return ErrWithdrawalRequestStatusInvalid
		}
		amount, err := parseWithdrawalAmount(request.Amount)
		if err != nil {
			return err
		}
		withdrawalFee, err := parseWithdrawalAmount(request.WithdrawalFee)
		if err != nil {
			return err
		}
		totalAmount := withdrawalTotalAmount(amount, withdrawalFee)
		if _, ok := amountMap[request.Address]; ok {
			amountMap[request.Address].Add(amountMap[request.Address], totalAmount)
		} else {
			amountMap[request.Address] = big.NewInt(0).Set(totalAmount)
		}
	}

	addresses := make([]string, 0, len(amountMap))
	for address := range amountMap {
		addresses = append(addresses, address)
	}

	var accounts []*models.RelayAccount
	if err := db.Model(&models.RelayAccount{}).Where("address IN (?)", addresses).Find(&accounts).Error; err != nil {
		return err
	}

	if len(accounts) != len(addresses) {
		return ErrWithdrawalRequestAddressNotExists
	}

	for _, account := range accounts {
		amount := amountMap[account.Address]
		if amount.Cmp(&account.Balance.Int) > 0 {
			return ErrWithdrawalRequestAmountTooLarge
		}
	}

	if err := checkWithdrawalDailyLimit(db, requests); err != nil {
		return err
	}

	for _, request := range requests {
		ba, err := blockchain.GetBenefitAddress(ctx, common.HexToAddress(request.Address), request.Network)
		if err != nil {
			return err
		}
		if !strings.EqualFold(ba.Hex(), request.BenefitAddress) {
			return ErrWithdrawalRequestBeneficialAddressInvalid
		}
	}

	return nil
}

// checkWithdrawalDailyLimit verifies that accepting the batch would not push any
// address over the configured per-address daily withdrawal count. The count is
// based on the wallet's own record creation time (UTC day) and excludes
// withdrawals that ended up failed or rejected. Requests already stored locally
// are excluded from the stored count so a re-synced batch is not counted twice.
func checkWithdrawalDailyLimit(db *gorm.DB, requests []relay_api.WithdrawalRequest) error {
	limit := config.GetConfig().Tasks.SyncWithdrawalRequests.MaxWithdrawalsPerAddressPerDay

	batchCounts := make(map[string]uint64)
	remoteIDs := make([]uint, 0, len(requests))
	for _, request := range requests {
		batchCounts[request.Address]++
		remoteIDs = append(remoteIDs, request.ID)
	}

	addresses := make([]string, 0, len(batchCounts))
	for address := range batchCounts {
		addresses = append(addresses, address)
	}

	now := time.Now().UTC()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	var storedCounts []struct {
		Address string
		Count   uint64
	}
	if err := db.Model(&models.WithdrawRecord{}).
		Select("address, COUNT(*) AS count").
		Where("address IN (?)", addresses).
		Where("created_at >= ?", dayStart).
		Where("status NOT IN (?)", []models.WithdrawStatus{models.WithdrawStatusFailed, models.WithdrawStatusFinishedRejected}).
		Where("remote_id NOT IN (?)", remoteIDs).
		Group("address").
		Find(&storedCounts).Error; err != nil {
		return err
	}

	totalCounts := make(map[string]uint64, len(batchCounts))
	for address, count := range batchCounts {
		totalCounts[address] = count
	}
	for _, storedCount := range storedCounts {
		totalCounts[storedCount.Address] += storedCount.Count
	}

	for address, count := range totalCounts {
		if count > limit {
			log.WithFields(log.Fields{
				"address":     address,
				"total_count": count,
				"limit":       limit,
			}).Error("Withdrawal daily limit exceeded")
			return ErrWithdrawalRequestDailyLimitExceeded
		}
	}
	return nil
}

func syncWithdrawalRequests(ctx context.Context, intervalSeconds uint) error {
	db := config.GetDB()

	var checkpoint models.WithdrawalRequestCheckpoint
	err := db.WithContext(ctx).First(&checkpoint).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}

	for {
		var taskFeeCheckpoint models.TaskFeeCheckpoint
		err = db.WithContext(ctx).First(&taskFeeCheckpoint).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		requests, err := relay_api.GetWithdrawalRequests(ctx, checkpoint.LatestWithdrawalRequestID, int(config.GetConfig().Tasks.SyncWithdrawalRequests.BatchSize))
		if err != nil {
			return err
		}

		end := 0
		for _, request := range requests {
			if request.RelayAccountEventID > taskFeeCheckpoint.LatestTaskFeeLogID {
				break
			}
			end++
		}
		requests = requests[:end]

		if len(requests) == 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(intervalSeconds) * time.Second):
				continue
			}
		}

		logWithdrawalRequestsReceived(requests)

		var records []*models.WithdrawRecord
		validRequests := make([]relay_api.WithdrawalRequest, 0, len(requests))
		seenFingerprints := make(map[string]struct{})
		for _, request := range requests {
			amount, err := parseWithdrawalAmount(request.Amount)
			if err != nil {
				return err
			}
			withdrawalFee, err := parseWithdrawalAmount(request.WithdrawalFee)
			if err != nil {
				return err
			}
			record := &models.WithdrawRecord{
				RemoteID:       request.ID,
				Address:        request.Address,
				BenefitAddress: request.BenefitAddress,
				Amount:         models.BigInt{Int: *amount},
				WithdrawalFee:  models.BigInt{Int: *withdrawalFee},
				Network:        request.Network,
				Status:         models.WithdrawStatusPending,
				Timestamp:      sql.NullInt64{Int64: request.Timestamp, Valid: request.Timestamp != 0},
				Signature:      sql.NullString{String: request.Signature, Valid: request.Signature != ""},
			}

			fingerprint, authorizationErr := withdrawalRequestAuthorization(request, amount)
			if authorizationErr == nil {
				var existingCount int64
				if _, exists := seenFingerprints[fingerprint]; exists {
					authorizationErr = ErrWithdrawalAuthorizationReplayed
				} else if err := db.WithContext(ctx).Model(&models.WithdrawRecord{}).
					Where("authorization_fingerprint = ? AND remote_id <> ?", fingerprint, request.ID).
					Count(&existingCount).Error; err != nil {
					return err
				} else if existingCount > 0 {
					authorizationErr = ErrWithdrawalAuthorizationReplayed
				}
			}
			if authorizationErr != nil {
				record.Status = models.WithdrawStatusFailed
				log.WithFields(withdrawalRequestLogFields(request)).
					WithError(authorizationErr).
					Info("Withdrawal authorization validation failed")
			} else {
				record.AuthorizationFingerprint = sql.NullString{String: fingerprint, Valid: true}
				seenFingerprints[fingerprint] = struct{}{}
				validRequests = append(validRequests, request)
			}
			records = append(records, record)
		}

		if len(validRequests) > 0 {
			err = checkWithdrawalRequests(ctx, db, validRequests)
			logWithdrawalValidationResults(validRequests, err)
			if err != nil {
				logWithdrawalRequestFailures(validRequests, err)
				return err
			}
		}

		if err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {

			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&records).Error; err != nil {
				return err
			}

			checkpoint.LatestWithdrawalRequestID = requests[len(requests)-1].ID
			checkpoint.LatestWithdrawalRequestTimestamp = requests[len(requests)-1].CreatedAt
			return tx.Save(&checkpoint).Error
		}); err != nil {
			logWithdrawalRequestFailures(requests, err)
			return err
		}
	}
}

func getUnfinishedWithdrawalRecords(ctx context.Context, db *gorm.DB, startID uint, limit int) ([]*models.WithdrawRecord, error) {
	var records []*models.WithdrawRecord
	err := db.WithContext(ctx).Where("status NOT IN (?)", []models.WithdrawStatus{models.WithdrawStatusFinished, models.WithdrawStatusFinishedRejected}).Where("id > ?", startID).Order("id ASC").Limit(limit).Find(&records).Error
	if err != nil {
		return nil, err
	}
	return records, nil
}

func processWithdrawalRecord(ctx context.Context, db *gorm.DB, record *models.WithdrawRecord) (err error) {
	var blockchainTransaction *models.BlockchainTransaction
	for record.Status == models.WithdrawStatusPending {
		if record.BlockchainTransactionID.Valid {
			rootID := uint(record.BlockchainTransactionID.Int64)
			committed, err := withdrawalChainHasCommittedBroadcast(ctx, db, rootID)
			if err != nil {
				return err
			}
			if !committed {
				if authorizationErr := ensureWithdrawalAuthorizationFingerprint(ctx, db, record); authorizationErr != nil {
					blockchainTransaction, err = getCurrentBlockchainTransaction(ctx, db, rootID)
					if err != nil {
						return err
					}
					if blockchainTransaction.Status == models.TransactionStatusPending ||
						blockchainTransaction.Status == models.TransactionStatusSending {
						if _, err := blockchainTransaction.RequestCancellation(ctx, db, authorizationErr.Error()); err != nil {
							return err
						}
					}
					if blockchainTransaction.Status == models.TransactionStatusPending {
						cancelled, err := blockchainTransaction.CancelRequestedUnbroadcasted(ctx, db)
						if err != nil {
							return err
						}
						if !cancelled {
							continue
						}
					}
					if blockchainTransaction.Status != models.TransactionStatusSending {
						if err := record.UpdateStatus(ctx, db, models.WithdrawStatusFailed); err != nil {
							return err
						}
						logWithdrawalRecordFailed(record, authorizationErr, blockchainTransaction)
						break
					}
				}
			}
		}

		if !record.BlockchainTransactionID.Valid {
			if authorizationErr := ensureWithdrawalAuthorizationFingerprint(ctx, db, record); authorizationErr != nil {
				if err := record.UpdateStatus(ctx, db, models.WithdrawStatusFailed); err != nil {
					return err
				}
				logWithdrawalRecordFailed(record, authorizationErr, nil)
				break
			}

			totalAmount := withdrawalTotalAmount(&record.Amount.Int, &record.WithdrawalFee.Int)
			var account models.RelayAccount
			if err := db.WithContext(ctx).Where("address = ?", record.Address).First(&account).Error; err != nil {
				return err
			}
			if account.Balance.Cmp(totalAmount) < 0 {
				if err := record.UpdateStatus(ctx, db, models.WithdrawStatusFailed); err != nil {
					return err
				}
				logWithdrawalRecordFailed(record, ErrWithdrawalRequestTaskFeeNotEnough, nil)
				break
			}

			var toAddress common.Address
			if record.BenefitAddress != "" {
				toAddress = common.HexToAddress(record.BenefitAddress)
			} else {
				toAddress = common.HexToAddress(record.Address)
			}
			blockchainConfig := config.GetConfig().Blockchains[record.Network]
			switch blockchainConfig.TokenType {
			case config.TokenTypeNative:
				blockchainTransaction, err = blockchain.NewSendETHTransaction(toAddress, big.NewInt(0).Set(&record.Amount.Int), record.Network)
			case config.TokenTypeERC20:
				blockchainTransaction, err = blockchain.NewSendERC20Transaction(common.HexToAddress(blockchainConfig.TokenAddress), toAddress, big.NewInt(0).Set(&record.Amount.Int), record.Network)
			default:
				err = blockchain.ErrBlockchainNotFound
			}
			if err != nil {
				return err
			}

			err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) (err error) {
				if err = blockchainTransaction.Save(ctx, tx); err != nil {
					return err
				}
				record.BlockchainTransactionID = sql.NullInt64{Int64: int64(blockchainTransaction.ID), Valid: true}
				return tx.Save(record).Error
			})
			if err != nil {
				return err
			}
		} else {
			blockchainTransaction, err = getCurrentBlockchainTransaction(ctx, db, uint(record.BlockchainTransactionID.Int64))
			if err != nil {
				return err
			}
		}

		for blockchainTransaction.Status != models.TransactionStatusConfirmed &&
			blockchainTransaction.Status != models.TransactionStatusFailed &&
			blockchainTransaction.Status != models.TransactionStatusCancelled {
			err = blockchainTransaction.Sync(ctx, db)
			if err != nil {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(1 * time.Second):
				continue
			}
		}

		if blockchainTransaction.Status == models.TransactionStatusConfirmed {
			if err := ensureWithdrawalFulfillSafe(ctx, db, record, blockchainTransaction); err != nil {
				return err
			}
			totalAmount := withdrawalTotalAmount(&record.Amount.Int, &record.WithdrawalFee.Int)
			remainingBalance := ""
			err = db.WithContext(ctx).Transaction(func(tx *gorm.DB) (err error) {
				var account models.RelayAccount
				err = tx.Model(&models.RelayAccount{}).Where("address = ?", record.Address).First(&account).Error
				if err != nil {
					return err
				}
				if account.Balance.Cmp(totalAmount) < 0 {
					return ErrWithdrawalRequestTaskFeeNotEnough
				}
				account.Balance.Sub(&account.Balance.Int, totalAmount)
				remainingBalance = account.Balance.String()
				err = tx.Save(&account).Error
				if err != nil {
					return err
				}
				err = record.UpdateStatus(ctx, tx, models.WithdrawStatusSuccess)
				if err != nil {
					return err
				}
				return nil
			})
			if err != nil {
				return err
			}
			toAddress := record.BenefitAddress
			if toAddress == "" {
				toAddress = record.Address
			}
			log.Infof(
				"Withdrawal debited: record_id=%d remote_id=%d network=%s address=%s to_address=%s amount=%s withdrawal_fee=%s total_debit=%s remaining_balance=%s blockchain_transaction_id=%d tx_hash=%s",
				record.ID,
				record.RemoteID,
				record.Network,
				record.Address,
				toAddress,
				record.Amount.String(),
				record.WithdrawalFee.String(),
				totalAmount.String(),
				remainingBalance,
				blockchainTransaction.ID,
				blockchainTransaction.TxHash.String,
			)
		} else if blockchainTransaction.Status == models.TransactionStatusCancelled {
			err = record.UpdateStatus(ctx, db, models.WithdrawStatusFailed)
			if err != nil {
				return err
			}
		} else if blockchainTransaction.RetryCount >= blockchainTransaction.MaxRetries {
			err = record.UpdateStatus(ctx, db, models.WithdrawStatusFailed)
			if err != nil {
				return err
			}
		}
	}

	if record.Status == models.WithdrawStatusSuccess {
		if blockchainTransaction == nil {
			if !record.BlockchainTransactionID.Valid {
				return fmt.Errorf("successful withdrawal record has no blockchain transaction")
			}
			blockchainTransaction, err = getCurrentBlockchainTransaction(ctx, db, uint(record.BlockchainTransactionID.Int64))
			if err != nil {
				return err
			}
		}
		if err := ensureWithdrawalFulfillSafe(ctx, db, record, blockchainTransaction); err != nil {
			return err
		}
		err = relay_api.FulfillWithdrawalRequest(ctx, record.RemoteID, blockchainTransaction.TxHash.String)
		if err != nil {
			return err
		}
		if err = record.UpdateStatus(ctx, db, models.WithdrawStatusFinished); err != nil {
			return err
		}
		logWithdrawalFulfilled(record, blockchainTransaction)
	} else {
		if err := rejectWithdrawalRequestSafely(ctx, db, record, blockchainTransaction); err != nil {
			return err
		}
		processErr := errors.New("withdrawal request rejected before blockchain broadcast")
		if blockchainTransaction != nil {
			processErr = fmt.Errorf("withdrawal request blockchain transaction ended with status %d", blockchainTransaction.Status)
		}
		logWithdrawalRecordFailed(record, processErr, blockchainTransaction)
	}
	return nil
}

func getCurrentBlockchainTransaction(ctx context.Context, db *gorm.DB, id uint) (*models.BlockchainTransaction, error) {
	blockchainTransaction, err := models.GetTransactionByID(ctx, db, id)
	if err != nil {
		return nil, err
	}
	if blockchainTransaction.Status != models.TransactionStatusFailed {
		return blockchainTransaction, nil
	}
	retryTransactions, err := models.GetRetryTransactionsByID(ctx, db, id)
	if err != nil {
		return nil, err
	}
	if len(retryTransactions) > 0 {
		blockchainTransaction = &retryTransactions[len(retryTransactions)-1]
	}
	return blockchainTransaction, nil
}

func ensureWithdrawalRejectSafe(
	ctx context.Context,
	db *gorm.DB,
	record *models.WithdrawRecord,
) error {
	if record == nil || !record.BlockchainTransactionID.Valid {
		return nil
	}

	transactions, err := models.ListTransactionChain(ctx, db, uint(record.BlockchainTransactionID.Int64))
	if err != nil {
		return err
	}
	outcome := models.ClassifyTransactionChain(transactions)
	if outcome.ConfirmedCount > 0 {
		blocker := outcome.Blocking
		for i := range transactions {
			if transactions[i].Status == models.TransactionStatusConfirmed {
				return stopForBlockingTimeout(record, &transactions[i])
			}
		}
		if blocker != nil {
			return stopForBlockingTimeout(record, blocker)
		}
		return stopForBlockingTimeout(record, &transactions[0])
	}
	if !outcome.AllProvenFail {
		if outcome.Blocking != nil {
			return stopForBlockingTimeout(record, outcome.Blocking)
		}
		return stopForBlockingTimeout(record, &transactions[0])
	}
	return nil
}

func ensureWithdrawalFulfillSafe(
	ctx context.Context,
	db *gorm.DB,
	record *models.WithdrawRecord,
	current *models.BlockchainTransaction,
) error {
	if record == nil || !record.BlockchainTransactionID.Valid {
		return fmt.Errorf("withdrawal fulfill safety check requires an attached blockchain transaction")
	}
	if current == nil || current.Status != models.TransactionStatusConfirmed || !current.TxHash.Valid {
		if current == nil {
			return ErrWithdrawalRequestTransactionUnconfirmedTimeout
		}
		return stopForBlockingTimeout(record, current)
	}

	transactions, err := models.ListTransactionChain(ctx, db, uint(record.BlockchainTransactionID.Int64))
	if err != nil {
		return err
	}
	outcome := models.ClassifyTransactionChain(transactions)
	if outcome.ConfirmedCount != 1 {
		if outcome.Blocking != nil {
			return stopForBlockingTimeout(record, outcome.Blocking)
		}
		return stopForBlockingTimeout(record, current)
	}
	for i := range transactions {
		transaction := &transactions[i]
		if transaction.Status == models.TransactionStatusConfirmed {
			continue
		}
		if !transaction.IsProvenTerminalFailure() {
			return stopForBlockingTimeout(record, transaction)
		}
	}
	return nil
}

func rejectWithdrawalRequestSafely(
	ctx context.Context,
	db *gorm.DB,
	record *models.WithdrawRecord,
	blockchainTransaction *models.BlockchainTransaction,
) error {
	if err := ensureWithdrawalRejectSafe(ctx, db, record); err != nil {
		return err
	}

	rejectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	fields := log.Fields{
		"record_id": record.ID,
		"remote_id": record.RemoteID,
	}
	if blockchainTransaction != nil {
		fields["blockchain_transaction_id"] = blockchainTransaction.ID
		fields["transaction_status"] = blockchainTransaction.Status
		fields["tx_hash"] = blockchainTransaction.TxHash.String
	}
	log.WithFields(fields).Info("ProcessWithdrawalRecords: rejecting withdrawal record")
	if err := relay_api.RejectWithdrawalRequest(rejectCtx, record.RemoteID); err != nil {
		log.Errorf("ProcessWithdrawalRecords: reject withdrawal record %d error %v", record.ID, err)
		return err
	}
	if err := record.UpdateStatus(rejectCtx, db, models.WithdrawStatusFinishedRejected); err != nil {
		log.Errorf("ProcessWithdrawalRecords: update rejected withdrawal record %d status error %v", record.ID, err)
		return err
	}
	return nil
}

func handleTimeoutWithdrawalRequest(ctx context.Context, db *gorm.DB, record *models.WithdrawRecord) (bool, error) {
	if err := db.WithContext(ctx).First(record, record.ID).Error; err != nil {
		return false, err
	}
	if record.BlockchainTransactionID.Valid {
		blockchainTransaction, err := getCurrentBlockchainTransaction(ctx, db, uint(record.BlockchainTransactionID.Int64))
		if err != nil {
			return false, err
		}
		if blockchainTransaction.Status == models.TransactionStatusConfirmed ||
			blockchainTransaction.Status == models.TransactionStatusCancelled ||
			(blockchainTransaction.Status == models.TransactionStatusFailed && blockchainTransaction.RetryCount >= blockchainTransaction.MaxRetries) {
			log.WithFields(log.Fields{
				"record_id":                 record.ID,
				"remote_id":                 record.RemoteID,
				"blockchain_transaction_id": blockchainTransaction.ID,
				"transaction_status":        blockchainTransaction.Status,
				"tx_hash":                   blockchainTransaction.TxHash.String,
			}).Info("ProcessWithdrawalRecords: finalizing timeout withdrawal record with terminal blockchain transaction")
			finalizeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			return true, processWithdrawalRecord(finalizeCtx, db, record)
		}
		if blockchainTransaction.HasCommittedBroadcast() ||
			blockchainTransaction.Status == models.TransactionStatusBroadcasting ||
			(blockchainTransaction.Status != models.TransactionStatusPending &&
				blockchainTransaction.Status != models.TransactionStatusSending) {
			return false, stopForBlockingTimeout(record, blockchainTransaction)
		}

		if _, err := blockchainTransaction.RequestCancellation(ctx, db, "Withdrawal request timed out before broadcast"); err != nil {
			return false, err
		}

		if blockchainTransaction.Status == models.TransactionStatusPending {
			cancelled, err := blockchainTransaction.CancelRequestedUnbroadcasted(ctx, db)
			if err != nil {
				return false, err
			}
			if cancelled {
				if err := rejectWithdrawalRequestSafely(context.Background(), db, record, blockchainTransaction); err != nil {
					return false, fmt.Errorf("ProcessWithdrawalRecords: cannot reject timeout withdrawal request due to %w", err)
				}
				logWithdrawalRecordFailed(record, ErrWithdrawalRequestTransactionUnconfirmedTimeout, blockchainTransaction)
				log.Infof("ProcessWithdrawalRecords: rejected timeout withdrawal record %d after cancelling unbroadcasted blockchain transaction %d", record.ID, blockchainTransaction.ID)
				return true, nil
			}
			return false, nil
		}

		if err := blockchainTransaction.Sync(ctx, db); err != nil {
			return false, err
		}
		if !blockchainTransaction.CancellationRequestedAt.Valid {
			return false, nil
		}
		settlementTimeout := time.Duration(
			config.GetConfig().Tasks.ProcessWithdrawalRequests.CancellationSettlementTimeoutSeconds,
		) * time.Second
		if !time.Now().Before(blockchainTransaction.CancellationRequestedAt.Time.Add(settlementTimeout)) {
			return false, stopForBlockingTimeout(record, blockchainTransaction)
		}
		return false, nil
	}
	if err := rejectWithdrawalRequestSafely(context.Background(), db, record, nil); err != nil {
		return false, fmt.Errorf("ProcessWithdrawalRecords: cannot reject timeout withdrawal request due to %w", err)
	}
	logWithdrawalRecordFailed(record, ErrWithdrawalRequestTransactionUnconfirmedTimeout, nil)
	log.Infof("ProcessWithdrawalRecords: rejected timeout withdrawal record %d before blockchain transaction creation", record.ID)
	return true, nil
}

func stopForBlockingTimeout(record *models.WithdrawRecord, blockchainTransaction *models.BlockchainTransaction) error {
	blockingMessage := fmt.Sprintf(
		"withdrawal processor stopped because timeout transaction is not cancellable or terminal: record_id=%d remote_id=%d blockchain_transaction_id=%d transaction_status=%d tx_hash=%s status_message=%s cancellation_requested_at=%v",
		record.ID,
		record.RemoteID,
		blockchainTransaction.ID,
		blockchainTransaction.Status,
		blockchainTransaction.TxHash.String,
		blockchainTransaction.StatusMessage.String,
		blockchainTransaction.CancellationRequestedAt,
	)
	log.WithFields(log.Fields{
		"record_id":                 record.ID,
		"remote_id":                 record.RemoteID,
		"blockchain_transaction_id": blockchainTransaction.ID,
		"transaction_status":        blockchainTransaction.Status,
		"tx_hash":                   blockchainTransaction.TxHash.String,
		"status_message":            blockchainTransaction.StatusMessage.String,
		"cancellation_requested_at": blockchainTransaction.CancellationRequestedAt,
	}).Error("ProcessWithdrawalRecords: " + blockingMessage)
	alert.SafeSendAlert("ProcessWithdrawalRequests", blockingMessage)
	return ErrWithdrawalRequestTransactionUnconfirmedTimeout
}

func processWithdrawalRecordWithRetry(ctx context.Context, db *gorm.DB, record *models.WithdrawRecord) error {
	deadline := record.CreatedAt.Add(time.Duration(config.GetConfig().Tasks.ProcessWithdrawalRequests.Timeout) * time.Second)

	for {
		if time.Now().After(deadline) {
			completed, err := handleTimeoutWithdrawalRequest(ctx, db, record)
			if err != nil || completed {
				return err
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
				continue
			}
		}

		recordCtx, cancel := context.WithDeadline(ctx, deadline)
		log.Infof("ProcessWithdrawalRecords: process withdrawal record %d", record.ID)
		err := processWithdrawalRecord(recordCtx, db, record)
		cancel()

		if err == nil {
			log.Infof("ProcessWithdrawalRecords: process withdrawal record %d successfully", record.ID)
			return nil
		}
		if errors.Is(err, context.DeadlineExceeded) {
			continue
		}
		log.Errorf("ProcessWithdrawalRecords: process withdrawal record %d error %v", record.ID, err)
		if IsWithdrawalRequestError(err) {
			logWithdrawalRecordFailed(record, err, nil)
			log.WithFields(log.Fields{
				"record_id": record.ID,
				"remote_id": record.RemoteID,
				"error":     err.Error(),
			}).Error("ProcessWithdrawalRecords: processor is stopping; later withdrawal records are blocked")
			return err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func processWithdrawalRecords(ctx context.Context) error {
	appConfig := config.GetConfig()
	db := config.GetDB()

	var startID uint
	limit := appConfig.Tasks.ProcessWithdrawalRequests.BatchSize
	interval := time.Duration(appConfig.Tasks.ProcessWithdrawalRequests.IntervalSeconds) * time.Second

	for {
		records, err := getUnfinishedWithdrawalRecords(ctx, db, startID, int(limit))
		if err != nil {
			return err
		}

		if len(records) == 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(interval):
				continue
			}
		}

		for _, record := range records {
			if err := processWithdrawalRecordWithRetry(ctx, db, record); err != nil {
				return err
			}
			startID = record.ID
		}
	}
}
