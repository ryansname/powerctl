# Discharge Arbiter Spec

Behavioural requirements for the Powerwall 2 discharge control. Implementation lives in
`src/powerwall_discharge_worker.go`; this spec is intentionally vendor-aware (Tesla-only)
but implementation-agnostic — internal names (vote types, channels, propagation windows)
are not part of the contract.

Tests reference tags via `// covers: <tag>` comments or test names that include the tag.

## User control

- **DISCHARGE-USER-1** — User-facing entity `select.powerctl_pw2_discharge_mode` exposes the three options `Auto`, `Force On`, `Force Off`.
- **DISCHARGE-USER-2** — Default user mode is `Auto`.
- **DISCHARGE-USER-3** — `Force On` engages discharge regardless of any automation request.
- **DISCHARGE-USER-4** — `Force Off` blocks discharge regardless of any automation request.
- **DISCHARGE-USER-5** — `Auto` delegates the decision to active automation requests.

## Automation requests

- **DISCHARGE-AUTO-1** — A safety / veto request from any automation source wins over any discharge request from any other source.
- **DISCHARGE-AUTO-2** — Any active automation request for discharge engages discharge, unless a safety veto is also active.

## Reconciliation (Tesla operation mode)

- **DISCHARGE-RECON-1** — Tesla operation mode converges to the desired discharge state (`Time-Based Control` when discharging, `Self-Consumption` otherwise).
- **DISCHARGE-RECON-2** — No command spam to Tesla: redundant commands restating the current intent are suppressed.
- **DISCHARGE-RECON-3** — User-mode changes take effect promptly and are never lost, including during any in-flight command cooldown.
- **DISCHARGE-RECON-4** — Self-healing: if Tesla doesn't reach the desired state, the system retries until it does.
- **DISCHARGE-RECON-5** — While discharge is requested, the discharge tariff remains active continuously, even if Tesla resets the schedule periodically.

## Passive mode

- **DISCHARGE-PASSIVE-1** — When user mode is `Auto` and no automation request is active, manual changes made from the Tesla app or HA are respected and not reverted.
- **DISCHARGE-PASSIVE-2** — When the system transitions from active discharge to passive (`Auto` with no active requests), Tesla is returned to Self-Consumption and the baseline tariff is restored.

## Startup

- **DISCHARGE-INIT-1** — On startup, Tesla's tariff is reset to the baseline Octopus tariff so a previously-stuck discharge tariff cannot persist across restarts.

## Worked workflows

Sanity-check scenarios; each is satisfied by the tags listed.

- **Manual override during quiet period** — User in Auto with no automation active, opens Tesla app, switches to Time-Based Control manually. System leaves it alone. (DISCHARGE-PASSIVE-1)
- **Cleanup after Force On** — User picks Force On (discharging), then returns to Auto with no active automation. System returns Tesla to Self-Consumption + baseline tariff. (DISCHARGE-USER-3, DISCHARGE-PASSIVE-2)
- **Power-cut prep lifecycle** — Power-cut prep arms and SOC reaches 90%; system discharges. User disarms prep; system returns Tesla to Self-Consumption. (DISCHARGE-AUTO-2, DISCHARGE-PASSIVE-2)
- **Safety overrides discharge** — Power-cut prep active (wants discharge) but a battery-low automation vetoes. In Auto, no discharge. User picks Force On; discharge engages anyway. (DISCHARGE-AUTO-1, DISCHARGE-USER-3)
- **Rapid toggle resilience** — User flips Force On then Force Off within seconds while a Tesla command is still propagating. The Force Off takes effect. (DISCHARGE-RECON-3)
