package agentcore

import "testing"

// TestNewWorkObserver_PanicsOnEmptySignals pins that a WorkSignals literal
// setting no field true is rejected at construction, since an observer
// with nothing to look for could never distinguish absent work from work
// it was never told to watch for.
func TestNewWorkObserver_PanicsOnEmptySignals(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Error("NewWorkObserver(WorkSignals{}) did not panic, want panic")
		}
	}()
	NewWorkObserver(WorkSignals{})
}

// TestWorkObserver_NilReceiverPanics pins that every WorkObserver method
// panics on a nil receiver rather than returning a zero value an
// uninitialized call site never asked for.
func TestWorkObserver_NilReceiverPanics(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		call func(*WorkObserver)
	}{
		{name: "ObserveAssistantOutput", call: func(o *WorkObserver) { o.ObserveAssistantOutput() }},
		{name: "ObserveToolActivity", call: func(o *WorkObserver) { o.ObserveToolActivity() }},
		{name: "Observed", call: func(o *WorkObserver) { o.Observed() }},
		{name: "Report", call: func(o *WorkObserver) { o.Report() }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			defer func() {
				if recover() == nil {
					t.Errorf("(*WorkObserver)(nil).%s() did not panic, want panic", tt.name)
				}
			}()
			var o *WorkObserver
			tt.call(o)
		})
	}
}

// TestWorkObserver_ObservePanicsOnUndeclaredSignal pins that observing a
// signal the constructing WorkSignals literal did not declare panics,
// since recording it would misrepresent what the adapter's runtime
// actually reported.
func TestWorkObserver_ObservePanicsOnUndeclaredSignal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		signals WorkSignals
		call    func(*WorkObserver)
	}{
		{
			name:    "ObserveAssistantOutput without AssistantOutput declared",
			signals: WorkSignals{ToolActivity: true},
			call:    func(o *WorkObserver) { o.ObserveAssistantOutput() },
		},
		{
			name:    "ObserveToolActivity without ToolActivity declared",
			signals: WorkSignals{AssistantOutput: true},
			call:    func(o *WorkObserver) { o.ObserveToolActivity() },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			o := NewWorkObserver(tt.signals)

			defer func() {
				if recover() == nil {
					t.Errorf("%s did not panic, want panic", tt.name)
				}
			}()
			tt.call(o)
		})
	}
}

// TestWorkObserver_ReportDetail pins Report's three distinct detail
// strings, one per WorkSignals declaration shape, when nothing was
// observed, and that Report returns WorkPresent with an empty detail once
// a declared signal was observed.
func TestWorkObserver_ReportDetail(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		signals    WorkSignals
		observe    func(*WorkObserver)
		wantReport WorkReport
		wantDetail string
	}{
		{
			name:       "both signals declared, unobserved",
			signals:    WorkSignals{AssistantOutput: true, ToolActivity: true},
			wantReport: WorkAbsent,
			wantDetail: workAbsentBothDetail,
		},
		{
			name:       "assistant output only, unobserved",
			signals:    WorkSignals{AssistantOutput: true},
			wantReport: WorkAbsent,
			wantDetail: workAbsentAssistantOnlyDetail,
		},
		{
			name:       "tool activity only, unobserved",
			signals:    WorkSignals{ToolActivity: true},
			wantReport: WorkAbsent,
			wantDetail: workAbsentToolOnlyDetail,
		},
		{
			name:       "both signals declared, assistant output observed",
			signals:    WorkSignals{AssistantOutput: true, ToolActivity: true},
			observe:    func(o *WorkObserver) { o.ObserveAssistantOutput() },
			wantReport: WorkPresent,
			wantDetail: "",
		},
		{
			name:       "assistant output only, observed",
			signals:    WorkSignals{AssistantOutput: true},
			observe:    func(o *WorkObserver) { o.ObserveAssistantOutput() },
			wantReport: WorkPresent,
			wantDetail: "",
		},
		{
			name:       "tool activity only, observed",
			signals:    WorkSignals{ToolActivity: true},
			observe:    func(o *WorkObserver) { o.ObserveToolActivity() },
			wantReport: WorkPresent,
			wantDetail: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			o := NewWorkObserver(tt.signals)
			if tt.observe != nil {
				tt.observe(o)
			}

			gotReport, gotDetail := o.Report()

			if gotReport != tt.wantReport {
				t.Errorf("Report() report = %v, want %v", gotReport, tt.wantReport)
			}
			if gotDetail != tt.wantDetail {
				t.Errorf("Report() detail = %q, want %q", gotDetail, tt.wantDetail)
			}
		})
	}
}

// TestWorkObserver_ReportMonotonic pins that once a declared signal is
// observed, Report never reports WorkAbsent again for that observer, even
// across a further observation of another declared signal that would
// otherwise be expected to change it.
func TestWorkObserver_ReportMonotonic(t *testing.T) {
	t.Parallel()

	o := NewWorkObserver(WorkSignals{AssistantOutput: true, ToolActivity: true})
	o.ObserveAssistantOutput()

	gotReport, gotDetail := o.Report()
	if gotReport != WorkPresent {
		t.Fatalf("Report() report = %v, want %v after ObserveAssistantOutput", gotReport, WorkPresent)
	}
	if gotDetail != "" {
		t.Fatalf("Report() detail = %q, want empty after ObserveAssistantOutput", gotDetail)
	}

	o.ObserveToolActivity()

	gotReport, gotDetail = o.Report()
	if gotReport != WorkPresent {
		t.Errorf("Report() report = %v, want %v after a second, later observation", gotReport, WorkPresent)
	}
	if gotDetail != "" {
		t.Errorf("Report() detail = %q, want empty after a second, later observation", gotDetail)
	}
}

// TestWorkObserver_Observed pins Observed as the adapters use it: a guard
// read before the turn finalizes, false until a declared signal is
// observed and true afterward.
func TestWorkObserver_Observed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		signals WorkSignals
		observe func(*WorkObserver)
	}{
		{
			name:    "assistant output",
			signals: WorkSignals{AssistantOutput: true},
			observe: func(o *WorkObserver) { o.ObserveAssistantOutput() },
		},
		{
			name:    "tool activity",
			signals: WorkSignals{ToolActivity: true},
			observe: func(o *WorkObserver) { o.ObserveToolActivity() },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			o := NewWorkObserver(tt.signals)

			if o.Observed() {
				t.Fatal("Observed() = true before any observation, want false")
			}

			tt.observe(o)

			if !o.Observed() {
				t.Error("Observed() = false after observation, want true")
			}
		})
	}
}
