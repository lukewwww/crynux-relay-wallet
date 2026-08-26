package tasks

import (
	"context"
	"crynux_relay_wallet/blockchain"
	"crynux_relay_wallet/config"
	"crynux_relay_wallet/models"
	"crynux_relay_wallet/relay_api"
	"crynux_relay_wallet/utils"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type TaskFeeError struct {
	Message string
}

func (e *TaskFeeError) Error() string {
	return e.Message
}

func NewTaskFeeError(message string) *TaskFeeError {
	return &TaskFeeError{Message: message}
}

func IsTaskFeeError(err error) bool {
	var taskFeeError *TaskFeeError
	return errors.As(err, &taskFeeError)
}

var ErrTaskFeeAmountTooLarge = NewTaskFeeError("task fee amount is greater than max task fee amount threshold")
var ErrTaskFeeAmountInvalid = NewTaskFeeError("cannot parse amount from task fee log")
var ErrTaskFeeAddressCountTooLarge = NewTaskFeeError("task fee logs count of a single address is greater than max address count threshold")
var ErrTaskFeeNewAddressCountTooLarge = NewTaskFeeError("task fee logs count of new addresses is greater than max new address count threshold")
var ErrTaskFeeDepositPayloadInvalid = NewTaskFeeError("deposit log payload is invalid")
var ErrTaskFeeDepositTxMismatch = NewTaskFeeError("deposit transaction does not match relay account event log")
var ErrTaskFeeDepositTxHashDuplicate = NewTaskFeeError("deposit transaction hash already exists")
var ErrTaskFeeDepositTxTooOld = NewTaskFeeError("deposit transaction is older than max age threshold")
var ErrTaskFeeUnknownEventType = NewTaskFeeError("unknown relay account event type")
var ErrTaskFeeVestingPayloadInvalid = NewTaskFeeError("vesting log payload is invalid")
var ErrTaskFeeVestingSignerMismatch = NewTaskFeeError("vesting signer does not match configured signer")
var ErrTaskFeeVestingSignatureInvalid = NewTaskFeeError("vesting signature is invalid")
var ErrTaskFeeVestingRecordNotFound = NewTaskFeeError("vesting record not found")
var ErrTaskFeeVestingReleaseInvalid = NewTaskFeeError("vesting release is invalid")
var ErrTaskFeeWithdrawalFeeAddressMismatch = NewTaskFeeError("withdrawal fee income address does not match configured address")

const maxTaskFeeLogFutureDrift = time.Hour

type depositPayload struct {
	TxHash  string `json:"tx_hash"`
	Network string `json:"network"`
}

var erc20TransferTopic = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))

func addressTopic(address string) common.Hash {
	return common.BytesToHash(common.HexToAddress(address).Bytes())
}

type vestingPayload struct {
	VestingID      uint   `json:"vesting_id"`
	Address        string `json:"address"`
	TotalAmount    string `json:"total_amount"`
	ReleasedAmount string `json:"released_amount"`
	StartTime      int64  `json:"start_time"`
	DurationDays   uint   `json:"duration_days"`
	Type           string `json:"type"`
	AdminSignature string `json:"admin_signature"`
}

type vestingReleasePayload struct {
	VestingID uint `json:"vesting_id"`
}

func StartSyncTaskFeeLogs(ctx context.Context) error {

	intervalSeconds := config.GetConfig().Tasks.SyncTaskFeeLogs.IntervalSeconds
	interval := time.Duration(intervalSeconds) * time.Second

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Infoln("Sync task fee logs task is stopping")
			return nil
		case <-ticker.C:
			if err := syncTaskFeeLogs(ctx, intervalSeconds); err != nil {
				log.Errorf("Failed to sync task fee logs: %v", err)
				if IsTaskFeeError(err) {
					return err
				}
			}
		}
	}
}

