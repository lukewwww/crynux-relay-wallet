package blockchain

import (
	"bytes"
	"context"
	"crynux_relay_wallet/blockchain/bindings"
	"crynux_relay_wallet/config"
	"crynux_relay_wallet/models"
	"database/sql"
	"errors"
	"math/big"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	log "github.com/sirupsen/logrus"
	"golang.org/x/time/rate"
	"gorm.io/gorm"
)

type BlockchainClient struct {
	Network                        string
	RpcEndpoint                    string
	RpcClient                      *ethclient.Client
	BenefitAddressContractInstance *bindings.BenefitAddress
	ChainID                        *big.Int
	GasLimit                       uint64
	GasLimitBufferPercent          uint64
	MaxFeePerGas                   *big.Int
	MaxPriorityFeePerGas           *big.Int
	Address                        string
	PrivateKey                     string
	Nonce                          *uint64
	NonceMu                        sync.Mutex
	Limiter                        *rate.Limiter
	SentTransactionCountLimit      uint64
}

var blockchainClients = make(map[string]*BlockchainClient)
var pattern *regexp.Regexp = regexp.MustCompile(`[Nn]once`)

var ErrBlockchainNotFound = errors.New("blockchain not found")

var erc20ABI = mustParseERC20ABI()

func mustParseERC20ABI() abi.ABI {
	parsed, err := abi.JSON(strings.NewReader(`[{"constant":true,"inputs":[{"name":"account","type":"address"}],"name":"balanceOf","outputs":[{"name":"","type":"uint256"}],"type":"function"},{"constant":false,"inputs":[{"name":"recipient","type":"address"},{"name":"amount","type":"uint256"}],"name":"transfer","outputs":[{"name":"","type":"bool"}],"type":"function"}]`))
	if err != nil {
		panic(err)
	}
	return parsed
}

type TransactionTransfer struct {
	Hash  common.Hash
	From  common.Address
	To    *common.Address
	Value *big.Int
	Input []byte
}

type rawTransactionTransfer struct {
	Hash  string `json:"hash"`
	From  string `json:"from"`
	To    string `json:"to"`
	Value string `json:"value"`
	Input string `json:"input"`
	Data  string `json:"data"`
}

func GetBlockchainClient(network string) (*BlockchainClient, error) {
	client, exists := blockchainClients[network]
	if !exists {
		return nil, ErrBlockchainNotFound
	}
	return client, nil
}

func initBlockchainClient(ctx context.Context, network string) error {
	appConfig := config.GetConfig()
	blockchain, exists := appConfig.Blockchains[network]
	if !exists {
		return ErrBlockchainNotFound
	}

	client, err := ethclient.Dial(blockchain.RpcEndpoint)
	if err != nil {
		return err
	}

	benefitAddressInstance, err := bindings.NewBenefitAddress(common.HexToAddress(blockchain.Contracts.BenefitAddress), client)
	if err != nil {
		return err
	}

	chainID, err := initChainID(ctx, client, blockchain.ChainID)
	if err != nil {
		return err
	}

	nonce, err := initNonce(ctx, client, blockchain.Account.Address)
	if err != nil {
		return err
	}

	limiter := rate.NewLimiter(rate.Limit(blockchain.RPS), int(blockchain.RPS))

	blockchainClients[network] = &BlockchainClient{
		Network:                        network,
		RpcEndpoint:                    blockchain.RpcEndpoint,
		RpcClient:                      client,
		BenefitAddressContractInstance: benefitAddressInstance,
		ChainID:                        chainID,
		GasLimit:                       blockchain.GasLimit,
		GasLimitBufferPercent:          blockchain.GasLimitBufferPercent,
		MaxFeePerGas:                   optionalUint64BigInt(blockchain.MaxFeePerGas),
		MaxPriorityFeePerGas:           optionalUint64BigInt(blockchain.MaxPriorityFeePerGas),
		Address:                        blockchain.Account.Address,
		PrivateKey:                     blockchain.Account.PrivateKey,
		Nonce:                          &nonce,
		NonceMu:                        sync.Mutex{},
		Limiter:                        limiter,
		SentTransactionCountLimit:      blockchain.SentTransactionCountLimit,
	}
	return nil
}

func Init(ctx context.Context) error {
	appConfig := config.GetConfig()
	for network := range appConfig.Blockchains {
		if err := initBlockchainClient(ctx, network); err != nil {
			return err
		}
	}
	return nil
}

