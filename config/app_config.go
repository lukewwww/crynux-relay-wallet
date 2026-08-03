package config

const (
	EnvProduction = "production"
	EnvDebug      = "debug"
	EnvTest       = "test"
)

const (
	TokenTypeNative = "native"
	TokenTypeERC20  = "erc20"
)

type AppConfig struct {
	Environment string `mapstructure:"environment"`

	Db struct {
		Driver           string `mapstructure:"driver"`
		ConnectionString string `mapstructure:"connection"`
		Log              struct {
			Level       string `mapstructure:"level"`
			Output      string `mapstructure:"output"`
			MaxFileSize int    `mapstructure:"max_file_size"`
			MaxDays     int    `mapstructure:"max_days"`
			MaxFileNum  int    `mapstructure:"max_file_num"`
		} `mapstructure:"log"`
	} `mapstructure:"db"`

	Log struct {
		Level            string `mapstructure:"level"`
		Output           string `mapstructure:"output"`
		MaxFileSize      int    `mapstructure:"max_file_size"`
		MaxDays          int    `mapstructure:"max_days"`
		MaxFileNum       int    `mapstructure:"max_file_num"`
		HeartbeatLogFile string `mapstructure:"heartbeat_log_file"`
		AlertLogFile     string `mapstructure:"alert_log_file"`
	} `mapstructure:"log"`

	Blockchains map[string]struct {
		RPS                   uint64 `mapstructure:"rps"`
		RpcEndpoint           string `mapstructure:"rpc_endpoint"`
		TokenType             string `mapstructure:"token_type"`
		TokenAddress          string `mapstructure:"token_address"`
		GasLimit              uint64 `mapstructure:"gas_limit"`
		GasLimitBufferPercent uint64 `mapstructure:"gas_limit_buffer_percent"`
		MaxFeePerGas          uint64 `mapstructure:"max_fee_per_gas"`
		MaxPriorityFeePerGas  uint64 `mapstructure:"max_priority_fee_per_gas"`
		ChainID               uint64 `mapstructure:"chain_id"`
		Account               struct {
			Address        string `mapstructure:"address"`
			PrivateKey     string `mapstructure:"private_key"`
			PrivateKeyFile string `mapstructure:"private_key_file"`
		} `mapstructure:"account"`
		Contracts struct {
			BenefitAddress string `mapstructure:"benefit_address"`
		} `mapstructure:"contracts"`
		MaxRetries                uint8  `mapstructure:"max_retries"`
		RetryInterval             uint64 `mapstructure:"retry_interval"`
		ReceiptWaitTime           uint64 `mapstructure:"receipt_wait_time"`
		ReceiptConfirmationBlocks uint64 `mapstructure:"receipt_confirmation_blocks"`
		SentTransactionCountLimit uint64 `mapstructure:"sent_transaction_count_limit"`
	} `mapstructure:"blockchains"`

	Relay struct {
		Api struct {
			Host           string `mapstructure:"host"`
			PrivateKey     string `mapstructure:"private_key"`
			PrivateKeyFile string `mapstructure:"private_key_file"`
		} `mapstructure:"api"`
		DepositAddress       string `mapstructure:"deposit_address"`
		WithdrawalFeeAddress string `mapstructure:"withdrawal_fee_address"`
		VestingSignerAddress string `mapstructure:"vesting_signer_address"`
	} `mapstructure:"relay"`

	Tasks struct {
		SyncTaskFeeLogs struct {
			IntervalSeconds            uint   `mapstructure:"interval_seconds"`
			BatchSize                  uint   `mapstructure:"batch_size"`
			MaxTaskFeeAmount           uint64 `mapstructure:"max_task_fee_amount"`
			MaxAddressLogsCountInBatch uint   `mapstructure:"max_address_logs_count_in_batch"`
			MaxNewAddressCountInBatch  uint   `mapstructure:"max_new_address_count_in_batch"`
			DepositMaxAgeSeconds       uint64 `mapstructure:"deposit_max_age_seconds"`
		} `mapstructure:"sync_task_fee_logs"`
		SyncWithdrawalRequests struct {
			IntervalSeconds                uint   `mapstructure:"interval_seconds"`
			BatchSize                      uint   `mapstructure:"batch_size"`
			MinWithdrawalAmount            uint64 `mapstructure:"min_withdrawal_amount"`
			MaxWithdrawalsPerAddressPerDay uint64 `mapstructure:"max_withdrawals_per_address_per_day"`
		} `mapstructure:"sync_withdrawal_requests"`
		ProcessWithdrawalRequests struct {
			IntervalSeconds                      uint   `mapstructure:"interval_seconds"`
			BatchSize                            uint   `mapstructure:"batch_size"`
			Timeout                              uint64 `mapstructure:"timeout"`
			CancellationSettlementTimeoutSeconds uint64 `mapstructure:"cancellation_settlement_timeout_seconds"`
		} `mapstructure:"process_withdrawal_requests"`
	} `mapstructure:"tasks"`
}
