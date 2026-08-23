#!/usr/bin/env python3
"""
OrcaSlicer post-processing script for a Bambu Lab P1S print-farm eject cycle.

What it does:
Before the print, it rotates the XL startup purge line by 2 degrees and moves
it rearward until its extrusion edge slightly overlaps the first-layer part.

After the normal P1S end G-code it:
  1. Removes Bambu's default final bed-lowering/current-reduction block.
  2. Turns the bed heater off and waits for the bed to cool.
  3. Moves the toolhead to X center / rear Y before large Z moves.
  4. Cycles the bed from 20 mm above bottom to 40 mm higher, 2 times.
  5. Raises the bed to a push height derived from the actual print height.
  6. Pushes the loosened part frontward in three tight X tracks: center, left, then right.
  7. Lowers the bed below the part before every fast rearward reposition.
  8. Leaves the bed at Z125, 3 mm physically above the previous Z128 position.

The rest of the startup G-code and adaptive leveling region are preserved.

IMPORTANT:
  - This assumes the user's mechanical bed-edge flexing extension is installed.
  - Dry-run this with NO PART on the plate first.
  - Verify rear/front Y coordinates and clearances on the specific machine.
  - The script intentionally refuses to modify a file if it cannot infer print height.

Usage with OrcaSlicer (which appends the generated path):
    python3 /Users/jeremyjacob/Desktop/farm_postprocess.py

The previous command ending in ``1`` remains accepted for compatibility with
an existing OrcaSlicer post-process setting; the number is ignored.
"""

from __future__ import annotations

import re
import sys
from math import atan2, ceil, cos, hypot, pi, radians, sin, sqrt, tan
from pathlib import Path

# -------------------------- USER SETTINGS --------------------------

COOL_TEMP_C = 44.0

# Some P1S firmware exits an M190 R cooling wait near 45 C when the temperature
# decline becomes too slow. Keep exhausting the chamber for three more minutes.
COOL_FALLBACK_DWELL_SECONDS = 180

# P1S nominal Z travel / build height.
Z_BOTTOM_MM = 256.0

# Stop the flex cycle 20 mm above physical bottom.
# On the P1S, larger Z means the bed is physically lower, so 256 - 20 = Z236.
FLEX_BOTTOM_Z_MM = 236.0

# Flex upward from that lower endpoint.
FLEX_STROKE_MM = 40
FLEX_UP_Z_MM = FLEX_BOTTOM_Z_MM - FLEX_STROKE_MM
FLEX_CYCLES = 2

# Motion speeds in mm/min.
Z_FLEX_FEED = 900        # 15 mm/s
Z_POSITION_FEED = 900    # 15 mm/s
XY_POSITION_FEED = 6000  # 100 mm/s when positioning out of the way
PUSH_FEED = 1050         # 17.5 mm/s pushing motion (1.75x previous)

# Toolhead position used for the push.
X_CENTER_MM = 128.0
X_LEFT_MM = 96.0
X_RIGHT_MM = 160.0
Y_REAR_MM = 255.0
Y_FRONT_MM = 0.0

# Bed position after ejection: 3 mm physically above the previous Z128 position.
Z_FINAL_MM = 125

# Push height.
# Shift the height calculation down by 2 inches so short prints naturally
# resolve to Z3 after clamping, keeping the nozzle clear of the plate.
PUSH_HEIGHT_OFFSET_MM = 50.8
PUSH_HEIGHT_FRACTION = 0.35
PUSH_Z_MIN_MM = 3.0
PUSH_Z_MAX_MM = 40.0
PUSH_Z_CLEARANCE_BELOW_TOP_MM = 2.0

# Before returning the toolhead rearward between push tracks, lower the bed so
# the nozzle clears the top of a still-attached part by this amount.
RETURN_Z_CLEARANCE_ABOVE_TOP_MM = 5.0