func isSupportedTaskFeeLogType(logType relay_api.TaskFeeLogType) bool {
	switch logType {
	case relay_api.TaskFeeLogTypeTaskIncome,
		relay_api.TaskFeeLogTypeDaoTaskShare,
		relay_api.TaskFeeLogTypeWithdrawalFeeIncome,
		relay_api.TaskFeeLogTypeDeposit,
		relay_api.TaskFeeLogTypeTaskPayment,
		relay_api.TaskFeeLogTypeTaskRefund,
		relay_api.TaskFeeLogTypeWithdraw,
		relay_api.TaskFeeLogTypeWithdrawRefund,
		relay_api.TaskFeeLogTypeUserDelegation,
		relay_api.TaskFeeLogTypeVestingCreated,
		relay_api.TaskFeeLogTypeVestingRelease:
		return true
	default:
		return false
	}
}

func isBalanceIgnoredTaskFeeLogType(logType relay_api.TaskFeeLogType) bool {
	return logType == relay_api.TaskFeeLogTypeWithdraw ||
		logType == relay_api.TaskFeeLogTypeWithdrawRefund ||
		logType == relay_api.TaskFeeLogTypeVestingCreated
}

func isValidVestingType(vestingType string) bool {
	switch vestingType {
	case models.VestingTypeNode, models.VestingTypeDelegation, models.VestingTypeOther:
		return true
	default:
		return false
	}
}

func mergeTaskFeeLogs(logs []relay_api.TaskFeeLog) (map[string]*big.Int, error) {
	merged := make(map[string]*big.Int)

	for _, eventLog := range logs {
		if !isSupportedTaskFeeLogType(eventLog.Type) {
			return nil, ErrTaskFeeUnknownEventType
		}
		if isBalanceIgnoredTaskFeeLogType(eventLog.Type) {
			continue
		}

		amount, success := new(big.Int).SetString(eventLog.Amount, 10)
		if !success {
			return nil, ErrTaskFeeAmountInvalid
		}

		if _, ok := merged[eventLog.Address]; !ok {
			merged[eventLog.Address] = big.NewInt(0)
		}

		switch eventLog.Type {
		case relay_api.TaskFeeLogTypeTaskPayment:
			merged[eventLog.Address].Sub(merged[eventLog.Address], amount)
		case relay_api.TaskFeeLogTypeTaskIncome,
			relay_api.TaskFeeLogTypeDaoTaskShare,
			relay_api.TaskFeeLogTypeWithdrawalFeeIncome,
			relay_api.TaskFeeLogTypeDeposit,
			relay_api.TaskFeeLogTypeTaskRefund,
			relay_api.TaskFeeLogTypeUserDelegation,
			relay_api.TaskFeeLogTypeVestingRelease:
			merged[eventLog.Address].Add(merged[eventLog.Address], amount)
		default:
			return nil, ErrTaskFeeUnknownEventType
		}
	}

	return merged, nil
}

func parseDepositPayload(eventLog relay_api.TaskFeeLog) (*depositPayload, error) {
	var payload depositPayload
	if strings.TrimSpace(eventLog.Payload) == "" {
		return nil, ErrTaskFeeDepositPayloadInvalid
	}
	if err := json.Unmarshal([]byte(eventLog.Payload), &payload); err != nil {
		return nil, ErrTaskFeeDepositPayloadInvalid
	}
	txHashBytes, err := hexutil.Decode(payload.TxHash)
	if payload.TxHash == "" || payload.Network == "" || err != nil || len(txHashBytes) != common.HashLength {
		return nil, ErrTaskFeeDepositPayloadInvalid
	}
	return &payload, nil
}

func normalizedDepositIdentity(payload *depositPayload) (string, string) {
	return strings.ToLower(payload.Network), strings.ToLower(common.HexToHash(payload.TxHash).Hex())
}

