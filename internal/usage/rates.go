package usage

// TokensPerSec returns completion tokens generated per second
// across all records, derived from the summed elapsed time.
// Completion tokens, not total: generation speed is the rate the
// user cares about. Zero when no time was measured (the summed
// elapsed is zero).
func (a *Account) TokensPerSec() float64 {
	elapsed := a.TotalStats().Elapsed
	if elapsed == 0 {
		return 0
	}
	return float64(a.Total().CompletionTokens) / elapsed.Seconds()
}

// BytesPerSec returns output bytes produced per second across all
// records, using the summed total output bytes. Zero when no time
// was measured.
func (a *Account) BytesPerSec() float64 {
	elapsed := a.TotalStats().Elapsed
	if elapsed == 0 {
		return 0
	}
	return float64(a.TotalStats().Output.Total()) / elapsed.Seconds()
}