func optionalUint64BigInt(value uint64) *big.Int {
	if value == 0 {
		return nil
	}
	return big.NewInt(0).SetUint64(value)
}

func initChainID(ctx context.Context, client *ethclient.Client, chainIDNum uint64) (*big.Int, error) {
	var chainID *big.Int
	if chainIDNum > 0 {
		chainID = big.NewInt(0).SetUint64(chainIDNum)
	} else {
		callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		id, err := client.ChainID(callCtx)
		if err != nil {
			return nil, err
		}
		chainID = id
	}
	return chainID, nil
}

func initNonce(ctx context.Context, client *ethclient.Client, address string) (uint64, error) {
	nonce, err := client.PendingNonceAt(ctx, common.HexToAddress(address))
	if err != nil {
		return 0, err
	}
	return nonce, nil
}

func (client *BlockchainClient) GetNonce(ctx context.Context) (uint64, error) {
	if client.Nonce == nil {
		nonce, err := initNonce(ctx, client.RpcClient, client.Address)
		if err != nil {
			return 0, err
		}
		client.Nonce = &nonce
	}
	return *client.Nonce, nil
}

func (client *BlockchainClient) IncrementNonce() {
	*client.Nonce++
}

func (client *BlockchainClient) AdvanceNoncePast(nonce int64) {
	next := uint64(nonce) + 1
	if client.Nonce == nil || *client.Nonce < next {
		client.Nonce = &next
	}
}

func matchNonceError(errStr string) bool {
	res := pattern.FindStringSubmatch(errStr)
	return res != nil
}

func isAlreadyKnownTransactionError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already known") ||
		strings.Contains(msg, "known transaction") ||
		strings.Contains(msg, "already imported")
}

func (client *BlockchainClient) processSendingTxError(err error) error {
	if ok := matchNonceError(err.Error()); ok {
		client.Nonce = nil
	}
	return err
}

func (client *BlockchainClient) SendSignedRawTransaction(ctx context.Context, signedRawTx string) (*types.Transaction, error) {
	raw, err := hexutil.Decode(signedRawTx)
	if err != nil {
		return nil, err
	}
	signedTx := new(types.Transaction)
	if err := signedTx.UnmarshalBinary(raw); err != nil {
		return nil, err
	}
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := client.Limiter.Wait(callCtx); err != nil {
		return nil, err
	}
	if err := client.RpcClient.SendTransaction(callCtx, signedTx); err != nil {
		return signedTx, client.processSendingTxError(err)
	}
	return signedTx, nil
}

func EncodeSignedRawTransaction(signedTx *types.Transaction) (string, error) {
	raw, err := signedTx.MarshalBinary()
	if err != nil {
		return "", err
	}
	return hexutil.Encode(raw), nil
}

func (client *BlockchainClient) resetNonce() {
	client.NonceMu.Lock()
	defer client.NonceMu.Unlock()
	client.Nonce = nil
}

func (client *BlockchainClient) BalanceAt(ctx context.Context, address common.Address) (*big.Int, error) {
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	return client.RpcClient.BalanceAt(callCtx, address, nil)
}

func (client *BlockchainClient) TokenBalanceAt(ctx context.Context, tokenAddress common.Address, address common.Address) (*big.Int, error) {
	data, err := erc20ABI.Pack("balanceOf", address)
	if err != nil {
		return nil, err
	}
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := client.Limiter.Wait(callCtx); err != nil {
		return nil, err
	}
	result, err := client.RpcClient.CallContract(callCtx, ethereum.CallMsg{
		To:   &tokenAddress,
		Data: data,
	}, nil)
	if err != nil {
		return nil, err
	}
	values, err := erc20ABI.Unpack("balanceOf", result)
	if err != nil {
		return nil, err
	}
	if len(values) != 1 {
		return nil, errors.New("invalid balanceOf result")
	}
	balance, ok := values[0].(*big.Int)
	if !ok {
		return nil, errors.New("invalid balanceOf value")
	}
	return balance, nil
}

