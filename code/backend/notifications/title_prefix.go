package notifications

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf16"
)

// MaxTitlePrefixLength is the longest notification title prefix, counted in
// UTF-16 code units. The settings field limits the text with maxLength, which
// browsers count in UTF-16 code units, so counting the same way here means the
// field and the server always agree: an emoji outside the Basic Multilingual
// Plane uses 2 of the 50, in both places. Counting runes or bytes instead would
// let the server refuse text the field accepted, or the other way round.
const MaxTitlePrefixLength = 50

// ErrTitlePrefixTooLong is returned for a prefix longer than
// MaxTitlePrefixLength. Its text is shown to the user as it is, in English,
// like the other settings and test errors.
var ErrTitlePrefixTooLong = errors.New("The notification title prefix can be at most 50 characters.")

// CleanTitlePrefix is the one place a notification title prefix is cleaned
// and checked. Saving the settings and both test buttons go through it, so a
// test never shows a title the saved setting couldn't produce.
//
// Line breaks, the Unicode line and paragraph separators (U+2028, U+2029) and
// every other control character, tab and NEL (U+0085) included, are removed
// without an error. A title is a single line on every desktop and service, and
// some characters break delivery outright: a NUL makes the Linux desktop
// notification fail, because D-Bus refuses strings that contain one.
//
// Spaces are kept exactly as typed. Don't trim the result: the prefix is put
// in front of the title with no separator, so a trailing space is how the user
// separates it from the title ("[Replicaro] " gives "[Replicaro] Task
// SUCCESSFUL").
//
// The length is checked after cleaning, so characters that are removed anyway
// don't count against the limit.
func CleanTitlePrefix(prefix string) (string, error) {
	cleaned := strings.Map(func(r rune) rune {
		// unicode.IsControl covers C0 (tab, line feed, carriage return, NUL),
		// DEL and C1, which includes U+0085.
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return -1
		}
		return r
	}, prefix)
	units := 0
	for _, r := range cleaned {
		// Invalid UTF-8 reads as U+FFFD, one unit, which is also what the JSON
		// decoder has already turned it into.
		units += utf16.RuneLen(r)
	}
	if units > MaxTitlePrefixLength {
		return "", ErrTitlePrefixTooLong
	}
	return cleaned, nil
}
