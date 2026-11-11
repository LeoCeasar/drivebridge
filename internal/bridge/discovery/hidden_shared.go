package discovery

// HiddenSharedOnly describes an automatically discovered hidden entrance. A
// separate personal, visible-shared or explicit manual membership keeps the
// resource available, even when hidden-source synchronization is disabled.
func HiddenSharedOnly(entry Entry) bool {
	if entry.Manual || entry.Placement == "personal" {
		return false
	}
	hidden := false
	for _, membership := range entry.Memberships {
		switch membership {
		case Personal, SharedVisible, Manual:
			return false
		case SharedHidden:
			hidden = true
		}
	}
	return hidden
}