func (client *BlockchainClient) WaitTxReceipt(ctx context.Context, txHash common.Hash) (*types.Receipt, error) {
	deadline, hasDeadline := ctx.Deadline()
	for {
		r, err := func() (*types.Receipt, error) {
			callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			return client.RpcClient.TransactionReceipt(callCtx, txHash)
		}()
		if err == ethereum.NotFound {
			time.Sleep(time.Second)
			continue
		}
		if hasDeadline && time.Now().Compare(deadline) >= 0 && err == context.DeadlineExceeded {
			log.Errorf("wait receipt of tx %s timeout", txHash.Hex())
			return nil, err
		}
		if err != nil {
			return nil, err
		}
		return r, nil
	}
}

func (client *BlockchainClient) SendETH(ctx context.Context, to common.Address, amount *big.Int) (*types.Transaction, error) {
	client.NonceMu.Lock()
	defer client.NonceMu.Unlock()

	nonce, err := client.GetNonce(ctx)
	if err != nil {
		return nil, err
	}

	transaction := &models.BlockchainTransaction{
		FromAddress: client.Address,
		ToAddress:   to.Hex(),
		Value:       amount.String(),
	}
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	gasLimit, err := estimateTransactionGasLimit(callCtx, client, transaction)
	if err != nil {
		return nil, err
	}
	gasFeeCap, gasTipCap, err := suggestDynamicFeeCaps(callCtx, client)
	if err != nil {
		return nil, err
	}
	tx, err := buildDynamicFeeTransaction(transaction, nonce, gasLimit, gasFeeCap, gasTipCap, client.ChainID)
	if err != nil {
		return nil, err
	}

	privateKey, err := crypto.HexToECDSA(client.PrivateKey)
	if err != nil {
		return nil, err
	}

	signedTx, err := types.SignTx(tx, types.LatestSignerForChainID(client.ChainID), privateKey)
	if err != nil {
		return nil, err
	}

	if err := client.Limiter.Wait(ctx); err != nil {
		return nil, err
	}

	err = client.RpcClient.SendTransaction(callCtx, signedTx)
	if err != nil {
		err = client.processSendingTxError(err)
		return nil, err
	}

	client.IncrementNonce()
	return signedTx, nil
}

func parseTransactionInput(input string, data string) ([]byte, error) {
	if input == "" {
		input = data
	}
	if input == "" {
		return nil, errors.New("transaction input field is empty")
	}
	return hexutil.Decode(input)
}

func parseRawTransactionTransfer(raw *rawTransactionTransfer, expectedHash common.Hash) (*TransactionTransfer, error) {
	if raw == nil {
		return nil, ethereum.NotFound
	}
	hash := common.HexToHash(raw.Hash)
	if raw.Hash == "" || hash != expectedHash {
		return nil, errors.New("transaction hash mismatch")
	}
	if !common.IsHexAddress(raw.From) {
		return nil, errors.New("transaction from address is invalid")
	}
	if !common.IsHexAddress(raw.To) {
		return nil, errors.New("transaction to address is invalid")
	}
	value, err := hexutil.DecodeBig(raw.Value)
	if err != nil {
		return nil, err
	}
	input, err := parseTransactionInput(raw.Input, raw.Data)
	if err != nil {
		return nil, err
	}
	to := common.HexToAddress(raw.To)
	return &TransactionTransfer{
		Hash:  hash,
		From:  common.HexToAddress(raw.From),
		To:    &to,
		Value: value,
		Input: input,
	}, nil
}

func (client *BlockchainClient) GetTransactionTransfer(ctx context.Context, txHash common.Hash) (*TransactionTransfer, error) {
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	rpcClient, err := rpc.DialContext(callCtx, client.RpcEndpoint)
	if err != nil {
		return nil, err
	}
	defer rpcClient.Close()

	var raw *rawTransactionTransfer
	if err := rpcClient.CallContext(callCtx, &raw, "eth_getTransactionByHash", txHash); err != nil {
		return nil, err
	}
	return parseRawTransactionTransfer(raw, txHash)
}

// QueueSendETH queues a send ETH transaction to be sent later
func QueueSendETH(ctx context.Context, db *gorm.DB, to common.Address, amount *big.Int, network string) (*models.BlockchainTransaction, error) {
	transaction, err := NewSendETHTransaction(to, amount, network)
	if err != nil {
		return nil, err
	}

	if err := transaction.Save(ctx, db); err != nil {
		return nil, err
	}

	return transaction, nil
}

