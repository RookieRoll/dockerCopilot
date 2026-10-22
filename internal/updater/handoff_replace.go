package updater

import "os"

// atomicReplaceHandoffFile publishes a fully synced sidecar without exposing
// partial contents to the startup script.
func atomicReplaceHandoffFile(source, destination string) error {
	return os.Rename(source, destination)
}
