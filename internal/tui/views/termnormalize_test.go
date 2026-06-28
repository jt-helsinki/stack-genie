package views

import "testing"

func TestNormalizeTerminalOutput(test *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "plain lines pass through",
			in:   "line one\nline two\n",
			want: "line one\nline two\n",
		},
		{
			name: "carriage return overwrites in place",
			in:   "progress 10%\rprogress 90%\rprogress 100%",
			want: "progress 100%",
		},
		{
			name: "carriage return then shorter keeps tail of old",
			in:   "downloading.....\rdone",
			want: "doneloading.....", // "done" overwrites the first 4 chars; the rest of the longer line remains
		},
		{
			name: "erase to end of line after carriage return",
			in:   "downloading.....\rdone\x1b[K",
			want: "done",
		},
		{
			name: "strip colour (SGR) leaving plain text",
			in:   "\x1b[31mred\x1b[0m text",
			want: "red text",
		},
		{
			name: "cursor up rewrites a previous row (multi-line progress)",
			// two layers printed, then cursor up 2 and both rewritten to 100%.
			in:   "layer1 0%\nlayer2 0%\n\x1b[2Alayer1 100%\nlayer2 100%\n",
			want: "layer1 100%\nlayer2 100%\n",
		},
	}
	for _, testCase := range cases {
		if got := normalizeTerminalOutput(testCase.in); got != testCase.want {
			test.Errorf("%s:\n in:   %q\n got:  %q\n want: %q", testCase.name, testCase.in, got, testCase.want)
		}
	}
}