func validateDepositLog(ctx context.Context, eventLog relay_api.TaskFeeLog) error {
	amount, success := new(big.Int).SetString(eventLog.Amount, 10)
	if !success {
		return ErrTaskFeeAmountInvalid
	}

	payload, err := parseDepositPayload(eventLog)
	if err != nil {
		return err
	}

	client, err := blockchain.GetBlockchainClient(payload.Network)
	if err != nil {
		return err
	}

	txHash := common.HexToHash(payload.TxHash)
	receipt, err := client.RpcClient.TransactionReceipt(ctx, txHash)
	if errors.Is(err, ethereum.NotFound) {
		return fmt.Errorf("deposit transaction not found: %s", payload.TxHash)
	}
	if err != nil {
		return err
	}
	if receipt.Status != types.ReceiptStatusSuccessful {
		return ErrTaskFeeDepositTxMismatch
	}

	blockchainConfig, ok := config.GetConfig().Blockchains[payload.Network]
	if !ok {
		return blockchain.ErrBlockchainNotFound
	}
	switch blockchainConfig.TokenType {
	case config.TokenTypeNative:
		return validateNativeDepositLog(ctx, client, receipt, txHash, eventLog, amount)
	case config.TokenTypeERC20:
		return validateERC20DepositLog(ctx, client, receipt, eventLog, amount, blockchainConfig.TokenAddress)
	default:
		return blockchain.ErrBlockchainNotFound
	}
}

func validateNativeDepositLog(ctx context.Context, client *blockchain.BlockchainClient, receipt *types.Receipt, txHash common.Hash, eventLog relay_api.TaskFeeLog, amount *big.Int) error {
	transfer, err := client.GetTransactionTransfer(ctx, txHash)
	if errors.Is(err, ethereum.NotFound) {
		return fmt.Errorf("deposit transaction not found: %s", txHash.Hex())
	}
	if err != nil {
		return err
	}
	if transfer.To == nil || !strings.EqualFold(transfer.To.Hex(), config.GetConfig().Relay.DepositAddress) {
		return ErrTaskFeeDepositTxMismatch
	}
	if !strings.EqualFold(transfer.From.Hex(), eventLog.Address) ||
		transfer.Value == nil ||
		transfer.Value.Cmp(amount) != 0 ||
		len(transfer.Input) != 0 {
		return ErrTaskFeeDepositTxMismatch
	}

	if receipt.BlockNumber == nil {
		return ErrTaskFeeDepositTxMismatch
	}
	header, err := client.RpcClient.HeaderByNumber(ctx, receipt.BlockNumber)
	if err != nil {
		return err
	}
	maxAgeSeconds := config.GetConfig().Tasks.SyncTaskFeeLogs.DepositMaxAgeSeconds
	if header.Time+maxAgeSeconds < uint64(time.Now().Unix()) {
		return ErrTaskFeeDepositTxTooOld
	}

	return nil
}

func validateERC20DepositLog(ctx context.Context, client *blockchain.BlockchainClient, receipt *types.Receipt, eventLog relay_api.TaskFeeLog, amount *big.Int, tokenAddress string) error {
	for _, receiptLog := range receipt.Logs {
		if len(receiptLog.Topics) != 3 ||
			receiptLog.Topics[0] != erc20TransferTopic ||
			!strings.EqualFold(receiptLog.Address.Hex(), tokenAddress) ||
			receiptLog.Topics[2] != addressTopic(config.GetConfig().Relay.DepositAddress) ||
			len(receiptLog.Data) != 32 {
			continue
		}
		fromAddress := common.BytesToAddress(receiptLog.Topics[1].Bytes()).Hex()
		if fromAddress == (common.Address{}).Hex() ||
			!strings.EqualFold(fromAddress, eventLog.Address) {
			continue
		}
		logAmount := new(big.Int).SetBytes(receiptLog.Data)
		if logAmount.Cmp(amount) != 0 {
			continue
		}
		if receipt.BlockNumber == nil {
			return ErrTaskFeeDepositTxMismatch
		}
		header, err := client.RpcClient.HeaderByNumber(ctx, receipt.BlockNumber)
		if err != nil {
			return err
		}
		maxAgeSeconds := config.GetConfig().Tasks.SyncTaskFeeLogs.DepositMaxAgeSeconds
		if header.Time+maxAgeSeconds < uint64(time.Now().Unix()) {
			return ErrTaskFeeDepositTxTooOld
		}
		return nil
	}
	return ErrTaskFeeDepositTxMismatch
}

