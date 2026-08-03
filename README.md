# Crynux Relay Wallet

A minimal, security-first wallet service for processing withdrawals in the Crynux Relay.

Design goal: maximize protection of user funds even if the Relay is fully compromised.

In the current design, user funds remain safe under that assumption; at worst, a small amount of system-wallet funds may be lost before risk controls trigger and halt payouts during a slow log-inflation attack.

## Crynux Relay (context)

- The Relay dispatches AI tasks and credits task fees to node addresses.
- Balances are keyed by address, and only that address can request a withdrawal with a valid signature. There is no address binding or address-change operation.
- Task-fee distribution is off-chain: the Relay database updates each node address balance. A centralized system wallet is only used when paying out withdrawals.
- For safety, the system wallet holds only enough CNX to cover day-to-day payouts. Operators top up manually when needed. Node operators submit withdrawal requests; once validated, the system wallet sends funds to the requesting node address.

## Why a separate Relay Wallet

- The public Relay API runs on the internet; the system wallet private key must not live there.
- The Relay Wallet runs in a locked-down environment: no public IP, no inbound access, and a small allowlist of outbound destinations only.
- The system wallet private key is generated on the Relay Wallet host and backed up manually. It never leaves that machine.
- The Relay Wallet pulls pending withdrawals from the Relay over a one-way connection, performs local risk checks, signs the transaction, and broadcasts it.

## Network and API access

- The Relay exposes HTTPS APIs to prevent on-path tampering.
- The Relay Wallet uses an egress IP allowlist to reach only the Relay and a few blockchain nodes.
- For privacy, the Relay can require an API key so that withdrawal activity is not broadly visible. The data itself is public and not privileged.
- Relay persists and returns the original user withdrawal timestamp and signature. The Relay Wallet independently reconstructs the signed message and recovers the requesting address without applying a processing-time freshness check.
- The Relay Wallet stores a unique authorization fingerprint. Missing, malformed, mismatched, or replayed authorizations are rejected before any blockchain transaction is created.
- A persisted transaction hash or signed raw transaction is committed broadcast evidence. Such a withdrawal MUST converge from blockchain evidence and MUST NOT be rejected because its user authorization fields are missing or invalid.

## Withdrawals

- A minimum withdrawal amount is enforced.
- The Relay may deduct a fee to cover blockchain gas costs.
- The tokens will be transferred to the [beneficial address](./docs/beneficial-address.md) if set.

## Risk controls

Even with the system wallet key kept offline, an attacker who compromises the Relay could attempt to inject fake withdrawals or manipulate balances.

The Relay Wallet therefore maintains its own local per-address balance by reconstructing it from task-fee distribution logs streamed from the Relay in real time.

Why logs?
- We do not trust the Relay-reported balance, because a compromised Relay can arbitrarily rewrite that number without leaving reliable traces. We accept additive deltas only. The wallet processes logs strictly by its own cursor and persists the last processed position, so previously applied state cannot be overwritten.
- Each log represents a single AI task's fee, which is orders of magnitude smaller than a typical address balance. This enables precise safeguards such as per-log maximums and per-interval accrual ceilings, and makes bursts of new-address appearances detectable—without penalizing normal withdrawals.
- Attack surface reduction: to move large value, an attacker must generate many consistent small logs over time, which is easier to detect and can be halted early; a forged balance snapshot could misstate an entire account instantly.

Alerts:
  - If a single task-fee log amount is abnormally high, the wallet raises an alert and halts.
  - If new addresses appear at an unusually high rate, the wallet raises an alert and halts.
  - If the number of task-fee logs spikes abnormally within a short window, the wallet raises an alert and halts.
  - Insufficient balance in the system wallet.
  - Withdrawal amount exceeds locally recorded balance (potentially an attack).

## Notes (scope and exclusions)

1) Slow-drip "log inflation"
   - Scope: within the Relay alone, this cannot be conclusively decided because the Relay is the only task data source.
   - Production requirement: pair with an upstream authority (e.g., gateway/billing) that publishes periodic settlement snapshots and reconciles against Relay logs, with tolerance and delay windows; sustained deviations beyond thresholds should auto-fuse withdrawals and alert. This document focuses on Relay/Wallet internals and does not detail the external guard.

2) Daily system-wide cap
   - The aggregate daily payout limit is effectively enforced by limiting funds kept in the system wallet; no extra mechanism is added here.

3) Per-network withdrawal rate limits
   - Every configured blockchain carries its own `max_withdrawals_per_day`. This includes blockchains used by nodes and fund-only blockchains.
   - During request synchronization, the wallet independently enforces that network's limit for both the requester address and the destination benefit address. Counts are isolated by network, use the wallet's own record creation time within the current UTC day, and exclude failed and rejected withdrawals.
   - Wallet limit values must equal the Relay limits for the same funding network. A breach means the Relay-side limit was bypassed or the configurations diverge, so the wallet halts withdrawal request synchronization and alerts operators.

See System Architecture for details: [docs/architecture.md](./docs/architecture.md)
