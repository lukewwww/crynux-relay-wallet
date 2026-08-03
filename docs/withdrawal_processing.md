# Withdrawal Processing Specification

## Scope

This document defines the end-to-end withdrawal handling flow in Relay Wallet, including request synchronization, validation, local persistence, blockchain execution, callback reporting, and timeout behavior.

Withdrawal fee amount source, fee receiver configuration, fee-income log validation, and payout/debit invariants are specified in [withdrawal_fee_processing.md](./withdrawal_fee_processing.md). Implementations MUST follow that specification.

## Entry Tasks

The wallet SHALL run two long-lived tasks for withdrawals:

- `StartSyncWithdrawalRequests`: pulls and stores requests from Relay.
- `StartProcessWithdrawalRequests`: executes locally stored requests and reports result.

## Request Synchronization Flow

`syncWithdrawalRequests` SHALL process withdrawals in batches using `LatestWithdrawalRequestID` as the pull cursor.

For each fetched batch:

1. Read current `LatestTaskFeeLogID` from task-fee checkpoint.
2. Apply gate rule:
   - A request is ingestible only if `relay_account_event_id <= LatestTaskFeeLogID`.
   - At first request that violates this rule, stop batch at that point.
3. If no ingestible requests remain, wait one sync interval and retry.
4. Reconstruct each request's canonical authorization message as `Crynux Relay\nAction: Withdraw <amount> from <address> to <benefit_address> on <network>\nAddress: <address>\nTimestamp: <timestamp>`, recover its signer with Ethereum personal-sign semantics, and require the recovered address to equal the request address. Processing delay MUST NOT be used as a timestamp freshness check.
5. Derive a SHA-256 authorization fingerprint from the canonical message and require uniqueness across local withdrawal records. Missing, malformed, signer-mismatched, and replayed authorizations MUST produce a local `failed` record with no blockchain transaction. Other request validation applies only to requests with valid, non-replayed authorization.
6. Insert local `withdraw_records` with `OnConflict DoNothing`.
7. Update withdrawal checkpoint to the last ingested request in the same transaction. Invalid authorization MUST NOT block checkpoint advancement.

## Validation Rules (`checkWithdrawalRequests`)

The wallet MUST enforce all rules below before storing a request:

- Amount MUST be parseable as integer and MUST be greater than or equal to configured minimum withdrawal amount.
- Request status MUST be `pending`.
- Every request address in the batch MUST already exist in local `relay_accounts`.
- Aggregated per-address withdrawal amount in the batch MUST NOT exceed local account balance.
- Accepting the batch MUST NOT push any address over the configured `max_withdrawals_per_address_per_day` count for the current UTC day. The count is the number of local `withdraw_records` for the address created in the current UTC day, measured by the wallet's own record creation time, excluding records with status `failed` or `finished-rejected` and excluding records whose remote ID is part of the current batch, plus the number of requests for the address in the current batch. Relay-reported request timestamps MUST NOT be used for this count.
- Benefit address fetched from chain (`GetBenefitAddress`) MUST equal request `benefit_address`.

System wallet token and gas balances MUST NOT be validated during request synchronization. Insufficient system wallet balance SHALL be handled during blockchain transaction sending.

Validation failure other than invalid authorization SHALL fail the sync attempt.

## Local Record Model and Status

The wallet stores each accepted request as `withdraw_records` with local status lifecycle:

- `pending` -> `success` -> `finished`
- `pending` -> `failed` -> `finished-rejected`

`success` and `failed` represent local execution outcome before relay callback completion.
`finished` represents `fulfill` callback completion and local finalization.
`finished-rejected` represents `reject` callback completion and local finalization. The two terminal statuses MUST remain distinct so that rejected withdrawals can be excluded from the per-address daily withdrawal count.

Each local withdrawal record MUST store the withdrawal fee reported by Relay. All wallet-side balance validation and debit rules MUST use `amount + withdrawal_fee`, because Relay charges the requester relay account by that same total amount when creating the `Withdraw` ledger event.
Each local withdrawal record MUST also store the Relay-supplied timestamp and signature. A cryptographically valid authorization MUST store its unique authorization fingerprint. Historical rows remain nullable; a historical row without committed broadcast evidence and without valid authorization MUST follow `failed` -> `finished-rejected`.

## Execution Flow (`processWithdrawalRecord`)

Withdrawal record processing MUST be serial. `StartProcessWithdrawalRequests` SHALL process at most one unfinished withdrawal record at a time, and it SHALL NOT start processing the next withdrawal record until the current record reaches `finished`.

