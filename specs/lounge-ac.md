# Lounge AC Spec

Behavioural requirements for making lounge AC commands stick. Implementation lives in
`src/lounge_ac_worker.go`. Internal names (workers, channels, phase enums) are not part of
the contract.

Tests reference tags via `// covers: <tag>` comments or test names that include the tag.

## Background

Two HA entities drive one physical air conditioner:

- `climate.lounge_ac_ir` — SmartIR over a Zigbee IR blaster. Rich option set, and the
  entity users and Siri actually drive. **Desired state.**
- `climate.lounge` — the unit's own Daikin WiFi module. Fewer options and slow to update,
  but it is the only thing here that reports what the AC is *really* doing. **Actual
  state.** Its state topic is shared with the tile worker.

An IR command lands roughly 80% of the time, for two compounding reasons:

1. Sending one IR code is not one Zigbee message. The Zosung TS1201 protocol
   (`zhaquirks.tuya.ts1201`) chunks the payload at `0x38` bytes and transfers it over a
   request/response loop across clusters `0xE004`/`0xED00` — a dozen-odd round trips to a
   battery-powered sleepy end device at LQI 72. Losing any single message aborts the
   transfer with no IR emitted at all.
2. SmartIR is optimistic and has no feedback (its only hook, `power_sensor`, restores
   previously-assumed attributes rather than syncing real state). It sets its own state on
   the service call regardless of whether the IR arrived. So a miss desyncs HA from
   reality, and re-asking for the same value is deduplicated upstream — by HomeKit/Siri, or
   by HA itself — and emits nothing. The failure latches until someone picks a *different*
   value.

The escalation exists because of (2): a retry is worthless if the retry is deduped. Going
through the service call again works because **SmartIR re-transmits on every service call**,
same value or not.

## Escalation

- **LAC-TRIGGER-1** — The sequence runs only when *desired* state changes (the IR entity's
  hvac mode or target temperature). A change in actual state alone never triggers anything:
  someone using the physical remote must not be fought by powerctl re-asserting.
- **LAC-TRIGGER-2** — The first valid desired state seen after startup is recorded without
  acting, so restarting powerctl never re-commands the AC.
- **LAC-TRIGGER-3** — Changing the setpoint while the unit is off is not a command. There is
  nothing to send, and an IR frame per nudge of the dial is a dozen wasted Zigbee round
  trips on a lossy mesh.
- **LAC-RESEND-1** — Shortly after a desired change, the same command is re-issued once to
  the IR entity, unconditionally and without checking anything first. This is the second IR
  transmission; it is what takes a ~80% per-attempt hit rate to ~96%.
- **LAC-CONFIRM-1** — After the resend, actual state is compared against desired on *every*
  update, and the sequence succeeds the moment they match. There is no fixed settling wait.
- **LAC-CONFIRM-2** — If no match is seen before a timeout, the WiFi fallback fires. The
  timeout must comfortably exceed the WiFi entity's poll interval, or a command that did
  land gets overridden before it could ever be observed.
- **LAC-FALLBACK-1** — On timeout, the equivalent command is issued to the WiFi entity.
- **LAC-TERMINAL-1** — The WiFi fallback ends the sequence. Nothing is watched afterwards:
  the WiFi path is reliable and there is no further escalation available, so there is
  nothing a continued watch could do.
- **LAC-SUPERSEDE-1** — A desired change while a sequence is in flight abandons it and
  starts over from the resend.
- **LAC-BOUND-1** — One desired change yields at most one IR resend and one WiFi fallback.
  No rate limiting is needed to achieve this: the resend writes the same value, so SmartIR's
  state does not change, so nothing looks like a new command and no feedback loop exists.

## Matching

- **LAC-MATCH-1** — Modes match when equal.
- **LAC-MATCH-2** — Temperatures match within 0.5°. The IR entity steps in 0.5° and the
  WiFi entity in 1.0°, so an exact compare would never match a fractional setpoint and the
  worker would fall back on every half-degree command forever.
