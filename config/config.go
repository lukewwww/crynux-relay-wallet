package config

import (
	"crypto/ecdsa"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/spf13/viper"
)

var appConfig *AppConfig

// InitConfig Init is an exported method that takes the config from the config file
// and unmarshal it into AppConfig struct
func InitConfig(configPath string) error {
	v := viper.New()
	v.SetConfigType("yml")
	v.SetConfigName("config")

	if configPath != "" {
		v.AddConfigPath(configPath)
	} else {
		v.AddConfigPath("/app/config")
		v.AddConfigPath("config")
	}

	if err := v.ReadInConfig(); err != nil {
		return err
	}

	appConfig = &AppConfig{}

	if err := v.Unmarshal(appConfig); err != nil {
		return err
	}
	if appConfig.Tasks.ProcessWithdrawalRequests.CancellationSettlementTimeoutSeconds == 0 {
		return errors.New("process withdrawal requests cancellation settlement timeout seconds not set")
	}

	if appConfig.Environment == EnvTest {
		privKey := GetTestPrivateKey()
		for network := range appConfig.Blockchains {
			blockchain := appConfig.Blockchains[network]
			blockchain.Account.PrivateKey = privKey
			appConfig.Blockchains[network] = blockchain
		}
		appConfig.Relay.Api.PrivateKey = privKey
	} else {
		loadedKeys, err := loadPrivateKeysFromFiles()
		if err != nil {
			return err
		}

		for network := range appConfig.Blockchains {
			blockchain := appConfig.Blockchains[network]
			blockchain.Account.PrivateKey = loadedKeys[blockchain.Account.PrivateKeyFile]
			appConfig.Blockchains[network] = blockchain
		}
		appConfig.Relay.Api.PrivateKey = loadedKeys[appConfig.Relay.Api.PrivateKeyFile]
	}
	if err := checkBlockchainAccount(); err != nil {
		return err
	}

	return nil
}

func checkBlockchainAccount() error {

	for network, blockchain := range appConfig.Blockchains {
		switch blockchain.TokenType {
		case TokenTypeNative:
			if blockchain.TokenAddress != "" {
				return errors.New("native blockchain token address must be empty")
			}
		case TokenTypeERC20:
			if !common.IsHexAddress(blockchain.TokenAddress) {
				return errors.New("erc20 blockchain token address is invalid")
			}
		default:
			return errors.New("blockchain token type is invalid")
		}
		if !common.IsHexAddress(blockchain.Contracts.BenefitAddress) {
			return errors.New("blockchain benefit address contract is invalid")
		}
		if blockchain.GasLimitBufferPercent == 0 {
			return fmt.Errorf("blockchain %s gas limit buffer percent not set", network)
		}
		if blockchain.ReceiptConfirmationBlocks == 0 {
			return fmt.Errorf("blockchain %s receipt confirmation blocks not set", network)
		}
		if blockchain.Account.PrivateKey == "" {
			return errors.New("blockchain account private key not set")
		}

		if blockchain.Account.Address == "" {
			return errors.New("blockchain account address not set")
		}

		// Check private key and address
		privateKey, err := crypto.HexToECDSA(blockchain.Account.PrivateKey)
		if err != nil {
			return err
		}

		publicKey := privateKey.Public()

		publicKeyECDSA, ok := publicKey.(*ecdsa.PublicKey)
		if !ok {
			return errors.New("error casting public key to ECDSA")
		}

		address := crypto.PubkeyToAddress(*publicKeyECDSA).Hex()

		if address != blockchain.Account.Address {
			return errors.New("account address and private key mismatch")
		}
	}

	return nil
}

func loadPrivateKeysFromFiles() (map[string]string, error) {
	fileSet := make(map[string]struct{})
	for network, blockchain := range appConfig.Blockchains {
		if blockchain.Account.PrivateKeyFile == "" {
			return nil, fmt.Errorf("blockchain %s account private key file not set", network)
		}
		fileSet[blockchain.Account.PrivateKeyFile] = struct{}{}
	}

	if appConfig.Relay.Api.PrivateKeyFile == "" {
		return nil, errors.New("relay api private key file not set")
	}
	fileSet[appConfig.Relay.Api.PrivateKeyFile] = struct{}{}

	privateKeys := make(map[string]string, len(fileSet))
	for file := range fileSet {
		privateKey, err := GetPrivateKey(file)
		if err != nil {
			return nil, err
		}
		privateKeys[file] = privateKey

		if err := os.Remove(file); err != nil {
			return nil, fmt.Errorf("remove private key file %s: %w", file, err)
		}
	}

	return privateKeys, nil
}

func GetPrivateKey(file string) (string, error) {
	if file == "" {
		return "", errors.New("private key file not set")
	}

	b, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("read private key file %s: %w", file, err)
	}
	return normalizePrivateKey(string(b)), nil
}

func normalizePrivateKey(privateKey string) string {
	privateKey = strings.TrimSpace(privateKey)
	if strings.HasPrefix(privateKey, "0x") || strings.HasPrefix(privateKey, "0X") {
		return privateKey[2:]
	}
	return privateKey
}

func GetTestPrivateKey() string {
	return ""
}

func GetConfig() *AppConfig {
	return appConfig
}
