package ops

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestJournalScriptSyntax(t *testing.T) {
	s, err := Script("journal", Params{JournalMB: 200, JournalDays: 14})
	if err != nil {
		t.Fatal(err)
	}
	for _, must := range []string{"SystemMaxUse=200M", "MaxRetentionSec=14day", "--vacuum-size=200M", "--vacuum-time=14d"} {
		if !strings.Contains(s, must) {
			t.Fatalf("脚本里应含 %q:\n%s", must, s)
		}
	}
	f, _ := os.CreateTemp("", "j*.sh")
	defer os.Remove(f.Name())
	f.WriteString(s)
	f.Close()
	if out, err := exec.Command("bash", "-n", f.Name()).CombinedOutput(); err != nil {
		t.Fatalf("脚本语法错误: %v\n%s", err, out)
	}
}
