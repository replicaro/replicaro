#!/usr/bin/env python3
"""Render the deterministic Finder background used by both macOS DMGs."""

import pathlib
import struct
import sys
import zlib

WIDTH = 820
HEIGHT = 560
SCALE = 4
TEXT = "Drag Replicaro to Applications"

# A tiny embedded bitmap face keeps the checked PNG independent of host fonts
# and rendering libraries. Supersampling gives its deliberately simple forms
# clean edges while preserving byte-for-byte reproducibility.
GLYPHS = {
    " ": ("00000",) * 7,
    "A": ("01110", "10001", "10001", "11111", "10001", "10001", "10001"),
    "D": ("11110", "10001", "10001", "10001", "10001", "10001", "11110"),
    "R": ("11110", "10001", "10001", "11110", "10100", "10010", "10001"),
    "a": ("00000", "01110", "00001", "01111", "10001", "10011", "01101"),
    "c": ("00000", "01110", "10001", "10000", "10000", "10001", "01110"),
    "e": ("00000", "01110", "10001", "11111", "10000", "10001", "01110"),
    "g": ("00000", "01110", "10001", "10011", "01101", "00001", "01110"),
    "i": ("00100", "00000", "01100", "00100", "00100", "00100", "01110"),
    "l": ("01100", "00100", "00100", "00100", "00100", "00100", "01110"),
    "n": ("00000", "10110", "11001", "10001", "10001", "10001", "10001"),
    "o": ("00000", "01110", "10001", "10001", "10001", "10001", "01110"),
    "p": ("00000", "11110", "10001", "10001", "11110", "10000", "10000"),
    "r": ("00000", "10110", "11001", "10000", "10000", "10000", "10000"),
    "s": ("00000", "01111", "10000", "01110", "00001", "10001", "01110"),
    "t": ("00100", "00100", "11111", "00100", "00100", "00101", "00010"),
}


def canvas(color):
    return bytearray(color * (WIDTH * SCALE * HEIGHT * SCALE))


def pixel(image, x, y, color):
    width = WIDTH * SCALE
    if 0 <= x < width and 0 <= y < HEIGHT * SCALE:
        offset = (y * width + x) * 3
        image[offset:offset + 3] = bytes(color)


def rectangle(image, left, top, right, bottom, color):
    width = WIDTH * SCALE
    row = bytes(color) * max(0, right - left)
    for y in range(max(0, top), min(HEIGHT * SCALE, bottom)):
        start = (y * width + max(0, left)) * 3
        image[start:start + len(row)] = row


def rounded_rectangle(image, left, top, right, bottom, radius, color):
    rectangle(image, left + radius, top, right - radius, bottom, color)
    rectangle(image, left, top + radius, right, bottom - radius, color)
    radius_squared = radius * radius
    for center_x in (left + radius, right - radius - 1):
        for center_y in (top + radius, bottom - radius - 1):
            for dy in range(-radius, radius + 1):
                span = int(max(0, radius_squared - dy * dy) ** 0.5)
                rectangle(image, center_x - span, center_y + dy,
                          center_x + span + 1, center_y + dy + 1, color)


def polygon(image, points, color):
    min_y = max(0, min(y for _, y in points))
    max_y = min(HEIGHT * SCALE - 1, max(y for _, y in points))
    for y in range(min_y, max_y + 1):
        intersections = []
        for index, (x1, y1) in enumerate(points):
            x2, y2 = points[(index + 1) % len(points)]
            if y1 == y2 or not (min(y1, y2) <= y < max(y1, y2)):
                continue
            intersections.append(int(x1 + (y - y1) * (x2 - x1) / (y2 - y1)))
        intersections.sort()
        for start, end in zip(intersections[::2], intersections[1::2]):
            rectangle(image, start, y, end + 1, y + 1, color)


def draw_text(image, text, center_x, top, size, color):
    advance = 6 * size
    width = len(text) * advance - size
    left = center_x - width // 2
    for character in text:
        glyph = GLYPHS[character]
        for row, bits in enumerate(glyph):
            for column, enabled in enumerate(bits):
                if enabled == "1":
                    rectangle(image, left + column * size, top + row * size,
                              left + (column + 1) * size, top + (row + 1) * size, color)
        left += advance


def render_pixels():
    scale = SCALE
    image = canvas((246, 244, 240))
    # Finder overlays the two icons at (180, 260) and (640, 260). The arrow
    # occupies only the clear space between those icon targets.
    rounded_rectangle(image, 282 * scale, 250 * scale, 521 * scale, 270 * scale,
                      10 * scale, (202, 218, 215))
    polygon(image, [(505 * scale, 226 * scale), (566 * scale, 260 * scale),
                    (505 * scale, 294 * scale)], (202, 218, 215))
    rounded_rectangle(image, 278 * scale, 246 * scale, 517 * scale, 266 * scale,
                      10 * scale, (15, 159, 154))
    polygon(image, [(501 * scale, 222 * scale), (562 * scale, 256 * scale),
                    (501 * scale, 290 * scale)], (15, 159, 154))

    rounded_rectangle(image, 70 * scale, 356 * scale, 750 * scale, 430 * scale,
                      18 * scale, (236, 234, 229))
    draw_text(image, TEXT, 410 * scale, 379 * scale, 4 * scale, (48, 57, 61))

    high_width = WIDTH * scale
    result = bytearray(WIDTH * HEIGHT * 3)
    for y in range(HEIGHT):
        for x in range(WIDTH):
            totals = [0, 0, 0]
            for dy in range(scale):
                for dx in range(scale):
                    offset = (((y * scale + dy) * high_width) + x * scale + dx) * 3
                    totals[0] += image[offset]
                    totals[1] += image[offset + 1]
                    totals[2] += image[offset + 2]
            output = (y * WIDTH + x) * 3
            area = scale * scale
            result[output:output + 3] = bytes(value // area for value in totals)
    return bytes(result)


def chunk(kind, data):
    return struct.pack(">I", len(data)) + kind + data + struct.pack(">I", zlib.crc32(kind + data))


def render_png():
    pixels = render_pixels()
    scanlines = b"".join(b"\x00" + pixels[y * WIDTH * 3:(y + 1) * WIDTH * 3]
                         for y in range(HEIGHT))
    header = struct.pack(">IIBBBBB", WIDTH, HEIGHT, 8, 2, 0, 0, 0)
    return (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", header) +
            chunk(b"IDAT", zlib.compress(scanlines, 9)) + chunk(b"IEND", b""))


def main():
    if len(sys.argv) != 2:
        raise SystemExit("usage: render-installer-background.py OUTPUT | --check=PATH")
    expected = render_png()
    if sys.argv[1].startswith("--check="):
        path = pathlib.Path(sys.argv[1].split("=", 1)[1])
        if path.is_symlink() or not path.is_file() or path.read_bytes() != expected:
            raise SystemExit("installer background differs from its deterministic source")
        return
    pathlib.Path(sys.argv[1]).write_bytes(expected)


if __name__ == "__main__":
    main()