func parseVestingPayload(eventLog relay_api.TaskFeeLog) (*vestingPayload, error) {
	if strings.TrimSpace(eventLog.Payload) == "" {
		return nil, ErrTaskFeeVestingPayloadInvalid
	}
	var payload vestingPayload
	if err := json.Unmarshal([]byte(eventLog.Payload), &payload); err != nil {
		return nil, ErrTaskFeeVestingPayloadInvalid
	}
	if payload.VestingID == 0 ||
		payload.Address == "" ||
		payload.TotalAmount == "" ||
		payload.ReleasedAmount == "" ||
		payload.StartTime <= 0 ||
		payload.DurationDays == 0 ||
		payload.Type == "" ||
		payload.AdminSignature == "" {
		return nil, ErrTaskFeeVestingPayloadInvalid
	}
	if !common.IsHexAddress(payload.Address) {
		return nil, ErrTaskFeeVestingPayloadInvalid
	}
	if !isValidVestingType(payload.Type) {
		return nil, ErrTaskFeeVestingPayloadInvalid
	}
	return &payload, nil
}

func parseVestingReleasePayload(eventLog relay_api.TaskFeeLog) (*vestingReleasePayload, error) {
	if strings.TrimSpace(eventLog.Payload) == "" {
		return nil, ErrTaskFeeVestingPayloadInvalid
	}
	var payload vestingReleasePayload
	if err := json.Unmarshal([]byte(eventLog.Payload), &payload); err != nil {
		return nil, ErrTaskFeeVestingPayloadInvalid
	}
	if payload.VestingID == 0 {
		return nil, ErrTaskFeeVestingPayloadInvalid
	}
	return &payload, nil
}

func recoverVestingSigner(payload *vestingPayload) (string, error) {
	signature := strings.TrimPrefix(payload.AdminSignature, "0x")
	sigBytes, err := hexutil.Decode("0x" + signature)
	if err != nil {
		return "", ErrTaskFeeVestingSignatureInvalid
	}
	if len(sigBytes) != 65 {
		return "", ErrTaskFeeVestingSignatureInvalid
	}
	if sigBytes[64] == 27 || sigBytes[64] == 28 {
		sigBytes[64] -= 27
	}

	message := models.BuildVestingSignMessage(models.VestingSignPayload{
		Address:      payload.Address,
		TotalAmount:  payload.TotalAmount,
		StartTime:    payload.StartTime,
		DurationDays: payload.DurationDays,
		Type:         payload.Type,
	})
	prefix := fmt.Sprintf("\x19Ethereum Signed Message:\n%d", len(message))
	messageHash := crypto.Keccak256([]byte(prefix + message))

	pubKey, err := crypto.SigToPub(messageHash, sigBytes)
	if err != nil {
		return "", ErrTaskFeeVestingSignatureInvalid
	}
	return crypto.PubkeyToAddress(*pubKey).Hex(), nil
}