# Dynamically place the XL startup purge line against the first-layer part.
# Positive angle rises toward the rear (+Y) as X increases. The line is
# translated rearward until its extrusion edge overlaps the part edge by the
# requested amount. Widths are estimates of the deposited first-layer roads.
PURGE_LINE_ANGLE_DEG = 2.0
PURGE_X_START_MM = 20.0
PURGE_X_SLOW_END_MM = 60.0
PURGE_X_END_MM = 220.0
PURGE_WIPE_END_X_MM = 223.0
PURGE_TEMPLATE_Y_MM = 5.0
PURGE_SLOW_WIDTH_MM = 2.4
PURGE_NORMAL_WIDTH_MM = 1.2
PART_FIRST_LAYER_WIDTH_MM = 0.5
PURGE_PART_EDGE_OVERLAP_MM = 0.15
PURGE_Y_MIN_MM = 0.0
PURGE_Y_MAX_MM = 255.0
FIRST_LAYER_Z_TOLERANCE_MM = 0.05

PURGE_PREP_MARKER = ";===== Move to leftmost bottom corner"
PURGE_START_MARKER = ";===== Start extrusion purge line"
PURGE_END_MARKER = ";===== End of purge line"
PURGE_FINAL_MARKER = ";===== Final state"

# Try the cooling-direction M190 R wait before the timed fallback. Set False
# only if a firmware version hangs indefinitely on M190 R.
USE_M190_R = True

MARKER = "; === P1S FARM EJECT POSTPROCESS v6 ==="
END_MARKER = "; === END P1S FARM EJECT POSTPROCESS ==="
EXECUTABLE_BLOCK_END = "; EXECUTABLE_BLOCK_END"
PURGE_PLACEMENT_MARKER = "; === ANGLED ATTACHED PURGE LINE v1 ==="

DEFAULT_BED_DROP_START = "M17 S"
DEFAULT_BED_DROP_CURRENT = "M17 Z0.4"
DEFAULT_BED_DROP_END = "M17 R ; restore z current"

# -----------------------------------------------------------------


def remove_default_end_bed_drop(gcode: str) -> tuple[str, int]:
    """Remove the stock final ~100 mm bed drop, preserving the earlier safety lift."""
    lines = gcode.splitlines(keepends=True)
    cleaned: list[str] = []
    index = 0
    removed_blocks = 0

    while index < len(lines):
        line = lines[index]
        if line.strip() == DEFAULT_BED_DROP_START and index + 1 < len(lines):
            next_line = lines[index + 1].strip()
            if next_line.startswith(DEFAULT_BED_DROP_CURRENT):
                end = index + 2
                while end < len(lines) and not lines[end].strip().startswith(
                    DEFAULT_BED_DROP_END
                ):
                    end += 1
                if end >= len(lines):
                    raise RuntimeError(
                        "Default end bed-drop block has no motor-current restore. "
                        "No changes were made."
                    )
                index = end + 1
                removed_blocks += 1
                continue
        cleaned.append(line)
        index += 1

    # Orca also stores machine_end_gcode as one CONFIG_BLOCK comment with
    # literal ``\n`` separators. Remove its duplicate so the export is clear.
    serialized_cleaned: list[str] = []
    serialized_start = rf"{DEFAULT_BED_DROP_START}\n{DEFAULT_BED_DROP_CURRENT}"
    for line in cleaned:
        if not line.lstrip().startswith("; machine_end_gcode ="):
            serialized_cleaned.append(line)
            continue
        start = line.find(serialized_start)
        if start < 0:
            serialized_cleaned.append(line)
            continue
        end = line.find(DEFAULT_BED_DROP_END, start)
        if end < 0:
            raise RuntimeError(
                "Serialized default end bed-drop block has no restore marker. "
                "No changes were made."
            )
        end += len(DEFAULT_BED_DROP_END)
        if line[end : end + 2] == r"\n":
            end += 2
        serialized_cleaned.append(line[:start] + line[end:])
        removed_blocks += 1

    return "".join(serialized_cleaned), removed_blocks


def infer_print_height(gcode: str) -> float:
    """
    Infer max_layer_z from the stock Bambu/Orca P1S machine-end line:
        G1 Z{max_layer_z + 0.5} F900 ; lower z a little

    After slicing, the placeholder is expanded to a number, so we recover
    print height as that Z value minus 0.5 mm.

    Several whitespace/comment variants are accepted. We deliberately do not
    take the global maximum Z, because stock end G-code later drops the bed
    toward Z=250 and would produce a false print height.
    """
    patterns = [
        r"(?im)^\s*G1\s+Z([0-9]+(?:\.[0-9]+)?)\s+F900\b[^\n;]*(?:;\s*lower\s+z\s+a\s+little)",
        r"(?im)^\s*G[01]\s+Z([0-9]+(?:\.[0-9]+)?)\s+F900\b.*?;\s*lower\s+z",
    ]
    for pat in patterns:
        m = re.search(pat, gcode)
        if m:
            z = float(m.group(1))
            h = z - 0.5
            if 0.2 <= h <= Z_BOTTOM_MM:
                return h

    raise RuntimeError(
        "Could not safely infer print height from the P1S machine-end G-code. "
        "No changes were made. Check whether your printer profile's end G-code "
        "still contains the stock 'lower z a little' move."
    )


