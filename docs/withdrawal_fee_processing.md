# Withdrawal Fee Processing Specification

## Scope

This document defines how Relay Wallet handles withdrawal fees while processing Relay withdrawal requests and Relay account event logs.

## Fee Source

Relay is the authority for withdrawal fee amount. Relay computes the fee per request from its own configuration (a fixed fee plus an amount-tiered proportional fee) and fixes the value when the withdrawal request is created. Relay Wallet MUST read `withdrawal_fee` from each withdrawal request returned by Relay `GetWithdrawalRequests`.

Relay Wallet MUST NOT compute the fee amount from local configuration. Relay Wallet MUST NOT derive a withdrawal request fee amount from `WithdrawalFeeIncome` logs.

## Fee Receiver Configuration

Relay Wallet MUST define a local `withdrawal_fee_address` configuration value.

This configured address is the only accepted relay account destination for withdrawal fee income logs. The configuration is used for log integrity validation only. It MUST NOT be used to compute withdrawal fee amount.

## Withdrawal Request Handling

For each accepted withdrawal request:

1. `amount` is the on-chain payout amount sent to the user's benefit address.
2. `withdrawal_fee` is the relay account fee amount charged by Relay.
3. Wallet-side local balance validation MUST use `amount + withdrawal_fee`.
4. Wallet-side local balance debit after confirmed chain payout MUST use `amount + withdrawal_fee`.
5. On-chain payout MUST send only `amount`.

Relay Wallet MUST persist the withdrawal fee with the local withdrawal record so that retries, restarts, and final debit use the same value received from Relay.

## Relay Account Log Handling

Relay creates a `WithdrawalFeeIncome` relay account event after a withdrawal is fulfilled and the fee is non-zero.

When Relay Wallet processes relay account logs:

1. A `WithdrawalFeeIncome` log MUST increase the local relay account balance for the log address by the log amount.
2. The log address MUST equal the configured `withdrawal_fee_address`.
3. If the log address does not equal the configured `withdrawal_fee_address`, Relay Wallet MUST fail log validation and halt syncing through the standard integrity-error path.

Relay Wallet MUST NOT create a separate local fee transfer during withdrawal execution. Fee receiver balance is updated only through the accepted `WithdrawalFeeIncome` relay account log.

## Rejection Handling

If a withdrawal is rejected by Relay Wallet and Relay accepts the rejection, Relay refunds `amount + withdrawal_fee` to the requester through a `WithdrawRefund` relay account event.

Relay Wallet does not apply `WithdrawRefund` logs to local balance in log sync. Withdrawal balance ownership remains in the withdrawal execution flow as specified in [withdrawal_processing.md](./withdrawal_processing.md).

## Invariants

- `amount` MUST mean the user on-chain payout amount.
- `withdrawal_fee` MUST mean the relay account fee amount charged by Relay.
- The requester local balance debit MUST equal `amount + withdrawal_fee`.
- The on-chain payout MUST equal `amount`.
- The fee receiver local balance increase MUST come from a `WithdrawalFeeIncome` relay account log whose address equals configured `withdrawal_fee_address`.
