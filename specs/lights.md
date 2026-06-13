# Lights Spec

Behavioural requirements for the lights automations. Implementation lives in
`src/lights_worker.go`; this spec replaces the retired Node-RED "Lights" flow.
Internal names (workers, channels, MQTT payload shapes) are not part of the
contract.

Tests reference tags via `// covers: <tag>` comments or test names that include
the tag. "Night" / "dark" means the HA `sun.sun` entity reports `below_horizon`.

## Outside light auto-off

- **LIGHT-OUT-1** — When the outside light turns on while it is dark, it is turned off again after 10 minutes if still on.
- **LIGHT-OUT-2** — When the outside light turns on while it is light (sun above horizon), it is turned off immediately.
- **LIGHT-OUT-3** — Auto-off is armed only by the off→on transition of the outside light: a light that is already on at startup is left alone, and a single on-period produces at most one auto-off.

## Kitchen follows mum's room

- **LIGHT-KIT-1** — During the night window, when mum's room light turns on and the kitchen light is currently off, the kitchen light is turned on.
- **LIGHT-KIT-2** — When mum's room light turns off, the kitchen light is turned off — but only if powerctl was the one that turned it on (LIGHT-KIT-1). A kitchen light switched on by someone else is never auto-turned-off.
- **LIGHT-KIT-3** — The night window is 19:30 local until sunrise. (Approximated as: local time ≥ 19:30, or it is still dark before noon. This is the one place the exact sunrise time is approximated because only sun above/below-horizon state is available.)
- **LIGHT-KIT-4** — Mum's room state at startup is not treated as a transition: only an actual off→on edge after startup turns the kitchen on.

## Garage mirrors outside

- **LIGHT-GAR-1** — The garage floodlight tracks the outside light: when outside turns on the garage turns on, when outside turns off the garage turns off.
- **LIGHT-GAR-2** — On startup the garage is synced once to match the outside light's current state.
- **LIGHT-GAR-3** — Garage commands are sent only on an outside-light change (no repeated commands while the state is steady).

## Sleep dim

- **LIGHT-DIM-1** — A "Sleep Ryan" trigger starts a 30-minute dim of Ryan's lights with a fixed end time (press time + 30 min). The target brightness is the linear ramp from 100% at the start to 0% at the end.
- **LIGHT-DIM-2** — The commanded brightness never exceeds the light's current level: if the light is already dimmer than the ramp, it is left untouched until the descending ramp reaches its level, then followed down. (Pressing never brightens the light.)
- **LIGHT-DIM-3** — At the end of the 30 minutes the light is turned off.
- **LIGHT-DIM-4** — The dim stops as soon as Ryan's lights are off (e.g. switched off manually).
- **LIGHT-DIM-5** — A new trigger while a dim is already running restarts it (the 30-minute ramp begins again from the new press time).
- **LIGHT-DIM-6** — If, mid-dim, the light is brightened (a meaningful upward change, not powerctl's own downward steps), the ramp is re-anchored so its target matches the new brightness and the dim continues down from there (the brightness is not pulled straight back down). A light taken back to full effectively gets a fresh ~30 minutes; a partial increase extends the dim proportionally.
- **LIGHT-DIM-7** — The trigger is a momentary button press: repeated presses are each honoured (the trigger is not a level/state).
