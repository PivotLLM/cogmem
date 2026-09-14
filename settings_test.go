// cogmem - Cognitive Memory
// License: MIT

package cogmem

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/PivotLLM/cogmem/consolidate"
)

// field reads a (possibly unexported) struct field by name. The worker and
// manager keep their resolved options private, so the settings mapping is
// checked by reading what the options actually set rather than by counting
// them.
func field(t *testing.T, v reflect.Value, name string) reflect.Value {
	t.Helper()
	if v.Kind() == reflect.Pointer {
		v = v.Elem()
	}
	f := v.FieldByName(name)
	if !f.IsValid() {
		t.Fatalf("%s has no field %q", v.Type(), name)
	}
	return f
}

func TestSettings_ComposerOptions(t *testing.T) {
	cases := []struct {
		name string
		in   PromptSettings
		want options
	}{
		{
			name: "zero means defaults",
			want: options{topKDomains: 3, maxChars: 4000, minConfidence: 0.65, fileMaxBytes: 256 * 1024, fileTotalMaxBytes: 512 * 1024},
		},
		{
			name: "every field maps",
			in:   PromptSettings{TopKDomains: 5, MaxChars: 1234, MinConfidence: 0.8, FileMaxBytes: 100, FileTotalMaxBytes: 300},
			want: options{topKDomains: 5, maxChars: 1234, minConfidence: 0.8, fileMaxBytes: 100, fileTotalMaxBytes: 300},
		},
		{
			name: "negative values are ignored",
			in:   PromptSettings{TopKDomains: -1, MaxChars: -1, MinConfidence: -0.5, FileMaxBytes: -1, FileTotalMaxBytes: -1},
			want: options{topKDomains: 3, maxChars: 4000, minConfidence: 0.65, fileMaxBytes: 256 * 1024, fileTotalMaxBytes: 512 * 1024},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := Settings{Prompt: tc.in}
			c := New(nil, s.ComposerOptions()...)
			got, want := c.opt, tc.want
			if got.topKDomains != want.topKDomains || got.maxChars != want.maxChars ||
				got.minConfidence != want.minConfidence || got.fileMaxBytes != want.fileMaxBytes ||
				got.fileTotalMaxBytes != want.fileTotalMaxBytes {
				t.Fatalf("options = %+v, want %+v", got, want)
			}
			if got.loadAttachment != nil {
				t.Fatal("ComposerOptions must not install a loader")
			}
		})
	}
	// IncludeDebugTrace is not a composer option: it is passed per Recall.
	s := Settings{Prompt: PromptSettings{IncludeDebugTrace: true}}
	if n := len(s.ComposerOptions()); n != 0 {
		t.Fatalf("IncludeDebugTrace produced %d composer options, want 0", n)
	}
}

func TestSettings_WorkerOptions(t *testing.T) {
	type resolved struct {
		batch          consolidate.BatchOptions
		proposeDomains bool
		autoPromote    bool
		eventDays      int
		retiredDays    int
		debugDump      string
		modelName      string
	}
	read := func(t *testing.T, w *consolidate.Worker) resolved {
		t.Helper()
		rv := reflect.ValueOf(w)
		bo := field(t, rv, "batchOpts")
		return resolved{
			batch: consolidate.BatchOptions{
				MaxMessages:     int(field(t, bo, "MaxMessages").Int()),
				MaxInputTokens:  int(field(t, bo, "MaxInputTokens").Int()),
				PerMessageChars: int(field(t, bo, "PerMessageChars").Int()),
				OverheadTokens:  int(field(t, bo, "OverheadTokens").Int()),
			},
			proposeDomains: field(t, rv, "proposeDomains").Bool(),
			autoPromote:    field(t, rv, "autoPromote").Bool(),
			eventDays:      int(field(t, rv, "eventDays").Int()),
			retiredDays:    int(field(t, rv, "retiredDays").Int()),
			debugDump:      field(t, rv, "debugDump").String(),
			modelName:      field(t, rv, "modelName").String(),
		}
	}
	defaults := consolidate.DefaultBatchOptions()
	cases := []struct {
		name      string
		s         Settings
		workspace string
		want      resolved
	}{
		{
			name:      "zero means defaults",
			workspace: "/ws",
			want:      resolved{batch: defaults},
		},
		{
			name: "every field maps",
			s: Settings{
				Consolidation: ConsolidationSettings{
					MaxBatchMessages: 7, MaxInputTokens: 900, PerMessageChars: 50,
					ProposeDomains: true, AutoPromote: true, DebugDump: true,
				},
				Retention: RetentionSettings{EventDays: 30, RetiredDays: 90},
			},
			workspace: "/ws",
			want: resolved{
				batch:          consolidate.BatchOptions{MaxMessages: 7, MaxInputTokens: 900, PerMessageChars: 50},
				proposeDomains: true, autoPromote: true,
				eventDays: 30, retiredDays: 90,
				debugDump: filepath.Join("/ws", "cogmem-dumps"),
			},
		},
		{
			name:      "debug dump needs a workspace",
			s:         Settings{Consolidation: ConsolidationSettings{DebugDump: true}},
			workspace: "",
			want:      resolved{batch: defaults},
		},
		{
			name:      "partial batch settings keep the other defaults",
			s:         Settings{Consolidation: ConsolidationSettings{MaxBatchMessages: 3}},
			workspace: "/ws",
			want:      resolved{batch: consolidate.BatchOptions{MaxMessages: 3, MaxInputTokens: defaults.MaxInputTokens, PerMessageChars: defaults.PerMessageChars}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := consolidate.NewWorker(nil, nil, tc.s.WorkerOptions(tc.workspace)...)
			if got := read(t, w); got != tc.want {
				t.Fatalf("worker = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestSettings_ManagerOptions(t *testing.T) {
	type resolved struct {
		everyN  int
		idle    time.Duration
		nightly string
		jitter  time.Duration
	}
	read := func(t *testing.T, m *consolidate.Manager) resolved {
		t.Helper()
		opt := field(t, reflect.ValueOf(m), "opt")
		return resolved{
			everyN:  int(field(t, opt, "everyNMessages").Int()),
			idle:    time.Duration(field(t, opt, "idle").Int()),
			nightly: field(t, opt, "nightlyAt").String(),
			jitter:  time.Duration(field(t, opt, "nightlyJitter").Int()),
		}
	}
	cases := []struct {
		name string
		in   ConsolidationSettings
		want resolved
	}{
		{name: "zero means every trigger off"},
		{
			name: "message and idle triggers",
			in:   ConsolidationSettings{EveryNMessages: 5, IdleMinutes: 10},
			want: resolved{everyN: 5, idle: 10 * time.Minute},
		},
		{
			name: "nightly defaults to 03:00 with 15 minutes of jitter",
			in:   ConsolidationSettings{Nightly: true},
			want: resolved{nightly: "03:00", jitter: 15 * time.Minute},
		},
		{
			name: "nightly at a configured time",
			in:   ConsolidationSettings{Nightly: true, NightlyAt: "22:30"},
			want: resolved{nightly: "22:30", jitter: 15 * time.Minute},
		},
		{
			name: "NightlyAt without Nightly is ignored",
			in:   ConsolidationSettings{NightlyAt: "22:30"},
		},
		{
			name: "non-positive counts disable",
			in:   ConsolidationSettings{EveryNMessages: -1, IdleMinutes: 0},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := consolidate.NewManager(nil, Settings{Consolidation: tc.in}.ManagerOptions()...)
			if got := read(t, m); got != tc.want {
				t.Fatalf("manager = %+v, want %+v", got, tc.want)
			}
		})
	}
}
