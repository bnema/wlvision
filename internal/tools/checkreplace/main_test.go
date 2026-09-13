package main

import (
	"strings"
	"testing"
)

func TestProblems(t *testing.T) {
	const (
		wlturbo = "github.com/bnema/wlturbo"
		devices = "github.com/bnema/libwldevices-go"
	)

	tests := []struct {
		name  string
		found map[string][]finding
		want  expectations
		// wantComplaints is how many complaints the graph must produce, and
		// wantSubstrings are phrases the complaints together must mention. Zero
		// complaints means the module graph is accepted.
		wantComplaints int
		wantSubstrings []string
	}{
		{
			name:  "a clean release module has no replacements",
			found: map[string][]finding{},
		},
		{
			name:           "any replacement fails the release guard",
			found:          map[string][]finding{wlturbo: {{dir: "wlvision", target: "../wlturbo"}}},
			wantComplaints: 1,
			wantSubstrings: []string{"found replace", "../wlturbo"},
		},
		{
			name: "the expected replacements are present",
			found: map[string][]finding{
				wlturbo: {{dir: "wlvision", target: "../wlturbo"}},
				devices: {{dir: "wlvision", target: "../libwldevices-go"}},
			},
			want: expectations{{old: wlturbo, new: "../wlturbo"}, {old: devices, new: "../libwldevices-go"}},
		},
		{
			// The defect this check exists to catch: the module path is right
			// and the checkout it points at is not.
			name:           "a replacement pointing at the wrong checkout fails",
			found:          map[string][]finding{wlturbo: {{dir: "wlvision", target: "/elsewhere/wlturbo"}}},
			want:           expectations{{old: wlturbo, new: "../wlturbo"}},
			wantComplaints: 1,
			wantSubstrings: []string{"expected replace", "../wlturbo", "/elsewhere/wlturbo"},
		},
		{
			name:           "a missing replacement fails",
			found:          map[string][]finding{},
			want:           expectations{{old: wlturbo, new: "../wlturbo"}},
			wantComplaints: 1,
			wantSubstrings: []string{"expected replace", "is missing"},
		},
		{
			name: "an unexpected replacement fails",
			found: map[string][]finding{
				wlturbo: {{dir: "wlvision", target: "../wlturbo"}},
				devices: {{dir: "wlvision", target: "../libwldevices-go"}},
			},
			want:           expectations{{old: wlturbo, new: "../wlturbo"}},
			wantComplaints: 1,
			wantSubstrings: []string{"unexpected replace", devices},
		},
		{
			// Replacements are not transitive, so the same module path is
			// replaced in more than one module graph; one match is enough.
			name: "the same module replaced in two graphs matches once",
			found: map[string][]finding{
				wlturbo: {
					{dir: "libwldevices-go", target: "/wrong/checkout"},
					{dir: "wlvision", target: "../wlturbo"},
				},
			},
			want: expectations{{old: wlturbo, new: "../wlturbo"}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			complaints := problems(test.found, test.want)

			if len(complaints) != test.wantComplaints {
				t.Fatalf("complaints = %v, want %d of them", complaints, test.wantComplaints)
			}
			joined := strings.Join(complaints, "\n")
			for _, want := range test.wantSubstrings {
				if !strings.Contains(joined, want) {
					t.Errorf("complaints = %q, want them to mention %q", joined, want)
				}
			}
		})
	}
}
