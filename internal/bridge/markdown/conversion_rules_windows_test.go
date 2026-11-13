package markdown

import (
	"bytes"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsConversionRulesReadWhileDeleteHandleOpen(t *testing.T) {
	config := conversionRulesFixturePath(t)
	if err := EnsureConversionRulesFile(config); err != nil {
		t.Fatal(err)
	}
	path := ConversionRulesPath(config)
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.DELETE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	current, err := ReadConversionRulesFile(path)
	if err != nil || !bytes.Equal(current, DefaultConversionRulesJSON()) {
		t.Fatal("settings read failed during compatible rename", err)
	}
	if _, err := LoadConversionRules(path); err != nil {
		t.Fatal("startup rules load failed during compatible rename", err)
	}
}