This serialization boundary is the withdrawal record processor. It does not require the lower-level blockchain transaction manager to become a global serial sender. The transaction manager may keep its existing queue and confirmation behavior, but only one withdrawal record may be actively driven by `processWithdrawalRecord` at a time.

Serial withdrawal processing and the execution-time balance gate jointly protect the wallet-local balance. Synchronization can accept requests in different batches before an earlier transfer is confirmed. The processor MUST therefore reload the address balance immediately before transaction creation and MUST NOT rely on synchronization-time validation or serialization alone.

For the active unfinished local record:

1. If no blockchain transaction is attached, validate the stored authorization and unique fingerprint. Invalid authorization MUST set the record to `failed`.
2. Reload the local relay account and require its balance to cover `amount + withdrawal_fee`. Insufficient balance MUST set the record to `failed`; the wallet MUST NOT create, sign, or broadcast a blockchain transaction.
3. Build the unsigned transaction payload for the target network.
4. Persist a `pending` blockchain transaction and store `blockchain_transaction_id` in the same database transaction. The wallet MUST persist this local transaction before any fee estimation, signing, or broadcast attempt.
5. The blockchain transaction sender atomically claims the persisted `pending` row by changing it to `sending`, estimates gas and fee caps, signs the transaction, persists `broadcasting` with `tx_hash`/`nonce`/`signed_raw_tx` before RPC submission, recovers by rebroadcasting that exact payload when needed, and marks it `sent` after acknowledgment or on-chain visibility.
6. Poll transaction status until terminal (`confirmed`, `failed`, or `cancelled`) or context cancellation.
7. If confirmed:
   - Verify the root transaction and its retry chain contain exactly one confirmed transfer and that every other attempt has proven terminal failure.
   - Load local relay account by record address.
   - Verify local balance is sufficient for `amount + withdrawal_fee`.
   - Decrease local balance by `amount + withdrawal_fee`.
   - Update record status to `success` in the same transaction.
8. If failed and retries are exhausted, or if cancelled before broadcast, update record status to `failed`.
9. After leaving pending loop:
   - If status is `success`, verify the same single-confirmed-chain invariant again, call Relay `FulfillWithdrawalRequest` with tx hash, and set local record status to `finished`.
   - Otherwise, only after the root transaction and retry chain have on-chain failure proof or an unbroadcasted cancellation, call Relay `RejectWithdrawalRequest` and set local record status to `finished-rejected`.

When processing starts from a persisted `success` record after restart, the processor MUST reload the current transaction from `blockchain_transaction_id`, including the latest retry-chain member, and run the single-confirmed safety gate before Fulfill. It MUST NOT debit the local balance again.

Reject and Fulfill MUST use the same complete-chain safety gate. A cancelled current retry MUST NOT bypass inspection of timeout-failed or otherwise uncertain ancestors. Any confirmed ancestor MUST block Reject. Any historical receipt-timeout failure, `broadcasting` row, `sent` row, or other unproven broadcast outcome MUST block Reject and Fulfill.
Committed broadcast evidence has priority over authorization validation. Once any transaction in the attached chain has `tx_hash`, `signed_raw_tx`, `broadcasting`, or `sent` state, missing or invalid authorization MUST NOT cause Reject or refund.
## Dynamic Fee Estimation and Sending

Withdrawal blockchain transactions on EVM-compatible networks SHALL use EIP-1559 dynamic fee transactions (`DynamicFeeTx`). The wallet does not use a configured legacy `gas_price` for withdrawal execution.

The blockchain configuration fields for withdrawal gas control are:

- `gas_limit`: maximum allowed gas limit after estimation and buffer.
- `gas_limit_buffer_percent`: required non-zero percentage buffer added to `eth_estimateGas` before comparing with `gas_limit` and sending the transaction.
- `max_fee_per_gas`: maximum allowed EIP-1559 `maxFeePerGas` in wei. A value of `0` means no wallet-side cap.
- `max_priority_fee_per_gas`: maximum allowed EIP-1559 `maxPriorityFeePerGas` in wei. A value of `0` means no wallet-side cap.

The sender MUST validate hot wallet payout balance before gas estimation:

- For native-token withdrawals, the hot wallet native token balance MUST cover the withdrawal amount.
- For ERC20 withdrawals, the hot wallet ERC20 token balance MUST cover the withdrawal amount.

The sender MUST estimate dynamic fee transaction parameters against the target network after it claims a pending blockchain transaction and validates payout balance:

