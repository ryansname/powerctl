# Water Tanks Spec

Behavioural requirements for water tank level reporting and pump control. Implementation
lives in `src/tank_levels_worker.go` and `src/pump_control_worker.go`; this spec replaces
the retired Node-RED "Utilities" flows. Internal names (workers, channels, MQTT payload
shapes) are not part of the contract.

Tests reference tags via `// covers: <tag>` comments or test names that include the tag.

The HA-side pump automations are out of scope and unchanged: starting the pump timer makes
HA turn the pump on, and HA turns it off when the timer finishes — the timer remains the
failsafe that bounds every pump run.

## Tank levels

- **TANK-CALIB-1** — Tank fill percent is the linear interpolation of the sensed voltage between the user-calibrated empty (0%) and full (100%) voltages (`input_number.*_tank_{empty,full}_voltage`), rounded to 0.1.
- **TANK-SMOOTH-1** — The sensed voltage is a robust statistic over roughly the last 5 minutes; transient spikes and glitches (including negative readings) do not move the reported level.
- **TANK-HEADER-1** — The header tank percent is published unclamped (it may read above 100% or below 0% when the sensor drifts outside calibration).
- **TANK-STORAGE-1** — The storage overall percent is clamped to 0–100.
- **TANK-STORAGE-2** — Storage tank 1 percent = (overall − 66.6) × 3, clamped to 0–100.
- **TANK-STORAGE-3** — Storage tank 2 percent = (overall − 33.3) × 3, clamped to 0–100.
- **TANK-STORAGE-4** — Storage tank 3 percent = overall × 3, clamped to 0–100.
- **TANK-VALID-1** — When a tank sensor has produced no data (offline since startup), no level is published and the HA entities become unavailable; levels are never fabricated.
- **TANK-VALID-2** — A degenerate calibration (full voltage not meaningfully above empty voltage) makes that tank group invalid, as TANK-VALID-1.

## Pump control

- **PUMP-CHECK-1** — A daily start check fires at 11:00am local time (the first evaluation with valid tank data during the 11:00 hour), at most once per calendar day.
- **PUMP-CHECK-2** — The daily check happens only at 11:00am: if the system isn't running (or tank data is invalid) for the whole 11:00 hour, that day's check is skipped — there is no catch-up later in the day and no persistence across restarts.
- **PUMP-START-1** — At the daily check, outside flush mode: the pump starts when the header tank is below 75%.
- **PUMP-START-2** — At the daily check, in flush mode: the pump starts only when the header tank is below 15% (deep drain).
- **PUMP-START-3** — At any time of day: the pump starts when the header tank is below 5%.
- **PUMP-STOP-1** — At any time of day: when the pump is on and the header tank is at/above 90%, the pump is turned off.
- **PUMP-FLUSH-1** — Flush mode is purely date-derived: the first 14 days of every third month (January, April, July, October). It is exposed to HA as the `Tank Flush Mode` binary sensor.
- **PUMP-GATE-1** — "Start" means starting `timer.pump_time_remaining` for 03:00:00, and only when `switch.pump` is off; HA automations own turning the pump on/off from there.
- **PUMP-RATE-1** — Start and stop commands are rate-limited (no service-call spam while HA state propagates).
- **PUMP-INVALID-1** — Invalid or missing header tank data never triggers a start or a stop, and does not consume the daily check.

## Worked workflows

- **Normal day** — Header at 68% at 11:00, not flush: daily check starts the timer; HA runs the pump; at 90% powerctl turns the pump off; the 3 h timer would have stopped it anyway. (PUMP-CHECK-1, PUMP-START-1, PUMP-STOP-1, PUMP-GATE-1)
- **Flush deep-drain** — July 3rd, header at 60% at 11:00: no start (below-15% required). Days later within the fortnight, header hits 14% at 11:00: pump starts and refills to 90%. (PUMP-FLUSH-1, PUMP-START-2)
- **Critical low during flush** — Header drops to 4% at 20:00 on July 3rd: pump starts immediately despite flush mode. (PUMP-START-3)
- **Sensor outage** — ADC offline all morning; entities go unavailable; readings return at 13:00 — the day's check is skipped, and the below-5% floor remains the safety net. (TANK-VALID-1, PUMP-INVALID-1, PUMP-CHECK-2, PUMP-START-3)
