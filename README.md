# PrintFarming

Tools and experiments for a Bambu Lab print farm.

## Bambuddy Plate Guard

[`plate-guard/`](plate-guard/) contains a Go service that receives Bambuddy lifecycle webhooks, checks first-layer quality and completed build plates with `gpt-5.6-sol`, pauses only confirmed first-layer failures, restores AMS Filament Backup after first-layer review, runs a five-minute post-success cooling fan cycle, and releases the next queued print only after every clear-plate check passes.

See [`plate-guard/README.md`](plate-guard/README.md) for configuration, Bambuddy setup, safe commissioning, and systemd installation instructions.

## Other Files

- `farm_postprocess.py`: OrcaSlicer post-processing for the P1S ejection cycle
- `xl_start.gcode`: startup G-code used by the postprocessor
- `framework-counts.html`: local framework production counter

Test all printer motion changes without a part on the plate before using them unattended.