1. Build the transaction call message from the pending withdrawal payload.
2. Call `eth_estimateGas`.
3. Add the configured gas limit buffer to the estimate.
4. Fail sending if the buffered gas limit exceeds configured `gas_limit`.
5. Read the latest block base fee.
6. Read the suggested priority fee.
7. Compute `maxFeePerGas = latestBaseFee * 2 + suggestedPriorityFee`.
8. Fail sending if `maxFeePerGas` exceeds configured `max_fee_per_gas` when that cap is non-zero.
9. Fail sending if `suggestedPriorityFee` exceeds configured `max_priority_fee_per_gas` when that cap is non-zero.

After fee estimation, the sender MUST validate hot wallet native token balance for gas. For native-token withdrawals, the required native balance MUST cover `withdrawal amount + gasLimit * maxFeePerGas`. For ERC20 withdrawals, the required native balance MUST cover `gasLimit * maxFeePerGas`.

If hot wallet native payout balance, ERC20 payout balance, or native gas balance is insufficient, the sender MUST return a distinct error for that shortage, log the error, send an operator alert through the standard alert path, and release the unbroadcasted transaction back to `pending`.

Blockchain transaction persistence is a local queueing step, not proof of network submission. If estimation fails or exceeds configured caps before broadcast, the sender MUST release the transaction back to `pending` without `tx_hash`, and the transaction remains eligible for a later send attempt.

The sender MUST use the persisted transaction state as the concurrency boundary. It MUST atomically change an unbroadcasted `pending` transaction with no cancellation request to `sending` before gas estimation or signing. A transaction with `cancellation_requested_at` set MUST NOT be claimed for sending.

After the sender signs a transaction and before it calls `eth_sendRawTransaction`, it MUST atomically prepare the broadcast by changing `sending` to `broadcasting` and persisting `tx_hash`, `nonce`, and `signed_raw_tx` in the same conditional update. That update MUST require `cancellation_requested_at IS NULL`. If cancellation was requested first, prepare MUST fail, the sender MUST release the unbroadcasted `sending` row, and the transaction MUST NOT be broadcast.

A `broadcasting` transaction is a committed broadcast attempt. It MUST NOT be released back to `pending`, MUST NOT accept cancellation, and MUST NOT cause a Relay reject or refund. The sender MUST recover `broadcasting` rows on startup and on every poll by querying the persisted hash and, when the transaction is not yet visible, rebroadcasting the exact persisted `signed_raw_tx`. The sender MUST change `broadcasting` to `sent` only after the RPC accepts the raw transaction, returns an already-known acknowledgment, or the chain makes the hash visible. The same network MUST NOT claim a new pending payout while any `broadcasting` row remains unresolved.

Before starting sender or confirmer workers, the transaction manager MUST synchronously recover legacy `sending` rows that have neither `tx_hash` nor `signed_raw_tx`. A row without a cancellation request MUST return to `pending`; a row with a cancellation request MUST become `cancelled`. `broadcasting`, `sent`, and any `sending` row with committed broadcast evidence MUST remain unchanged. Recovery failure MUST abort transaction-manager startup.

Timeout cancellation and sender release MUST use a persisted cancellation handshake. The timeout handler MUST set `cancellation_requested_at` and the cancellation reason exactly once on an unbroadcasted `pending` or `sending` transaction. A repeated cancellation request MUST preserve the original timestamp and reason. If sending fails before broadcast preparation, the sender MUST atomically release `sending` to `cancelled` when cancellation has been requested, or to `pending` otherwise. If release reaches `pending` before the cancellation request obtains the row lock, the cancellation request MUST make that row ineligible for another sender claim and the timeout handler MUST then change it to `cancelled`.

Successful broadcast preparation has priority over concurrent cancellation. Once `tx_hash` and `signed_raw_tx` are persisted, the transaction MUST proceed to `sent`/`confirmed`/`failed` from chain evidence and MUST NOT be rejected or refunded because of a concurrent cancellation request.

The confirmer MUST query the receipt before applying any receipt-wait deadline. After a receipt is found, the confirmer MUST query the latest block and MUST wait until `latest_block >= receipt.block_number + receipt_confirmation_blocks` before acting on that receipt. `receipt_confirmation_blocks` is a required per-network configuration value that counts blocks after the receipt block. While waiting for that depth, the transaction MUST remain `sent`. If a later poll finds the receipt missing or changed after a reorg, the confirmer MUST continue from the latest chain evidence and MUST NOT apply an earlier provisional receipt outcome.

