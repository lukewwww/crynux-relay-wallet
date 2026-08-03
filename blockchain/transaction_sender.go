package blockchain

import (
	"bytes"
	"context"
	"crynux_relay_wallet/alert"
	"crynux_relay_wallet/config"
	"crynux_relay_wallet/models"
	"errors"
	"fmt"
	"math"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

var ErrHotWalletNativeBalanceInsufficient = errors.New("relay hot wallet native token balance is insufficient")
var ErrHotWalletERC20BalanceInsufficient = errors.New("relay hot wallet ERC20 token balance is insufficient")
var ErrHotWalletNativeGasFeeInsufficient = errors.New("relay hot wallet native gas fee is insufficient")

// TransactionSender sends pending transactions from database to blockchain
type TransactionSender struct {
	db            *gorm.DB
	processingTxs sync.Map
	txQueue       chan *models.BlockchainTransaction
	stopChan      chan struct{}
	isRunning     bool
	batchSize     int
	pollInterval  time.Duration
}

// NewTransactionSender creates a new transaction sender instance
func NewTransactionSender(db *gorm.DB) *TransactionSender {
	return &TransactionSender{
		db:           db,
		txQueue:      make(chan *models.BlockchainTransaction, 100),
		stopChan:     make(chan struct{}),
		isRunning:    false,
		batchSize:    50,
		pollInterval: 5 * time.Second,
	}
}

// Start starts the transaction sender goroutine
func (ts *TransactionSender) Start(ctx context.Context) {
	if ts.isRunning {
		return
	}

	ts.isRunning = true
	go ts.run(ctx)
	log.Info("Transaction sender started")
}

// Stop stops the transaction sender goroutine
func (ts *TransactionSender) Stop() {
	if !ts.isRunning {
		return
	}

	close(ts.stopChan)
	ts.isRunning = false
	log.Info("Transaction sender stopped")
}

// run is the main loop for sending transactions
func (ts *TransactionSender) run(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go ts.getPendingTransactions(ctx)
	go ts.processPendingTransactions(ctx)

	select {
	case <-ts.stopChan:
		close(ts.txQueue)
		return
	case <-ctx.Done():
		close(ts.txQueue)
		return
	}
}

func (ts *TransactionSender) collectTransactions(
	ctx context.Context,
	fetcher func(context.Context, *gorm.DB, int, int) ([]models.BlockchainTransaction, error),
) ([]models.BlockchainTransaction, error) {
	var allTransactions []models.BlockchainTransaction
	offset := 0
	for {
		transactions, err := fetcher(ctx, ts.db, offset, ts.batchSize)
		if err != nil {
			return nil, err
		}
		if len(transactions) == 0 {
			break
		}
		allTransactions = append(allTransactions, transactions...)
		offset += len(transactions)
	}
	return allTransactions, nil
}

func (ts *TransactionSender) enqueueTransactions(ctx context.Context, transactions []models.BlockchainTransaction) int {
	var cnt int
	for i := range transactions {
		transaction := transactions[i]
		_, loaded := ts.processingTxs.LoadOrStore(transaction.ID, struct{}{})
		if loaded {
			continue
		}
		select {
		case <-ctx.Done():
			ts.processingTxs.Delete(transaction.ID)
			return cnt
		case ts.txQueue <- &transaction:
			cnt++
		}
	}
	return cnt
}

// getPendingTransactions gets broadcasting and pending transactions and adds them to the queue
func (ts *TransactionSender) getPendingTransactions(ctx context.Context) {
	ticker := time.NewTicker(ts.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			broadcasting, err := ts.collectTransactions(ctx, models.GetBroadcastingTransactions)
			if err != nil {
				log.Errorf("Error getting broadcasting transactions: %v", err)
				continue
			}
			broadcastingCount := ts.enqueueTransactions(ctx, broadcasting)
			if broadcastingCount > 0 {
				log.Infof("Processing %d broadcasting transactions for recovery", broadcastingCount)
			}

			pending, err := ts.collectTransactions(ctx, models.GetPendingTransactions)
			if err != nil {
				log.Errorf("Error getting pending transactions: %v", err)
				continue
			}
			pendingCount := ts.enqueueTransactions(ctx, pending)
			if pendingCount > 0 {
				log.Infof("Processing %d pending transactions for sending", pendingCount)
			}
		}
	}
}

