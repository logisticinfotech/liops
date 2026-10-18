// SPDX-License-Identifier: Apache-2.0

package jobs

// Progress is a snapshot of the job currently running, for the UI bar.
// Total == 0 means indeterminate (e.g. pgBackRest has no step count).
type Progress struct {
	Kind    string // dumps | diff | full
	Label   string
	Detail  string
	Current int
	Total   int
	// Bytes means Current and Total are byte counts (a backup), so the bar shows a
	// percentage; otherwise they are items (databases).
	Bytes bool
}

// Percent is Current as a whole percentage of Total.
func (p Progress) Percent() int {
	if p.Total <= 0 {
		return 0
	}
	return int(int64(p.Current) * 100 / int64(p.Total))
}

// Active reports whether a job is running.
func (p Progress) Active() bool {
	return p.Kind != ""
}

// Determinate is true when Current/Total can drive a <progress value max> bar.
func (p Progress) Determinate() bool {
	return p.Total > 0
}

func (r *Runner) Progress() Progress {
	r.progressMu.Lock()
	defer r.progressMu.Unlock()
	return r.progress
}

func (r *Runner) setProgress(p Progress) {
	r.progressMu.Lock()
	defer r.progressMu.Unlock()
	r.progress = p
}

func (r *Runner) clearProgress() {
	r.progressMu.Lock()
	defer r.progressMu.Unlock()
	r.progress = Progress{}
}

// bumpProgress sets Current and Detail while keeping Kind/Label/Total.
func (r *Runner) bumpProgress(current int, detail string) {
	r.progressMu.Lock()
	defer r.progressMu.Unlock()
	r.progress.Current = current
	r.progress.Detail = detail
}