def choose_push_z(print_height: float) -> float:
    # Continuous calculation: subtract a 2-inch height offset before applying
    # the push-height fraction. The clamp then naturally gives Z3 for short prints.
    top_limited = max(PUSH_Z_MIN_MM, print_height - PUSH_Z_CLEARANCE_BELOW_TOP_MM)
    requested = (print_height - PUSH_HEIGHT_OFFSET_MM) * PUSH_HEIGHT_FRACTION
    return max(
        PUSH_Z_MIN_MM,
        min(requested, PUSH_Z_MAX_MM, top_limited),
    )


def choose_return_z(print_height: float) -> float:
    return_z = print_height + RETURN_Z_CLEARANCE_ABOVE_TOP_MM
    if return_z > Z_BOTTOM_MM:
        raise RuntimeError(
            "Print is too tall to provide safe nozzle clearance during the "
            f"rearward eject returns: height={print_height:.3f} mm, requested "
            f"return Z={return_z:.3f} mm, machine limit={Z_BOTTOM_MM:.3f} mm. "
            "No changes were made."
        )
    return return_z


_NUMBER = r"[-+]?(?:\d+(?:\.\d*)?|\.\d+)(?:[eE][-+]?\d+)?"
_WORD_RE = re.compile(rf"([A-Za-z])\s*({_NUMBER})")
_COMMAND_RE = re.compile(r"^\s*([GMT]\d+(?:\.\d+)?)\b", re.IGNORECASE)


def _is_marker_line(line: str, marker: str) -> bool:
    """Match an executable comment line, not a serialized CONFIG_BLOCK copy."""
    return line.lstrip().startswith(marker)


def _command_words(line: str) -> tuple[str | None, dict[str, float]]:
    """Return the executable command and numeric words from one G-code line."""
    code = line.split(";", 1)[0]
    command_match = _COMMAND_RE.match(code)
    if not command_match:
        return None, {}
    command = command_match.group(1).upper()
    words = {
        letter.upper(): float(value)
        for letter, value in _WORD_RE.findall(code[command_match.end() :])
    }
    return command, words


def _feature_from_comment(line: str, current: str | None) -> str | None:
    match = re.search(r";\s*(?:FEATURE|TYPE)\s*:\s*(.+?)\s*$", line, re.IGNORECASE)
    return match.group(1).strip() if match else current


def _is_part_feature(feature: str | None) -> bool:
    """Exclude detached helper structures when Orca supplies feature labels."""
    if feature is None:
        return True
    normalized = feature.lower().replace("_", " ").replace("-", " ")
    excluded = ("skirt", "support", "prime tower", "wipe tower")
    return not any(name in normalized for name in excluded)


def _arc_points(
    x0: float,
    y0: float,
    x1: float,
    y1: float,
    words: dict[str, float],
    clockwise: bool,
) -> list[tuple[float, float]]:
    """Approximate an IJ arc finely enough for first-contact calculations."""
    if "I" not in words and "J" not in words:
        return [(x0, y0), (x1, y1)]

    cx = x0 + words.get("I", 0.0)
    cy = y0 + words.get("J", 0.0)
    radius = hypot(x0 - cx, y0 - cy)
    if radius <= 1e-9:
        return [(x0, y0), (x1, y1)]

    start_angle = atan2(y0 - cy, x0 - cx)
    end_angle = atan2(y1 - cy, x1 - cx)
    if hypot(x1 - x0, y1 - y0) <= 1e-7:
        sweep = -2.0 * pi if clockwise else 2.0 * pi
    elif clockwise:
        sweep = -((start_angle - end_angle) % (2.0 * pi))
    else:
        sweep = (end_angle - start_angle) % (2.0 * pi)

    step_count = max(1, ceil(abs(sweep) / radians(5.0)))
    points = [(x0, y0)]
    for step in range(1, step_count):
        angle = start_angle + sweep * step / step_count
        points.append((cx + radius * cos(angle), cy + radius * sin(angle)))
    points.append((x1, y1))
    return points


