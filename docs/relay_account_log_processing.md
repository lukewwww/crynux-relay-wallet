# Relay Account Log Processing Specification

## Scope

This document defines how the Relay Wallet processes Relay Account logs and how withdrawal request ingestion is gated by log progress.

## Log Source and Schema

The wallet MUST fetch account logs from `/v1/relay_account/event_logs`.

Each log record MUST include:

- `id`
- `created_at`
- `address`
- `amount`
- `type`
- `payload`

For `Deposit`, payload MUST include `tx_hash` and `network`. For `VestingCreated`, payload MUST include the signed vesting schedule fields and signature. For `VestingRelease`, payload MUST include only `vesting_id`.

## Log Type Handling Rules

For local `relay_accounts.balance`, the wallet MUST apply each log type as follows:

- `TaskIncome` (`type=0`): increase balance (`+amount`)
- `DaoTaskShare` (`type=1`): increase balance (`+amount`)
- `WithdrawalFeeIncome` (`type=2`): increase balance (`+amount`)
- `Deposit` (`type=3`): increase balance (`+amount`)
- `TaskPayment` (`type=4`): decrease balance (`-amount`)
- `TaskRefund` (`type=5`): increase balance (`+amount`)
- `Withdraw` (`type=6`): ignore in log sync
- `WithdrawRefund` (`type=7`): ignore in log sync
- `UserDelegation` (`type=8`): increase balance (`+amount`)
- `VestingCreated` (`type=9`): no balance change, create/update local vesting schedule
- `VestingRelease` (`type=10`): increase balance (`+amount`) only after local schedule validation

Wallet MUST use an explicit event-type allowlist. Unknown event types MUST fail validation, halt sync, and trigger alert.
`DaoTaskShare` represents the DAO share of user-paid task fees. DAO token emission MUST NOT be represented as `DaoTaskShare` and MUST NOT enter Wallet log synchronization.

## Validation Rules Before Balance Apply

For each fetched batch, the wallet MUST validate:

- `amount` MUST be parseable as an integer string.
- For `Deposit`, payload MUST be valid JSON and include `tx_hash` and `network`.
- For `Deposit`, the wallet MUST independently verify the transaction on chain before balance apply:
  - transaction exists and is successful
  - native deposit transaction receiver equals configured relay deposit address
  - native deposit transaction sender equals log `address`
  - native deposit transaction value equals log `amount`
  - ERC20 deposit receipt contains a configured-token `Transfer` event to configured relay deposit address
  - ERC20 deposit `Transfer` sender equals log `address` and is not the zero address
  - ERC20 deposit `Transfer` amount equals log `amount`
- Per-log max amount threshold MUST be enforced for all balance-applied types except `Deposit` and `VestingRelease`.
- Per-address log count threshold MUST be enforced using only non-ignored logs.
- New-address count threshold MUST be enforced using only non-ignored logs.
- For `VestingCreated`, payload MUST include signed vesting schedule fields and signature.
- For `VestingCreated`, wallet MUST recover signer from payload signature and MUST match configured vesting signer address.
- For `VestingRelease`, payload MUST include `vesting_id`.
- For `VestingRelease`, wallet MUST require an existing local vesting record created from an earlier accepted `VestingCreated` event.
- For `VestingRelease`, the local vesting record MUST have `active` status. A `completed` or `deprecated` record MUST fail with the vesting release validation error.
- For `VestingRelease`, wallet MUST verify:
  - event `address` equals local vesting record address
  - `release_to = local released_amount + amount`
  - cap (`release_to <= local total_amount`)
  - schedule bound (`release_to <= should_released(created_at)` from local schedule fields).

If a batch contains only ignored log types (`Withdraw` and `WithdrawRefund`), validation SHALL pass and balance merge result SHALL be empty.
`VestingCreated` is also balance-ignored.

## Checkpoint Progress Rules

After a batch is accepted, the wallet MUST advance:

- `LatestTaskFeeLogID`
- `LatestTaskFeeLogTimestamp`

to the last fetched log in that batch, including batches where all logs are ignored for balance updates.

Balance changes, vesting changes, deposit records, and checkpoint changes for one fetched batch MUST commit in one database transaction. If any log fails validation, including a release for a deprecated vesting record, the entire batch MUST roll back and both checkpoint fields MUST remain unchanged. The synchronization task MUST return the existing validation error so the existing task alert path is triggered.

Local vesting deprecation is applied by database migration to vesting records that are active and not soft-deleted when the migration runs. The migration MUST preserve relay account balances, released amounts, vesting schedule fields, signatures, and task-fee checkpoints. `VestingCreated` logs accepted after the migration MUST continue to create active records.

## Withdrawal Request Gate Rule

Each withdrawal request includes `relay_account_event_id`.

The wallet SHALL ingest a request only when:

`relay_account_event_id <= LatestTaskFeeLogID`

This guarantees that account logs up to the withdrawal anchor event are already synchronized before local withdrawal processing starts.

## Withdrawal Balance Ownership Rule

The wallet SHALL keep withdrawal balance ownership in withdrawal execution flow:

- On confirmed chain transfer, local account balance is debited in withdrawal processing.
- On failed or rejected withdrawal, log sync does not apply withdrawal balance adjustment.
- Log sync MUST ignore `Withdraw` and `WithdrawRefund` to avoid double-application with wallet-side withdrawal accounting.

Complete withdrawal lifecycle behavior is specified in [withdrawal_processing.md](./withdrawal_processing.md). Implementations MUST follow that specification.