func upsertVestingCreateLog(ctx context.Context, tx *gorm.DB, eventLog relay_api.TaskFeeLog, payload *vestingPayload) error {
	eventAmount, ok := new(big.Int).SetString(eventLog.Amount, 10)
	if !ok || eventAmount.Sign() != 0 {
		return ErrTaskFeeVestingPayloadInvalid
	}
	totalAmount, ok := new(big.Int).SetString(payload.TotalAmount, 10)
	if !ok || totalAmount.Sign() <= 0 {
		return ErrTaskFeeVestingPayloadInvalid
	}
	releasedAmount, ok := new(big.Int).SetString(payload.ReleasedAmount, 10)
	if !ok || releasedAmount.Sign() < 0 || releasedAmount.Cmp(totalAmount) > 0 {
		return ErrTaskFeeVestingPayloadInvalid
	}
	recoveredSigner, err := recoverVestingSigner(payload)
	if err != nil {
		return err
	}
	if !strings.EqualFold(recoveredSigner, config.GetConfig().Relay.VestingSignerAddress) {
		return ErrTaskFeeVestingSignerMismatch
	}

	var record models.VestingRecord
	err = tx.WithContext(ctx).
		Model(&models.VestingRecord{}).
		Where("relay_vesting_id = ?", payload.VestingID).
		First(&record).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		record = models.VestingRecord{
			RelayVestingID: payload.VestingID,
			Address:        payload.Address,
			TotalAmount:    models.BigInt{Int: *totalAmount},
			ReleasedAmount: models.BigInt{Int: *releasedAmount},
			StartTime:      time.Unix(payload.StartTime, 0).UTC(),
			DurationDays:   payload.DurationDays,
			Type:           payload.Type,
			AdminSignature: payload.AdminSignature,
			Status:         models.VestingStatusActive,
		}
		if releasedAmount.Cmp(totalAmount) == 0 {
			record.Status = models.VestingStatusCompleted
		}
		return tx.WithContext(ctx).Create(&record).Error
	}

	if !strings.EqualFold(record.Address, payload.Address) ||
		record.TotalAmount.String() != totalAmount.String() ||
		record.StartTime.Unix() != payload.StartTime ||
		record.DurationDays != payload.DurationDays ||
		record.Type != payload.Type {
		return ErrTaskFeeVestingPayloadInvalid
	}
	return nil
}

func applyVestingReleaseLog(ctx context.Context, tx *gorm.DB, eventLog relay_api.TaskFeeLog, payload *vestingReleasePayload) error {
	eventAmount, ok := new(big.Int).SetString(eventLog.Amount, 10)
	if !ok || eventAmount.Sign() <= 0 {
		return ErrTaskFeeVestingReleaseInvalid
	}
	eventCreatedAt := time.Unix(int64(eventLog.CreatedAt), 0).UTC()
	if eventCreatedAt.After(time.Now().UTC().Add(maxTaskFeeLogFutureDrift)) {
		return ErrTaskFeeVestingReleaseInvalid
	}

	var record models.VestingRecord
	if err := tx.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Model(&models.VestingRecord{}).
		Where("relay_vesting_id = ?", payload.VestingID).
		First(&record).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrTaskFeeVestingRecordNotFound
		}
		return err
	}

	if record.Status != models.VestingStatusActive {
		return ErrTaskFeeVestingReleaseInvalid
	}
	if !strings.EqualFold(record.Address, eventLog.Address) {
		return ErrTaskFeeVestingReleaseInvalid
	}

	releaseTo := big.NewInt(0).Add(&record.ReleasedAmount.Int, eventAmount)
	if releaseTo.Cmp(&record.TotalAmount.Int) > 0 {
		return ErrTaskFeeVestingReleaseInvalid
	}

	shouldReleased := models.ComputeVestingShouldReleased(
		&record.TotalAmount.Int,
		record.StartTime,
		record.DurationDays,
		eventCreatedAt,
	)
	if releaseTo.Cmp(shouldReleased) > 0 {
		return ErrTaskFeeVestingReleaseInvalid
	}

	newStatus := models.VestingStatusActive
	if releaseTo.Cmp(&record.TotalAmount.Int) == 0 {
		newStatus = models.VestingStatusCompleted
	}
	return tx.WithContext(ctx).
		Model(&models.VestingRecord{}).
		Where("id = ?", record.ID).
		Updates(map[string]interface{}{
			"released_amount": models.BigInt{Int: *releaseTo},
			"status":          newStatus,
		}).Error
}