// processPendingTransactions processes transactions from the queue
func (ts *TransactionSender) processPendingTransactions(ctx context.Context) {
	for transaction := range ts.txQueue {
		if err := ts.sendTransaction(ctx, transaction); err != nil {
			log.Errorf("Failed to send transaction %d: %v", transaction.ID, err)
		}
	}
}

// sendTransaction sends a single transaction to the blockchain
func (ts *TransactionSender) sendTransaction(ctx context.Context, transaction *models.BlockchainTransaction) error {
	defer func() {
		ts.processingTxs.Delete(transaction.ID)
	}()

	if err := transaction.Sync(ctx, ts.db); err != nil {
		return err
	}

	if transaction.Status == models.TransactionStatusBroadcasting {
		return ts.recoverBroadcastingTransaction(ctx, transaction)
	}

	if transaction.Status != models.TransactionStatusPending {
		return nil
	}

	if transaction.TxHash.Valid || transaction.SignedRawTx.Valid {
		return nil
	}

	if transaction.NextRetryAt.Valid && transaction.NextRetryAt.Time.After(time.Now()) {
		return nil
	}

	broadcastingCount, err := models.GetBroadcastingTransactionCountByNetwork(ctx, ts.db, transaction.Network)
	if err != nil {
		return err
	}
	if broadcastingCount > 0 {
		log.Infof("Skipping pending transaction %d because network %s has unresolved broadcasting transactions", transaction.ID, transaction.Network)
		return nil
	}

	claimed, err := transaction.ClaimForSending(ctx, ts.db)
	if err != nil {
		if errors.Is(err, models.ErrRetryAncestorsUnsafe) {
			msg := fmt.Sprintf(
				"blocked retry broadcast because ancestors lack proven on-chain failure: transaction_id=%d network=%s retry_transaction_id=%v",
				transaction.ID,
				transaction.Network,
				transaction.RetryTransactionID,
			)
			log.Error(msg)
			alert.SafeSendAlert("TransactionSender", msg)
			return err
		}
		return err
	}
	if !claimed {
		return nil
	}

	sentTransactionCount, err := models.GetSentTransactionCountByNetwork(ctx, ts.db, transaction.Network)
	if err != nil {
		if releaseErr := transaction.ReleaseSending(ctx, ts.db, err.Error()); releaseErr != nil {
			log.Errorf("Failed to release transaction %d after send count error: %v", transaction.ID, releaseErr)
		}
		return err
	}

	client, err := GetBlockchainClient(transaction.Network)
	if err != nil {
		if releaseErr := transaction.ReleaseSending(ctx, ts.db, err.Error()); releaseErr != nil {
			log.Errorf("Failed to release transaction %d after client error: %v", transaction.ID, releaseErr)
		}
		return err
	}

	if uint64(sentTransactionCount) >= client.SentTransactionCountLimit {
		log.Infof("Sent transaction count limit reached for transaction: %d, network %s, skipping", transaction.ID, transaction.Network)
		if err := transaction.ReleaseSending(ctx, ts.db, "sent transaction count limit reached"); err != nil {
			return err
		}
		return nil
	}

	client.NonceMu.Lock()
	defer client.NonceMu.Unlock()

	nonce, err := client.GetNonce(ctx)
	if err != nil {
		if releaseErr := transaction.ReleaseSending(ctx, ts.db, err.Error()); releaseErr != nil {
			log.Errorf("Failed to release transaction %d after nonce error: %v", transaction.ID, releaseErr)
		}
		return err
	}

	signedTx, err := ts.buildSignedTransaction(ctx, client, transaction, nonce)
	if err != nil {
		log.Errorf("Failed to build signed transaction %d, nonce: %d, %v", transaction.ID, nonce, err)
		err = client.processSendingTxError(err)
		ts.alertHotWalletBalanceError(transaction, err)
		if releaseErr := transaction.ReleaseSending(ctx, ts.db, err.Error()); releaseErr != nil {
			log.Errorf("Failed to release transaction %d after build error: %v", transaction.ID, releaseErr)
		}
		return err
	}

	signedRawTx, err := EncodeSignedRawTransaction(signedTx)
	if err != nil {
		if releaseErr := transaction.ReleaseSending(ctx, ts.db, err.Error()); releaseErr != nil {
			log.Errorf("Failed to release transaction %d after encode error: %v", transaction.ID, releaseErr)
		}
		return err
	}

	prepared, err := transaction.PrepareBroadcast(ctx, ts.db, signedTx.Hash().Hex(), int64(nonce), signedRawTx)
	if err != nil {
		if releaseErr := transaction.ReleaseSending(ctx, ts.db, err.Error()); releaseErr != nil {
			log.Errorf("Failed to release transaction %d after prepare error: %v", transaction.ID, releaseErr)
		}
		return err
	}
	if !prepared {
		if releaseErr := transaction.ReleaseSending(ctx, ts.db, "cancellation requested before broadcast"); releaseErr != nil {
			log.Errorf("Failed to release transaction %d after cancelled prepare: %v", transaction.ID, releaseErr)
		}
		return nil
	}

	return ts.acknowledgeBroadcast(ctx, client, transaction)
}

