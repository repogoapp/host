//go:build windows

package procscan

import "context"

// Cwds is empty: Windows exposes no cheap per-process working directory.
func Cwds(context.Context, []int) map[int]string { return nil }