func applyVestingLogs(ctx context.Context, tx *gorm.DB, logs []relay_api.TaskFeeLog) error {
	hasVestingEvents := false
	for _, eventLog := range logs {
		if eventLog.Type != relay_api.TaskFeeLogTypeVestingCreated &&
			eventLog.Type != relay_api.TaskFeeLogTypeVestingRelease {
			continue
		}
		hasVestingEvents = true
	}
	if !hasVestingEvents {
		return nil
	}
	if strings.TrimSpace(config.GetConfig().Relay.VestingSignerAddress) == "" {
		return ErrTaskFeeVestingSignerMismatch
	}

	for _, eventLog := range logs {
		if eventLog.Type != relay_api.TaskFeeLogTypeVestingCreated &&
			eventLog.Type != relay_api.TaskFeeLogTypeVestingRelease {
			continue
		}
		switch eventLog.Type {
		case relay_api.TaskFeeLogTypeVestingCreated:
			payload, err := parseVestingPayload(eventLog)
			if err != nil {
				return err
			}
			if !strings.EqualFold(payload.Address, eventLog.Address) {
				return ErrTaskFeeVestingPayloadInvalid
			}
			if err := upsertVestingCreateLog(ctx, tx, eventLog, payload); err != nil {
				return err
			}
		case relay_api.TaskFeeLogTypeVestingRelease:
			payload, err := parseVestingReleasePayload(eventLog)
			if err != nil {
				return err
			}
			if err := applyVestingReleaseLog(ctx, tx, eventLog, payload); err != nil {
				return err
			}
		default:
			return ErrTaskFeeUnknownEventType
		}
	}
	return nil
}

func checkTaskFeeLogs(ctx context.Context, db *gorm.DB, logs []relay_api.TaskFeeLog) error {
	appConfig := config.GetConfig()
	maxTaskFeeAmount := utils.EtherToWei(big.NewInt(int64(appConfig.Tasks.SyncTaskFeeLogs.MaxTaskFeeAmount)))

	addressLogCount := make(map[string]int)
	for _, eventLog := range logs {
		if !isSupportedTaskFeeLogType(eventLog.Type) {
			return ErrTaskFeeUnknownEventType
		}
		amount, success := new(big.Int).SetString(eventLog.Amount, 10)
		if !success {
			return ErrTaskFeeAmountInvalid
		}

		if eventLog.Type == relay_api.TaskFeeLogTypeDeposit {
			logDepositRequestReceived(eventLog)
			err := validateDepositLog(ctx, eventLog)
			logDepositValidationResult(eventLog, err)
			if err != nil {
				return err
			}
		} else if eventLog.Type == relay_api.TaskFeeLogTypeWithdrawalFeeIncome {
			if !strings.EqualFold(eventLog.Address, appConfig.Relay.WithdrawalFeeAddress) {
				return ErrTaskFeeWithdrawalFeeAddressMismatch
			}
		} else if eventLog.Type == relay_api.TaskFeeLogTypeVestingCreated {
			if amount.Sign() != 0 {
				return ErrTaskFeeVestingPayloadInvalid
			}
			if _, err := parseVestingPayload(eventLog); err != nil {
				return err
			}
		} else if eventLog.Type == relay_api.TaskFeeLogTypeVestingRelease {
			if _, err := parseVestingReleasePayload(eventLog); err != nil {
				return err
			}
		} else if !isBalanceIgnoredTaskFeeLogType(eventLog.Type) && amount.Cmp(maxTaskFeeAmount) > 0 {
			return ErrTaskFeeAmountTooLarge
		}

		if isBalanceIgnoredTaskFeeLogType(eventLog.Type) {
			continue
		}
		addressLogCount[eventLog.Address]++
	}

	var maxCount int
	for _, count := range addressLogCount {
		if count > maxCount {
			maxCount = count
		}
	}
	if uint(maxCount) > appConfig.Tasks.SyncTaskFeeLogs.MaxAddressLogsCountInBatch {
		return ErrTaskFeeAddressCountTooLarge
	}

	if len(addressLogCount) == 0 {
		return nil
	}

	addresses := make([]string, 0, len(addressLogCount))
	for address := range addressLogCount {
		addresses = append(addresses, address)
	}

	var accounts []*models.RelayAccount
	if err := db.WithContext(ctx).Model(&models.RelayAccount{}).Where("address IN (?)", addresses).Find(&accounts).Error; err != nil {
		return err
	}

	existedAddresses := make(map[string]bool)
	for _, account := range accounts {
		existedAddresses[account.Address] = true
	}

	var newAddressCount int
	for address := range addressLogCount {
		if _, ok := existedAddresses[address]; !ok {
			newAddressCount++
		}
	}
	if uint(newAddressCount) > appConfig.Tasks.SyncTaskFeeLogs.MaxNewAddressCountInBatch {
		return ErrTaskFeeNewAddressCountTooLarge
	}

	return nil
}