func (ts *TransactionSender) recoverBroadcastingTransaction(ctx context.Context, transaction *models.BlockchainTransaction) error {
	if !transaction.TxHash.Valid || !transaction.SignedRawTx.Valid || !transaction.Nonce.Valid {
		msg := fmt.Sprintf("broadcasting transaction %d is missing signed payload", transaction.ID)
		log.Error(msg)
		alert.SafeSendAlert("TransactionSender", msg)
		return errors.New(msg)
	}

	client, err := GetBlockchainClient(transaction.Network)
	if err != nil {
		return err
	}

	client.NonceMu.Lock()
	defer client.NonceMu.Unlock()

	visible, err := ts.isTransactionVisibleOnChain(ctx, client, transaction.TxHash.String)
	if err != nil {
		return err
	}
	if visible {
		return ts.markBroadcastAcknowledged(ctx, client, transaction)
	}

	_, err = client.SendSignedRawTransaction(ctx, transaction.SignedRawTx.String)
	if err != nil {
		if isAlreadyKnownTransactionError(err) {
			return ts.markBroadcastAcknowledged(ctx, client, transaction)
		}
		visible, visibilityErr := ts.isTransactionVisibleOnChain(ctx, client, transaction.TxHash.String)
		if visibilityErr != nil {
			return visibilityErr
		}
		if visible {
			return ts.markBroadcastAcknowledged(ctx, client, transaction)
		}
		log.Errorf("Failed to rebroadcast transaction %d: %v", transaction.ID, err)
		return err
	}

	return ts.markBroadcastAcknowledged(ctx, client, transaction)
}

func (ts *TransactionSender) acknowledgeBroadcast(ctx context.Context, client *BlockchainClient, transaction *models.BlockchainTransaction) error {
	_, err := client.SendSignedRawTransaction(ctx, transaction.SignedRawTx.String)
	if err != nil {
		if isAlreadyKnownTransactionError(err) {
			return ts.markBroadcastAcknowledged(ctx, client, transaction)
		}
		visible, visibilityErr := ts.isTransactionVisibleOnChain(ctx, client, transaction.TxHash.String)
		if visibilityErr != nil {
			log.Errorf("Failed to send raw transaction %d after prepare: %v", transaction.ID, err)
			return err
		}
		if visible {
			return ts.markBroadcastAcknowledged(ctx, client, transaction)
		}
		log.Errorf("Failed to send raw transaction %d after prepare: %v", transaction.ID, err)
		return err
	}
	return ts.markBroadcastAcknowledged(ctx, client, transaction)
}