def first_layer_part_segments(
    gcode: str,
) -> tuple[list[tuple[float, float, float, float]], float]:
    """Extract positive-extrusion XY segments on the first printed Z plane."""
    lines = gcode.splitlines()
    end_markers = [
        i for i, line in enumerate(lines) if _is_marker_line(line, PURGE_END_MARKER)
    ]
    if len(end_markers) != 1:
        raise RuntimeError(
            f"Expected one XL purge end marker, found {len(end_markers)}. "
            "No changes were made."
        )
    collect_after = end_markers[0]

    x = y = z = e = 0.0
    xyz_absolute = True
    e_absolute = True
    feature: str | None = None
    candidates: list[tuple[float, float, float, float, float, str | None]] = []

    for line_number, line in enumerate(lines):
        feature = _feature_from_comment(line, feature)
        command, words = _command_words(line)
        if command is None:
            continue
        if command == "G90":
            xyz_absolute = True
            continue
        if command == "G91":
            xyz_absolute = False
            continue
        if command == "M82":
            e_absolute = True
            continue
        if command == "M83":
            e_absolute = False
            continue
        if command == "G92":
            x = words.get("X", x)
            y = words.get("Y", y)
            z = words.get("Z", z)
            e = words.get("E", e)
            continue
        if command not in {"G0", "G1", "G2", "G3"}:
            continue

        new_x = words.get("X", x if xyz_absolute else 0.0)
        new_y = words.get("Y", y if xyz_absolute else 0.0)
        new_z = words.get("Z", z if xyz_absolute else 0.0)
        if not xyz_absolute:
            new_x += x
            new_y += y
            new_z += z

        if "E" in words:
            new_e = words["E"] if e_absolute else e + words["E"]
            extrusion_delta = new_e - e
        else:
            new_e = e
            extrusion_delta = 0.0

        if (
            line_number > collect_after
            and extrusion_delta > 1e-7
            and hypot(new_x - x, new_y - y) > 1e-7
            and _is_part_feature(feature)
        ):
            if command in {"G2", "G3"}:
                points = _arc_points(
                    x, y, new_x, new_y, words, clockwise=(command == "G2")
                )
            else:
                points = [(x, y), (new_x, new_y)]
            for start, end in zip(points, points[1:]):
                candidates.append(
                    (start[0], start[1], end[0], end[1], new_z, feature)
                )

        x, y, z, e = new_x, new_y, new_z, new_e

    if not candidates:
        raise RuntimeError(
            "Could not find any first-layer part extrusion after the XL purge line. "
            "No changes were made."
        )

    first_z = min(segment[4] for segment in candidates)
    first_layer = [
        (x0, y0, x1, y1)
        for x0, y0, x1, y1, segment_z, _feature in candidates
        if abs(segment_z - first_z) <= FIRST_LAYER_Z_TOLERANCE_MM
    ]
    return first_layer, first_z


def _points_in_x_range(
    segment: tuple[float, float, float, float],
    x_min: float,
    x_max: float,
) -> list[tuple[float, float]]:
    """Return segment endpoints and boundary crossings inside an X interval."""
    x0, y0, x1, y1 = segment
    points: list[tuple[float, float]] = []
    if x_min - 1e-9 <= x0 <= x_max + 1e-9:
        points.append((x0, y0))
    if x_min - 1e-9 <= x1 <= x_max + 1e-9:
        points.append((x1, y1))
    if abs(x1 - x0) > 1e-9:
        for boundary in (x_min, x_max):
            fraction = (boundary - x0) / (x1 - x0)
            if 0.0 < fraction < 1.0:
                points.append((boundary, y0 + fraction * (y1 - y0)))
    return points


