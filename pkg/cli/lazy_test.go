package cli

import (
	"testing"
)

func TestRequiredRuntimes(t *testing.T) {
	tests := []struct {
		cmd  Command
		want []string
	}{
		{Command{Mode: ModeRun, Language: "python"}, []string{"python"}},
		{Command{Mode: ModeRun, Language: "go"}, []string{"go"}},
		{Command{Mode: ModeExec, Language: "javascript"}, []string{"javascript"}},
		{Command{Mode: ModeREPL}, []string{"python", "javascript", "java", "ruby"}},
	}
	for _, tt := range tests {
		got := RequiredRuntimes(tt.cmd)
		if len(got) != len(tt.want) {
			t.Errorf("RequiredRuntimes(%+v) = %v, want %v", tt.cmd, got, tt.want)
			continue
		}
		for i, g := range got {
			if g != tt.want[i] {
				t.Errorf("RequiredRuntimes(%+v)[%d] = %q, want %q", tt.cmd, i, g, tt.want[i])
			}
		}
	}
}