- **LAC-MATCH-3** — When desired mode is `off`, only the mode is compared; a setpoint is
  meaningless with the unit off.

**Known limitation.** A half-degree change is unverifiable in principle: a 1.0°-step entity
cannot distinguish 20.0 from 20.5, and 0.5 is exactly the tolerance. Such a command is
reported as matched immediately, so it gets the IR double-send but no confirmation and no
fallback. Demanding a *fresh* reading instead does not help — if the unit really did move
20.0 → 20.5 the WiFi entity still reports 20 and never republishes, so a freshness rule
would time out and fall back over a command that worked. The failures worth catching (wrong
mode, several degrees out) sit far outside tolerance and detect normally.

## Sending

- **LAC-SEND-1** — Fractional setpoints are **floored** when commanding the WiFi entity
  (20.5° → 20°): cooler is preferred. This stays consistent with LAC-MATCH-2, since a
  floored value is always within tolerance of the value it came from.
- **LAC-SEND-2** — The WiFi fallback sets mode and setpoint as two separate calls, where the
  IR resend uses one. Whether `set_temperature` honours an `hvac_mode` kwarg is the
  integration's choice, and a fallback that applied the setpoint but silently not the mode
  would be worse than no fallback. The IR path stays a single call for the opposite reason:
  SmartIR puts the whole state in one frame, and each frame is a dozen Zigbee round trips.

Modes are mirrored as-is, with no allowlist. HA's climate domain defines exactly seven hvac
modes and the Daikin reports at least `cool`/`heat`/`heat_cool` (see `acHvacActionStates`),
so a filter would reject nothing. The entities' differing option sets are in fan and swing,
which are not mirrored at all.

## Validity

- **LAC-VALID-1** — Missing or nonsensical desired state (no mode, or a setpoint outside a
  plausible range) never triggers a resend or a fallback.
- **LAC-VALID-2** — Unusable actual state means the confirmation is skipped rather than
  failed: with nothing trustworthy to compare against, powerctl must not fall back on the
  assumption that the IR missed.
- **LAC-VALID-3** — The SmartIR entity's topics are pre-seeded with sentinels that read as
  invalid. Subscribing is the same as requiring at startup, and statestream publishes on
  change without retaining; SmartIR's optimistic state changes only when someone commands
  the AC, so a quiet week would otherwise hold `statsWorker`'s gate shut and take the whole
  daemon — inverters included — with it. The `climate.lounge` topics need no seed: the
  Daikin reports `current_temperature` constantly, and the tile worker already depends on
  its state topic arriving.

## Worked workflows

- **Command lands first time** — Siri sets 21° cool. powerctl re-sends once. The WiFi entity
  reports cool/21° a minute later: matched immediately, sequence ends, no fallback and no
  waiting out the timeout. (LAC-RESEND-1, LAC-CONFIRM-1)
- **Both IR attempts miss** — Siri sets 18° cool; neither transfer completes. The WiFi
  entity keeps reading heat/24° until the timeout expires, so powerctl commands the WiFi
  entity to cool/18° and stops. (LAC-CONFIRM-2, LAC-FALLBACK-1, LAC-TERMINAL-1)
- **Latched desync, the case this exists for** — An earlier miss left HA showing cool/20°
  while the unit is really heat/24°. Asking Siri for cool/20° again is deduped and emits
  nothing, so powerctl sees no desired change and correctly does nothing. Setting 20.5°
  *is* a change: resend, confirm, and fall back if needed — which also resyncs reality.
  (LAC-TRIGGER-1)
- **Someone uses the physical remote** — The unit is turned to 24° by hand. Actual state
  changes, desired does not; powerctl stays out of it. (LAC-TRIGGER-1)
- **AC off overnight** — Desired mode is `off` with no setpoint; the comparison ignores
  temperature and matches on mode alone. (LAC-MATCH-3)
- **powerctl restart** — The IR entity has held cool/21° for hours. On startup that is
  recorded as the baseline, not treated as a new command, so nothing is sent.
  (LAC-TRIGGER-2)