def choose_attached_purge_line(
    segments: list[tuple[float, float, float, float]],
) -> tuple[float, float, float, float]:
    """Return start/end Y and the part point that establishes first contact."""
    slope = tan(radians(PURGE_LINE_ANGLE_DEG))
    normal_scale = sqrt(1.0 + slope * slope)
    best: tuple[float, float, float] | None = None

    regions = (
        (PURGE_X_START_MM, PURGE_X_SLOW_END_MM, PURGE_SLOW_WIDTH_MM),
        (PURGE_X_SLOW_END_MM, PURGE_X_END_MM, PURGE_NORMAL_WIDTH_MM),
    )
    for segment in segments:
        for region_start, region_end, purge_width in regions:
            centerline_gap = max(
                0.0,
                (purge_width + PART_FIRST_LAYER_WIDTH_MM) / 2.0
                - PURGE_PART_EDGE_OVERLAP_MM,
            )
            for point_x, point_y in _points_in_x_range(
                segment, region_start, region_end
            ):
                intercept = (
                    point_y
                    - slope * (point_x - PURGE_X_START_MM)
                    - centerline_gap * normal_scale
                )
                if best is None or intercept < best[0]:
                    best = (intercept, point_x, point_y)

    if best is None:
        raise RuntimeError(
            f"The first layer does not cross the purge-line X range "
            f"{PURGE_X_START_MM:g}..{PURGE_X_END_MM:g}. No changes were made."
        )

    intercept, contact_x, contact_y = best
    start_y = intercept
    end_y = intercept + slope * (PURGE_X_END_MM - PURGE_X_START_MM)
    if start_y < PURGE_TEMPLATE_Y_MM - 1e-6:
        raise RuntimeError(
            "The part is too close to the front for a rearward-only attached "
            f"purge line (first-contact start would be Y{start_y:.3f}, forward "
            f"of the template position Y{PURGE_TEMPLATE_Y_MM:.3f}). Move the part "
            "rearward. No changes were made."
        )
    if not (
        PURGE_Y_MIN_MM <= start_y <= PURGE_Y_MAX_MM
        and PURGE_Y_MIN_MM <= end_y <= PURGE_Y_MAX_MM
    ):
        raise RuntimeError(
            "Calculated attached purge line is outside the allowed Y range: "
            f"start Y{start_y:.3f}, end Y{end_y:.3f}. No changes were made."
        )
    return start_y, end_y, contact_x, contact_y


def _replace_y_word(line: str, new_y: float) -> str:
    code, separator, comment = line.partition(";")
    replaced, count = re.subn(
        rf"(?i)(?<![A-Za-z])Y\s*{_NUMBER}", f"Y{new_y:.3f}", code, count=1
    )
    if count != 1:
        raise RuntimeError(f"Could not replace purge-line Y coordinate in: {line}")
    return replaced + (separator + comment if separator else "")


def _insert_y_word(line: str, new_y: float) -> str:
    code, separator, comment = line.partition(";")
    replaced, count = re.subn(
        rf"(?i)((?<![A-Za-z])X\s*{_NUMBER})",
        rf"\1 Y{new_y:.3f}",
        code,
        count=1,
    )
    if count != 1:
        raise RuntimeError(f"Could not add purge-wipe Y coordinate in: {line}")
    return replaced + (separator + comment if separator else "")


