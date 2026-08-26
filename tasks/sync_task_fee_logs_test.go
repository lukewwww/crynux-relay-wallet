package tasks

import (
	"context"
	"crynux_relay_wallet/blockchain"
	"crynux_relay_wallet/config"
	"crynux_relay_wallet/models"
	"crynux_relay_wallet/relay_api"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestMergeTaskFeeLogsRejectsUnknownEventType(t *testing.T) {
	_, err := mergeTaskFeeLogs([]relay_api.TaskFeeLog{
		{
			Address: "0xabc",
			Amount:  "1",
			Type:    relay_api.TaskFeeLogType(100),
		},
	})
	if !errors.Is(err, ErrTaskFeeUnknownEventType) {
		t.Fatalf("expected ErrTaskFeeUnknownEventType, got %v", err)
	}
}

func TestMergeTaskFeeLogsHandlesVestingTypes(t *testing.T) {
	merged, err := mergeTaskFeeLogs([]relay_api.TaskFeeLog{
		{
			Address: "0xabc",
			Amount:  "0",
			Type:    relay_api.TaskFeeLogTypeVestingCreated,
		},
		{
			Address: "0xabc",
			Amount:  "25",
			Type:    relay_api.TaskFeeLogTypeVestingRelease,
		},
	})
	if err != nil {
		t.Fatalf("mergeTaskFeeLogs failed: %v", err)
	}
	if got := merged["0xabc"].String(); got != "25" {
		t.Fatalf("expected merged vesting release amount 25, got %s", got)
	}
}

func TestMergeTaskFeeLogsHandlesUserDelegation(t *testing.T) {
	merged, err := mergeTaskFeeLogs([]relay_api.TaskFeeLog{
		{
			Address: "0xabc",
			Amount:  "13",
			Type:    relay_api.TaskFeeLogTypeUserDelegation,
		},
	})
	if err != nil {
		t.Fatalf("mergeTaskFeeLogs failed: %v", err)
	}
	if got := merged["0xabc"].String(); got != "13" {
		t.Fatalf("expected merged user delegation amount 13, got %s", got)
	}
}

func TestCheckTaskFeeLogsValidatesWithdrawalFeeAddress(t *testing.T) {
	feeAddress := common.HexToAddress("0x2222222222222222222222222222222222222222")
	otherAddress := common.HexToAddress("0x3333333333333333333333333333333333333333")

	configDir := t.TempDir()
	configContent := fmt.Sprintf(`
environment: test
relay:
  withdrawal_fee_address: %s
tasks:
  sync_task_fee_logs:
    max_task_fee_amount: 100
    max_address_logs_count_in_batch: 10
    max_new_address_count_in_batch: 10
  process_withdrawal_requests:
    cancellation_settlement_timeout_seconds: 30
`, feeAddress.Hex())
	if err := os.WriteFile(filepath.Join(configDir, "config.yml"), []byte(configContent), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := config.InitConfig(configDir); err != nil {
		t.Fatalf("init config: %v", err)
	}

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(&models.RelayAccount{}); err != nil {
		t.Fatalf("migrate db: %v", err)
	}

	err = checkTaskFeeLogs(context.Background(), db, []relay_api.TaskFeeLog{
		{
			Address: feeAddress.Hex(),
			Amount:  "1",
			Type:    relay_api.TaskFeeLogTypeWithdrawalFeeIncome,
		},
	})
	if err != nil {
		t.Fatalf("expected matching withdrawal fee address to pass, got %v", err)
	}

	err = checkTaskFeeLogs(context.Background(), db, []relay_api.TaskFeeLog{
		{
			Address: otherAddress.Hex(),
			Amount:  "1",
			Type:    relay_api.TaskFeeLogTypeWithdrawalFeeIncome,
		},
	})
	if !errors.Is(err, ErrTaskFeeWithdrawalFeeAddressMismatch) {
		t.Fatalf("expected ErrTaskFeeWithdrawalFeeAddressMismatch, got %v", err)
	}
}

func TestParseVestingReleasePayloadOnlyRequiresVestingID(t *testing.T) {
	payload, err := parseVestingReleasePayload(relay_api.TaskFeeLog{
		Payload: `{"vesting_id":42}`,
	})
	if err != nil {
		t.Fatalf("parseVestingReleasePayload failed: %v", err)
	}
	if payload.VestingID != 42 {
		t.Fatalf("expected vesting id 42, got %d", payload.VestingID)
	}
}

func TestParseVestingReleasePayloadRejectsMissingVestingID(t *testing.T) {
	_, err := parseVestingReleasePayload(relay_api.TaskFeeLog{
		Payload: `{}`,
	})
	if !errors.Is(err, ErrTaskFeeVestingPayloadInvalid) {
		t.Fatalf("expected ErrTaskFeeVestingPayloadInvalid, got %v", err)
	}
}

func TestApplyVestingReleaseLogRejectsFarFutureCreatedAt(t *testing.T) {
	err := applyVestingReleaseLog(context.Background(), nil, relay_api.TaskFeeLog{
		Amount:    "1",
		CreatedAt: uint64(time.Now().UTC().Add(maxTaskFeeLogFutureDrift + time.Second).Unix()),
	}, &vestingReleasePayload{
		VestingID: 42,
	})
	if !errors.Is(err, ErrTaskFeeVestingReleaseInvalid) {
		t.Fatalf("expected ErrTaskFeeVestingReleaseInvalid, got %v", err)
	}
}

func TestApplyTaskFeeLogBatchRejectsDeprecatedVestingWithoutAdvancingCheckpoint(t *testing.T) {
	configDir := t.TempDir()
	configContent := `
environment: test
relay:
  vesting_signer_address: 0x1111111111111111111111111111111111111111
tasks:
  process_withdrawal_requests:
    cancellation_settlement_timeout_seconds: 30
`
	if err := os.WriteFile(filepath.Join(configDir, "config.yml"), []byte(configContent), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := config.InitConfig(configDir); err != nil {
		t.Fatalf("init config: %v", err)
	}

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(
		&models.VestingRecord{},
		&models.RelayAccount{},
		&models.TaskFeeCheckpoint{},
	); err != nil {
		t.Fatalf("migrate db: %v", err)
	}

	address := common.HexToAddress("0x2222222222222222222222222222222222222222").Hex()
	record := models.VestingRecord{
		RelayVestingID: 42,
		Address:        address,
		TotalAmount:    models.BigInt{Int: *big.NewInt(100)},
		ReleasedAmount: models.BigInt{Int: *big.NewInt(20)},
		StartTime:      time.Now().UTC().AddDate(0, 0, -10),
		DurationDays:   10,
		Type:           models.VestingTypeOther,
		AdminSignature: "signature",
		Status:         models.VestingStatusDeprecated,
	}
	account := models.RelayAccount{
		Address: address,
		Balance: models.BigInt{Int: *big.NewInt(50)},
	}
	checkpoint := models.TaskFeeCheckpoint{
		LatestTaskFeeLogID:        10,
		LatestTaskFeeLogTimestamp: 100,
	}
	if err := db.Create(&record).Error; err != nil {
		t.Fatalf("create vesting record: %v", err)
	}
	if err := db.Create(&account).Error; err != nil {
		t.Fatalf("create relay account: %v", err)
	}
	if err := db.Create(&checkpoint).Error; err != nil {
		t.Fatalf("create checkpoint: %v", err)
	}

	err = applyTaskFeeLogBatch(context.Background(), db, []relay_api.TaskFeeLog{
		{
			ID:        11,
			CreatedAt: uint64(time.Now().UTC().Unix()),
			Address:   address,
			Amount:    "10",
			Type:      relay_api.TaskFeeLogTypeVestingRelease,
			Payload:   `{"vesting_id":42}`,
		},
	}, &checkpoint)
	if !errors.Is(err, ErrTaskFeeVestingReleaseInvalid) {
		t.Fatalf("expected ErrTaskFeeVestingReleaseInvalid, got %v", err)
	}
	if checkpoint.LatestTaskFeeLogID != 10 || checkpoint.LatestTaskFeeLogTimestamp != 100 {
		t.Fatalf("in-memory checkpoint advanced: %+v", checkpoint)
	}

	var persistedRecord models.VestingRecord
	if err := db.First(&persistedRecord, record.ID).Error; err != nil {
		t.Fatalf("reload vesting record: %v", err)
	}
	if persistedRecord.ReleasedAmount.String() != "20" || persistedRecord.Status != models.VestingStatusDeprecated {
		t.Fatalf("vesting record changed: released=%s status=%d", persistedRecord.ReleasedAmount.String(), persistedRecord.Status)
	}
	var persistedAccount models.RelayAccount
	if err := db.First(&persistedAccount, account.ID).Error; err != nil {
		t.Fatalf("reload relay account: %v", err)
	}
	if persistedAccount.Balance.String() != "50" {
		t.Fatalf("relay account balance changed: %s", persistedAccount.Balance.String())
	}
	var persistedCheckpoint models.TaskFeeCheckpoint
	if err := db.First(&persistedCheckpoint, checkpoint.ID).Error; err != nil {
		t.Fatalf("reload checkpoint: %v", err)
	}
	if persistedCheckpoint.LatestTaskFeeLogID != 10 || persistedCheckpoint.LatestTaskFeeLogTimestamp != 100 {
		t.Fatalf("persisted checkpoint advanced: %+v", persistedCheckpoint)
	}
}

func TestParseVestingPayloadRequiresValidType(t *testing.T) {
	validPayload := `{"vesting_id":42,"address":"0x1111111111111111111111111111111111111111","total_amount":"1000","released_amount":"0","start_time":1767225600,"duration_days":30,"type":"delegation","admin_signature":"0xabc"}`
	payload, err := parseVestingPayload(relay_api.TaskFeeLog{Payload: validPayload})
	if err != nil {
		t.Fatalf("parseVestingPayload failed: %v", err)
	}
	if payload.Type != models.VestingTypeDelegation {
		t.Fatalf("expected delegation type, got %s", payload.Type)
	}

	missingTypePayload := `{"vesting_id":42,"address":"0x1111111111111111111111111111111111111111","total_amount":"1000","released_amount":"0","start_time":1767225600,"duration_days":30,"admin_signature":"0xabc"}`
	_, err = parseVestingPayload(relay_api.TaskFeeLog{Payload: missingTypePayload})
	if !errors.Is(err, ErrTaskFeeVestingPayloadInvalid) {
		t.Fatalf("expected missing type to fail with ErrTaskFeeVestingPayloadInvalid, got %v", err)
	}

	invalidTypePayload := `{"vesting_id":42,"address":"0x1111111111111111111111111111111111111111","total_amount":"1000","released_amount":"0","start_time":1767225600,"duration_days":30,"type":"delegator","admin_signature":"0xabc"}`
	_, err = parseVestingPayload(relay_api.TaskFeeLog{Payload: invalidTypePayload})
	if !errors.Is(err, ErrTaskFeeVestingPayloadInvalid) {
		t.Fatalf("expected invalid type to fail with ErrTaskFeeVestingPayloadInvalid, got %v", err)
	}
}

type depositRPCState struct {
	txHash        common.Hash
	from          common.Address
	to            common.Address
	value         *big.Int
	input         string
	receiptStatus string
	blockNumber   uint64
	blockTime     uint64
	receiptLogs   []any
}

func zeroBloomHex() string {
	return "0x" + strings.Repeat("0", 512)
}

func newDepositRPCServer(t *testing.T, state depositRPCState) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
			ID     json.RawMessage   `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode rpc request: %v", err)
		}

		result := any(nil)
		receiptLogs := state.receiptLogs
		if receiptLogs == nil {
			receiptLogs = []any{}
		}
		switch request.Method {
		case "eth_getTransactionCount":
			result = "0x0"
		case "eth_getTransactionReceipt":
			result = map[string]any{
				"transactionHash":   state.txHash.Hex(),
				"transactionIndex":  "0x0",
				"blockHash":         common.HexToHash("0x100").Hex(),
				"blockNumber":       fmt.Sprintf("0x%x", state.blockNumber),
				"from":              state.from.Hex(),
				"to":                state.to.Hex(),
				"cumulativeGasUsed": "0x5208",
				"gasUsed":           "0x5208",
				"effectiveGasPrice": "0x1",
				"contractAddress":   nil,
				"logs":              receiptLogs,
				"logsBloom":         zeroBloomHex(),
				"status":            state.receiptStatus,
				"type":              "0x0",
			}
		case "eth_getTransactionByHash":
			result = map[string]any{
				"type":  "0x7e",
				"hash":  state.txHash.Hex(),
				"from":  state.from.Hex(),
				"to":    state.to.Hex(),
				"value": "0x" + state.value.Text(16),
				"input": state.input,
			}
		case "eth_getBlockByNumber":
			result = map[string]any{
				"parentHash":       common.HexToHash("0x1").Hex(),
				"sha3Uncles":       common.HexToHash("0x2").Hex(),
				"miner":            common.HexToAddress("0x3333333333333333333333333333333333333333").Hex(),
				"stateRoot":        common.HexToHash("0x3").Hex(),
				"transactionsRoot": common.HexToHash("0x4").Hex(),
				"receiptsRoot":     common.HexToHash("0x5").Hex(),
				"logsBloom":        zeroBloomHex(),
				"difficulty":       "0x0",
				"number":           fmt.Sprintf("0x%x", state.blockNumber),
				"gasLimit":         "0x1c9c380",
				"gasUsed":          "0x5208",
				"timestamp":        fmt.Sprintf("0x%x", state.blockTime),
				"extraData":        "0x",
				"mixHash":          common.HexToHash("0x6").Hex(),
				"nonce":            "0x0000000000000000",
				"baseFeePerGas":    "0x1",
				"transactions":     []any{},
				"uncles":           []any{},
			}
		default:
			t.Fatalf("unexpected rpc method %s", request.Method)
		}

		response := map[string]any{
			"jsonrpc": "2.0",
			"id":      request.ID,
			"result":  result,
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Fatalf("encode rpc response: %v", err)
		}
	}))
}

func initDepositValidationConfig(t *testing.T, network string, rpcEndpoint string, depositAddress common.Address) {
	initDepositValidationConfigWithToken(t, network, rpcEndpoint, depositAddress, config.TokenTypeNative, "")
}

func initDepositValidationConfigWithToken(t *testing.T, network string, rpcEndpoint string, depositAddress common.Address, tokenType string, tokenAddress string) {
	t.Helper()

	privateKey, err := crypto.GenerateKey()
	if err != nil {
		t.Fatalf("generate private key: %v", err)
	}
	privateKeyFile := filepath.Join(t.TempDir(), "private_key")
	if err := os.WriteFile(privateKeyFile, []byte(fmt.Sprintf("%x", crypto.FromECDSA(privateKey))), 0600); err != nil {
		t.Fatalf("write private key: %v", err)
	}
	accountAddress := crypto.PubkeyToAddress(privateKey.PublicKey)

	configDir := t.TempDir()
	tokenAddressConfig := ""
	if tokenAddress != "" {
		tokenAddressConfig = fmt.Sprintf("    token_address: %s\n", tokenAddress)
	}
	configContent := fmt.Sprintf(`
environment: debug
blockchains:
  %s:
    rps: 100
    rpc_endpoint: %s
    token_type: %s
%s
    gas_limit: 21000
    gas_limit_buffer_percent: 20
    max_fee_per_gas: 100000000000
    max_priority_fee_per_gas: 1000000000
    chain_id: 42161
    account:
      address: %s
      private_key_file: %s
    contracts:
      benefit_address: "0x0000000000000000000000000000000000000000"
    max_retries: 1
    retry_interval: 1
    receipt_wait_time: 30
    receipt_confirmation_blocks: 1
    sent_transaction_count_limit: 100
    max_withdrawals_per_day: 10
relay:
  api:
    private_key_file: %s
  deposit_address: %s
tasks:
  sync_task_fee_logs:
    deposit_max_age_seconds: 3600
  process_withdrawal_requests:
    cancellation_settlement_timeout_seconds: 30
`, network, rpcEndpoint, tokenType, tokenAddressConfig, accountAddress.Hex(), filepath.ToSlash(privateKeyFile), filepath.ToSlash(privateKeyFile), depositAddress.Hex())
	if err := os.WriteFile(filepath.Join(configDir, "config.yml"), []byte(configContent), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := config.InitConfig(configDir); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := blockchain.Init(context.Background()); err != nil {
		t.Fatalf("init blockchain: %v", err)
	}
}

func validateDepositForState(t *testing.T, state depositRPCState, eventLog relay_api.TaskFeeLog) error {
	return validateDepositForStateWithDepositAddress(t, state, eventLog, state.to)
}

func validateDepositForStateWithDepositAddress(t *testing.T, state depositRPCState, eventLog relay_api.TaskFeeLog, depositAddress common.Address) error {
	t.Helper()

	network := fmt.Sprintf("deposit_test_%d", time.Now().UnixNano())
	server := newDepositRPCServer(t, state)
	defer server.Close()
	initDepositValidationConfig(t, network, server.URL, depositAddress)
	eventLog.Payload = fmt.Sprintf(`{"tx_hash":"%s","network":"%s"}`, state.txHash.Hex(), network)
	return validateDepositLog(context.Background(), eventLog)
}

func validateERC20DepositForState(t *testing.T, state depositRPCState, eventLog relay_api.TaskFeeLog, depositAddress common.Address, tokenAddress common.Address) error {
	t.Helper()

	network := fmt.Sprintf("deposit_test_%d", time.Now().UnixNano())
	server := newDepositRPCServer(t, state)
	defer server.Close()
	initDepositValidationConfigWithToken(t, network, server.URL, depositAddress, config.TokenTypeERC20, tokenAddress.Hex())
	eventLog.Payload = fmt.Sprintf(`{"tx_hash":"%s","network":"%s"}`, state.txHash.Hex(), network)
	return validateDepositLog(context.Background(), eventLog)
}

func erc20TransferReceiptLog(txHash common.Hash, tokenAddress common.Address, fromAddress common.Address, toAddress common.Address, amount *big.Int) map[string]any {
	return map[string]any{
		"address":          tokenAddress.Hex(),
		"topics":           []string{erc20TransferTopic.Hex(), addressTopic(fromAddress.Hex()).Hex(), addressTopic(toAddress.Hex()).Hex()},
		"data":             fmt.Sprintf("0x%064x", amount),
		"blockNumber":      "0x10",
		"transactionHash":  txHash.Hex(),
		"transactionIndex": "0x0",
		"blockHash":        common.HexToHash("0x100").Hex(),
		"logIndex":         "0x0",
		"removed":          false,
	}
}

func TestValidateDepositLogAcceptsRawTransfer(t *testing.T) {
	txHash := common.HexToHash("0x1234")
	from := common.HexToAddress("0x1111111111111111111111111111111111111111")
	to := common.HexToAddress("0x2222222222222222222222222222222222222222")

	err := validateDepositForState(t, depositRPCState{
		txHash:        txHash,
		from:          from,
		to:            to,
		value:         big.NewInt(42),
		input:         "0x",
		receiptStatus: "0x1",
		blockNumber:   16,
		blockTime:     uint64(time.Now().Unix()),
	}, relay_api.TaskFeeLog{
		Address: from.Hex(),
		Amount:  "42",
		Type:    relay_api.TaskFeeLogTypeDeposit,
	})
	if err != nil {
		t.Fatalf("validateDepositLog failed: %v", err)
	}
}

func TestValidateDepositLogRejectsRawTransferMismatches(t *testing.T) {
	txHash := common.HexToHash("0x1234")
	from := common.HexToAddress("0x1111111111111111111111111111111111111111")
	to := common.HexToAddress("0x2222222222222222222222222222222222222222")

	tests := []struct {
		name    string
		state   depositRPCState
		log     relay_api.TaskFeeLog
		wantErr error
	}{
		{
			name: "wrong to",
			state: depositRPCState{
				txHash:        txHash,
				from:          from,
				to:            common.HexToAddress("0x3333333333333333333333333333333333333333"),
				value:         big.NewInt(42),
				input:         "0x",
				receiptStatus: "0x1",
				blockNumber:   16,
				blockTime:     uint64(time.Now().Unix()),
			},
			log:     relay_api.TaskFeeLog{Address: from.Hex(), Amount: "42", Type: relay_api.TaskFeeLogTypeDeposit},
			wantErr: ErrTaskFeeDepositTxMismatch,
		},
		{
			name: "wrong from",
			state: depositRPCState{
				txHash:        txHash,
				from:          from,
				to:            to,
				value:         big.NewInt(42),
				input:         "0x",
				receiptStatus: "0x1",
				blockNumber:   16,
				blockTime:     uint64(time.Now().Unix()),
			},
			log:     relay_api.TaskFeeLog{Address: common.HexToAddress("0x3333333333333333333333333333333333333333").Hex(), Amount: "42", Type: relay_api.TaskFeeLogTypeDeposit},
			wantErr: ErrTaskFeeDepositTxMismatch,
		},
		{
			name: "wrong value",
			state: depositRPCState{
				txHash:        txHash,
				from:          from,
				to:            to,
				value:         big.NewInt(42),
				input:         "0x",
				receiptStatus: "0x1",
				blockNumber:   16,
				blockTime:     uint64(time.Now().Unix()),
			},
			log:     relay_api.TaskFeeLog{Address: from.Hex(), Amount: "43", Type: relay_api.TaskFeeLogTypeDeposit},
			wantErr: ErrTaskFeeDepositTxMismatch,
		},
		{
			name: "failed receipt",
			state: depositRPCState{
				txHash:        txHash,
				from:          from,
				to:            to,
				value:         big.NewInt(42),
				input:         "0x",
				receiptStatus: "0x0",
				blockNumber:   16,
				blockTime:     uint64(time.Now().Unix()),
			},
			log:     relay_api.TaskFeeLog{Address: from.Hex(), Amount: "42", Type: relay_api.TaskFeeLogTypeDeposit},
			wantErr: ErrTaskFeeDepositTxMismatch,
		},
		{
			name: "non-empty input",
			state: depositRPCState{
				txHash:        txHash,
				from:          from,
				to:            to,
				value:         big.NewInt(42),
				input:         "0x1234",
				receiptStatus: "0x1",
				blockNumber:   16,
				blockTime:     uint64(time.Now().Unix()),
			},
			log:     relay_api.TaskFeeLog{Address: from.Hex(), Amount: "42", Type: relay_api.TaskFeeLogTypeDeposit},
			wantErr: ErrTaskFeeDepositTxMismatch,
		},
		{
			name: "too old",
			state: depositRPCState{
				txHash:        txHash,
				from:          from,
				to:            to,
				value:         big.NewInt(42),
				input:         "0x",
				receiptStatus: "0x1",
				blockNumber:   16,
				blockTime:     uint64(time.Now().Add(-2 * time.Hour).Unix()),
			},
			log:     relay_api.TaskFeeLog{Address: from.Hex(), Amount: "42", Type: relay_api.TaskFeeLogTypeDeposit},
			wantErr: ErrTaskFeeDepositTxTooOld,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			depositAddress := tt.state.to
			if tt.name == "wrong to" {
				depositAddress = to
			}
			err := validateDepositForStateWithDepositAddress(t, tt.state, tt.log, depositAddress)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("expected %v, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestValidateDepositLogAcceptsERC20TransferFromEventAddress(t *testing.T) {
	txHash := common.HexToHash("0x1234")
	eventAddress := common.HexToAddress("0x1111111111111111111111111111111111111111")
	sponsoredSender := common.HexToAddress("0x3333333333333333333333333333333333333333")
	depositAddress := common.HexToAddress("0x2222222222222222222222222222222222222222")
	tokenAddress := common.HexToAddress("0x4444444444444444444444444444444444444444")
	amount := big.NewInt(42)

	err := validateERC20DepositForState(t, depositRPCState{
		txHash:        txHash,
		from:          sponsoredSender,
		to:            tokenAddress,
		value:         big.NewInt(0),
		input:         "0x1234",
		receiptStatus: "0x1",
		blockNumber:   16,
		blockTime:     uint64(time.Now().Unix()),
		receiptLogs: []any{
			erc20TransferReceiptLog(txHash, tokenAddress, eventAddress, depositAddress, amount),
		},
	}, relay_api.TaskFeeLog{
		Address: eventAddress.Hex(),
		Amount:  amount.String(),
		Type:    relay_api.TaskFeeLogTypeDeposit,
	}, depositAddress, tokenAddress)
	if err != nil {
		t.Fatalf("validateDepositLog failed: %v", err)
	}
}

func TestValidateDepositLogRejectsERC20ZeroFromAddress(t *testing.T) {
	txHash := common.HexToHash("0x1234")
	depositAddress := common.HexToAddress("0x2222222222222222222222222222222222222222")
	tokenAddress := common.HexToAddress("0x4444444444444444444444444444444444444444")
	amount := big.NewInt(42)

	err := validateERC20DepositForState(t, depositRPCState{
		txHash:        txHash,
		from:          common.HexToAddress("0x3333333333333333333333333333333333333333"),
		to:            tokenAddress,
		value:         big.NewInt(0),
		input:         "0x1234",
		receiptStatus: "0x1",
		blockNumber:   16,
		blockTime:     uint64(time.Now().Unix()),
		receiptLogs: []any{
			erc20TransferReceiptLog(txHash, tokenAddress, common.Address{}, depositAddress, amount),
		},
	}, relay_api.TaskFeeLog{
		Address: common.Address{}.Hex(),
		Amount:  amount.String(),
		Type:    relay_api.TaskFeeLogTypeDeposit,
	}, depositAddress, tokenAddress)
	if !errors.Is(err, ErrTaskFeeDepositTxMismatch) {
		t.Fatalf("expected ErrTaskFeeDepositTxMismatch, got %v", err)
	}
}

func TestSaveDepositRecordsRejectsDuplicateTransactionHash(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(&models.DepositRecord{}); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	configDir := t.TempDir()
	configContent := `
environment: test
relay:
  deposit_address: 0x2222222222222222222222222222222222222222
tasks:
  process_withdrawal_requests:
    cancellation_settlement_timeout_seconds: 30
`
	if err := os.WriteFile(filepath.Join(configDir, "config.yml"), []byte(configContent), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := config.InitConfig(configDir); err != nil {
		t.Fatalf("init config: %v", err)
	}

	logs := []relay_api.TaskFeeLog{
		{
			ID:      1,
			Address: "0x1111111111111111111111111111111111111111",
			Amount:  "42",
			Type:    relay_api.TaskFeeLogTypeDeposit,
			Payload: `{"tx_hash":"0x0000000000000000000000000000000000000000000000000000000000001234","network":"arbitrum"}`,
		},
		{
			ID:      2,
			Address: "0x1111111111111111111111111111111111111111",
			Amount:  "42",
			Type:    relay_api.TaskFeeLogTypeDeposit,
			Payload: `{"tx_hash":"0x0000000000000000000000000000000000000000000000000000000000001234","network":"arbitrum"}`,
		},
	}
	err = saveDepositRecords(context.Background(), db, logs)
	if !errors.Is(err, ErrTaskFeeDepositTxHashDuplicate) {
		t.Fatalf("expected ErrTaskFeeDepositTxHashDuplicate, got %v", err)
	}
}