func depositLogFields(eventLog relay_api.TaskFeeLog) log.Fields {
	fields := log.Fields{
		"relay_account_event_id": eventLog.ID,
		"address":                eventLog.Address,
		"amount":                 eventLog.Amount,
	}
	payload, err := parseDepositPayload(eventLog)
	if err != nil {
		fields["payload_valid"] = false
		return fields
	}
	network, txHash := normalizedDepositIdentity(payload)
	fields["payload_valid"] = true
	fields["network"] = network
	fields["tx_hash"] = txHash
	addBlockchainLogFields(fields, network)
	return fields
}

func addBlockchainLogFields(fields log.Fields, network string) {
	blockchainConfig, ok := config.GetConfig().Blockchains[network]
	if !ok {
		return
	}
	fields["token_type"] = blockchainConfig.TokenType
	if blockchainConfig.TokenType == config.TokenTypeERC20 {
		fields["token_address"] = blockchainConfig.TokenAddress
	}
}

func logDepositRequestReceived(eventLog relay_api.TaskFeeLog) {
	log.WithFields(depositLogFields(eventLog)).Info("Deposit request received")
}

func logDepositValidationResult(eventLog relay_api.TaskFeeLog, validationErr error) {
	fields := depositLogFields(eventLog)
	if validationErr != nil {
		fields["error"] = validationErr.Error()
		log.WithFields(fields).Info("Deposit validation failed")
		return
	}
	log.WithFields(fields).Info("Deposit validation succeeded")
}

func processTaskFeeLogs(ctx context.Context, db *gorm.DB, logs []relay_api.TaskFeeLog) error {
	merged, err := mergeTaskFeeLogs(logs)
	if err != nil {
		return err
	}

	var addresses []string
	for address := range merged {
		addresses = append(addresses, address)
	}
	if len(addresses) == 0 {
		return nil
	}

	dbCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var accounts []*models.RelayAccount
	if err := db.WithContext(dbCtx).Model(&models.RelayAccount{}).Where("address IN (?)", addresses).Find(&accounts).Error; err != nil {
		return err
	}

	existedAddresses := make([]string, len(accounts))
	existedAddressMap := make(map[string]bool)
	for i, account := range accounts {
		existedAddresses[i] = account.Address
		existedAddressMap[account.Address] = true
	}

	for _, account := range accounts {
		amount, ok := merged[account.Address]
		if !ok {
			continue
		}
		account.Balance.Add(&account.Balance.Int, amount)
	}

	var newAccounts []*models.RelayAccount
	for address, amount := range merged {
		if _, ok := existedAddressMap[address]; !ok {
			newAccounts = append(newAccounts, &models.RelayAccount{Address: address, Balance: models.BigInt{Int: *amount}})
		}
	}

	if len(newAccounts) > 0 {
		if err := db.WithContext(dbCtx).CreateInBatches(newAccounts, 100).Error; err != nil {
			return err
		}
	}

	if len(accounts) > 0 {
		var cases string
		for _, account := range accounts {
			cases += fmt.Sprintf(" WHEN address = '%s' THEN '%s'", account.Address, account.Balance.String())
		}
		if err := db.WithContext(dbCtx).Model(&models.RelayAccount{}).Where("address IN (?)", existedAddresses).
			Update("balance", gorm.Expr("CASE"+cases+" END")).Error; err != nil {
			return err
		}
	}

	return nil
}