def angle_and_attach_purge_line(
    gcode: str,
) -> tuple[str, float, float, float, float, float]:
    """Angle the XL purge line and translate it to first contact with the part."""
    lines = gcode.splitlines(keepends=True)
    marker_counts = {
        marker: sum(_is_marker_line(line, marker) for line in lines)
        for marker in (
            PURGE_PREP_MARKER,
            PURGE_START_MARKER,
            PURGE_END_MARKER,
            PURGE_FINAL_MARKER,
        )
    }
    bad_markers = {marker: count for marker, count in marker_counts.items() if count != 1}
    if bad_markers:
        details = ", ".join(f"{marker!r}: {count}" for marker, count in bad_markers.items())
        raise RuntimeError(
            f"Could not identify the XL startup purge block ({details}). "
            "No changes were made."
        )

    segments, first_z = first_layer_part_segments(gcode)
    start_y, end_y, contact_x, contact_y = choose_attached_purge_line(segments)
    slope = tan(radians(PURGE_LINE_ANGLE_DEG))

    prep_index = next(
        i for i, line in enumerate(lines) if _is_marker_line(line, PURGE_PREP_MARKER)
    )
    final_index = next(
        i for i, line in enumerate(lines) if _is_marker_line(line, PURGE_FINAL_MARKER)
    )
    purge_end_index = next(
        i for i, line in enumerate(lines) if _is_marker_line(line, PURGE_END_MARKER)
    )
    if not prep_index < purge_end_index < final_index:
        raise RuntimeError(
            "XL purge markers are out of order. No changes were made."
        )
    expected_counts = {
        PURGE_X_START_MM: 2,
        PURGE_X_SLOW_END_MM: 1,
        PURGE_X_END_MM: 1,
    }
    replaced_counts = {x_value: 0 for x_value in expected_counts}
    circle_indices: list[int] = []
    wipe_index: int | None = None

    for index in range(prep_index, final_index):
        command, words = _command_words(lines[index])
        if command not in {"G0", "G1", "G2", "G3"} or "X" not in words:
            continue
        if (
            command in {"G2", "G3"}
            and "Y" in words
            and abs(words["X"] - PURGE_X_END_MM) <= 1e-6
            and abs(words["Y"] - PURGE_TEMPLATE_Y_MM) <= 1e-6
        ):
            circle_indices.append(index)
            continue
        if (
            command in {"G0", "G1"}
            and "Y" not in words
            and abs(words["X"] - PURGE_WIPE_END_X_MM) <= 1e-6
        ):
            wipe_index = index
            continue
        if "Y" not in words:
            continue
        if abs(words["Y"] - PURGE_TEMPLATE_Y_MM) > 1e-6:
            continue
        for target_x in expected_counts:
            if abs(words["X"] - target_x) <= 1e-6:
                target_y = start_y + slope * (target_x - PURGE_X_START_MM)
                newline = "\n" if lines[index].endswith("\n") else ""
                lines[index] = _replace_y_word(lines[index].rstrip("\r\n"), target_y) + newline
                replaced_counts[target_x] += 1
                break

    if replaced_counts != expected_counts:
        raise RuntimeError(
            "XL purge commands did not match the expected template: "
            f"expected {expected_counts}, replaced {replaced_counts}. No changes were made."
        )
    if len(circle_indices) != 2 or wipe_index is None:
        raise RuntimeError(
            "XL circular wipe did not match the expected template: "
            f"circles={len(circle_indices)}, straight_wipe={wipe_index is not None}. "
            "No changes were made."
        )

    wipe_y = start_y + slope * (PURGE_WIPE_END_X_MM - PURGE_X_START_MM)
    newline = "\n" if lines[wipe_index].endswith("\n") else ""
    lines[wipe_index] = (
        _insert_y_word(lines[wipe_index].rstrip("\r\n"), wipe_y) + newline
    )
    for index in reversed(circle_indices):
        del lines[index]

    for index, line in enumerate(lines):
        if line.lstrip().startswith(";===== Circular purge wipe"):
            newline = "\n" if line.endswith("\n") else ""
            lines[index] = ";===== Straight purge wipe ==================================" + newline
            break

    start_index = next(
        i for i, line in enumerate(lines) if _is_marker_line(line, PURGE_START_MARKER)
    )
    metadata = [
        PURGE_PLACEMENT_MARKER + "\n",
        f"; angle: {PURGE_LINE_ANGLE_DEG:.3f} deg\n",
        f"; first-layer Z: {first_z:.3f} mm\n",
        f"; purge endpoints: X{PURGE_X_START_MM:.3f} Y{start_y:.3f} -> "
        f"X{PURGE_X_END_MM:.3f} Y{end_y:.3f}\n",
        f"; attachment part point: X{contact_x:.3f} Y{contact_y:.3f}\n",
    ]
    lines[start_index + 1 : start_index + 1] = metadata
    return "".join(lines), start_y, end_y, contact_x, contact_y, first_z


def cooling_block() -> list[str]:
    lines = [
        "; --- cool build plate before flex/eject ---",
        "M140 S0 ; bed heater off",
        "M106 P2 S255 ; auxiliary fan on to help cool bed/part",
        "M106 P3 S255 ; chamber exhaust fan on to remove trapped heat",
    ]
    if USE_M190_R:
        lines += [
            f"M190 R{COOL_TEMP_C:g} ; wait for bed to cool to target",
        ]
    else:
        # Repeating S waits is a fallback for Bambu firmware variants that
        # do not reliably wait for cooling with R.
        lines += [
            f"M190 S{COOL_TEMP_C:g}",
            f"M190 S{COOL_TEMP_C:g}",
            f"M190 S{COOL_TEMP_C:g}",
        ]
    lines += [
        f"G4 S{COOL_FALLBACK_DWELL_SECONDS:g} ; continue forced cooling after firmware wait",
        "M106 P2 S0 ; auxiliary fan off",
        "M106 P3 S0 ; chamber exhaust fan off",
        "M400",
    ]
    return lines