func (ts *TransactionSender) markBroadcastAcknowledged(ctx context.Context, client *BlockchainClient, transaction *models.BlockchainTransaction) error {
	marked, err := transaction.MarkSent(ctx, ts.db)
	if err != nil {
		log.Errorf("Failed to mark transaction %d as sent, hash: %s, nonce: %d, %v", transaction.ID, transaction.TxHash.String, transaction.Nonce.Int64, err)
		return err
	}
	if !marked && transaction.Status != models.TransactionStatusSent {
		return fmt.Errorf("transaction %d could not be marked sent", transaction.ID)
	}
	if transaction.Nonce.Valid {
		client.AdvanceNoncePast(transaction.Nonce.Int64)
	}
	log.Infof("Transaction %d sent successfully with hash: %s, nonce: %d", transaction.ID, transaction.TxHash.String, transaction.Nonce.Int64)
	return nil
}

func (ts *TransactionSender) isTransactionVisibleOnChain(ctx context.Context, client *BlockchainClient, txHash string) (bool, error) {
	hash := common.HexToHash(txHash)
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	receipt, err := client.RpcClient.TransactionReceipt(callCtx, hash)
	if err == nil && receipt != nil {
		return true, nil
	}
	if err != nil && !errors.Is(err, ethereum.NotFound) {
		return false, err
	}

	transfer, err := client.GetTransactionTransfer(ctx, hash)
	if err == nil && transfer != nil {
		return true, nil
	}
	if errors.Is(err, ethereum.NotFound) {
		return false, nil
	}
	return false, err
}

func (ts *TransactionSender) buildSignedTransaction(ctx context.Context, client *BlockchainClient, transaction *models.BlockchainTransaction, nonce uint64) (*types.Transaction, error) {
	if transaction.FromAddress != client.Address {
		return nil, fmt.Errorf("from address is not the same as the client address")
	}

	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if err := validateHotWalletPayoutBalance(callCtx, client, transaction); err != nil {
		return nil, err
	}

	gasLimit, err := estimateTransactionGasLimit(callCtx, client, transaction)
	if err != nil {
		return nil, err
	}

	gasFeeCap, gasTipCap, err := suggestDynamicFeeCaps(callCtx, client)
	if err != nil {
		return nil, err
	}

	if err := validateHotWalletGasBalance(callCtx, client, transaction, gasLimit, gasFeeCap); err != nil {
		return nil, err
	}

	rawTx, err := buildDynamicFeeTransaction(transaction, nonce, gasLimit, gasFeeCap, gasTipCap, client.ChainID)
	if err != nil {
		return nil, err
	}

	privateKey, err := crypto.HexToECDSA(client.PrivateKey)
	if err != nil {
		return nil, err
	}
	return types.SignTx(rawTx, types.LatestSignerForChainID(client.ChainID), privateKey)
}

func isHotWalletBalanceError(err error) bool {
	return errors.Is(err, ErrHotWalletNativeBalanceInsufficient) ||
		errors.Is(err, ErrHotWalletERC20BalanceInsufficient) ||
		errors.Is(err, ErrHotWalletNativeGasFeeInsufficient)
}

func (ts *TransactionSender) alertHotWalletBalanceError(transaction *models.BlockchainTransaction, err error) {
	if !isHotWalletBalanceError(err) {
		return
	}
	if transaction.StatusMessage.Valid && transaction.StatusMessage.String == err.Error() {
		return
	}
	msg := fmt.Sprintf("Hot wallet balance is insufficient for transaction %d on network %s: %v", transaction.ID, transaction.Network, err)
	log.Error(msg)
	alert.SafeSendAlert("TransactionSender", msg)
}

