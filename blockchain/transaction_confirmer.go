package blockchain

import (
	"context"
	"crynux_relay_wallet/alert"
	"crynux_relay_wallet/config"
	"crynux_relay_wallet/models"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

// TransactionConfirmer confirms the status of sent transactions
type TransactionConfirmer struct {
	db            *gorm.DB
	processingTxs sync.Map
	txQueue       chan *models.BlockchainTransaction
	stopChan      chan struct{}
	isRunning     bool
	batchSize     int
	pollInterval  time.Duration
	limiter       chan struct{}
}

// NewTransactionConfirmer creates a new transaction confirmer instance
func NewTransactionConfirmer(db *gorm.DB) *TransactionConfirmer {
	return &TransactionConfirmer{
		db:           db,
		txQueue:      make(chan *models.BlockchainTransaction, 100),
		stopChan:     make(chan struct{}),
		isRunning:    false,
		batchSize:    50,
		pollInterval: 5 * time.Second,
		limiter:      make(chan struct{}, 10),
	}
}

// Start starts the transaction confirmer goroutine
func (tc *TransactionConfirmer) Start(ctx context.Context) {
	if tc.isRunning {
		return
	}

	tc.isRunning = true
	go tc.run(ctx)
	log.Info("Transaction confirmer started")
}

// Stop stops the transaction confirmer goroutine
func (tc *TransactionConfirmer) Stop() {
	if !tc.isRunning {
		return
	}

	close(tc.stopChan)
	tc.isRunning = false
	log.Info("Transaction confirmer stopped")
}

// run is the main loop for confirming transactions
func (tc *TransactionConfirmer) run(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	go tc.getSentTransactions(ctx)
	go tc.processSentTransactions(ctx)

	select {
	case <-tc.stopChan:
		close(tc.txQueue)
		return
	case <-ctx.Done():
		close(tc.txQueue)
		return
	}
}

// processSentTransactions processes a batch of sent transactions for confirmation
func (tc *TransactionConfirmer) getSentTransactions(ctx context.Context) {
	ticker := time.NewTicker(tc.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Get sent transactions that need confirmation
			transactions, err := func(ctx context.Context) ([]models.BlockchainTransaction, error) {
				var allTransactions []models.BlockchainTransaction
				offset := 0
				for {
					transactions, err := models.GetSentTransactions(ctx, tc.db, offset, tc.batchSize)
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
			}(ctx)
			if err != nil {
				log.Errorf("Error getting sent transactions: %v", err)
				continue
			}
			if len(transactions) == 0 {
				continue
			}
			var cnt int
			for i := range transactions {
				transaction := transactions[i]
				_, loaded := tc.processingTxs.LoadOrStore(transaction.ID, struct{}{})
				if !loaded {
					select {
					case <-ctx.Done():
						return
					case tc.txQueue <- &transaction:
						cnt++
					}
				}
			}
			log.Infof("Processing %d sent transactions for confirmation", cnt)
		}
	}
}

func (tc *TransactionConfirmer) processSentTransactions(ctx context.Context) {
	for transaction := range tc.txQueue {
		tc.limiter <- struct{}{}
		go func(transaction *models.BlockchainTransaction) {
			defer func() {
				<-tc.limiter
			}()
			if err := tc.confirmTransaction(ctx, transaction); err != nil {
				log.Errorf("Failed to confirm transaction %d: %v", transaction.ID, err)
			}
		}(transaction)
	}
}

// confirmTransaction confirms the status of a single transaction
func (tc *TransactionConfirmer) confirmTransaction(ctx context.Context, transaction *models.BlockchainTransaction) error {
	defer func() {
		tc.processingTxs.Delete(transaction.ID)
	}()

	if !transaction.TxHash.Valid {
		log.Warnf("Transaction %d has no tx hash", transaction.ID)
		return nil
	}
	if !transaction.SentAt.Valid {
		log.Warnf("Transaction %d has no sent at", transaction.ID)
		return nil
	}

	appConfig := config.GetConfig()
	blockchain, ok := appConfig.Blockchains[transaction.Network]
	if !ok {
		return fmt.Errorf("network %s not found", transaction.Network)
	}
	client, err := GetBlockchainClient(transaction.Network)
	if err != nil {
		log.Errorf("Error getting blockchain client: %v", err)
		return err
	}

	txHash := common.HexToHash(transaction.TxHash.String)
	receipt, err := client.RpcClient.TransactionReceipt(ctx, txHash)
	if err != nil {
		if errors.Is(err, ethereum.NotFound) {
			waitDeadline := transaction.SentAt.Time.Add(time.Duration(blockchain.ReceiptWaitTime) * time.Second)
			if time.Now().After(waitDeadline) {
				return tc.handleDelayedReceipt(ctx, transaction)
			}
			log.Debugf("Transaction %s is still pending", txHash.Hex())
			return nil
		}
		log.Errorf("Error getting receipt for transaction %s: %v", txHash.Hex(), err)
		return err
	}

	if receipt.BlockNumber == nil {
		return fmt.Errorf("transaction %d receipt has no block number", transaction.ID)
	}

	latestHeader, err := client.RpcClient.HeaderByNumber(ctx, nil)
	if err != nil {
		return err
	}
	if latestHeader == nil || latestHeader.Number == nil {
		return fmt.Errorf("latest block header unavailable for network %s", transaction.Network)
	}

	requiredConfirmations := blockchain.ReceiptConfirmationBlocks
	if !receiptHasRequiredConfirmations(latestHeader.Number.Uint64(), receipt.BlockNumber.Uint64(), requiredConfirmations) {
		log.Debugf(
			"Transaction %d receipt waiting for confirmations: latest=%d receipt_block=%d need=%d",
			transaction.ID,
			latestHeader.Number.Uint64(),
			receipt.BlockNumber.Uint64(),
			requiredConfirmations,
		)
		return nil
	}

	if receipt.Status == types.ReceiptStatusSuccessful {
		if err := tc.handleSuccessfulTransaction(ctx, transaction, receipt); err != nil {
			log.Errorf("Failed to handle successful transaction: %v", err)
			return err
		}
	} else {
		if err := tc.handleFailedTransaction(ctx, client, transaction, receipt); err != nil {
			log.Errorf("Failed to handle failed transaction: %v", err)
			return err
		}
	}

	return nil
}

// handleSuccessfulTransaction handles a successful transaction
func (tc *TransactionConfirmer) handleSuccessfulTransaction(ctx context.Context, transaction *models.BlockchainTransaction, receipt *types.Receipt) error {
	marked, err := transaction.MarkConfirmed(ctx, tc.db, receipt.BlockNumber.Int64(), int64(receipt.GasUsed), receipt.EffectiveGasPrice.String())
	if err != nil {
		return err
	}
	if !marked {
		log.Infof("Transaction %d already left sent before confirmation write", transaction.ID)
		return nil
	}

	log.Infof("Transaction %d confirmed successfully in block %d", transaction.ID, receipt.BlockNumber.Int64())
	return nil
}

// handleFailedTransaction handles a transaction with an on-chain receipt status of 0
func (tc *TransactionConfirmer) handleFailedTransaction(ctx context.Context, client *BlockchainClient, transaction *models.BlockchainTransaction, receipt *types.Receipt) error {
	errorMsg, err := client.GetErrorMessageFromTransaction(ctx, transaction, receipt)
	if err != nil {
		errorMsg = fmt.Sprintf("Transaction failed with status 0: %v", err)
	}

	if err := tc.db.Transaction(func(tx *gorm.DB) error {
		marked, err := transaction.MarkFailedFromSent(ctx, tx, receipt.BlockNumber.Int64(), int64(receipt.GasUsed), receipt.EffectiveGasPrice.String(), errorMsg)
		if err != nil {
			return err
		}
		if !marked {
			return nil
		}
		if transaction.RetryCount < transaction.MaxRetries {
			if err := transaction.CreateRetryTransaction(ctx, tx); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	log.Infof("Transaction %d failed on-chain, will retry (attempt %d/%d)", transaction.ID, transaction.RetryCount+1, transaction.MaxRetries)

	return nil
}

func (tc *TransactionConfirmer) handleDelayedReceipt(ctx context.Context, transaction *models.BlockchainTransaction) error {
	alerted, err := transaction.MarkReceiptDelayed(ctx, tc.db)
	if err != nil {
		return err
	}
	if !alerted {
		return nil
	}

	blockingMessage := fmt.Sprintf(
		"transaction receipt delayed without on-chain failure proof: transaction_id=%d network=%s tx_hash=%s nonce=%v",
		transaction.ID,
		transaction.Network,
		transaction.TxHash.String,
		transaction.Nonce,
	)
	log.WithFields(log.Fields{
		"transaction_id": transaction.ID,
		"network":        transaction.Network,
		"tx_hash":        transaction.TxHash.String,
		"nonce":          transaction.Nonce,
	}).Error("TransactionConfirmer: " + blockingMessage)
	alert.SafeSendAlert("TransactionConfirmer", blockingMessage)
	return nil
}

func receiptHasRequiredConfirmations(latestBlock, receiptBlock, required uint64) bool {
	if latestBlock < receiptBlock {
		return false
	}
	return latestBlock-receiptBlock >= required
}
