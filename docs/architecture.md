## System Architecture

### Relay API

The Relay Wallet interacts with Relay by calling four API endpoints:

- GetTaskFeeLogs
- GetWithdrawalRequests
- FullfillWithdrawalRequest
- RejectWithdrawalRequest

The first two fetch data from Relay, while the latter two report the processing status of withdrawals back to Relay.

The Relay API client code resides in `/relay_api` as wrapper functions for these calls. This directory also defines the data structures for task fee logs and withdrawal requests.

### Task Fee Log Sync

Task fee log synchronization starts from `tasks.StartSyncTaskFeeLogs`, which is launched as a goroutine in `/main.go`. It runs continuously in the background, retrieving the latest task fee logs from the Relay API and updating the locally stored balances of node relay accounts based on those logs.

`GetTaskFeeLogs` supports incremental, gap-free syncing via a pivot ID. On startup, the Relay Wallet reads the last synced task fee log ID from the local database (stored in `models/system.go`). If the record is empty, no prior sync has occurred, so syncing begins at ID=0. Otherwise, the stored ID is passed to the API, which returns records starting from the next ID.

After retrieving records, the wallet updates, within a single database transaction, both the node relay account balances and the latest task fee log ID to ensure they are always consistent.
Vesting releases are accepted only when the corresponding local vesting record is active. The transition migration marks every existing active, non-deleted local vesting record as deprecated while preserving its released amount, schedule, signature, relay account balance, and task-fee checkpoint. A release for a completed or deprecated record fails validation, rolls back the entire fetched batch, and leaves the checkpoint unchanged. Vesting records created from later accepted `VestingCreated` logs remain active.

`DaoTaskShare` is limited to the DAO share of user-paid task fees. DAO token emission is not a Relay Account log type and is not processed by Wallet log synchronization.
Detailed type handling rules, validation requirements, checkpoint behavior, and withdrawal gate semantics are specified in [relay_account_log_processing.md](./relay_account_log_processing.md). Implementations MUST follow that specification.
Deposit-specific validation, deduplication, and fail-fast rules are specified in [deposit_processing.md](./deposit_processing.md). Implementations MUST follow that specification.

### Withdrawal Request Processing

Withdrawal handling uses a two-stage pipeline: `tasks.StartSyncWithdrawalRequests` pulls and stores gated requests from Relay, and `tasks.StartProcessWithdrawalRequests` executes on-chain transfers and reports fulfill or reject back to Relay.
Withdrawal execution is serial at the local withdrawal-record level: the wallet processes one unfinished withdrawal record through chain execution, Relay callback, and local finalization before starting the next record. Batch synchronization balance validation is an intake risk control. Immediately before creating each blockchain transaction, the processor MUST reload the local account balance and require coverage of `amount + withdrawal_fee`; this execution gate prevents requests accepted in different synchronization batches from reusing the same balance.
Relay supplies the original user timestamp and signature with every withdrawal. The wallet MUST independently recover the signer from the canonical Relay withdrawal message and MUST enforce a unique local authorization fingerprint. Invalid or replayed authorizations are stored as failed records so the synchronization checkpoint can advance and the existing Reject callback can settle them. Authorization validation applies only before blockchain commitment; any record with persisted broadcast evidence MUST converge from blockchain state and MUST NOT be refunded because authorization data is absent or invalid.
EVM withdrawal transactions are persisted locally before fee estimation, signing, or broadcast. The transaction sender atomically claims pending rows, signs under dynamic fee caps, persists the signed raw transaction as `broadcasting` before RPC submission, recovers unresolved broadcasts by rebroadcasting the same payload, and only then advances to `sent` for confirmation. The confirmer waits for a configured number of blocks after the receipt block before treating receipt status as final, including `0` for immediate finality at the receipt block, creates at most one retry from a confirmed receipt failure, and blocks retry broadcast when any ancestor lacks that proven failure.
Detailed withdrawal request synchronization, validation, execution, callback, timeout handling, and balance ownership rules are specified in [withdrawal_processing.md](./withdrawal_processing.md). Implementations MUST follow that specification.

### Other Components

#### DB Models

- Uses gorm v2 for database operations.
- All models are under `/models`.
- Database initialization and configuration are in `/config/db.go`.

#### Logs

- A global logger is configured. Initialization code is in `/config/log.go`.

#### Tasks

- The application starts multiple long-running, concurrently executing background tasks (for example, syncing task fee logs and processing withdrawals).
- Task code resides in `/tasks` and is started from `/main.go` as goroutines.
- Graceful shutdown is supported via OS signal handling (e.g., Ctrl+C). From `/main.go`, signals are captured and used to cancel contexts passed into tasks. Tasks listen to `ctx.Done()` and exit cleanly after finishing in-flight work (committing any pending operations before returning).
- Task exceptions are isolated: if a task encounters an unrecoverable error or panic, only that task exits and an alert is sent via the standard alerting path. Other tasks continue running and remain unaffected. Implemented in `/main.go`.

#### DB Migration

- Model changes are applied via migrations to keep different deployment environments in sync.
- Any model change must first add a migration under `/migrate`. See `/migrate/migrate.go` for the mechanism.
- On startup, `/main.go` automatically runs database migrations.
- Data migrations that intentionally have no business-data rollback leave migrated records unchanged during migration rollback. Restoring the pre-migration business state requires restoring a database backup.

#### Alert

- The project integrates with external services (such as AWS CloudWatch) to provide alerting. Related code is in `/alert`.
- To ensure the alerting path itself remains healthy and to avoid missing alerts due to bugs or unexpected shutdowns of the Relay Wallet, the system sends proactive heartbeats through the exact same alerting pathway (same service and same code). The external service is configured to trigger an alert if heartbeats are missing.
