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
- **LAC-SCHEDULE-1** — A desired change starts a fixed schedule, every step timed from the
  change itself rather than chained off the step before it:

  | at | step |
  | --- | --- |
  | +5s | refresh actual state |
  | +7s | re-send the command to the IR entity |
  | +12s | refresh actual state |
  | +14s | command the WiFi entity directly |

  The whole escalation is therefore over in a bounded 14 seconds, and no step can push the
  ones after it out.
- **LAC-CONFIRM-1** — Before every step, actual state is compared against desired, and the
  moment they match the sequence ends with all remaining steps abandoned. There is nothing
  left to escalate, and each remaining step costs either a poll or a dozen Zigbee round
  trips.
- **LAC-REFRESH-1** — A refresh tells the unit's own integration to poll now rather than on
  its own schedule, and does not command the unit. Each is followed by slack — enough for a
  round trip and the state to propagate — so the step after it decides on a current reading.
  Without them the reading at each decision is near-certainly older than the command being
  decided about, and the decision could only ever be made blind.
- **LAC-RESEND-1** — The re-send re-issues the same command to the IR entity. This is the
  second IR transmission; together with the first it takes a ~80% per-attempt hit rate to
  ~96%, and the refresh before it means the frame is spent only where the first was observed
  to miss.
- **LAC-FALLBACK-1** — The last step issues the equivalent command to the WiFi entity.
- **LAC-TERMINAL-1** — The WiFi fallback ends the sequence. Nothing is watched afterwards:
  the WiFi path is reliable and there is no further escalation available, so there is
  nothing a continued watch could do.
- **LAC-SUPERSEDE-1** — A desired change while a sequence is in flight abandons it and
  restarts the schedule from the top.
- **LAC-BOUND-1** — One desired change runs the schedule at most once, and at most one step
  fires per evaluation — a stalled input must not put two IR frames on the mesh in the same
  instant, which is the collision the spacing exists to avoid. No rate limiting is needed
  beyond that: the resend writes the same value, so SmartIR's state does not change, so
  nothing looks like a new command and no feedback loop exists.

## Matching

- **LAC-MATCH-1** — Modes match when equal.
- **LAC-MATCH-2** — Temperatures match within 0.5°. The IR entity steps in 0.5° and the
  WiFi entity in 1.0°, so an exact compare would never match a fractional setpoint and the
  worker would fall back on every half-degree command forever.
- **LAC-MATCH-3** — When desired mode is `off`, only the mode is compared; a setpoint is
  meaningless with the unit off.

**Known limitation.** A half-degree change is unverifiable in principle: a 1.0°-step entity
cannot distinguish 20.0 from 20.5, and 0.5 is exactly the tolerance. Such a command reads as
matched at the first comparison, so it gets one IR frame and no escalation — and if that
frame missed, the unit sits 0.5° from where it was asked to be, with the right mode. That is
below what anyone notices, and it does not latch: the next step is a full degree away from
what the unit reports, so it compares as a mismatch and escalates
normally. Demanding a *fresh* reading instead does not help — if the unit really did move
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
  plausible range) never starts a sequence.
- **LAC-VALID-2** — Unusable actual state neither confirms nor condemns: the IR steps still
  run, since telling the unit twice costs it nothing, but the WiFi fallback is skipped.
  With nothing trustworthy to compare against, powerctl must not take the unit over on the
  assumption that the IR missed.
- **LAC-VALID-3** — The SmartIR entity's topics are pre-seeded with sentinels that read as
  invalid. Subscribing is the same as requiring at startup, and statestream publishes on
  change without retaining; SmartIR's optimistic state changes only when someone commands
  the AC, so a quiet week would otherwise hold `statsWorker`'s gate shut and take the whole
  daemon — inverters included — with it. The `climate.lounge` topics need no seed: the
  Daikin reports `current_temperature` constantly, and the tile worker already depends on
  its state topic arriving.

## Logging

- **LAC-LOG-1** — Every change to either entity is logged, with the value it replaced. The
  IR entity is the only thing that starts a sequence and the WiFi entity the only thing that
  ends one early, so between them these explain every decision taken.
- **LAC-LOG-2** — Every outgoing command is logged: each refresh, the IR re-send, and the
  WiFi fallback.
- **LAC-LOG-3** — The two silent endings — confirmed, and gave up without falling back — are
  logged too. Otherwise a sequence that succeeded and one that never started look identical.

The point of this is being able to answer "why is the lounge the wrong temperature" from the
log alone, without a live mesh to poke at. It also makes the ~80% IR hit rate an observable
number rather than an assumption, since a miss shows up as a re-send with the unit still
reporting the old value.

## Worked workflows

- **Command lands first time** — Siri sets 21° cool. At +5s powerctl polls the unit, which
  reports cool/21°; at +7s that match ends the sequence, so no second IR frame is sent and
  it is over seven seconds after the command rather than a poll interval later.
  (LAC-REFRESH-1, LAC-CONFIRM-1)
- **First attempt misses, second lands** — Siri sets 18° cool. The +5s poll still reads
  heat/24°, so powerctl re-sends at +7s. The +12s poll comes back cool/18°: matched at +14s,
  no fallback. (LAC-RESEND-1, LAC-CONFIRM-1)
- **Both IR attempts miss** — Siri sets 18° cool; neither transfer completes. Both polls
  read heat/24°, so at +14s powerctl commands the WiFi entity to cool/18° and stops.
  (LAC-FALLBACK-1, LAC-TERMINAL-1)
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