def build_eject_gcode(print_height: float) -> str:
    push_z = choose_push_z(print_height)
    return_z = choose_return_z(print_height)

    lines: list[str] = [
        "",
        MARKER,
        f"; inferred print height: {print_height:.3f} mm",
        f"; selected push Z: {push_z:.3f} mm",
        f"; selected safe return Z: {return_z:.3f} mm",
        "M400",
        "G90 ; absolute XYZ",
        "M17 X1.2 Y1.2 Z0.75 ; restore normal-ish P1S motor current",
        "M104 S0 ; hotend off",
    ]

    lines.extend(cooling_block())

    # Move the head out of the print area BEFORE raising the bed from the bottom.
    # This is intentionally earlier than the user's conceptual sequence to reduce
    # the risk of a tall print colliding with a stationary nozzle.
    lines += [
        "; --- park toolhead at rear/center before large Z strokes ---",
        f"G1 X{X_CENTER_MM:.3f} Y{Y_REAR_MM:.3f} F{XY_POSITION_FEED:g}",
        "M400",
        "",
        "; --- flex build plate using the user's mechanical edge extension ---",
        f"G1 Z{FLEX_BOTTOM_Z_MM:.3f} F{Z_POSITION_FEED:g} ; lower flex endpoint",
        "M400",
    ]

    for i in range(1, FLEX_CYCLES + 1):
        lines += [
            f"; flex cycle {i}/{FLEX_CYCLES}",
            f"G1 Z{FLEX_UP_Z_MM:.3f} F{Z_FLEX_FEED:g} ; bed 40 mm higher",
            "M400",
            f"G1 Z{FLEX_BOTTOM_Z_MM:.3f} F{Z_FLEX_FEED:g} ; bed back to lower flex endpoint",
            "M400",
        ]

    lines += [
        "",
        "; --- eject ---",
        f"G1 X{X_CENTER_MM:.3f} Y{Y_REAR_MM:.3f} F{XY_POSITION_FEED:g}",
        "M400",
        f"G1 Z{push_z:.3f} F{Z_POSITION_FEED:g} ; raise bed to push/contact height",
        "M400",
        "; push track 1/3: center",
        f"G1 X{X_CENTER_MM:.3f} Y{Y_REAR_MM:.3f} F{XY_POSITION_FEED:g}",
        f"G1 Y{Y_FRONT_MM:.3f} F{PUSH_FEED:g} ; center push",
        "M400",
        "; lower bed below a possibly stuck part before rearward return",
        f"G1 Z{return_z:.3f} F{Z_POSITION_FEED:g} ; nozzle clears part top",
        "M400",
        "; clear rearward jog to left track",
        f"G1 Y{Y_REAR_MM:.3f} F{XY_POSITION_FEED:g}",
        f"G1 X{X_LEFT_MM:.3f} F{XY_POSITION_FEED:g}",
        f"G1 Z{push_z:.3f} F{Z_POSITION_FEED:g} ; restore push/contact height",
        "M400",
        "; push track 2/3: left",
        f"G1 Y{Y_FRONT_MM:.3f} F{PUSH_FEED:g} ; left push",
        "M400",
        "; lower bed below a possibly stuck part before rearward return",
        f"G1 Z{return_z:.3f} F{Z_POSITION_FEED:g} ; nozzle clears part top",
        "M400",
        "; clear rearward jog to right track",
        f"G1 Y{Y_REAR_MM:.3f} F{XY_POSITION_FEED:g}",
        f"G1 X{X_RIGHT_MM:.3f} F{XY_POSITION_FEED:g}",
        f"G1 Z{push_z:.3f} F{Z_POSITION_FEED:g} ; restore push/contact height",
        "M400",
        "; push track 3/3: right",
        f"G1 Y{Y_FRONT_MM:.3f} F{PUSH_FEED:g} ; right push",
        "M400",
        "",
        "; --- leave printer ready for farm handling ---",
        f"G1 Z{Z_FINAL_MM:.3f} F{Z_POSITION_FEED:g} ; bed 3 mm above previous Z128 position",
        "M400",
        "M17 X0.8 Y0.8 Z0.5 ; reduce holding current",
        END_MARKER,
        "",
    ]

    return "\n".join(lines)