func NewSendETHTransaction(to common.Address, amount *big.Int, network string) (*models.BlockchainTransaction, error) {
	appConfig := config.GetConfig()
	blockchain, ok := appConfig.Blockchains[network]
	if !ok {
		return nil, ErrBlockchainNotFound
	}

	transaction := &models.BlockchainTransaction{
		Network:     network,
		Type:        "SendETH",
		Status:      models.TransactionStatusPending,
		FromAddress: blockchain.Account.Address,
		ToAddress:   to.Hex(),
		Value:       amount.String(),
		MaxRetries:  blockchain.MaxRetries,
	}

	return transaction, nil
}

func QueueSendERC20(ctx context.Context, db *gorm.DB, tokenAddress common.Address, to common.Address, amount *big.Int, network string) (*models.BlockchainTransaction, error) {
	transaction, err := NewSendERC20Transaction(tokenAddress, to, amount, network)
	if err != nil {
		return nil, err
	}

	if err := transaction.Save(ctx, db); err != nil {
		return nil, err
	}

	return transaction, nil
}

func NewSendERC20Transaction(tokenAddress common.Address, to common.Address, amount *big.Int, network string) (*models.BlockchainTransaction, error) {
	appConfig := config.GetConfig()
	blockchain, ok := appConfig.Blockchains[network]
	if !ok {
		return nil, ErrBlockchainNotFound
	}
	data, err := erc20ABI.Pack("transfer", to, amount)
	if err != nil {
		return nil, err
	}

	transaction := &models.BlockchainTransaction{
		Network:     network,
		Type:        "ERC20::transfer",
		Status:      models.TransactionStatusPending,
		FromAddress: blockchain.Account.Address,
		ToAddress:   tokenAddress.Hex(),
		Value:       "0",
		Data: sql.NullString{
			String: hexutil.Encode(data),
			Valid:  true,
		},
		MaxRetries: blockchain.MaxRetries,
	}

	return transaction, nil
}

func parseTransactionData(data sql.NullString) ([]byte, error) {
	if !data.Valid || data.String == "" {
		return nil, nil
	}
	return hexutil.Decode(data.String)
}

func BuildCallMsgFromTransaction(transaction *models.BlockchainTransaction) (ethereum.CallMsg, error) {
	value, ok := new(big.Int).SetString(transaction.Value, 10)
	if !ok {
		return ethereum.CallMsg{}, errors.New("transaction value is invalid")
	}
	data, err := parseTransactionData(transaction.Data)
	if err != nil {
		return ethereum.CallMsg{}, err
	}
	if !common.IsHexAddress(transaction.FromAddress) {
		return ethereum.CallMsg{}, errors.New("transaction from address is invalid")
	}
	if !common.IsHexAddress(transaction.ToAddress) {
		return ethereum.CallMsg{}, errors.New("transaction to address is invalid")
	}
	to := common.HexToAddress(transaction.ToAddress)
	return ethereum.CallMsg{
		From:  common.HexToAddress(transaction.FromAddress),
		To:    &to,
		Value: value,
		Data:  data,
	}, nil
}

func (client *BlockchainClient) GetErrorMessageFromTransaction(ctx context.Context, transaction *models.BlockchainTransaction, receipt *types.Receipt) (string, error) {
	msg, err := BuildCallMsgFromTransaction(transaction)
	if err != nil {
		return "", err
	}

	blockNumber := big.NewInt(0).Sub(receipt.BlockNumber, big.NewInt(1))

	ctx2, cancel2 := context.WithTimeout(ctx, 30*time.Second)
	defer cancel2()

	res, err := client.RpcClient.CallContract(ctx2, msg, blockNumber)
	if err != nil {
		return "", err
	}

	errMsg, err := unpackError(res)
	if err != nil {
		errMsg = "Unknown tx error" + hexutil.Encode(res)
	}
	return errMsg, err
}

var (
	errorSig     = []byte{0x08, 0xc3, 0x79, 0xa0} // Keccak256("Error(string)")[:4]
	abiString, _ = abi.NewType("string", "", nil)
)

func unpackError(result []byte) (string, error) {
	if len(result) < 4 {
		return "", errors.New("tx result length too short")
	}
	if !bytes.Equal(result[:4], errorSig) {
		return "", errors.New("tx result not of type Error(string)")
	}

	vs, err := abi.Arguments{{Type: abiString}}.UnpackValues(result[4:])
	if err != nil {
		return "", err
	}

	return vs[0].(string), nil
}
