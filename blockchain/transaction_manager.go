package blockchain

import (
	"context"
	"crynux_relay_wallet/models"
	"sync"

	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

// TransactionManager manages the entire transaction lifecycle
type TransactionManager struct {
	db        *gorm.DB
	sender    *TransactionSender
	confirmer *TransactionConfirmer
	isRunning bool
	mu        sync.RWMutex
}

// NewTransactionManager creates a new transaction manager instance
func NewTransactionManager(db *gorm.DB) *TransactionManager {
	sender := NewTransactionSender(db)
	confirmer := NewTransactionConfirmer(db)

	return &TransactionManager{
		db:        db,
		sender:    sender,
		confirmer: confirmer,
		isRunning: false,
	}
}

// Start starts the transaction manager and all its components
func (tm *TransactionManager) Start(ctx context.Context) error {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if tm.isRunning {
		return nil
	}

	if err := models.RecoverLegacySendingTransactions(ctx, tm.db); err != nil {
		return err
	}

	tm.isRunning = true

	// Start sender and confirmer
	tm.sender.Start(ctx)
	tm.confirmer.Start(ctx)

	log.Info("Transaction manager started")
	return nil
}

// Stop stops the transaction manager and all its components
func (tm *TransactionManager) Stop() {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	if !tm.isRunning {
		return
	}

	tm.sender.Stop()
	tm.confirmer.Stop()
	tm.isRunning = false

	log.Info("Transaction manager stopped")
}

// IsRunning returns whether the transaction manager is running
func (tm *TransactionManager) IsRunning() bool {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	return tm.isRunning
}