func parseTransactionValue(transaction *models.BlockchainTransaction) (*big.Int, error) {
	value, ok := new(big.Int).SetString(transaction.Value, 10)
	if !ok {
		return nil, errors.New("transaction value is invalid")
	}
	return value, nil
}

func parseERC20TransferAmount(transaction *models.BlockchainTransaction) (*big.Int, error) {
	data, err := parseTransactionData(transaction.Data)
	if err != nil {
		return nil, err
	}
	method := erc20ABI.Methods["transfer"]
	if len(data) < len(method.ID) || !bytes.Equal(data[:len(method.ID)], method.ID) {
		return nil, errors.New("transaction data is not ERC20 transfer data")
	}
	values, err := method.Inputs.Unpack(data[len(method.ID):])
	if err != nil {
		return nil, err
	}
	if len(values) != 2 {
		return nil, errors.New("invalid ERC20 transfer arguments")
	}
	amount, ok := values[1].(*big.Int)
	if !ok {
		return nil, errors.New("invalid ERC20 transfer amount")
	}
	return amount, nil
}

func validateHotWalletPayoutBalance(ctx context.Context, client *BlockchainClient, transaction *models.BlockchainTransaction) error {
	blockchainConfig, ok := config.GetConfig().Blockchains[transaction.Network]
	if !ok {
		return ErrBlockchainNotFound
	}
	fromAddress := common.HexToAddress(client.Address)
	switch blockchainConfig.TokenType {
	case config.TokenTypeNative:
		value, err := parseTransactionValue(transaction)
		if err != nil {
			return err
		}
		balance, err := client.BalanceAt(ctx, fromAddress)
		if err != nil {
			return err
		}
		if balance.Cmp(value) < 0 {
			return fmt.Errorf("%w: network %s, balance %s, required %s", ErrHotWalletNativeBalanceInsufficient, transaction.Network, balance.String(), value.String())
		}
	case config.TokenTypeERC20:
		amount, err := parseERC20TransferAmount(transaction)
		if err != nil {
			return err
		}
		tokenBalance, err := client.TokenBalanceAt(ctx, common.HexToAddress(blockchainConfig.TokenAddress), fromAddress)
		if err != nil {
			return err
		}
		if tokenBalance.Cmp(amount) < 0 {
			return fmt.Errorf("%w: network %s, balance %s, required %s", ErrHotWalletERC20BalanceInsufficient, transaction.Network, tokenBalance.String(), amount.String())
		}
	default:
		return ErrBlockchainNotFound
	}
	return nil
}

func validateHotWalletGasBalance(ctx context.Context, client *BlockchainClient, transaction *models.BlockchainTransaction, gasLimit uint64, gasFeeCap *big.Int) error {
	blockchainConfig, ok := config.GetConfig().Blockchains[transaction.Network]
	if !ok {
		return ErrBlockchainNotFound
	}
	required := big.NewInt(0).Mul(big.NewInt(0).SetUint64(gasLimit), gasFeeCap)
	if blockchainConfig.TokenType == config.TokenTypeNative {
		value, err := parseTransactionValue(transaction)
		if err != nil {
			return err
		}
		required.Add(required, value)
	}
	balance, err := client.BalanceAt(ctx, common.HexToAddress(client.Address))
	if err != nil {
		return err
	}
	if balance.Cmp(required) < 0 {
		return fmt.Errorf("%w: network %s, balance %s, required %s", ErrHotWalletNativeGasFeeInsufficient, transaction.Network, balance.String(), required.String())
	}
	return nil
}

func estimateTransactionGasLimit(ctx context.Context, client *BlockchainClient, transaction *models.BlockchainTransaction) (uint64, error) {
	msg, err := BuildCallMsgFromTransaction(transaction)
	if err != nil {
		return 0, err
	}
	if err := client.Limiter.Wait(ctx); err != nil {
		return 0, err
	}
	estimatedGas, err := client.RpcClient.EstimateGas(ctx, msg)
	if err != nil {
		return 0, err
	}
	gasLimit, err := addGasLimitBuffer(estimatedGas, client.GasLimitBufferPercent)
	if err != nil {
		return 0, err
	}
	if client.GasLimit > 0 && gasLimit > client.GasLimit {
		return 0, fmt.Errorf("estimated gas limit %d exceeds configured gas limit %d", gasLimit, client.GasLimit)
	}
	return gasLimit, nil
}

