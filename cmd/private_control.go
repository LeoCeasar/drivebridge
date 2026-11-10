package cmd

import "github.com/spf13/cobra"

const privateControlAnnotation = "drivebridge-private-control"

// Product processes have their own authenticated control plane. Inherited
// RCLONE_RC/metrics settings must not create an additional listener before RunE.
func privateControlCommand(root *cobra.Command, args []string) bool {
	c, _, _ := root.Find(args)
	for ; c != nil; c = c.Parent() {
		if c.Annotations[privateControlAnnotation] == "true" {
			return true
		}
	}
	return false
}
