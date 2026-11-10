package cmd

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestPrivateProductControlRecognizesFlagsAndChildren(t *testing.T) {
	r := &cobra.Command{Use: "drivebridge"}
	r.PersistentFlags().String("config", "", "native config")
	p := &cobra.Command{Use: "feishu", Annotations: map[string]string{privateControlAnnotation: "true"}}
	p.PersistentFlags().String("bridge-config", "", "product config")
	p.AddCommand(&cobra.Command{Use: "_worker"})
	r.AddCommand(p, &cobra.Command{Use: "mount"})
	for _, args := range [][]string{{"feishu", "_worker"}, {"--config", "native.conf", "feishu", "--bridge-config", "my product.conf", "_worker"}} {
		if !privateControlCommand(r, args) {
			t.Fatalf("product control not isolated: %q", args)
		}
	}
	if privateControlCommand(r, []string{"--config", "feishu", "mount"}) {
		t.Fatal("native command changed by flag value")
	}
}