def insert_before_executable_end(gcode: str, eject: str) -> str:
    """Insert the eject sequence inside the firmware executable envelope."""
    marker_count = gcode.count(EXECUTABLE_BLOCK_END)
    if marker_count != 1:
        raise RuntimeError(
            "Expected exactly one EXECUTABLE_BLOCK_END marker, found "
            f"{marker_count}. No changes were made."
        )
    insertion = gcode.index(EXECUTABLE_BLOCK_END)
    return gcode[:insertion].rstrip() + "\n" + eject.rstrip() + "\n" + gcode[insertion:]


def process_file(path: Path) -> None:
    original = path.read_text(encoding="utf-8", errors="replace")

    if re.search(r"(?m)^; === INDEXED REAR PURGE LINE v\d+\b", original):
        raise RuntimeError(
            "G-code contains the retired custom purge line. "
            "Export a fresh file from OrcaSlicer."
        )
    if MARKER in original:
        upgraded, removed_bed_drop_blocks = remove_default_end_bed_drop(original)
        purge_status = "already_attached"
        if PURGE_PLACEMENT_MARKER not in upgraded:
            (
                upgraded,
                start_y,
                end_y,
                contact_x,
                contact_y,
                first_z,
            ) = angle_and_attach_purge_line(upgraded)
            purge_status = (
                f"upgraded angle={PURGE_LINE_ANGLE_DEG:.3f}deg "
                f"Y{start_y:.3f}->Y{end_y:.3f} "
                f"contact=X{contact_x:.3f},Y{contact_y:.3f} "
                f"first_layer_z={first_z:.3f}"
            )
        if upgraded != original:
            path.write_text(upgraded, encoding="utf-8")
        print(
            f"[p1s_farm_eject] already processed: {path} | "
            f"purge={purge_status} | "
            f"removed_bed_drop_blocks={removed_bed_drop_blocks}"
        )
        return

    height = infer_print_height(original)
    postprocessed, removed_bed_drop_blocks = remove_default_end_bed_drop(
        original
    )
    (
        postprocessed,
        purge_start_y,
        purge_end_y,
        contact_x,
        contact_y,
        first_layer_z,
    ) = angle_and_attach_purge_line(postprocessed)
    eject = build_eject_gcode(height)

    # Preserve the slicer's complete normal end sequence, then add the farm cycle.
    # This means filament unload, heater shutdown, timelapse completion, etc. happen
    # normally before the mechanical eject sequence begins.
    completed = insert_before_executable_end(postprocessed, eject)
    path.write_text(completed, encoding="utf-8")
    print(
        f"[p1s_farm_eject] processed {path} | "
        f"print_height={height:.3f} mm | push_z={choose_push_z(height):.3f} mm | "
        f"return_z={choose_return_z(height):.3f} mm | "
        f"purge_angle={PURGE_LINE_ANGLE_DEG:.3f} deg | "
        f"purge_y={purge_start_y:.3f}->{purge_end_y:.3f} | "
        f"contact=X{contact_x:.3f},Y{contact_y:.3f} | "
        f"first_layer_z={first_layer_z:.3f} | "
        f"removed_bed_drop_blocks={removed_bed_drop_blocks}"
    )


def main() -> int:
    if len(sys.argv) == 2:
        path_argument = sys.argv[1]
    elif len(sys.argv) == 3:
        # Backward compatibility with the previous OrcaSlicer command, which
        # supplied an indexed-purge number. The number is now ignored.
        if sys.argv[1].isdigit():
            path_argument = sys.argv[2]
        elif sys.argv[2].isdigit():
            path_argument = sys.argv[1]
        else:
            print("The legacy extra argument must be an integer.", file=sys.stderr)
            return 2
    else:
        print(
            "Usage: farm_postprocess.py <gcode-file>\n"
            "OrcaSlicer command example: python3 "
            "/Users/jeremyjacob/Desktop/farm_postprocess.py",
            file=sys.stderr,
        )
        return 2

    path = Path(path_argument).expanduser().resolve()
    if not path.is_file():
        print(f"Not a file: {path}", file=sys.stderr)
        return 2

    try:
        process_file(path)
    except Exception as exc:
        print(f"[p1s_farm_eject] ERROR: {exc}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
