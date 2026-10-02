package shipping

import "github.com/repogo/host/internal/session/native"

// ParseNative reads a transcript's usage with the Rust parser: ok false when
// the host does not link it or it cannot read the file, and the provider's
// line parser reads it instead.
func ParseNative(parser native.Parser, path string) ([]Record, bool) {
	if !native.Available {
		return nil, false
	}
	usage, err := native.ScanUsage(parser, path)
	if err != nil {
		return nil, false
	}
	out := make([]Record, len(usage))
	for i, u := range usage {
		out[i] = Record{
			Key: u.Key, Session: u.Session, TimestampMs: u.At, Model: u.Model,
			Tokens: Tokens{
				Uncached: u.Uncached, Cached: u.Cached,
				Creation: u.Creation, Creation1h: u.Creation1h,
				Output: u.Output, Reasoning: u.Reasoning,
			},
			Fast:        u.Fast,
			ReportedUSD: u.ReportedUSD, HasReported: u.HasReported,
		}
	}
	return out, true
}
