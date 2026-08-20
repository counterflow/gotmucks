package gotmucks

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// MinimumVersion is the oldest tmux this package supports.
//
// The floor is set by "new-session -e", which arrived in tmux 3.2. Everything
// else used here is older, so 3.2 is the single constraint.
//
// It is a function rather than a variable because it is what [Connect] and
// [Client.CheckVersion] gate on, and a variable could be moved by anything
// else in the process — including a dependency — with the version check then
// quietly passing something it was written to refuse.
func MinimumVersion() Version { return Version{Major: 3, Minor: 2} }

// Version is a tmux version.
//
// tmux versions are a major and minor number with an optional letter suffix
// ("3.2a"), and development builds carry a "next-" prefix ("next-3.4").
// Unnumbered builds ("master") parse as [Version.Unknown].
type Version struct {
	// Major and Minor are the numeric components.
	Major, Minor int
	// Suffix is the trailing letter revision, "a" in "3.2a". Empty if absent.
	Suffix string
	// Next reports a development build, the "next-" prefix in "next-3.4". It
	// is the tree heading towards that release rather than the release, so it
	// sorts before it; see [Version.Compare].
	Next bool
	// Raw is the string this was parsed from.
	Raw string
	// Unknown reports that no version number could be found, as for builds
	// that report "master".
	Unknown bool
}

// String renders the version the way tmux writes it.
func (v Version) String() string {
	if v.Unknown {
		if v.Raw != "" {
			return v.Raw
		}
		return "unknown"
	}
	var b strings.Builder
	if v.Next {
		b.WriteString("next-")
	}
	fmt.Fprintf(&b, "%d.%d%s", v.Major, v.Minor, v.Suffix)
	return b.String()
}

// Compare orders two versions: -1 if v is older than w, 0 if equal, +1 if
// newer. The letter suffix is compared lexically, so 3.2 < 3.2a < 3.2b, and a
// hyphenated suffix is a pre-release of the version it hangs off rather than a
// revision of it, so 3.4-rc1 < 3.4 < 3.4a.
//
// A "next-" build is the development tree heading towards a release, not that
// release, so next-3.2 < 3.2 for the same reason 3.2-rc1 does. This is what
// the prefix is for: a next-3.2 predates 3.2 and so may predate anything 3.2
// added, and reading it as 3.2 would let it through a check for a feature it
// does not have. Where that is not wanted — a tip-of-tree build known to be
// new enough — [WithoutVersionCheck] is the way past it.
//
// An unknown version is treated as newer than any known one: builds that do
// not report a number are development builds of the tip, and refusing to run
// against them would be the wrong default. That is not in tension with the
// rule above; "master" says nothing about which release it is heading for,
// while "next-3.2" says exactly that.
func (v Version) Compare(w Version) int {
	switch {
	case v.Unknown && w.Unknown:
		return 0
	case v.Unknown:
		return 1
	case w.Unknown:
		return -1
	}
	if v.Major != w.Major {
		return sign(v.Major - w.Major)
	}
	if v.Minor != w.Minor {
		return sign(v.Minor - w.Minor)
	}
	if v.Next != w.Next {
		if v.Next {
			return -1
		}
		return 1
	}
	if pv, pw := isPreRelease(v.Suffix), isPreRelease(w.Suffix); pv != pw {
		if pv {
			return -1
		}
		return 1
	}
	return strings.Compare(v.Suffix, w.Suffix)
}

// isPreRelease reports a suffix that hangs off the version rather than
// revising it, which tmux writes with a hyphen: "3.4-rc1" precedes "3.4".
func isPreRelease(suffix string) bool { return strings.HasPrefix(suffix, "-") }

// AtLeast reports whether v is w or newer.
func (v Version) AtLeast(w Version) bool { return v.Compare(w) >= 0 }

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}

// ParseVersion reads the output of "tmux -V", with or without the leading
// "tmux " word.
func ParseVersion(s string) (Version, error) {
	raw := strings.TrimSpace(s)
	v := Version{Raw: raw}
	if raw == "" {
		return v, fmt.Errorf("gotmucks: empty tmux version string")
	}

	field := raw
	// "tmux 3.2a" — take the last whitespace-separated field.
	if fields := strings.Fields(raw); len(fields) > 1 {
		field = fields[len(fields)-1]
	}

	if rest, ok := strings.CutPrefix(field, "next-"); ok {
		v.Next = true
		field = rest
	}

	major, rest, ok := strings.Cut(field, ".")
	maj, err := strconv.Atoi(major)
	if !ok || err != nil {
		// Some distributions prefix an OS name, e.g. "openbsd-7.4". Stripping
		// that is a fallback rather than the first move: doing it first also
		// eats the tail of a release candidate, and "3.4-rc1" would parse as
		// "rc1" and be discarded as unknown.
		if i := strings.LastIndexByte(field, '-'); i >= 0 {
			field = field[i+1:]
			major, rest, ok = strings.Cut(field, ".")
			maj, err = strconv.Atoi(major)
		}
	}
	if !ok || err != nil {
		v.Unknown = true
		return v, nil
	}

	// The minor component runs until the first non-digit; anything after it
	// is the letter suffix.
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		v.Unknown = true
		return v, nil
	}
	min, err := strconv.Atoi(rest[:end])
	if err != nil {
		v.Unknown = true
		return v, nil
	}

	v.Major, v.Minor, v.Suffix = maj, min, rest[end:]
	return v, nil
}

// Version reports the version of the tmux binary this client runs.
//
// It does not need a running server: "tmux -V" answers from the binary alone.
func (c *Client) Version(ctx context.Context) (Version, error) {
	out, _, err := c.run(ctx, "-V")
	if err != nil {
		return Version{}, err
	}
	return ParseVersion(string(out))
}

// CheckVersion reports an error wrapping [ErrUnsupportedVersion] if the tmux
// binary is older than [MinimumVersion].
func (c *Client) CheckVersion(ctx context.Context) error {
	v, err := c.Version(ctx)
	if err != nil {
		return err
	}
	if floor := MinimumVersion(); !v.AtLeast(floor) {
		return fmt.Errorf("gotmucks: tmux %s is older than the required %s: %w",
			v, floor, ErrUnsupportedVersion)
	}
	return nil
}