Receipt `status=1` with the required confirmation depth MUST mark the transaction confirmed through a conditional `sent -> confirmed` update. Receipt `status=0` with the required confirmation depth is the only automatic on-chain failure proof that MAY create one new-nonce retry. That failure transition MUST be a conditional `sent -> failed` update, and only the first successful transition MAY create a retry. The sender MUST refuse to claim a retry while any ancestor lacks that proven on-chain failure. If the receipt is still missing after `receipt_wait_time`, the confirmer MUST keep the transaction in `sent`, persist a delayed-receipt status message at most once, alert operators, and continue polling. Receipt delay MUST NOT mark the transaction failed and MUST NOT create a retry.

This design deliberately keeps withdrawal execution globally serial. A withdrawal record delayed by dynamic fee caps blocks later withdrawal records, including records for other networks, until it succeeds or reaches timeout. If timeout occurs while its blockchain transaction is still unbroadcasted and cancellable, the wallet MUST cancel that transaction before rejecting the Relay withdrawal.

After a transaction has a persisted `tx_hash` or `signed_raw_tx`, withdrawal timeout MUST NOT be used to reject or refund the Relay withdrawal. The withdrawal MUST remain bound to the chain transaction result. Before any Relay reject for a withdrawal with an attached blockchain transaction, including timeout cancellation of an unbroadcasted current row, the processor MUST inspect the root transaction and its retry chain. Proven terminal failure means an unbroadcasted `cancelled` row or a receipt `status=0` failure that reached the configured confirmation depth. If any row is `broadcasting`, `sent`, failed only because of a historical receipt timeout, confirmed, or otherwise lacks that proven failure, the processor MUST fail-stop without rejecting.

When the timeout handler runs, including after a service restart, it MUST inspect the persisted blockchain transaction state once per processing-loop iteration. This inspection is a recovery step for records whose chain transaction state changed outside the active withdrawal processor, such as a transaction confirmed after the processor had already stopped. If the transaction is already in a terminal local state, processing MUST resume through the normal success or failure finalization path. If the transaction is broadcast and non-terminal, the processor MUST log an error with the withdrawal record ID, remote ID, blockchain transaction ID, transaction status, transaction hash, and cancellation request timestamp, then return an error for the alerting path and stop processing later withdrawal records.

The wallet does not separately estimate or cap rollup parent-chain data fees through chain-specific fee oracle contracts. For supported EVM rollups, fee control is limited to standard gas estimation, buffered `gas_limit`, and EIP-1559 fee caps. Arbitrum Nitro-style gas estimates include the parent-chain posting buffer in the returned gas estimate. Base-style L1 security fee estimation is not a separate requirement in this wallet.

## Timeout Handling

Each record processing attempt SHALL run with a per-record deadline:

- deadline = `record.CreatedAt + ProcessWithdrawalRequests.Timeout`

If deadline is exceeded:

- If no blockchain transaction is attached, call Relay `RejectWithdrawalRequest` and set local status to `finished-rejected`.
- If the current blockchain transaction is unbroadcasted `pending`, persist its cancellation request, atomically change it to `cancelled`, call Relay `RejectWithdrawalRequest`, and set local status to `finished-rejected`.
- If the current blockchain transaction is unbroadcasted `sending`, persist its cancellation request and return to the existing per-record processing loop while the sender settles the transaction. The settlement deadline MUST equal the persisted `cancellation_requested_at + cancellation_settlement_timeout_seconds`. Process restart and repeated timeout handling MUST NOT move this deadline.
- If sender release changes the requested transaction to `cancelled`, resume normal rejected finalization. If sender broadcast preparation persists `tx_hash`/`signed_raw_tx` and later reaches `sent`, do not reject the Relay withdrawal.
- If an unbroadcasted `sending` transaction remains unsettled at the cancellation settlement deadline, do not reject the Relay withdrawal. Log the blocking transaction context, return timeout error for the alerting path, and stop processing later withdrawal records.
- If the current blockchain transaction is `broadcasting`, has any persisted broadcast payload, or is otherwise neither cancellable nor terminal at timeout handling time, do not reject the Relay withdrawal. Log the blocking transaction context, return timeout error for the alerting path, and stop processing later withdrawal records.
- If the current blockchain transaction reached a terminal local state before timeout handling, resume normal finalization from that terminal state. This covers recovery after processor interruption; it is not a continued wait past the withdrawal deadline.

## Balance Ownership Rule

Local account balance adjustment for withdrawals SHALL remain owned by withdrawal execution flow:

- Balance is decreased only after confirmed chain transfer.
- The decreased amount MUST be `amount + withdrawal_fee`.
- Current balance MUST be checked again immediately before blockchain transaction creation. This check is the execution gate for requests accepted in different synchronization batches.
- Withdrawal-related Relay account logs (`Withdraw`, `WithdrawRefund`) are not used to adjust local balance in log sync.
