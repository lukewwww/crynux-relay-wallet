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
4. Validate ingestible requests with `checkWithdrawalRequests`.
5. Insert local `withdraw_records` with `OnConflict DoNothing`.
6. Update withdrawal checkpoint to the last ingested request in the same transaction.

## Validation Rules (`checkWithdrawalRequests`)

The wallet MUST enforce all rules below before storing a request:

- Amount MUST be parseable as integer and MUST be greater than or equal to configured minimum withdrawal amount.
- Request status MUST be `pending`.
- Every request address in the batch MUST already exist in local `relay_accounts`.
- Aggregated per-address withdrawal amount in the batch MUST NOT exceed local account balance.
- Benefit address fetched from chain (`GetBenefitAddress`) MUST equal request `benefit_address`.

System wallet token and gas balances MUST NOT be validated during request synchronization. Insufficient system wallet balance SHALL be handled during blockchain transaction sending.

Validation failure SHALL fail the sync attempt.

## Local Record Model and Status

The wallet stores each accepted request as `withdraw_records` with local status lifecycle:

- `pending` -> `success` or `failed` -> `finished`

`success` and `failed` represent local execution outcome before relay callback completion.
`finished` represents callback completion (`fulfill` or `reject`) and local finalization.

Each local withdrawal record MUST store the withdrawal fee reported by Relay. All wallet-side balance validation and debit rules MUST use `amount + withdrawal_fee`, because Relay charges the requester relay account by that same total amount when creating the `Withdraw` ledger event.

## Execution Flow (`processWithdrawalRecord`)

Withdrawal record processing MUST be serial. `StartProcessWithdrawalRequests` SHALL process at most one unfinished withdrawal record at a time, and it SHALL NOT start processing the next withdrawal record until the current record reaches `finished`.

This serialization boundary is the withdrawal record processor. It does not require the lower-level blockchain transaction manager to become a global serial sender. The transaction manager may keep its existing queue and confirmation behavior, but only one withdrawal record may be actively driven by `processWithdrawalRecord` at a time.

Serial withdrawal processing solves the wallet-local balance race where multiple withdrawal records can queue chain transfers before any one of them performs the final local balance debit. Because withdrawals are processed one record at a time, a confirmed withdrawal updates local balance before the next withdrawal can queue or monitor its transfer. This preserves the existing simple balance model without adding a separate reserved-balance state.

For the active unfinished local record:

1. If no blockchain transaction is attached, build the unsigned transaction payload for the target network.
2. Persist a `pending` blockchain transaction and store `blockchain_transaction_id` in the same database transaction. The wallet MUST persist this local transaction before any fee estimation, signing, or broadcast attempt.
3. The blockchain transaction sender atomically claims the persisted `pending` row by changing it to `sending`, estimates gas and fee caps, signs the transaction, broadcasts it, and marks it `sent` with `tx_hash` and `nonce` only after `eth_sendRawTransaction` succeeds.
4. Poll transaction status until terminal (`confirmed`, `failed`, or `cancelled`) or context cancellation.
5. If confirmed:
   - Load local relay account by record address.
   - Verify local balance is sufficient for `amount + withdrawal_fee`.
   - Decrease local balance by `amount + withdrawal_fee`.
   - Update record status to `success` in the same transaction.
6. If failed and retries are exhausted, or if cancelled before broadcast, update record status to `failed`.
7. After leaving pending loop:
   - If status is `success`, call Relay `FulfillWithdrawalRequest` with tx hash.
   - Otherwise call Relay `RejectWithdrawalRequest`.
8. Set local record status to `finished`.

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

The sender MUST use the persisted transaction state as the concurrency boundary. It MUST atomically change an unbroadcasted `pending` transaction to `sending` before gas estimation, signing, or broadcasting. A timeout handler MUST NOT cancel a transaction in `sending`. If sending fails before successful broadcast, the sender MUST return the transaction to `pending`. A transaction becomes `sent` only after broadcast succeeds and the wallet records the returned transaction hash and nonce.

This design deliberately keeps withdrawal execution globally serial. A withdrawal record delayed by dynamic fee caps blocks later withdrawal records, including records for other networks, until it succeeds or reaches timeout. If timeout occurs while its blockchain transaction is still unbroadcasted and cancellable, the wallet MUST cancel that transaction before rejecting the Relay withdrawal.

After a transaction is broadcast and a `tx_hash` is recorded, withdrawal timeout MUST NOT be used to reject or refund the Relay withdrawal. The withdrawal MUST remain bound to the chain transaction result.

When the timeout handler runs, including after a service restart, it MUST first inspect the persisted blockchain transaction state. This inspection is a recovery step for records whose chain transaction state changed outside the active withdrawal processor, such as a transaction confirmed after the processor had already stopped. If the transaction is already in a terminal local state, processing MUST resume through the normal success or failure finalization path. If the transaction is still broadcast and non-terminal at that moment, the processor MUST NOT keep waiting past the withdrawal deadline. It MUST log an error with the withdrawal record ID, remote ID, blockchain transaction ID, transaction status, and transaction hash, then return an error for the alerting path and stop processing later withdrawal records.

The wallet does not separately estimate or cap rollup parent-chain data fees through chain-specific fee oracle contracts. For supported EVM rollups, fee control is limited to standard gas estimation, buffered `gas_limit`, and EIP-1559 fee caps. Arbitrum Nitro-style gas estimates include the parent-chain posting buffer in the returned gas estimate. Base-style L1 security fee estimation is not a separate requirement in this wallet.

## Timeout Handling

Each record processing attempt SHALL run with a per-record deadline:

- deadline = `record.CreatedAt + ProcessWithdrawalRequests.Timeout`

If deadline is exceeded:

- If no blockchain transaction is attached, call Relay `RejectWithdrawalRequest` and set local status to `finished`.
- If the current blockchain transaction is `pending` and has no `tx_hash`, atomically change it to `cancelled`, call Relay `RejectWithdrawalRequest`, and set local status to `finished`.
- If the current blockchain transaction has been broadcast or is otherwise not cancellable and has not reached a terminal state at timeout handling time, do not reject the Relay withdrawal and do not continue waiting. Log the blocking transaction context, return timeout error for the alerting path, and stop processing later withdrawal records.
- If the current blockchain transaction reached a terminal local state before timeout handling, resume normal finalization from that terminal state. This covers recovery after processor interruption; it is not a continued wait past the withdrawal deadline.

## Balance Ownership Rule

Local account balance adjustment for withdrawals SHALL remain owned by withdrawal execution flow:

- Balance is decreased only after confirmed chain transfer.
- The decreased amount MUST be `amount + withdrawal_fee`.
- Withdrawal-related Relay account logs (`Withdraw`, `WithdrawRefund`) are not used to adjust local balance in log sync.
