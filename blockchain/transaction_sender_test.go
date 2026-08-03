package blockchain

import (
	"context"
	"crynux_relay_wallet/models"
	"math/big"
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestEncodeAndSendSignedRawRoundTripShape(t *testing.T) {
	privateKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	to := common.HexToAddress("0x2222222222222222222222222222222222222222")
	inner := types.NewTx(&types.DynamicFeeTx{
		ChainID:   big.NewInt(1),
		Nonce:     1,
		GasTipCap: big.NewInt(1),
		GasFeeCap: big.NewInt(2),
		Gas:       21000,
		To:        &to,
		Value:     big.NewInt(1),
	})
	signed, err := types.SignTx(inner, types.LatestSignerForChainID(big.NewInt(1)), privateKey)
	if err != nil {
		t.Fatalf("sign tx: %v", err)
	}
	encoded, err := EncodeSignedRawTransaction(signed)
	if err != nil {
		t.Fatalf("encode signed raw: %v", err)
	}
	if encoded == "" {
		t.Fatal("encoded raw is empty")
	}

	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "sender.db")), &gorm.Config{})
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
		t.Fatalf("migrate: %v", err)
	}
	transaction := &models.BlockchainTransaction{
		Network:     "testnet",
		Type:        "SendETH",
		Status:      models.TransactionStatusSending,
		FromAddress: crypto.PubkeyToAddress(privateKey.PublicKey).Hex(),
		ToAddress:   to.Hex(),
		Value:       "1",
	}
	if err := transaction.Save(context.Background(), db); err != nil {
		t.Fatalf("save transaction: %v", err)
	}
	prepared, err := transaction.PrepareBroadcast(context.Background(), db, signed.Hash().Hex(), 1, encoded)
	if err != nil || !prepared {
		t.Fatalf("prepare broadcast: prepared=%v err=%v", prepared, err)
	}
	marked, err := transaction.MarkSent(context.Background(), db)
	if err != nil || !marked {
		t.Fatalf("mark sent: marked=%v err=%v", marked, err)
	}
	if transaction.TxHash.String != signed.Hash().Hex() {
		t.Fatalf("hash mismatch: %s vs %s", transaction.TxHash.String, signed.Hash().Hex())
	}
}

func TestAdvanceNoncePast(t *testing.T) {
	nonce := uint64(5)
	client := &BlockchainClient{Nonce: &nonce}
	client.AdvanceNoncePast(7)
	if *client.Nonce != 8 {
		t.Fatalf("expected nonce 8, got %d", *client.Nonce)
	}
	client.AdvanceNoncePast(3)
	if *client.Nonce != 8 {
		t.Fatalf("nonce must not move backwards, got %d", *client.Nonce)
	}
}
