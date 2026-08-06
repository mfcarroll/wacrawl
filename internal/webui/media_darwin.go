//go:build darwin

package webui

import (
	"os"
	"syscall"
)

// sfDataless mirrors SF_DATALESS from <sys/stat.h>: the file's bytes live with
// a dataless-file provider (for WhatsApp media, content that has not been
// downloaded yet) and reading it would block until macOS materializes it.
const sfDataless = 0x40000000

func fileMaterialized(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return true
	}
	return stat.Flags&sfDataless == 0
}

// openMediaFile opens with O_NONBLOCK so a dataless file fails fast instead of
// stalling the request goroutine on materialization; the flag has no effect on
// reads of ordinary regular files.
func openMediaFile(root *os.Root, path string) (*os.File, error) {
	return root.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0) // #nosec G304 G703 -- containedMediaPath resolves and confines the path before this rooted call.
}

// openMediaFileBlocking omits O_NONBLOCK so that a dataless file is materialized
// by its provider rather than failing. Used when the archive's media lives in a
// cloud-synced folder (Google Drive, iCloud), where placeholders are the normal
// resting state and fetching on demand is the intended behaviour.
func openMediaFileBlocking(root *os.Root, path string) (*os.File, error) {
	return root.OpenFile(path, os.O_RDONLY, 0) // #nosec G304 G703 -- containedMediaPath resolves and confines the path before this rooted call.
}
