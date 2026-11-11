package larkcli

import (
	"context"
	"reflect"
	"testing"

	"golang.org/x/sys/windows"
)

func TestCLIChildIsHiddenAndKeepsLiteralArguments(t *testing.T) {
	r := NewDirect(`C:\Program Files\DriveBridge\lark-cli.exe`)
	args := []string{"api", "GET", "/open-apis/drive/v1/files", "--data", `{"name":"a & b"}`}
	cmd := r.command(context.Background(), args, `C:\Users\test user`)
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.HideWindow || cmd.SysProcAttr.CreationFlags&windows.CREATE_NO_WINDOW == 0 || cmd.SysProcAttr.CreationFlags&windows.DETACHED_PROCESS != 0 {
		t.Fatal("tray-owned official CLI child can open a console or outlive cancellation")
	}
	if !reflect.DeepEqual(cmd.Args[1:], args) || cmd.Dir != `C:\Users\test user` || cmd.Env == nil {
		t.Fatal("hiding the child changed its literal arguments, environment or working directory")
	}
}