func buildDepositRecords(logs []relay_api.TaskFeeLog) ([]*models.DepositRecord, error) {
	depositRecords := make([]*models.DepositRecord, 0)
	for _, eventLog := range logs {
		if eventLog.Type != relay_api.TaskFeeLogTypeDeposit {
			continue
		}

		amount, success := new(big.Int).SetString(eventLog.Amount, 10)
		if !success {
			return nil, ErrTaskFeeAmountInvalid
		}

		payload, err := parseDepositPayload(eventLog)
		if err != nil {
			return nil, err
		}
		network, txHash := normalizedDepositIdentity(payload)

		depositRecords = append(depositRecords, &models.DepositRecord{
			Network:             network,
			TxHash:              txHash,
			DepositAddress:      strings.ToLower(config.GetConfig().Relay.DepositAddress),
			FromAddress:         strings.ToLower(eventLog.Address),
			Amount:              models.BigInt{Int: *new(big.Int).Set(amount)},
			RelayAccountEventID: eventLog.ID,
		})
	}
	return depositRecords, nil
}

func saveDepositRecords(ctx context.Context, db *gorm.DB, logs []relay_api.TaskFeeLog) error {
	depositRecords, err := buildDepositRecords(logs)
	if err != nil {
		return err
	}

	if len(depositRecords) == 0 {
		return nil
	}

	dbCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	result := db.WithContext(dbCtx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "network"}, {Name: "tx_hash"}},
		DoNothing: true,
	}).CreateInBatches(depositRecords, 100)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != int64(len(depositRecords)) {
		return ErrTaskFeeDepositTxHashDuplicate
	}
	return nil
}

func logDepositCredits(logs []relay_api.TaskFeeLog) {
	depositRecords, err := buildDepositRecords(logs)
	if err != nil {
		log.Errorf("SyncTaskFeeLogs: failed to build deposit credit logs: %v", err)
		return
	}
	for _, record := range depositRecords {
		fields := log.Fields{
			"relay_account_event_id": record.RelayAccountEventID,
			"network":                record.Network,
			"tx_hash":                record.TxHash,
			"from_address":           record.FromAddress,
			"deposit_address":        record.DepositAddress,
			"amount":                 record.Amount.String(),
		}
		addBlockchainLogFields(fields, record.Network)
		log.WithFields(fields).Info("Deposit credited")
	}
}

func logDepositCreditFailures(logs []relay_api.TaskFeeLog, creditErr error) {
	for _, eventLog := range logs {
		if eventLog.Type != relay_api.TaskFeeLogTypeDeposit {
			continue
		}
		fields := depositLogFields(eventLog)
		fields["error"] = creditErr.Error()
		log.WithFields(fields).Info("Deposit credit failed")
	}
}

func applyTaskFeeLogBatch(ctx context.Context, db *gorm.DB, logs []relay_api.TaskFeeLog, checkpoint *models.TaskFeeCheckpoint) error {
	nextCheckpoint := *checkpoint
	nextCheckpoint.LatestTaskFeeLogID = logs[len(logs)-1].ID
	nextCheckpoint.LatestTaskFeeLogTimestamp = logs[len(logs)-1].CreatedAt

	if err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := saveDepositRecords(ctx, tx, logs); err != nil {
			return err
		}
		if err := applyVestingLogs(ctx, tx, logs); err != nil {
			return err
		}
		if err := processTaskFeeLogs(ctx, tx, logs); err != nil {
			return err
		}

		return tx.Save(&nextCheckpoint).Error
	}); err != nil {
		return err
	}
	*checkpoint = nextCheckpoint
	return nil
}

func syncTaskFeeLogs(ctx context.Context, intervalSeconds uint) error {
	db := config.GetDB()

	var checkpoint models.TaskFeeCheckpoint
	err := db.WithContext(ctx).First(&checkpoint).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}

	batchSize := int(config.GetConfig().Tasks.SyncTaskFeeLogs.BatchSize)
	for {
		logs, err := relay_api.GetTaskFeeLogs(ctx, checkpoint.LatestTaskFeeLogID, batchSize)
		if err != nil {
			return err
		}

		if len(logs) == 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(intervalSeconds) * time.Second):
				continue
			}
		}

		if err := checkTaskFeeLogs(ctx, db, logs); err != nil {
			logDepositCreditFailures(logs, err)
			return err
		}

		if err := applyTaskFeeLogBatch(ctx, db, logs, &checkpoint); err != nil {
			logDepositCreditFailures(logs, err)
			return err
		}
		logDepositCredits(logs)
	}
}
