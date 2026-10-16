// SPDX-License-Identifier: Apache-2.0

package jobs

// Progress is a snapshot of the job or restore currently running, for the UI bar.
// Total == 0 means indeterminate (e.g. pgBackRest has no step count).
type Progress struct {
	Kind    string // dumps | diff | full | restore
	Label   string
	Detail  string
	Current int
	Total   int
	Done    bool
	Err     string
}

// Active reports whether progress is worth showing (running or just finished with an error to display).
func (p Progress) Active() bool {
	return p.Kind != "" && (!p.Done || p.Err != "")
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

// ClearProgress clears the UI progress bar (e.g. after a finished restore is recorded).
func (r *Runner) ClearProgress() { r.clearProgress() }

// bumpProgress sets Current and Detail while keeping Kind/Label/Total.
func (r *Runner) bumpProgress(current int, detail string) {
	r.progressMu.Lock()
	defer r.progressMu.Unlock()
	r.progress.Current = current
	r.progress.Detail = detail
}

// finishProgress marks the current progress done (optionally with an error).
func (r *Runner) finishProgress(err error) {
	r.progressMu.Lock()
	defer r.progressMu.Unlock()
	if r.progress.Kind == "" {
		return
	}
	r.progress.Done = true
	if err != nil {
		r.progress.Err = firstLine(err.Error())
	} else if r.progress.Total > 0 {
		r.progress.Current = r.progress.Total
	}
}