func ValidateTransactionFeeCaps(ctx context.Context, transaction *models.BlockchainTransaction) error {
	client, err := GetBlockchainClient(transaction.Network)
	if err != nil {
		return err
	}
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := estimateTransactionGasLimit(callCtx, client, transaction); err != nil {
		return err
	}
	_, _, err = suggestDynamicFeeCaps(callCtx, client)
	return err
}

func addGasLimitBuffer(estimatedGas uint64, bufferPercent uint64) (uint64, error) {
	if estimatedGas > math.MaxUint64/(100+bufferPercent) {
		return 0, errors.New("estimated gas limit is too large")
	}
	gasLimit := estimatedGas * (100 + bufferPercent) / 100
	if gasLimit == 0 {
		return 0, errors.New("estimated gas limit is zero")
	}
	return gasLimit, nil
}

func suggestDynamicFeeCaps(ctx context.Context, client *BlockchainClient) (*big.Int, *big.Int, error) {
	if err := client.Limiter.Wait(ctx); err != nil {
		return nil, nil, err
	}
	header, err := client.RpcClient.HeaderByNumber(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	if header.BaseFee == nil {
		return nil, nil, errors.New("latest block does not include base fee")
	}
	if err := client.Limiter.Wait(ctx); err != nil {
		return nil, nil, err
	}
	gasTipCap, err := client.RpcClient.SuggestGasTipCap(ctx)
	if err != nil {
		return nil, nil, err
	}
	gasFeeCap := big.NewInt(0).Mul(header.BaseFee, big.NewInt(2))
	gasFeeCap.Add(gasFeeCap, gasTipCap)
	if err := validateDynamicFeeCaps(gasFeeCap, gasTipCap, client.MaxFeePerGas, client.MaxPriorityFeePerGas); err != nil {
		return nil, nil, err
	}
	return gasFeeCap, gasTipCap, nil
}

func validateDynamicFeeCaps(gasFeeCap *big.Int, gasTipCap *big.Int, maxFeePerGas *big.Int, maxPriorityFeePerGas *big.Int) error {
	if maxFeePerGas != nil && gasFeeCap.Cmp(maxFeePerGas) > 0 {
		return fmt.Errorf("estimated max fee per gas %s exceeds configured cap %s", gasFeeCap.String(), maxFeePerGas.String())
	}
	if maxPriorityFeePerGas != nil && gasTipCap.Cmp(maxPriorityFeePerGas) > 0 {
		return fmt.Errorf("estimated max priority fee per gas %s exceeds configured cap %s", gasTipCap.String(), maxPriorityFeePerGas.String())
	}
	return nil
}

func buildDynamicFeeTransaction(transaction *models.BlockchainTransaction, nonce uint64, gasLimit uint64, gasFeeCap *big.Int, gasTipCap *big.Int, chainID *big.Int) (*types.Transaction, error) {
	value, err := parseTransactionValue(transaction)
	if err != nil {
		return nil, err
	}
	if !common.IsHexAddress(transaction.ToAddress) {
		return nil, errors.New("transaction to address is invalid")
	}
	data, err := parseTransactionData(transaction.Data)
	if err != nil {
		return nil, err
	}
	toAddress := common.HexToAddress(transaction.ToAddress)
	return types.NewTx(&types.DynamicFeeTx{
		ChainID:   chainID,
		Nonce:     nonce,
		GasTipCap: gasTipCap,
		GasFeeCap: gasFeeCap,
		Gas:       gasLimit,
		To:        &toAddress,
		Value:     value,
		Data:      data,
	}), nil
}
